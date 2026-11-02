package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeKubectl(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "kubectl")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARVESTER_KUBECTL_PATH", p)
	return p
}

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
	fakeKubectl(t, "exec sleep 10\n")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := waitForVMContext(ctx, config{VMName: "vm"}, "Running"); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err=%v elapsed=%v", err, time.Since(start))
	}
}
func TestWaitForVMRetainsAPIError(t *testing.T) {
	fakeKubectl(t, "printf 'API denied' >&2; exit 1\n")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := waitForVMContext(ctx, config{VMName: "vm"}, "Running")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "API denied") {
		t.Fatalf("expected deadline and API diagnostic, got %v", err)
	}
}

func TestDeleteOwnershipAndUID(t *testing.T) {
	for _, foreign := range []bool{true, false} {
		t.Run(map[bool]string{true: "foreign", false: "replaced"}[foreign], func(t *testing.T) {
			dir := t.TempDir()
			body := filepath.Join(dir, "body")
			labels := `{"devsy.sh/provider":"harvester","devsy.sh/machine-id":"vm"}`
			if foreign {
				labels = `{"devsy.sh/provider":"other"}`
			}
			fakeKubectl(t, "if [ \"$1\" = get ]; then printf '%s' '"+`{"metadata":{"uid":"old","labels":`+labels+`}}`+"'; else cat > '"+body+"'; echo 'Conflict: UID precondition failed' >&2; exit 1; fi\n")
			_, err := deleteOwned(config{VMName: "vm", Namespace: "default"}, "virtualmachine", "vm", "/apis/kubevirt.io/v1/namespaces/default/virtualmachines/vm")
			if err == nil {
				t.Fatal("expected refusal")
			}
			payload, readErr := os.ReadFile(body)
			if foreign {
				if !errors.Is(readErr, os.ErrNotExist) {
					t.Fatalf("foreign resource deletion attempted: %s", payload)
				}
				return
			}
			if readErr != nil {
				t.Fatal(readErr)
			}
			var opts struct {
				Preconditions struct {
					UID string `json:"uid"`
				} `json:"preconditions"`
			}
			if err := json.Unmarshal(payload, &opts); err != nil || opts.Preconditions.UID != "old" {
				t.Fatalf("payload %s, err %v", payload, err)
			}
		})
	}
}

func TestDeleteRejectsInvalidResourcePath(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "called")
	fakeKubectl(t, "touch '"+marker+"'\n")
	for _, c := range []config{
		{Namespace: "../other", VMName: "vm"},
		{Namespace: "default", VMName: "../other"},
	} {
		if err := deleteMachine(c); err == nil {
			t.Fatalf("accepted invalid resource path: %+v", c)
		}
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("kubectl ran for invalid resource path: %v", err)
	}
}
