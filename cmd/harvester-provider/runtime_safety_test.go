package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestISODevices(t *testing.T) {
	m, err := buildVMManifest(config{CPU: "2", Memory: "2Gi", Disk: "10Gi", VMName: "vm"}, imageInfo{Format: "iso"})
	if err != nil {
		t.Fatal(err)
	}
	disks := m.Spec.Template.Spec.Domain.Devices.Disks
	if disks[0].Name != "rootdisk" || disks[0].Disk == nil || disks[0].CDROM != nil || disks[1].Name != "installer" || disks[1].CDROM == nil {
		t.Fatalf("unexpected devices: %#v", disks)
	}
}

func TestQuantityEquality(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{{"1.5Gi", "1536Mi", true}, {"1Gi", "2Gi", false}, {"bad", "bad", false}} {
		if got := equalQuantity(tc.a, tc.b); got != tc.want {
			t.Errorf("%s %s: %v", tc.a, tc.b, got)
		}
	}
}

func TestWaitForVMRespectsContext(t *testing.T) {
	lifecycleFixture(t)
	t.Setenv("HARVESTER_TEST_VM_STATUS_STALL", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := waitForVMContext(ctx, config{VMName: "vm"}, "Running"); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err=%v elapsed=%v", err, time.Since(start))
	}
}

func TestWaitForVMRetainsAPIError(t *testing.T) {
	lifecycleFixture(t)
	t.Setenv("HARVESTER_TEST_VM_STATUS_ERROR", "API denied")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := waitForVMContext(ctx, config{VMName: "vm"}, "Running")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "API denied") {
		t.Fatalf("expected deadline and API diagnostic, got %v", err)
	}
}

func TestDeleteRejectsInvalidResourcePath(t *testing.T) {
	t.Setenv("HARVESTER_KUBECTL_PATH", filepath.Join(t.TempDir(), "missing"))
	for _, c := range []config{
		{Namespace: "../other", VMName: "vm"},
		{Namespace: "default", VMName: "../other"},
	} {
		err := deleteMachine(c)
		if err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Fatalf("accepted invalid resource path %+v: %v", c, err)
		}
	}
}
