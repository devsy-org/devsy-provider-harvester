package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestManagedCustomSourceSubstitutesOnlyExplicitPublicKeyMarker(t *testing.T) {
	c := config{SSHMode: "managed", SSHPublicKey: syntheticPublicKey(), UserData: "#cloud-config\nssh_authorized_keys:\n  - {{HARVESTER_SSH_PUBLIC_KEY}}\n"}
	data, err := resolveUserData(c)
	if err != nil || !strings.Contains(data, c.SSHPublicKey) || strings.Contains(data, managedPublicKeyMarker) {
		t.Fatalf("public-key substitution failed: %v", err)
	}
	c.UserData = "#cloud-config\nusers: [default]\n"
	if _, err := resolveUserData(c); err == nil || !strings.Contains(err.Error(), "marker") {
		t.Fatal("managed custom source without public-key marker accepted")
	}
	c.SSHMode = "manual"
	if data, err := resolveUserData(c); err != nil || data != c.UserData {
		t.Fatal("manual custom source was changed")
	}
}

func TestGuestProbeRejectsMissingOrWrongEngineExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the probe executes in the POSIX guest")
	}
	for _, missing := range []bool{true, false} {
		t.Run(map[bool]string{true: "missing", false: "wrong-engine"}[missing], func(t *testing.T) {
			lifecycleFixture(t)
			dir := t.TempDir()
			engine := filepath.Join(dir, "engine")
			used := filepath.Join(dir, "engine-used")
			if !missing {
				body := "#!/bin/sh\nif [ \"$1\" = --version ]; then printf 'Docker version test\\n'; else printf used > " + quoteGuestWord(used) + "; fi\n"
				if err := os.WriteFile(engine, []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			file := filepath.Join(dir, "ssh.json")
			t.Setenv("HARVESTER_TEST_SSH_CALLS", file)
			if err := probeGuest(context.Background(), config{ContainerRuntime: "podman", ContainerCommand: engine, SSHUser: "developer", SSHPort: "22"}, "192.0.2.10"); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var args []string
			if err := json.Unmarshal(data, &args); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/sh", "-c", args[len(args)-1])
			cmd.Env = []string{"PATH=" + dir}
			err = cmd.Run()
			want := 46
			if missing {
				want = 43
			}
			if err == nil || cmd.ProcessState.ExitCode() != want {
				t.Fatalf("probe accepted the missing/wrong engine: %v", err)
			}
			if _, err := os.Stat(used); !os.IsNotExist(err) {
				t.Fatal("probe used a mismatched engine")
			}
		})
	}
}

func TestManagedCustomSourceChecksExpandedSizeBeforeResources(t *testing.T) {
	c := config{SSHMode: "managed", SSHPublicKey: syntheticPublicKey(), UserData: strings.Repeat("x", 65500) + managedPublicKeyMarker}
	if _, err := resolveUserData(c); err == nil || !strings.Contains(err.Error(), "64 KiB") {
		t.Fatal("expanded source passed the input-size boundary")
	}
	noise := make([]byte, 4096)
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}
	c.UserData = "#cloud-config\n#" + base64.StdEncoding.EncodeToString(noise) + "\nssh_authorized_keys: [" + managedPublicKeyMarker + "]\n"
	if _, err := resolveUserData(c); err == nil {
		t.Fatal("oversized encoded source accepted")
	}
}

func TestRockyPodmanBootstrapAndRuntimeValidation(t *testing.T) {
	c := config{CloudInitProfile: "rocky-9-podman-v1", ContainerRuntime: "podman", SSHUser: "developer", VMName: "podman-fixture", SSHPublicKey: syntheticPublicKey()}
	data, err := renderBundledProfile(c)
	if err != nil || !strings.Contains(data, rootfulPodmanCommand) || strings.Contains(data, "enable-linger") || strings.Contains(data, "docker-ce") {
		t.Fatalf("Podman bootstrap unavailable: %v", err)
	}
	c.ContainerRuntime = "docker"
	if _, err := renderBundledProfile(c); err == nil || !strings.Contains(strings.ToLower(err.Error()), "runtime") {
		t.Fatal("mismatched profile/runtime accepted")
	}
}

func TestPodmanProbeUsesConfiguredEngine(t *testing.T) {
	lifecycleFixture(t)
	file := filepath.Join(t.TempDir(), "ssh.jsonl")
	t.Setenv("HARVESTER_TEST_SSH_CALLS", file)
	c := config{SSHUser: "developer", SSHPort: "22", CloudInitProfile: "rocky-9-podman-v1", ContainerRuntime: "podman"}
	if err := probeGuest(context.Background(), c, "192.0.2.10"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	if err := json.Unmarshal(data, &args); err != nil {
		t.Fatal(err)
	}
	command := args[len(args)-1]
	if !strings.Contains(command, rootfulPodmanCommand+" info") || strings.Contains(command, "export XDG_RUNTIME_DIR") || strings.Contains(command, "docker info") || strings.Contains(command, "docker compose") {
		t.Fatal("readiness did not select the preset's engine identity")
	}
}

func TestBundledCloudInitStatusUsesNoninteractivePrivilege(t *testing.T) {
	for _, profile := range []string{"ubuntu-24.04-docker-v1", "rocky-9-podman-v1"} {
		t.Run(profile, func(t *testing.T) {
			lifecycleFixture(t)
			file := filepath.Join(t.TempDir(), "ssh.jsonl")
			t.Setenv("HARVESTER_TEST_SSH_CALLS", file)
			engine := "docker"
			if profile == "rocky-9-podman-v1" {
				engine = "podman"
			}
			if err := probeGuest(context.Background(), config{SSHUser: "developer", SSHPort: "22", CloudInitProfile: profile, ContainerRuntime: engine}, "192.0.2.10"); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var args []string
			if err := json.Unmarshal(data, &args); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(args[len(args)-1], "sudo -n cloud-init status --wait") {
				t.Fatal("cloud-init status can require root to read its cache on Rocky")
			}
		})
	}
}
