package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGuestWaitRetainsAuthenticationFailureWhenFinalAPICallExpires(t *testing.T) {
	lifecycleFixture(t)
	t.Setenv("HARVESTER_TEST_UID_STALL_AFTER_FIRST", filepath.Join(t.TempDir(), "uid-count"))
	t.Setenv("HARVESTER_TEST_SSH_ERROR_OUTPUT", "Permission denied (publickey).")
	ctx, cancel := context.WithTimeout(context.Background(), 2400*time.Millisecond)
	defer cancel()
	err := waitForGuestContext(ctx, config{SSHUser: "developer", SSHPort: "22"})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "publickey") {
		t.Fatalf("deadline hid the actionable SSH failure: %v", err)
	}
}

func TestGeneratedProbeClassifiesAuthenticationWithoutGuestOutput(t *testing.T) {
	lifecycleFixture(t)
	t.Setenv("HARVESTER_TEST_SSH_ERROR_OUTPUT", "Permission denied (publickey). PRIVATE_GUEST_DIAGNOSTIC")
	err := probeGuest(context.Background(), config{SSHUser: "developer", SSHPort: "22", CloudInitProfile: "ubuntu-24.04-docker-v1"}, "192.0.2.10")
	if err == nil || !strings.Contains(err.Error(), "authentication") || strings.Contains(err.Error(), "PRIVATE_GUEST_DIAGNOSTIC") {
		t.Fatalf("authentication was not safely classified: %v", err)
	}
}

func TestProbeDistinguishesMissingAndInaccessibleEngine(t *testing.T) {
	for _, tc := range []struct{ code, diagnostic string }{{"43", "not installed"}, {"44", "not accessible"}} {
		t.Run(tc.code, func(t *testing.T) {
			lifecycleFixture(t)
			t.Setenv("HARVESTER_TEST_SSH_EXIT_CODE", tc.code)
			err := probeGuest(context.Background(), config{SSHUser: "developer", SSHPort: "22"}, "192.0.2.10")
			if err == nil || !strings.Contains(err.Error(), tc.diagnostic) {
				t.Fatalf("runtime failure not identified: %v", err)
			}
		})
	}
}

func TestManagedCustomProbeKeepsCustomDiagnostics(t *testing.T) {
	exe, _, _ := managedFixture(t)
	t.Setenv("HARVESTER_USER_DATA", "#cloud-config\nssh_authorized_keys:\n  - "+managedPublicKeyMarker+"\n")
	_, stderr, err := runLifecycleFixture(t, exe, "create")
	if err != nil {
		t.Fatalf("create: %s %v", stderr, err)
	}
	t.Setenv("HARVESTER_TEST_SSH_ERROR_OUTPUT", "Permission denied (publickey). CUSTOM_DIAGNOSTIC")
	err = probeGuest(context.Background(), load(), "192.0.2.10")
	if err == nil || !strings.Contains(err.Error(), "CUSTOM_DIAGNOSTIC") {
		t.Fatalf("managed key ownership redacted a custom source: %v", err)
	}
	t.Setenv("HARVESTER_TEST_SSH_EXIT_CODE", "44")
	err = probeGuest(context.Background(), load(), "192.0.2.10")
	if err == nil || !strings.Contains(err.Error(), "CUSTOM_DIAGNOSTIC") {
		t.Fatalf("managed custom engine diagnostic was discarded: %v", err)
	}
	if _, err := os.Stat(os.Getenv("HARVESTER_TEST_CREATED_VM")); err != nil {
		t.Fatal(err)
	}
}

func TestCustomEngineFailureDiagnosticScope(t *testing.T) {
	for _, profile := range []string{"", "ubuntu-24.04-docker-v1"} {
		t.Run(profile, func(t *testing.T) {
			lifecycleFixture(t)
			t.Setenv("HARVESTER_TEST_SSH_EXIT_CODE", "44")
			t.Setenv("HARVESTER_TEST_SSH_ERROR_OUTPUT", "CUSTOM_ENGINE_DIAGNOSTIC")
			err := probeGuest(context.Background(), config{SSHUser: "developer", SSHPort: "22", CloudInitProfile: profile}, "192.0.2.10")
			if err == nil || strings.Contains(err.Error(), "CUSTOM_ENGINE_DIAGNOSTIC") != (profile == "") {
				t.Fatalf("engine error violated source diagnostic scope: %v", err)
			}
		})
	}
}

func TestManagedNamespaceLookupHonorsProbeDeadline(t *testing.T) {
	exe, _, _ := managedFixture(t)
	_, stderr, err := runLifecycleFixture(t, exe, "create")
	if err != nil {
		t.Fatalf("create: %s %v", stderr, err)
	}
	t.Setenv("HARVESTER_TEST_NAMESPACE_STALL", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = probeGuest(ctx, load(), "192.0.2.10")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("managed identity lookup escaped the probe deadline: %v, %v", err, time.Since(start))
	}
}
