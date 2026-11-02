package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func mustFixture(err error) {
	if err != nil {
		os.Exit(2)
	}
}

func mustFixtureResult[T any](value T, err error) T {
	mustFixture(err)
	return value
}

func init() {
	if os.Getenv("HARVESTER_TEST_LIFECYCLE") != "1" {
		return
	}
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	switch name {
	case "fake-kubectl":
		args := strings.Join(os.Args[1:], " ")
		if state := os.Getenv("HARVESTER_TEST_DELETE_STATE"); state != "" && (strings.HasPrefix(args, "get virtualmachine ") || strings.HasPrefix(args, "get pvc ") || strings.HasPrefix(args, "delete --raw ")) {
			deleteFixture(state, os.Args[1:])
			os.Exit(0)
		}
		if file := os.Getenv("HARVESTER_TEST_KUBECTL_CALLS"); file != "" {
			f := mustFixtureResult(os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600))
			mustFixture(json.NewEncoder(f).Encode(os.Args[1:]))
			mustFixture(f.Close())
		}
		switch {
		case strings.HasPrefix(args, "delete "):
			if file := os.Getenv("HARVESTER_TEST_DELETE_CALLS"); file != "" {
				f := mustFixtureResult(os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600))
				mustFixture(json.NewEncoder(f).Encode(os.Args[1:]))
				mustFixture(f.Close())
			}
			for _, target := range strings.Split(os.Getenv("HARVESTER_TEST_DELETE_FAILURES"), ",") {
				if target != "" && strings.Contains(args, " "+target+" ") {
					mustFixtureResult(os.Stderr.WriteString("permission denied deleting " + target))
					os.Exit(7)
				}
			}
		case strings.Contains(args, "get virtualmachineimages "):
			if os.Getenv("HARVESTER_TEST_IMAGE_LIST_FAILURE") == "1" {
				mustFixtureResult(os.Stderr.WriteString("forbidden listing images"))
				os.Exit(1)
			}
			data, err := os.ReadFile(os.Getenv("HARVESTER_TEST_IMAGE_LIST"))
			if err != nil {
				os.Exit(2)
			}
			mustFixtureResult(os.Stdout.Write(data))
		case strings.Contains(args, "get virtualmachineimage "):
			if os.Getenv("HARVESTER_TEST_IMAGE_FAILURE") == "1" {
				mustFixtureResult(os.Stderr.WriteString("image is unavailable"))
				os.Exit(1)
			}
			if file := os.Getenv("HARVESTER_TEST_IMAGE_OBJECT"); file != "" {
				data, err := os.ReadFile(file)
				if err != nil {
					os.Exit(2)
				}
				mustFixtureResult(os.Stdout.Write(data))
				break
			}
			mustFixtureResult(io.WriteString(os.Stdout, `{"metadata":{"name":"test-image","namespace":"harvester-public"},"status":{"storageClassName":"test-image-storage","conditions":[{"type":"Imported","status":"True"}]}}`))
		case strings.Contains(args, "get virtualmachine "):
			if strings.Contains(args, "jsonpath={.metadata.uid}") {
				if os.Getenv("HARVESTER_TEST_UID_FAILURE") == "1" {
					mustFixtureResult(os.Stderr.WriteString("forbidden reading VM identity"))
					os.Exit(1)
				}
				mustFixtureResult(io.WriteString(os.Stdout, env("HARVESTER_TEST_VM_UID", "fixture-vm-uid")))
				break
			}
			if strings.Contains(args, "--ignore-not-found") {
				if failure := os.Getenv("HARVESTER_TEST_VM_LOOKUP_FAILURE"); failure != "" {
					mustFixtureResult(os.Stderr.WriteString(failure))
					os.Exit(1)
				}
				if file := os.Getenv("HARVESTER_TEST_EXISTING_VM"); file != "" && os.Getenv("HARVESTER_TEST_VM_LOOKUP_EMPTY") != "1" {
					data, err := os.ReadFile(file)
					if err != nil {
						os.Exit(2)
					}
					mustFixtureResult(os.Stdout.Write(data))
				}
				break
			}
			mustFixture(json.NewEncoder(os.Stdout).Encode(map[string]any{"status": map[string]string{"printableStatus": env("HARVESTER_TEST_VM_PHASE", "Running")}}))
		case strings.Contains(args, "get vmi "):
			if os.Getenv("HARVESTER_TEST_DISCOVERY_STALL") == "1" {
				time.Sleep(time.Minute)
			}
			mustFixtureResult(io.WriteString(os.Stdout, `{"status":{"interfaces":[{"interfaceName":"docker0","ipAddress":"172.17.0.1"},{"name":"default","ipAddress":"192.0.2.10"}]}}`))
		case strings.Contains(args, "get pvc "):
			if file := os.Getenv("HARVESTER_TEST_EXISTING_PVC"); file != "" && os.Getenv("HARVESTER_TEST_PVC_LOOKUP_EMPTY") != "1" {
				data, err := os.ReadFile(file)
				if err != nil {
					os.Exit(2)
				}
				mustFixtureResult(os.Stdout.Write(data))
			}
		case strings.Contains(args, "get network-attachment-definition"):
			if os.Getenv("HARVESTER_TEST_NETWORK_FAILURE") == "1" {
				mustFixtureResult(os.Stderr.WriteString("network attachment not found"))
				os.Exit(1)
			}
		case strings.Contains(args, "apply -f -"), strings.Contains(args, "create -f -"):
			var object map[string]any
			if err := json.NewDecoder(os.Stdin).Decode(&object); err != nil {
				os.Exit(2)
			}
			fmtKind, _ := object["kind"].(string)
			if file := os.Getenv("HARVESTER_TEST_EXISTING_PVC"); file != "" && fmtKind == "PersistentVolumeClaim" {
				if strings.Contains(args, "create -f -") {
					mustFixtureResult(os.Stderr.WriteString("Error from server (AlreadyExists): persistentvolumeclaim already exists"))
					os.Exit(1)
				}
				data := mustFixtureResult(json.Marshal(object))
				if err := os.WriteFile(file, data, 0600); err != nil {
					os.Exit(2)
				}
			}
			if file := os.Getenv("HARVESTER_TEST_EXISTING_VM"); file != "" && fmtKind == "VirtualMachine" {
				if strings.Contains(args, "create -f -") {
					mustFixtureResult(os.Stderr.WriteString("Error from server (AlreadyExists): virtualmachine already exists"))
					os.Exit(1)
				}
				data := mustFixtureResult(json.Marshal(object))
				if err := os.WriteFile(file, data, 0600); err != nil {
					os.Exit(2)
				}
			}
			mustFixtureResult(io.WriteString(os.Stdout, fmtKind+" applied\n"))
		}
	case "ssh":
		if code := os.Getenv("HARVESTER_TEST_SSH_EXIT_CODE"); code != "" {
			n, _ := strconv.Atoi(code)
			os.Exit(n)
		}
		if noise := os.Getenv("HARVESTER_TEST_SSH_ERROR_OUTPUT"); noise != "" {
			mustFixtureResult(fmt.Fprint(os.Stderr, noise))
			os.Exit(1)
		}
		if file := os.Getenv("HARVESTER_TEST_SSH_CALLS"); file != "" {
			f := mustFixtureResult(os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600))
			mustFixture(json.NewEncoder(f).Encode(os.Args[1:]))
			mustFixture(f.Close())
		}
		if os.Getenv("HARVESTER_TEST_HOST_KEY_FAILURE") == "1" {
			mustFixtureResult(os.Stderr.WriteString("WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!\nHost key verification failed."))
			os.Exit(255)
		}
		if os.Getenv("HARVESTER_TEST_PROXY_CHILD") == "1" {
			time.Sleep(time.Minute)
			os.Exit(0)
		}
		if file := os.Getenv("HARVESTER_TEST_PROXY_PID_FILE"); file != "" {
			child := exec.Command(os.Args[0])
			child.Env = append(os.Environ(), "HARVESTER_TEST_PROXY_CHILD=1")
			child.Stdout, child.Stderr = os.Stdout, os.Stderr
			if err := child.Start(); err != nil {
				os.Exit(2)
			}
			mustFixture(os.WriteFile(file, []byte(strconv.Itoa(child.Process.Pid)), 0600))
			time.Sleep(time.Minute)
		}
		if os.Getenv("HARVESTER_TEST_SSH_STALL") == "1" {
			time.Sleep(time.Minute)
		}
		command := os.Args[len(os.Args)-1]
		if !strings.Contains(command, "docker info") {
			mustFixtureResult(io.WriteString(os.Stdout, "protocol\x00bytes\n"))
		}
	default:
		main()
	}
	os.Exit(0)
}

type deleteEvent struct {
	Action      string `json:"action"`
	Name        string `json:"name"`
	UID         string `json:"uid,omitempty"`
	Propagation string `json:"propagation,omitempty"`
}

func deleteFixture(state string, args []string) {
	event := deleteEvent{Action: args[0]}
	if args[0] == "get" {
		event.Name = args[2]
	} else {
		event.Name = filepath.Base(args[2])
		var options struct {
			Preconditions struct {
				UID string `json:"uid"`
			} `json:"preconditions"`
			Propagation string `json:"propagationPolicy"`
		}
		if json.NewDecoder(os.Stdin).Decode(&options) != nil {
			os.Exit(2)
		}
		event.UID, event.Propagation = options.Preconditions.UID, options.Propagation
	}
	f, err := os.OpenFile(os.Getenv("HARVESTER_TEST_DELETE_CALLS"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		os.Exit(2)
	}
	if json.NewEncoder(f).Encode(event) != nil || f.Close() != nil {
		os.Exit(2)
	}
	path := filepath.Join(state, event.Name)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		os.Exit(2)
	}
	if event.Action == "get" {
		_, _ = os.Stdout.Write(data)
		return
	}
	for _, target := range strings.Split(os.Getenv("HARVESTER_TEST_DELETE_FAILURES"), ",") {
		if target == event.Name {
			_, _ = os.Stderr.WriteString("permission denied deleting " + target)
			os.Exit(7)
		}
	}
	if os.Getenv("HARVESTER_TEST_REPLACE_ON_DELETE") == event.Name {
		data = []byte(`{"metadata":{"uid":"replacement","labels":{"devsy.sh/provider":"harvester","devsy.sh/machine-id":"machine-1"}}}`)
		if os.WriteFile(path, data, 0600) != nil {
			os.Exit(2)
		}
	}
	var object struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
	}
	if json.Unmarshal(data, &object) != nil {
		os.Exit(2)
	}
	if event.UID == "" || event.UID != object.Metadata.UID || event.Propagation != "Foreground" {
		_, _ = os.Stderr.WriteString("UID precondition failed")
		os.Exit(7)
	}
	if os.Remove(path) != nil {
		os.Exit(2)
	}
}

func deleteLifecycleSetup(t *testing.T) (string, string, string) {
	t.Helper()
	exe := lifecycleFixture(t)
	t.Setenv("HARVESTER_VM_NAME", "machine-1")
	dir := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls.jsonl")
	t.Setenv("HARVESTER_TEST_DELETE_STATE", dir)
	t.Setenv("HARVESTER_TEST_DELETE_CALLS", calls)
	for _, name := range []string{"machine-1", "machine-1-rootdisk", "machine-1-image", "machine-1-blankdisk"} {
		data := fmt.Sprintf(`{"metadata":{"uid":%q,"labels":{"devsy.sh/provider":"harvester","devsy.sh/machine-id":"machine-1"}}}`, "uid-"+name)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return exe, dir, calls
}

func readDeleteEvents(t *testing.T, path string) []deleteEvent {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var events []deleteEvent
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var event deleteEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

func lifecycleFixture(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	for _, name := range []string{"fake-kubectl", "ssh"} {
		data, err := os.ReadFile(exe)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+suffix), data, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HARVESTER_TEST_LIFECYCLE", "1")
	t.Setenv("HARVESTER_KUBECTL_PATH", filepath.Join(dir, "fake-kubectl"+suffix))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, name := range []string{"KUBECONFIG", "HARVESTER_CONTEXT", "HARVESTER_SSH_HOST", "HARVESTER_SSH_FLAGS", "HARVESTER_SSH_PUBLIC_KEY", "HARVESTER_USER_DATA_FILE", "HARVESTER_CLOUD_INIT_TEMPLATE", "HARVESTER_NETWORK"} {
		t.Setenv(name, "")
	}
	t.Setenv("HARVESTER_IMAGE", "test-image")
	t.Setenv("HARVESTER_IMAGE_PIN", "")
	t.Setenv("HARVESTER_IMAGE_CHOICE", "")
	t.Setenv("HARVESTER_IMAGE_SELECTION_MODE", "")
	t.Setenv("HARVESTER_IMAGE_TYPE", "qcow2")
	t.Setenv("HARVESTER_USER_DATA", "#cloud-config\nsecret-fixture-must-not-be-logged\n")
	return exe
}

func runLifecycleFixture(t *testing.T, exe string, args ...string) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func TestLifecycleProgressAndProtocolOutput(t *testing.T) {
	exe := lifecycleFixture(t)
	for _, operation := range []string{"init", "create", "start", "stop", "delete"} {
		t.Run(operation, func(t *testing.T) {
			phase := "Running"
			if operation == "stop" {
				phase = "Stopped"
			}
			t.Setenv("HARVESTER_TEST_VM_PHASE", phase)
			out, stderr, err := runLifecycleFixture(t, exe, operation)
			if err != nil {
				t.Fatalf("%s failed: %v: %s", operation, err, stderr)
			}
			if !strings.Contains(out, "harvester:") {
				t.Fatalf("%s emitted no lifecycle progress: %q", operation, out)
			}
			if strings.Contains(out+stderr, "secret-fixture-must-not-be-logged") {
				t.Fatal("cloud-init content leaked into progress output")
			}
			if operation == "create" {
				previous := -1
				for _, stage := range []string{"Resolving cloud-init", "Resolving VM image", "Provisioning root disk", "Creating virtual machine", "Waiting for virtual machine", "Waiting for guest IP", "Guest IP available", "SSH and Docker are ready", "Machine ready"} {
					index := strings.Index(out, stage)
					if index <= previous {
						t.Fatalf("missing or out-of-order stage %q: %s", stage, out)
					}
					previous = index
				}
			}
		})
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"status"}, "Running"},
		{[]string{"command", "--command", "protocol-fixture"}, "protocol\x00bytes\n"},
	} {
		out, stderr, err := runLifecycleFixture(t, exe, tc.args...)
		if err != nil || out != tc.want || stderr != "" {
			t.Fatalf("protocol output changed for %v: stdout=%q stderr=%q err=%v", tc.args, out, stderr, err)
		}
	}
}

func TestCreateProgressStopsAtFailedStage(t *testing.T) {
	exe := lifecycleFixture(t)
	t.Setenv("HARVESTER_TEST_IMAGE_FAILURE", "1")
	out, stderr, err := runLifecycleFixture(t, exe, "create")
	if err == nil || !strings.Contains(stderr, "image is unavailable") {
		t.Fatalf("expected the image lookup failure: %v %q", err, stderr)
	}
	if !strings.Contains(out, "Resolving VM image") || strings.Contains(out, "Creating virtual machine") || strings.Contains(out, "Machine ready") {
		t.Fatalf("progress misreported a failed create: %q", out)
	}
}

func TestSSHHostUsesPrimaryKubeVirtIPAddress(t *testing.T) {
	lifecycleFixture(t)
	host, err := sshHost(config{VMName: "machine-1", Namespace: "default"})
	if err != nil || host != "192.0.2.10" {
		t.Fatalf("expected default-network ipAddress, not a guest Docker bridge: host=%q err=%v", host, err)
	}
}

func TestConfiguredHarvesterNetwork(t *testing.T) {
	t.Setenv("HARVESTER_NETWORK", "default/mgmt-untagged")
	c := load()
	vm, err := buildVMManifest(c, imageInfo{Format: "qcow2"})
	if err != nil {
		t.Fatal(err)
	}
	network := vm.Spec.Template.Spec.Networks[0]
	if network.Multus == nil || network.Multus.NetworkName != "default/mgmt-untagged" || network.Pod != nil {
		t.Fatalf("configured network was not attached: %#v", network)
	}
	t.Setenv("HARVESTER_NETWORK", "")
	vm, err = buildVMManifest(load(), imageInfo{Format: "qcow2"})
	if err != nil || vm.Spec.Template.Spec.Networks[0].Pod == nil {
		t.Fatalf("default pod network changed: %#v %v", vm.Spec.Template.Spec.Networks, err)
	}
}

func TestInvalidNetworkFailsBeforeProvisioning(t *testing.T) {
	exe := lifecycleFixture(t)
	for _, network := range []string{"a/b/c", "/missing-namespace", "default/"} {
		t.Setenv("HARVESTER_NETWORK", network)
		out, _, err := runLifecycleFixture(t, exe, "create")
		if err == nil || strings.Contains(out, "Provisioning root disk") {
			t.Fatalf("invalid network %q reached provisioning: %q %v", network, out, err)
		}
	}
	t.Setenv("HARVESTER_NETWORK", "default/mgmt-untagged")
	t.Setenv("HARVESTER_TEST_NETWORK_FAILURE", "1")
	for _, operation := range []string{"init", "create"} {
		out, stderr, err := runLifecycleFixture(t, exe, operation)
		if err == nil || !strings.Contains(stderr, "network attachment not found") || strings.Contains(out, "Provisioning root disk") || strings.Contains(out, "Provider initialized") {
			t.Fatalf("missing network was accepted for %s: %q %q %v", operation, out, stderr, err)
		}
	}
}

func TestGuestReadinessProbeHonorsDeadline(t *testing.T) {
	lifecycleFixture(t)
	t.Setenv("HARVESTER_TEST_SSH_STALL", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := probeGuest(ctx, config{SSHPort: "22", SSHUser: "ubuntu"}, "192.0.2.10")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled SSH probe did not report its deadline: %v", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("stalled SSH probe exceeded its deadline by more than 5 seconds")
	}
}

func TestGuestIPDiscoveryHonorsReadinessDeadline(t *testing.T) {
	lifecycleFixture(t)
	t.Setenv("HARVESTER_TEST_DISCOVERY_STALL", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := sshHostContext(ctx, config{VMName: "machine-1", Namespace: "default"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled discovery ignored readiness deadline: %v", err)
	}
}

func TestGuestReadinessProbeBoundsProxyPipeCleanup(t *testing.T) {
	lifecycleFixture(t)
	file := filepath.Join(t.TempDir(), "proxy.pid")
	t.Setenv("HARVESTER_TEST_PROXY_PID_FILE", file)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- probeGuest(ctx, config{SSHPort: "22", SSHUser: "ubuntu"}, "192.0.2.10") }()
	defer func() {
		if data, err := os.ReadFile(file); err == nil {
			pid, _ := strconv.Atoi(string(data))
			if pid > 0 {
				if child, err := os.FindProcess(pid); err == nil {
					_ = child.Kill()
					_, _ = child.Wait()
				}
			}
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, err := os.ReadFile(file); err == nil && len(data) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("SSH fixture did not publish its proxy child")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected probe result after cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("proxy-held output pipes blocked readiness cancellation")
	}
}

func TestDeleteReportsDiskFailuresAndContinuesCleanup(t *testing.T) {
	for _, failures := range []string{"machine-1-image", "machine-1-blankdisk", "machine-1-rootdisk,machine-1-blankdisk"} {
		t.Run(failures, func(t *testing.T) {
			exe, dir, calls := deleteLifecycleSetup(t)
			t.Setenv("HARVESTER_TEST_DELETE_FAILURES", failures)
			out, stderr, err := runLifecycleFixture(t, exe, "delete")
			if err == nil || strings.Contains(out, "VM and PVC deletion confirmed") {
				t.Fatalf("cleanup incorrectly succeeded: %q %q %v", out, stderr, err)
			}
			for _, name := range strings.Split(failures, ",") {
				if !strings.Contains(stderr, name) {
					t.Fatalf("missing %s: %q", name, stderr)
				}
			}
			attempts := 0
			for _, event := range readDeleteEvents(t, calls) {
				if event.Action == "delete" {
					attempts++
				}
			}
			if attempts != 4 {
				t.Fatalf("expected all four delete attempts, got %d", attempts)
			}
			t.Setenv("HARVESTER_TEST_DELETE_FAILURES", "")
			out, stderr, err = runLifecycleFixture(t, exe, "delete")
			if err != nil || !strings.Contains(out, "VM and PVC deletion confirmed") {
				t.Fatalf("retry failed: %q %q %v", out, stderr, err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("remaining resources: %v, %v", entries, err)
			}
		})
	}
}

func TestDeleteConfirmsVMBeforeDeletingDisks(t *testing.T) {
	exe, _, calls := deleteLifecycleSetup(t)
	out, stderr, err := runLifecycleFixture(t, exe, "delete")
	if err != nil || stderr != "" || !strings.Contains(out, "VM and PVC deletion confirmed") {
		t.Fatalf("expected confirmed cleanup: %q %q %v", out, stderr, err)
	}
	events := readDeleteEvents(t, calls)
	expected := []deleteEvent{
		{Action: "get", Name: "machine-1"}, {Action: "delete", Name: "machine-1", UID: "uid-machine-1", Propagation: "Foreground"}, {Action: "get", Name: "machine-1"},
	}
	if len(events) != 12 {
		t.Fatalf("unexpected events: %+v", events)
	}
	for i, want := range expected {
		if events[i] != want {
			t.Fatalf("event %d: got %+v want %+v", i, events[i], want)
		}
	}
	for i := 3; i < len(events); i += 3 {
		get, del, confirm := events[i], events[i+1], events[i+2]
		if get.Action != "get" || del.Action != "delete" || confirm.Action != "get" || get.Name != del.Name || confirm.Name != del.Name || del.UID != "uid-"+del.Name || del.Propagation != "Foreground" {
			t.Fatalf("invalid disk sequence: %+v", events[i:i+3])
		}
	}
}

func TestDeleteRejectsForeignResourcesAndRetries(t *testing.T) {
	exe, dir, calls := deleteLifecycleSetup(t)
	foreign := filepath.Join(dir, "machine-1-rootdisk")
	if err := os.WriteFile(foreign, []byte(`{"metadata":{"uid":"foreign","labels":{"devsy.sh/provider":"other"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := runLifecycleFixture(t, exe, "delete")
	if err == nil || !strings.Contains(stderr, "not owned") {
		t.Fatalf("foreign disk accepted: %q %v", stderr, err)
	}
	for _, event := range readDeleteEvents(t, calls) {
		if event.Action == "delete" && event.Name == "machine-1-rootdisk" {
			t.Fatal("foreign disk was deleted")
		}
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteVMFailurePreventsDiskDeletion(t *testing.T) {
	exe, _, calls := deleteLifecycleSetup(t)
	t.Setenv("HARVESTER_TEST_DELETE_FAILURES", "machine-1")
	_, _, err := runLifecycleFixture(t, exe, "delete")
	if err == nil {
		t.Fatal("VM failure ignored")
	}
	for _, event := range readDeleteEvents(t, calls) {
		if event.Name != "machine-1" {
			t.Fatalf("disk touched after VM failure: %+v", event)
		}
	}
}

func TestDeleteRejectsForeignVM(t *testing.T) {
	exe, dir, calls := deleteLifecycleSetup(t)
	vm := filepath.Join(dir, "machine-1")
	if err := os.WriteFile(vm, []byte(`{"metadata":{"uid":"foreign","labels":{"devsy.sh/provider":"other"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := runLifecycleFixture(t, exe, "delete")
	if err == nil || !strings.Contains(stderr, "not owned") {
		t.Fatalf("foreign VM accepted: %q %v", stderr, err)
	}
	events := readDeleteEvents(t, calls)
	if len(events) != 1 || events[0].Action != "get" {
		t.Fatalf("unexpected operations: %+v", events)
	}
}

func TestDeleteRejectsReplacement(t *testing.T) {
	for _, name := range []string{"machine-1", "machine-1-rootdisk"} {
		t.Run(name, func(t *testing.T) {
			exe, dir, _ := deleteLifecycleSetup(t)
			t.Setenv("HARVESTER_TEST_REPLACE_ON_DELETE", name)
			_, stderr, err := runLifecycleFixture(t, exe, "delete")
			if err == nil || !strings.Contains(stderr, "UID precondition failed") {
				t.Fatalf("replacement accepted: %q %v", stderr, err)
			}
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil || !strings.Contains(string(data), `"uid":"replacement"`) {
				t.Fatalf("replacement missing: %s %v", data, err)
			}
		})
	}
}
