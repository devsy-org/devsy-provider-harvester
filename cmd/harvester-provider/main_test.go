package main

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func init() {
	if os.Getenv("HARVESTER_TEST_START_CHILD") == "1" {
		body, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(2)
		}
		args := strings.Join(os.Args[1:], " ")
		if args != "replace --raw /apis/subresources.kubevirt.io/v1/namespaces/default/virtualmachines/machine-1/start -f -" || string(body) != "{}" {
			if _, err := os.Stderr.WriteString("start must use the KubeVirt start subresource with an empty JSON object"); err != nil {
				os.Exit(2)
			}
			os.Exit(1)
		}
		os.Exit(0)
	}
}

func TestStartUsesKubeVirtStartSubresource(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARVESTER_TEST_START_CHILD", "1")
	t.Setenv("HARVESTER_KUBECTL_PATH", executable)
	if err := requestVMStart(config{Namespace: "default", VMName: "machine-1"}); err != nil {
		t.Fatal(err)
	}
}

func TestKubectlExplicitPathWorksWithoutShellPATH(t *testing.T) {
	if os.Getenv("HARVESTER_TEST_KUBECTL_CHILD") == "1" {
		if _, err := os.Stdout.WriteString("explicit-kubectl-path-ok"); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARVESTER_TEST_KUBECTL_CHILD", "1")
	t.Setenv("HARVESTER_KUBECTL_PATH", "kubectl")
	t.Setenv("KUBECTL", executable)
	t.Setenv("PATH", "")
	out, err := kubectl(config{}, "-test.run=TestKubectlExplicitPathWorksWithoutShellPATH")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "explicit-kubectl-path-ok" {
		t.Fatalf("unexpected output %q", out)
	}
}

func TestSSHTransportHasNoSessionDeadline(t *testing.T) {
	cmd := sshExecCommand(config{SSHPort: "22", SSHUser: "ubuntu"}, "host", "devsy-harvester-fixture-vm", "long-running command")
	if cmd.Cancel != nil {
		t.Fatal("Devsy SSH transports must remain open for the session lifetime")
	}
}

func TestBuildVMManifestUsesHarvesterImagePVCAndRunStrategy(t *testing.T) {
	c := config{Namespace: "default", VMName: "machine-1", Image: "harvester-public/ubuntu", CPU: "4", Memory: "8Gi", Disk: "40Gi"}
	manifest, err := buildVMManifest(c, imageInfo{Namespace: "harvester-public", Name: "ubuntu", StorageClass: "longhorn-image-harvester-public-ubuntu", Format: "raw"})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Spec.Running != nil {
		t.Fatal("manifest must not set deprecated running with runStrategy")
	}
	if manifest.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim == nil {
		t.Fatal("manifest must use a PVC-backed root disk")
	}
	if manifest.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName == "" {
		t.Fatal("root disk PVC claim name is required")
	}
}

func TestBuildVMManifestSetsHarvesterMemoryFields(t *testing.T) {
	for _, memory := range []string{"2Gi", "8Gi", "512Mi"} {
		c := config{Namespace: "default", VMName: "machine-1", CPU: "2", Memory: memory, Disk: "12Gi"}
		manifest, err := buildVMManifest(c, imageInfo{Format: "qcow2"})
		if err != nil {
			t.Fatal(err)
		}
		domain := manifest.Spec.Template.Spec.Domain
		if domain.Memory.Guest != memory || domain.Resources.Limits.Memory != memory {
			t.Fatalf("unexpected memory configuration: %#v", domain)
		}
	}
}

func TestBuildPVCManifestUsesImageStorageClassAndBlockMode(t *testing.T) {
	c := config{Namespace: "default", VMName: "machine-1", Disk: "40Gi"}
	pvc, err := buildPVCManifest(c, imageInfo{Namespace: "harvester-public", Name: "ubuntu", StorageClass: "longhorn-image-harvester-public-ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	if pvc.Spec.StorageClassName != "longhorn-image-harvester-public-ubuntu" {
		t.Fatalf("storage class: %#v", pvc.Spec.StorageClassName)
	}
	if pvc.Spec.VolumeMode != "Block" {
		t.Fatalf("volume mode: %#v", pvc.Spec.VolumeMode)
	}
}

func TestBuildVMManifestUsesCDROMForISOImage(t *testing.T) {
	c := config{Namespace: "default", VMName: "machine-1", CPU: "4", Memory: "8Gi", Disk: "40Gi"}
	manifest, err := buildVMManifest(c, imageInfo{Namespace: "default", Name: "installer", StorageClass: "longhorn-image-default-installer", Format: "iso"})
	if err != nil {
		t.Fatal(err)
	}
	disks := manifest.Spec.Template.Spec.Domain.Devices.Disks
	if len(disks) != 2 || disks[0].Name != "rootdisk" || disks[0].Disk == nil || disks[0].CDROM != nil || disks[1].Name != "installer" || disks[1].CDROM == nil {
		t.Fatalf("ISO requires a writable root disk and an installer CD-ROM: %#v", disks)
	}
}

func TestParseImageReference(t *testing.T) {
	got, err := parseImageReference("custom/image", "harvester-public")
	if err != nil {
		t.Fatal(err)
	}
	if got.Namespace != "custom" || got.Name != "image" {
		t.Fatalf("got %#v", got)
	}
}

func TestNotFoundErrorDetection(t *testing.T) {
	if !isNotFoundError(errors.New("kubectl: virtualmachine not found: exit status 1")) {
		t.Fatal("expected not-found error")
	}
	if isNotFoundError(errors.New("kubectl: forbidden: exit status 1")) {
		t.Fatal("forbidden must not be treated as not found")
	}
}

func TestStatusForVMUsesPhaseAndNotFound(t *testing.T) {
	if got := statusForVM(nil, true); got != "NotFound" {
		t.Fatalf("got %q", got)
	}
	if got := statusForVM(&vmStatus{Ready: true}, false); got != "Running" {
		t.Fatalf("got %q", got)
	}
	if got := statusForVM(&vmStatus{Ready: false, Phase: "Scheduling"}, false); got != "Busy" {
		t.Fatalf("got %q", got)
	}
	if got := statusForVM(&vmStatus{Ready: false, Phase: "Stopped"}, false); got != "Stopped" {
		t.Fatalf("got %q", got)
	}
}

func TestValidateConfigRequiresImageAndSSHBootstrap(t *testing.T) {
	c := config{Image: "ubuntu"}
	if err := validateConfig(c, "create"); err == nil {
		t.Fatal("expected missing SSH bootstrap configuration to fail")
	}
}
