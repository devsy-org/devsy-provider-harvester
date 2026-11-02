package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func syntheticPublicKey() string {
	b := []byte{}
	for _, part := range [][]byte{[]byte("ssh-ed25519"), make([]byte, 32)} {
		b = binary.BigEndian.AppendUint32(b, uint32(len(part)))
		b = append(b, part...)
	}
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(b) + " synthetic-test-only \"quoted\": [comment] # inert"
}

func TestBundledProfile(t *testing.T) {
	t.Setenv("HARVESTER_CLOUD_INIT_PROFILE", "ubuntu-24.04-docker-v1")
	t.Setenv("HARVESTER_SSH_USER", "developer")
	t.Setenv("HARVESTER_VM_NAME", "dev-test")
	t.Setenv("HARVESTER_SSH_PUBLIC_KEY", syntheticPublicKey())
	c := load()
	data, err := resolveUserData(c)
	if err != nil || !strings.Contains(data, "docker-ce-cli") || !strings.Contains(data, `name: "developer"`) || !strings.Contains(data, `hostname: "dev-test"`) {
		t.Fatalf("profile not rendered: %v", err)
	}
	for _, setter := range []func(*config){func(c *config) { c.UserData = "SECRET" }, func(c *config) { c.UserDataFile = "SECRET" }, func(c *config) { c.CloudInitTemplate = "SECRET" }} {
		conflict := c
		setter(&conflict)
		_, err := resolveUserData(conflict)
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("source conflict not safely rejected")
		}
	}
	for _, variable := range []string{"HARVESTER_SSH_USER", "HARVESTER_VM_NAME", "HARVESTER_SSH_PUBLIC_KEY", "HARVESTER_CLOUD_INIT_PROFILE"} {
		t.Run(variable, func(t *testing.T) {
			t.Setenv(variable, "SECRET\nruncmd: [bad]")
			_, err := resolveUserData(load())
			if err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatal("invalid input not safely rejected")
			}
		})
	}
	for _, user := range []string{"root", "", "-option", "name with spaces"} {
		bad := c
		bad.SSHUser = user
		if _, err := resolveUserData(bad); err == nil {
			t.Fatal("unsafe user accepted")
		}
	}
	for _, key := range []string{"", "ssh-ed25519 YQ==", syntheticPublicKey() + "\nssh-rsa AAAA"} {
		bad := c
		bad.SSHPublicKey = key
		if _, err := resolveUserData(bad); err == nil {
			t.Fatal("invalid key accepted")
		}
	}
}

func TestBundledReadinessBudget(t *testing.T) {
	t.Setenv("HARVESTER_CLOUD_INIT_PROFILE", "ubuntu-24.04-docker-v1")
	if guestReadyTimeout(load()) != 15*time.Minute {
		t.Fatal("first boot needs bounded package installation budget")
	}
	t.Setenv("HARVESTER_CLOUD_INIT_PROFILE", "")
	if guestReadyTimeout(load()) != 90*time.Second {
		t.Fatal("custom source readiness changed")
	}
}

// Export only synthetic data for the container schema check; never operator config.
func TestBundledSchemaFixture(t *testing.T) {
	path := os.Getenv("HARVESTER_TEST_SCHEMA_OUTPUT")
	if path == "" {
		t.Skip("schema fixture export not requested")
	}
	data, err := renderBundledProfile(config{CloudInitProfile: "ubuntu-24.04-docker-v1", SSHUser: "developer", VMName: "devsy-schema-test", SSHPublicKey: syntheticPublicKey()})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestGuestProbeDiagnosticScope(t *testing.T) {
	for _, profile := range []string{"", "ubuntu-24.04-docker-v1"} {
		for _, diagnostic := range []string{"Host key verification failed SECRET_BOOTSTRAP_LOG", "Permission denied SECRET_BOOTSTRAP_LOG"} {
			t.Run(profile+"/"+strings.Fields(diagnostic)[0], func(t *testing.T) {
				lifecycleFixture(t)
				t.Setenv("HARVESTER_TEST_SSH_ERROR_OUTPUT", diagnostic)
				err := probeGuest(context.Background(), config{SSHPort: "22", SSHUser: "ubuntu", CloudInitProfile: profile}, "192.0.2.10")
				if err == nil {
					t.Fatal("probe error missing")
				}
				if got, want := strings.Contains(err.Error(), diagnostic), profile == ""; got != want {
					t.Fatal("guest diagnostic visibility differs from source mode")
				}
				if got, want := errors.Is(err, errHostKeyVerification), strings.HasPrefix(diagnostic, "Host key"); got != want {
					t.Fatal("host-key failure classification lost")
				}
			})
		}
	}
}

func TestBundledProbeWaitsForCloudInitAndComponents(t *testing.T) {
	lifecycleFixture(t)
	path := filepath.Join(t.TempDir(), "ssh-calls.json")
	t.Setenv("HARVESTER_TEST_SSH_CALLS", path)
	if err := probeGuest(context.Background(), config{SSHPort: "22", SSHUser: "ubuntu", CloudInitProfile: "ubuntu-24.04-docker-v1"}, "192.0.2.10"); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []string{"cloud-init status --wait", "docker info", "docker compose version", "docker buildx version", "systemctl is-active --quiet qemu-guest-agent"} {
		if !strings.Contains(string(calls), check) {
			t.Fatalf("missing readiness check: %s", check)
		}
	}
}

func TestBundledBootstrapHasHardDeadline(t *testing.T) {
	data, err := renderBundledProfile(config{CloudInitProfile: "ubuntu-24.04-docker-v1", SSHUser: "developer", VMName: "test", SSHPublicKey: syntheticPublicKey()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(data, "[timeout, '--kill-after=30', '840', /usr/local/sbin/devsy-docker-bootstrap]") {
		t.Fatal("bootstrap must terminate processes that ignore TERM within readiness budget")
	}
}

func TestBundledManifestFitsKubeVirtInlineLimit(t *testing.T) {
	c := load()
	c.CloudInitProfile = "ubuntu-24.04-docker-v1"
	c.SSHUser = "developer"
	c.VMName = "proof"
	c.SSHPublicKey = syntheticPublicKey()
	data, err := resolveUserData(c)
	if err != nil {
		t.Fatal(err)
	}
	c.UserData = data
	vm, err := buildVMManifest(c, imageInfo{Format: "qcow2"})
	if err != nil {
		t.Fatal(err)
	}
	if vm.Metadata.Annotations["devsy.sh/cloud-init-profile"] != c.CloudInitProfile {
		t.Fatal("bundled VM must retain its creation profile for reattachment")
	}
	cloud := vm.Spec.Template.Spec.Volumes[len(vm.Spec.Template.Spec.Volumes)-1].CloudInitNoCloud
	if cloud["userData"] != "" {
		t.Fatal("uncompressed profile exceeds KubeVirt inline limit")
	}
	compressed, err := base64.StdEncoding.DecodeString(cloud["userDataBase64"])
	if err != nil {
		t.Fatal(err)
	}
	if len(compressed) > 2048 {
		t.Fatal("compressed profile exceeds KubeVirt inline limit")
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	}()
	decoded, err := io.ReadAll(reader)
	if err != nil || string(decoded) != data {
		t.Fatal("compressed profile did not round-trip")
	}
}

func TestBundledRejectsOversizedCompressedKeyBeforeProvisioning(t *testing.T) {
	// Deterministic public test noise, not credentials. A repeated string would compress.
	var comment strings.Builder
	for n := 0; n < 150; n++ {
		sum := sha256.Sum256([]byte(strconv.Itoa(n)))
		comment.WriteString(base64.StdEncoding.EncodeToString(sum[:]))
	}
	_, err := resolveUserData(config{CloudInitProfile: "ubuntu-24.04-docker-v1", SSHUser: "developer", VMName: "proof", SSHPublicKey: syntheticPublicKey() + comment.String()})
	if err == nil || !strings.Contains(err.Error(), "2 KiB") {
		t.Fatal("oversized compressed profile must fail before provisioning")
	}
}

func TestBundledProfileRejectsISOBeforeProvisioning(t *testing.T) {
	exe := lifecycleFixture(t)
	t.Setenv("HARVESTER_CLOUD_INIT_PROFILE", "ubuntu-24.04-docker-v1")
	t.Setenv("HARVESTER_USER_DATA", "")
	t.Setenv("HARVESTER_SSH_PUBLIC_KEY", syntheticPublicKey())
	t.Setenv("HARVESTER_IMAGE_TYPE", "iso")
	t.Setenv("HARVESTER_STORAGE_CLASS", "test-storage")
	out, stderr, err := runLifecycleFixture(t, exe, "create")
	if err == nil || !strings.Contains(stderr, "cloud image, not an ISO") || strings.Contains(out, "Provisioning root disk") {
		t.Fatalf("ISO bootstrap reached resource creation: %q %q %v", out, stderr, err)
	}
}

func TestReusedVMReadinessFollowsItsOriginalProfile(t *testing.T) {
	for _, bundled := range []bool{false, true} {
		t.Run(strconv.FormatBool(bundled), func(t *testing.T) {
			exe := lifecycleFixture(t)
			data := existingVMFixture
			if bundled {
				data = strings.Replace(data, `"uid":"existing-vm-uid"`, `"uid":"existing-vm-uid","annotations":{"devsy.sh/cloud-init-profile":"ubuntu-24.04-docker-v1"}`, 1)
				t.Setenv("HARVESTER_CLOUD_INIT_PROFILE", "")
			} else {
				t.Setenv("HARVESTER_CLOUD_INIT_PROFILE", "ubuntu-24.04-docker-v1")
			}
			vmFile, callsFile := existingVMFiles(t, data)
			sshFile := filepath.Join(t.TempDir(), "ssh.json")
			t.Setenv("HARVESTER_TEST_SSH_CALLS", sshFile)
			_, stderr, err := runLifecycleFixture(t, exe, "create")
			if err != nil {
				t.Fatalf("reuse failed: %q %v", stderr, err)
			}
			calls, err := os.ReadFile(sshFile)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(calls), "cloud-init status") != bundled {
				t.Fatalf("saved provider settings changed existing VM readiness: %s", calls)
			}
			assertNoDefinitionWrites(t, vmFile, callsFile, data)
		})
	}
}

func TestBundledKeysRejectInvalidCryptographicValues(t *testing.T) {
	for _, parts := range [][][]byte{
		{[]byte("ssh-rsa"), []byte{0}, make([]byte, 256)},
		{[]byte("ecdsa-sha2-nistp256"), []byte("nistp256"), append([]byte{4}, make([]byte, 64)...)},
	} {
		var wire []byte
		for _, part := range parts {
			wire = binary.BigEndian.AppendUint32(wire, uint32(len(part)))
			wire = append(wire, part...)
		}
		key := string(parts[0]) + " " + base64.StdEncoding.EncodeToString(wire)
		if validPublicKey(key) {
			t.Fatalf("invalid public key values accepted for %s", parts[0])
		}
	}
}

func TestBundledTerminalFailureStopsReadinessImmediately(t *testing.T) {
	lifecycleFixture(t)
	t.Setenv("HARVESTER_TEST_SSH_EXIT_CODE", "42")
	ctxStart := time.Now()
	err := waitForGuest(config{SSHPort: "22", SSHUser: "ubuntu", CloudInitProfile: "ubuntu-24.04-docker-v1"})
	if !errors.Is(err, errBundledBootstrap) || time.Since(ctxStart) > 5*time.Second {
		t.Fatalf("terminal bootstrap failure was retried: %v", err)
	}
}

func TestBundledProbeChecksBootstrapFailureBeforeDocker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("guest POSIX shell executes on Linux, not the Windows client")
	}
	lifecycleFixture(t)
	file := filepath.Join(t.TempDir(), "ssh.json")
	t.Setenv("HARVESTER_TEST_SSH_CALLS", file)
	if err := probeGuest(context.Background(), config{SSHPort: "22", SSHUser: "ubuntu", CloudInitProfile: "ubuntu-24.04-docker-v1"}, "192.0.2.10"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	if err = json.Unmarshal(data, &args); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// Simulate failed cloud-init before Docker installation. The actual probe must
	// return the terminal signal instead of waiting for a nonexistent Docker CLI.
	if err = os.WriteFile(filepath.Join(dir, "cloud-init"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", args[len(args)-1])
	cmd.Env = []string{"PATH=" + dir}
	err = cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 42 {
		t.Fatalf("bootstrap failure hidden behind Docker: %v", err)
	}
}

func TestBundledKeysAcceptGeneratedRSAAndECDSA(t *testing.T) {
	var candidates [][][]byte
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	candidates = append(candidates, [][]byte{[]byte("ssh-rsa"), {1, 0, 1}, append([]byte{0}, key.N.Bytes()...)})
	for _, curve := range []elliptic.Curve{elliptic.P256(), elliptic.P384(), elliptic.P521()} {
		key, err := ecdsa.GenerateKey(curve, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		name := "nistp" + strconv.Itoa(curve.Params().BitSize)
		publicKey, err := key.PublicKey.ECDH()
		if err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, [][]byte{[]byte("ecdsa-sha2-" + name), []byte(name), publicKey.Bytes()})
	}
	for _, parts := range candidates {
		var wire []byte
		for _, part := range parts {
			wire = binary.BigEndian.AppendUint32(wire, uint32(len(part)))
			wire = append(wire, part...)
		}
		key := string(parts[0]) + " " + base64.StdEncoding.EncodeToString(wire)
		if !validPublicKey(key) {
			t.Fatalf("valid generated key rejected: %s", parts[0])
		}
	}
}
