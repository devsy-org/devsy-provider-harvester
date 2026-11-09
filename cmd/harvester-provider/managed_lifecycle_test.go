package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func managedFixture(t *testing.T) (string, string, string) {
	t.Helper()
	exe := lifecycleFixture(t)
	root := filepath.Join(t.TempDir(), "credentials")
	vm := filepath.Join(t.TempDir(), "created-vm.json")
	t.Setenv("HARVESTER_SSH_MODE", "managed")
	t.Setenv("HARVESTER_MANAGED_SSH_DIR", root)
	t.Setenv("HARVESTER_TEST_CREATED_VM", vm)
	t.Setenv("HARVESTER_USER_DATA", "")
	t.Setenv("HARVESTER_VM_NAME", "managed-fixture")
	t.Setenv("HARVESTER_SSH_USER", "developer")
	return exe, root, vm
}

func TestManagedSlotUsesNamespaceUIDAndActualVMName(t *testing.T) {
	lifecycleFixture(t)
	root := filepath.Join(t.TempDir(), "keys")
	c := config{Namespace: "default", VMName: "actual-vm", ManagedSSHDir: root, Kubeconfig: "first-config", Context: "first-context"}
	if err := managedLocation(context.Background(), &c); err != nil {
		t.Fatal(err)
	}
	alias := config{Namespace: "default", VMName: "actual-vm", ManagedSSHDir: root, Kubeconfig: "renamed-config", Context: "renamed-context"}
	if err := managedLocation(context.Background(), &alias); err != nil || alias.ManagedSlot != c.ManagedSlot {
		t.Fatal("kubeconfig aliases changed the effective machine identity")
	}
	for _, change := range []string{"namespace", "name"} {
		t.Run(change, func(t *testing.T) {
			other := alias
			other.ManagedSlot = ""
			if change == "namespace" {
				t.Setenv("HARVESTER_TEST_NAMESPACE_UID", "another-cluster-namespace")
			} else {
				other.VMName = "another-vm"
			}
			if err := managedLocation(context.Background(), &other); err != nil || other.ManagedSlot == c.ManagedSlot {
				t.Fatal("different machine identities shared a credential slot")
			}
		})
	}
}

func TestManagedPVCOnlyRetryRequiresOriginalKeyAndBindings(t *testing.T) {
	exe, root, file := managedFixture(t)
	_, stderr, err := runLifecycleFixture(t, exe, "create")
	if err != nil {
		t.Fatalf("create: %s %v", stderr, err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var vm vmManifest
	if err := json.Unmarshal(data, &vm); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	claim := vm.Metadata
	claim.Name += "-rootdisk"
	claim.Annotations["harvesterhci.io/imageId"] = "harvester-public/test-image"
	pvc := pvcManifest{APIVersion: "v1", Kind: "PersistentVolumeClaim", Metadata: claim, Spec: pvcSpec{AccessModes: []string{"ReadWriteMany"}, VolumeMode: "Block", StorageClassName: "test-image-storage", Resources: pvcResources{Requests: pvcStorageRequest{Storage: "40Gi"}}}}
	pvcFile := filepath.Join(t.TempDir(), "disk.json")
	write := func() {
		t.Helper()
		data, err := json.Marshal(pvc)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pvcFile, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	t.Setenv("HARVESTER_TEST_EXISTING_PVC", pvcFile)
	_, stderr, err = runLifecycleFixture(t, exe, "create")
	if err != nil {
		t.Fatalf("matching PVC-only retry failed: %s %v", stderr, err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{managedFingerprintAnnotation, managedUserAnnotation, managedRuntimeAnnotation, profileAnnotation, managedDigestAnnotation} {
		t.Run(field, func(t *testing.T) {
			original := pvc.Metadata.Annotations[field]
			pvc.Metadata.Annotations[field] = "changed"
			write()
			_, stderr, err := runLifecycleFixture(t, exe, "create")
			pvc.Metadata.Annotations[field] = original
			if err == nil || !strings.Contains(stderr, "original") {
				t.Fatalf("incompatible partial state accepted: %s %v", stderr, err)
			}
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				t.Fatal("a VM was created with incompatible disk bindings")
			}
		})
	}
	write()
	keys, err := filepath.Glob(filepath.Join(root, "*", "id_rsa"))
	if err != nil || len(keys) != 1 {
		t.Fatal("missing test identity")
	}
	if err := os.Remove(keys[0]); err != nil {
		t.Fatal(err)
	}
	_, stderr, err = runLifecycleFixture(t, exe, "create")
	if err == nil || !strings.Contains(stderr, "missing") {
		t.Fatalf("PVC-only retry regenerated a key: %s %v", stderr, err)
	}
}

func TestManagedLockDeadlineDoesNotRemoveWaiterLock(t *testing.T) {
	root := filepath.Join(t.TempDir(), "keys")
	unlock, err := lockManagedSlot(context.Background(), root, "slot")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if release, err := lockManagedSlot(ctx, root, "slot"); err == nil {
		release()
		t.Fatal("second process acquired a held identity lock")
	}
	unlock()
	if _, err := os.Stat(filepath.Join(root, "slot.lock")); err != nil {
		t.Fatal("stable lock pathname disappeared")
	}
}

func TestManagedCreateAndReconnectWithoutOperatorKeys(t *testing.T) {
	exe, root, file := managedFixture(t)
	out, stderr, err := runLifecycleFixture(t, exe, "create")
	if err != nil || !strings.Contains(out, "Machine ready") {
		t.Fatalf("managed create failed: %s %v", stderr, err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var vm vmManifest
	if err := json.Unmarshal(data, &vm); err != nil {
		t.Fatal(err)
	}
	if vm.Metadata.Annotations[managedModeAnnotation] != "managed" || vm.Metadata.Annotations[managedFingerprintAnnotation] == "" {
		t.Fatal("created VM lacks its managed identity binding")
	}
	if strings.Contains(out+stderr+string(data), "PRIVATE KEY") {
		t.Fatal("private key escaped the client credential store")
	}
	files, err := filepath.Glob(filepath.Join(root, "*", "id_rsa"))
	if err != nil || len(files) != 1 {
		t.Fatal("expected exactly one client identity")
	}
	key, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARVESTER_USER_DATA_FILE", filepath.Join(t.TempDir(), "removed-creation-file"))
	out, stderr, err = runLifecycleFixture(t, exe, "create")
	if err != nil || !strings.Contains(out, "Reusing existing") {
		t.Fatalf("reconnect reopened creation inputs: %s %v", stderr, err)
	}
	after, err := os.ReadFile(files[0])
	if err != nil || string(after) != string(key) {
		t.Fatal("reconnect replaced its key")
	}
	if err := os.Remove(files[0]); err != nil {
		t.Fatal(err)
	}
	_, stderr, err = runLifecycleFixture(t, exe, "create")
	if err == nil || !strings.Contains(stderr, "missing") {
		t.Fatalf("missing existing key was not refused: %s %v", stderr, err)
	}
	if _, err := os.Stat(files[0]); !os.IsNotExist(err) {
		t.Fatal("reconnect generated a replacement key")
	}
}

func TestManagedNamespacePermissionFailsBeforeWrites(t *testing.T) {
	exe, root, vm := managedFixture(t)
	t.Setenv("HARVESTER_TEST_NAMESPACE_FAILURE", "1")
	_, stderr, err := runLifecycleFixture(t, exe, "create")
	if err == nil || !strings.Contains(stderr, "namespace") {
		t.Fatalf("namespace access not diagnosed: %s %v", stderr, err)
	}
	for _, path := range []string{root, vm} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("state was created without a namespace identity")
		}
	}
}

func TestManagedInitValidatesPresetWithoutCreatingIdentity(t *testing.T) {
	exe, root, _ := managedFixture(t)
	t.Setenv("HARVESTER_CLOUD_INIT_PROFILE", "ubuntu-24.04-docker-v1")
	_, stderr, err := runLifecycleFixture(t, exe, "init")
	if err != nil {
		t.Fatalf("managed init required an operator public key: %s %v", stderr, err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("provider init generated placeholder machine credentials")
	}
}

func TestManagedReconnectRejectsRuntimeAndUserChanges(t *testing.T) {
	exe, _, _ := managedFixture(t)
	_, stderr, err := runLifecycleFixture(t, exe, "create")
	if err != nil {
		t.Fatalf("create: %s %v", stderr, err)
	}
	for _, option := range []string{"HARVESTER_SSH_USER", "HARVESTER_CONTAINER_RUNTIME"} {
		t.Run(option, func(t *testing.T) {
			value := "another-user"
			if option == "HARVESTER_CONTAINER_RUNTIME" {
				value = "podman"
			}
			t.Setenv(option, value)
			_, stderr, err := runLifecycleFixture(t, exe, "create")
			if err == nil || !strings.Contains(stderr, "original") {
				t.Fatalf("incompatible connection settings accepted: %s %v", stderr, err)
			}
		})
	}
}

func TestManagedCredentialRootOutsideExports(t *testing.T) {
	for _, variable := range []string{"PROVIDER_FOLDER", "MACHINE_FOLDER", "WORKSPACE_FOLDER"} {
		t.Run(variable, func(t *testing.T) {
			exe, _, _ := managedFixture(t)
			dir := t.TempDir()
			t.Setenv(variable, dir)
			t.Setenv("HARVESTER_MANAGED_SSH_DIR", filepath.Join(dir, "keys"))
			_, stderr, err := runLifecycleFixture(t, exe, "create")
			if err == nil || !strings.Contains(stderr, "export") {
				t.Fatalf("exported credential location accepted: %s %v", stderr, err)
			}
		})
	}
}

func TestMalformedSSHFlagsFailBeforeProvisioning(t *testing.T) {
	exe := lifecycleFixture(t)
	t.Setenv("HARVESTER_SSH_FLAGS", `-i "broken`)
	_, stderr, err := runLifecycleFixture(t, exe, "create")
	if err == nil || !strings.Contains(stderr, "quote") {
		t.Fatalf("invalid flags reached resource creation: %s %v", stderr, err)
	}
}

func TestManagedDeletionRetainsKeysUntilRemoteDeletionIsConfirmed(t *testing.T) {
	for _, failure := range []string{"", "vm", "disk", "replacement"} {
		t.Run(failure, func(t *testing.T) {
			exe, root, vmFile := managedFixture(t)
			_, stderr, err := runLifecycleFixture(t, exe, "create")
			if err != nil {
				t.Fatalf("create: %s %v", stderr, err)
			}
			data, err := os.ReadFile(vmFile)
			if err != nil {
				t.Fatal(err)
			}
			state := t.TempDir()
			if err := os.WriteFile(filepath.Join(state, "managed-fixture"), data, 0600); err != nil {
				t.Fatal(err)
			}
			var vm vmManifest
			if err := json.Unmarshal(data, &vm); err != nil {
				t.Fatal(err)
			}
			vm.Metadata.Name += "-rootdisk"
			vm.Metadata.UID = "disk-uid"
			disk, err := json.Marshal(vm)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(state, vm.Metadata.Name), disk, 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HARVESTER_TEST_DELETE_STATE", state)
			t.Setenv("HARVESTER_TEST_DELETE_CALLS", filepath.Join(t.TempDir(), "delete-calls"))
			if failure == "vm" {
				t.Setenv("HARVESTER_TEST_DELETE_FAILURES", "managed-fixture")
			} else if failure == "disk" {
				t.Setenv("HARVESTER_TEST_DELETE_FAILURES", "managed-fixture-rootdisk")
			} else if failure == "replacement" {
				t.Setenv("HARVESTER_TEST_REPLACE_ON_DELETE", "managed-fixture")
			}
			_, stderr, err = runLifecycleFixture(t, exe, "delete")
			if (err != nil) != (failure != "") {
				t.Fatalf("unexpected deletion result: %s %v", stderr, err)
			}
			keys, err := filepath.Glob(filepath.Join(root, "*", "id_rsa"))
			if err != nil || (len(keys) == 0) != (failure == "") {
				t.Fatal("credential deletion did not follow proven remote deletion")
			}
		})
	}
}
