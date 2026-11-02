package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSSHTrustFollowsVMIdentityForProbeAndTransport(t *testing.T) {
	exe := lifecycleFixture(t)
	file := filepath.Join(t.TempDir(), "ssh.jsonl")
	t.Setenv("HARVESTER_TEST_SSH_CALLS", file)
	for _, uid := range []string{"first-vm", "replacement-vm"} {
		t.Setenv("HARVESTER_TEST_VM_UID", uid)
		for _, host := range []string{"192.0.2.10", "192.0.2.11"} {
			t.Setenv("HARVESTER_SSH_HOST", host)
			for _, args := range [][]string{{"create"}, {"command", "--command", "protocol-fixture"}} {
				if err := os.WriteFile(file, nil, 0600); err != nil {
					t.Fatal(err)
				}
				_, stderr, err := runLifecycleFixture(t, exe, args...)
				if err != nil {
					t.Fatalf("SSH failed: %s %v", stderr, err)
				}
				data, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				var sshArgs []string
				if err := json.Unmarshal(data, &sshArgs); err != nil {
					t.Fatal(err)
				}
				for _, option := range []string{"HostKeyAlias=devsy-harvester-" + uid, "StrictHostKeyChecking=accept-new", "CheckHostIP=no"} {
					found := false
					for _, value := range sshArgs {
						found = found || value == option
					}
					if !found {
						t.Fatalf("missing SSH trust option %q in %v", option, sshArgs)
					}
				}
			}
		}
	}
}

func TestSSHIdentityLookupFailureDoesNotConnect(t *testing.T) {
	exe := lifecycleFixture(t)
	file := filepath.Join(t.TempDir(), "ssh.jsonl")
	t.Setenv("HARVESTER_TEST_SSH_CALLS", file)
	t.Setenv("HARVESTER_TEST_UID_FAILURE", "1")
	_, stderr, err := runLifecycleFixture(t, exe, "command", "--command", "protocol-fixture")
	if err == nil || !strings.Contains(stderr, "forbidden") {
		t.Fatalf("identity lookup failure was ignored: %q %v", stderr, err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("SSH connected without a verified VM identity")
	}
}

func TestChangedHostKeyFailsReadinessImmediately(t *testing.T) {
	exe := lifecycleFixture(t)
	file := filepath.Join(t.TempDir(), "ssh.jsonl")
	t.Setenv("HARVESTER_TEST_SSH_CALLS", file)
	t.Setenv("HARVESTER_TEST_HOST_KEY_FAILURE", "1")
	out, stderr, err := runLifecycleFixture(t, exe, "create")
	if err == nil || !strings.Contains(stderr, "Host key verification failed") || strings.Contains(stderr, "timed out") || strings.Contains(out, "Machine ready") {
		t.Fatalf("host-key failure was retried or hidden: %q %q %v", out, stderr, err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Split(strings.TrimSpace(string(data)), "\n")) != 1 {
		t.Fatalf("host-key failure was retried: %s", data)
	}
}

func TestSSHRejectsMissingOrMalformedVMIdentity(t *testing.T) {
	for _, uid := range []string{" ", "bad uid", "bad\nuid"} {
		t.Run(uid, func(t *testing.T) {
			exe := lifecycleFixture(t)
			file := filepath.Join(t.TempDir(), "ssh.jsonl")
			t.Setenv("HARVESTER_TEST_VM_UID", uid)
			t.Setenv("HARVESTER_TEST_SSH_CALLS", file)
			_, stderr, err := runLifecycleFixture(t, exe, "command", "--command", "protocol-fixture")
			if err == nil || !strings.Contains(stderr, "valid UID") {
				t.Fatalf("invalid VM identity accepted: %q %v", stderr, err)
			}
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				t.Fatal("SSH connected with an invalid VM identity")
			}
		})
	}
}
