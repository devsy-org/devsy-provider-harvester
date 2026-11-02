package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This is API state after Harvester admission, not a provider-built manifest.
const existingVMFixture = `{
  "metadata": {"name":"machine-1", "namespace":"default", "uid":"existing-vm-uid",
    "labels":{"devsy.sh/provider":"harvester", "devsy.sh/machine-id":"machine-1"}},
  "spec":{"runStrategy":"RerunOnFailure", "template":{"spec":{"domain":{
    "devices":{"interfaces":[{"name":"default","bridge":{},"macAddress":"aa:bb:cc:dd:ee:ff"}]},
    "resources":{"requests":{"memory":"5461Mi"},"limits":{"memory":"8Gi"}}
  }}}},
  "status":{"ready":true,"printableStatus":"Running"}
}`

const existingPVCFixture = `{
  "metadata":{"name":"machine-1-rootdisk","namespace":"default","uid":"existing-disk-uid",
    "labels":{"devsy.sh/provider":"harvester","devsy.sh/machine-id":"machine-1"},
    "annotations":{"harvesterhci.io/imageId":"harvester-public/test-image"}},
  "spec":{"accessModes":["ReadWriteMany"],"volumeMode":"Block",
    "storageClassName":"test-image-storage","resources":{"requests":{"storage":"40Gi"}}}
}`

func existingVMFiles(t *testing.T, data string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	vmFile, callsFile := filepath.Join(dir, "vm.json"), filepath.Join(dir, "calls.jsonl")
	if err := os.WriteFile(vmFile, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARVESTER_VM_NAME", "machine-1")
	t.Setenv("HARVESTER_NAMESPACE", "default")
	t.Setenv("HARVESTER_TEST_EXISTING_VM", vmFile)
	t.Setenv("HARVESTER_TEST_KUBECTL_CALLS", callsFile)
	return vmFile, callsFile
}

func assertNoDefinitionWrites(t *testing.T, vmFile, callsFile, original string) string {
	t.Helper()
	data, err := os.ReadFile(vmFile)
	if err != nil || string(data) != original {
		t.Fatalf("reattachment rewrote Harvester VM state: %s %v", data, err)
	}
	calls, err := os.ReadFile(callsFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		var args []string
		if err := json.Unmarshal([]byte(line), &args); err != nil {
			t.Fatal(err)
		}
		if args[0] != "get" && (args[0] != "replace" || !strings.Contains(strings.Join(args, " "), "/start")) {
			t.Fatalf("reattachment must not write VM/PVC definitions: %v", args)
		}
	}
	return string(calls)
}

func TestCreateReusesVMWithoutChangingHarvesterFields(t *testing.T) {
	exe := lifecycleFixture(t)
	vmFile, callsFile := existingVMFiles(t, existingVMFixture)
	out, stderr, err := runLifecycleFixture(t, exe, "create")
	if err != nil || stderr != "" || !strings.Contains(out, "SSH and Docker are ready") || !strings.Contains(out, "Machine ready") {
		t.Fatalf("existing VM was not checked for readiness: %q %q %v", out, stderr, err)
	}
	assertNoDefinitionWrites(t, vmFile, callsFile, existingVMFixture)
}

func TestCreateReuseDoesNotRequireCreationInputs(t *testing.T) {
	exe := lifecycleFixture(t)
	vmFile, callsFile := existingVMFiles(t, existingVMFixture)
	t.Setenv("HARVESTER_IMAGE", "")
	t.Setenv("HARVESTER_USER_DATA", "")
	t.Setenv("HARVESTER_USER_DATA_FILE", filepath.Join(t.TempDir(), "removed-cloud-init.yaml"))
	out, stderr, err := runLifecycleFixture(t, exe, "create")
	if err != nil || !strings.Contains(out, "Machine ready") {
		t.Fatalf("reattachment attempted to resolve creation inputs: %q %q %v", out, stderr, err)
	}
	assertNoDefinitionWrites(t, vmFile, callsFile, existingVMFixture)
}

func TestCreateRefusesUnsafeExistingVM(t *testing.T) {
	for _, tc := range []struct {
		name, data, failure, want string
	}{
		{"foreign provider", strings.Replace(existingVMFixture, `"harvester"`, `"other"`, 1), "", "not owned"},
		{"wrong machine", strings.Replace(existingVMFixture, `"devsy.sh/machine-id":"machine-1"`, `"devsy.sh/machine-id":"other"`, 1), "", "not owned"},
		{"deleting", strings.Replace(existingVMFixture, `"uid":"existing-vm-uid"`, `"uid":"existing-vm-uid","deletionTimestamp":"2026-10-16T17:00:00Z"`, 1), "", "deleting"},
		{"lookup denied", existingVMFixture, "forbidden reading virtualmachine", "forbidden"},
		{"bad response", `{`, "", "decode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exe := lifecycleFixture(t)
			vmFile, callsFile := existingVMFiles(t, tc.data)
			t.Setenv("HARVESTER_TEST_VM_LOOKUP_FAILURE", tc.failure)
			out, stderr, err := runLifecycleFixture(t, exe, "create")
			if err == nil || !strings.Contains(stderr, tc.want) || strings.Contains(out, "Machine ready") {
				t.Fatalf("unsafe adoption was not refused: %q %q %v", out, stderr, err)
			}
			assertNoDefinitionWrites(t, vmFile, callsFile, tc.data)
		})
	}
}

func TestCreateStartsStoppedVMWithoutRewritingDefinition(t *testing.T) {
	exe := lifecycleFixture(t)
	data := strings.Replace(existingVMFixture, `"printableStatus":"Running"`, `"printableStatus":"Stopped"`, 1)
	vmFile, callsFile := existingVMFiles(t, data)
	out, stderr, err := runLifecycleFixture(t, exe, "create")
	if err != nil || !strings.Contains(out, "Machine ready") {
		t.Fatalf("stopped VM could not be reused: %q %q %v", out, stderr, err)
	}
	calls := assertNoDefinitionWrites(t, vmFile, callsFile, data)
	if !strings.Contains(calls, `/virtualmachines/machine-1/start`) {
		t.Fatalf("stopped VM was not started via its subresource: %s", calls)
	}
}

func TestCreateCannotOverwriteVMAppearingAfterLookup(t *testing.T) {
	exe := lifecycleFixture(t)
	vmFile, _ := existingVMFiles(t, existingVMFixture)
	t.Setenv("HARVESTER_TEST_VM_LOOKUP_EMPTY", "1")
	out, stderr, err := runLifecycleFixture(t, exe, "create")
	if err == nil || !strings.Contains(stderr, "AlreadyExists") || strings.Contains(out, "Machine ready") {
		t.Fatalf("concurrent VM creation was not refused: %q %q %v", out, stderr, err)
	}
	data, readErr := os.ReadFile(vmFile)
	if readErr != nil || string(data) != existingVMFixture {
		t.Fatalf("concurrent create rewrote the existing VM: %s %v", data, readErr)
	}
}

func TestCreateCannotOverwriteDiskAppearingAfterLookup(t *testing.T) {
	exe := lifecycleFixture(t)
	file := filepath.Join(t.TempDir(), "pvc.json")
	if err := os.WriteFile(file, []byte(existingPVCFixture), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARVESTER_TEST_EXISTING_PVC", file)
	t.Setenv("HARVESTER_TEST_PVC_LOOKUP_EMPTY", "1")
	out, stderr, err := runLifecycleFixture(t, exe, "create")
	if err == nil || !strings.Contains(stderr, "AlreadyExists") || strings.Contains(out, "Creating virtual machine") {
		t.Fatalf("concurrent disk creation was not refused: %q %q %v", out, stderr, err)
	}
	data, readErr := os.ReadFile(file)
	if readErr != nil || string(data) != existingPVCFixture {
		t.Fatalf("concurrent create rewrote the existing disk: %s %v", data, readErr)
	}
}

func TestCreateRetriesOnlyCompatibleOwnedDisk(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		wantOK     bool
	}{
		{"matching partial create", existingPVCFixture, true},
		{"foreign", strings.Replace(existingPVCFixture, `"harvester"`, `"other"`, 1), false},
		{"wrong machine", strings.Replace(existingPVCFixture, `"devsy.sh/machine-id":"machine-1"`, `"devsy.sh/machine-id":"other"`, 1), false},
		{"wrong image", strings.Replace(existingPVCFixture, `harvester-public/test-image`, `harvester-public/other`, 1), false},
		{"wrong size", strings.Replace(existingPVCFixture, `40Gi`, `80Gi`, 1), false},
		{"wrong storage class", strings.Replace(existingPVCFixture, `test-image-storage`, `other-storage`, 1), false},
		{"deleting", strings.Replace(existingPVCFixture, `"uid":"existing-disk-uid"`, `"uid":"existing-disk-uid","deletionTimestamp":"2026-10-16T17:00:00Z"`, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exe := lifecycleFixture(t)
			file := filepath.Join(t.TempDir(), "pvc.json")
			if err := os.WriteFile(file, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HARVESTER_VM_NAME", "machine-1")
			t.Setenv("HARVESTER_DISK", "40Gi")
			t.Setenv("HARVESTER_IMAGE_NAMESPACE", "harvester-public")
			t.Setenv("HARVESTER_TEST_EXISTING_PVC", file)
			out, stderr, err := runLifecycleFixture(t, exe, "create")
			if (err == nil) != tc.wantOK || strings.Contains(out, "Machine ready") != tc.wantOK {
				t.Fatalf("unexpected partial-create recovery: %q %q %v", out, stderr, err)
			}
			data, readErr := os.ReadFile(file)
			if readErr != nil || string(data) != tc.data {
				t.Fatalf("retry modified an existing disk: %s %v", data, readErr)
			}
		})
	}
}
