package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

const commandTimeout = 2 * time.Minute

var errHostKeyVerification = errors.New("SSH host key verification failed; verify the VM's host key before updating its known_hosts entry")

var errBundledBootstrap = errors.New("bundled cloud-init failed; verify the cloud image matches its bootstrap profile and inspect cloud-init status and /var/log/cloud-init-output.log in the guest")

var errSSHAuthentication = errors.New("SSH authentication failed; check HARVESTER_SSH_USER and the client's identity file or agent")

var errEngineMissing = errors.New("selected container runtime is not installed in the guest")

var errEngineMismatch = errors.New("guest executable reports a different container runtime; check HARVESTER_CONTAINER_COMMAND")

var errEngineAccess = errors.New("selected container runtime is not accessible to the SSH user; check its service and user permissions")

var errBootstrapPrivilege = errors.New("bundled bootstrap needs passwordless sudo to inspect cloud-init status")

type config struct {
	Kubeconfig, Context, Namespace, VMName                      string
	Image, ImageNS, ImageType, StorageClass                     string
	CPU, Memory, Disk, Network                                  string
	SSHHost, SSHUser, SSHPort, SSHFlags, SSHPublicKey           string
	SSHMode, SSHKeyPath, ManagedSSHDir, ManagedSlot             string
	ManagedFingerprint, ContainerRuntime, ContainerCommand      string
	UserData, UserDataFile, CloudInitTemplate, CloudInitProfile string
	ExpectedVMUID, ExpectedRootDiskUID                          string
}

func env(n, fallback string) string {
	if v := os.Getenv(n); v != "" {
		return v
	}
	return fallback
}
func load() config {
	c := config{
		Kubeconfig:          env("KUBECONFIG", ""),
		Context:             os.Getenv("HARVESTER_CONTEXT"),
		Namespace:           env("HARVESTER_NAMESPACE", "default"),
		VMName:              env("HARVESTER_VM_NAME", env("MACHINE_ID", "devsy-workspace")),
		Image:               configuredImage(),
		ImageNS:             env("HARVESTER_IMAGE_NAMESPACE", "harvester-public"),
		ImageType:           env("HARVESTER_IMAGE_TYPE", "raw"),
		StorageClass:        os.Getenv("HARVESTER_STORAGE_CLASS"),
		CPU:                 env("HARVESTER_CPU", "4"),
		Memory:              env("HARVESTER_MEMORY", "8Gi"),
		Disk:                env("HARVESTER_DISK", "40Gi"),
		SSHHost:             os.Getenv("HARVESTER_SSH_HOST"),
		SSHUser:             env("HARVESTER_SSH_USER", "ubuntu"),
		SSHPort:             env("HARVESTER_SSH_PORT", "22"),
		SSHFlags:            os.Getenv("HARVESTER_SSH_FLAGS"),
		SSHPublicKey:        os.Getenv("HARVESTER_SSH_PUBLIC_KEY"),
		UserData:            os.Getenv("HARVESTER_USER_DATA"),
		UserDataFile:        os.Getenv("HARVESTER_USER_DATA_FILE"),
		CloudInitTemplate:   os.Getenv("HARVESTER_CLOUD_INIT_TEMPLATE"),
		Network:             os.Getenv("HARVESTER_NETWORK"),
		CloudInitProfile:    os.Getenv("HARVESTER_CLOUD_INIT_PROFILE"),
		SSHMode:             env("HARVESTER_SSH_MODE", "manual"),
		SSHKeyPath:          os.Getenv("HARVESTER_SSH_KEY_PATH"),
		ManagedSSHDir:       os.Getenv("HARVESTER_MANAGED_SSH_DIR"),
		ContainerRuntime:    env("HARVESTER_CONTAINER_RUNTIME", "docker"),
		ContainerCommand:    os.Getenv("HARVESTER_CONTAINER_COMMAND"),
		ExpectedVMUID:       os.Getenv("HARVESTER_EXPECTED_VM_UID"),
		ExpectedRootDiskUID: os.Getenv("HARVESTER_EXPECTED_ROOTDISK_UID"),
	}
	c.ContainerCommand = runtimeCommand(c)
	return c
}

func progress(message string) {
	_, _ = fmt.Fprintln(os.Stdout, "harvester: "+message)
}

func checkNetwork(c config) error {
	if c.Network == "" {
		return nil
	}
	namespace, name := c.Namespace, c.Network
	parts := strings.Split(c.Network, "/")
	if len(parts) == 2 {
		namespace, name = parts[0], parts[1]
	}
	if len(parts) > 2 || namespace == "" || name == "" {
		return errors.New("HARVESTER_NETWORK must be NAME or NAMESPACE/NAME")
	}
	progress("Checking VM network attachment")
	_, err := kubectl(c, "get", "network-attachment-definition", name, "-n", namespace, "-o", "name", "--request-timeout=10s")
	return err
}

type imageInfo struct{ Namespace, Name, StorageClass, Format string }

func parseImageReference(ref, defaultNamespace string) (imageInfo, error) {
	parts := strings.Split(ref, "/")
	if len(parts) > 2 || ref == "" {
		return imageInfo{}, errors.New("HARVESTER_IMAGE must be IMAGE or NAMESPACE/IMAGE")
	}
	if len(parts) == 1 {
		return imageInfo{Namespace: defaultNamespace, Name: parts[0]}, nil
	}
	return imageInfo{Namespace: parts[0], Name: parts[1]}, nil
}

type metadata struct {
	Name              string            `json:"name"`
	UID               string            `json:"uid,omitempty"`
	Namespace         string            `json:"namespace"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	DeletionTimestamp string            `json:"deletionTimestamp,omitempty"`
}
type vmVolume struct {
	Name                  string `json:"name"`
	PersistentVolumeClaim *struct {
		ClaimName string `json:"claimName"`
	} `json:"persistentVolumeClaim,omitempty"`
	CloudInitNoCloud map[string]string `json:"cloudInitNoCloud,omitempty"`
}
type vmBus struct {
	Bus string `json:"bus"`
}
type vmDisk struct {
	Name  string `json:"name"`
	Disk  *vmBus `json:"disk,omitempty"`
	CDROM *vmBus `json:"cdrom,omitempty"`
}
type vmInterface struct {
	Name   string   `json:"name"`
	Bridge struct{} `json:"bridge"`
}
type vmDomain struct {
	CPU struct {
		Cores int `json:"cores"`
	} `json:"cpu"`
	Memory struct {
		Guest string `json:"guest"`
	} `json:"memory"`
	Resources struct {
		Requests vmMemoryResource `json:"requests"`
		Limits   vmMemoryResource `json:"limits"`
	} `json:"resources"`
	Devices struct {
		Disks      []vmDisk      `json:"disks"`
		Interfaces []vmInterface `json:"interfaces"`
	} `json:"devices"`
}
type vmMemoryResource struct {
	Memory string `json:"memory"`
}
type vmMultus struct {
	NetworkName string `json:"networkName"`
}
type vmNetwork struct {
	Name   string    `json:"name"`
	Pod    *struct{} `json:"pod,omitempty"`
	Multus *vmMultus `json:"multus,omitempty"`
}
type pvcSpec struct {
	AccessModes      []string     `json:"accessModes"`
	Resources        pvcResources `json:"resources"`
	StorageClassName string       `json:"storageClassName"`
	VolumeMode       string       `json:"volumeMode"`
}
type pvcResources struct {
	Requests pvcStorageRequest `json:"requests"`
}
type pvcStorageRequest struct {
	Storage string `json:"storage"`
}
type vmManifest struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   metadata `json:"metadata"`
	Spec       struct {
		RunStrategy string `json:"runStrategy"`
		Running     *bool  `json:"running,omitempty"`
		Template    struct {
			Metadata metadata `json:"metadata"`
			Spec     struct {
				Domain   vmDomain    `json:"domain"`
				Networks []vmNetwork `json:"networks"`
				Volumes  []vmVolume  `json:"volumes"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}
type pvcManifest struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   metadata `json:"metadata"`
	Spec       pvcSpec  `json:"spec"`
}
type vmStatus struct {
	Ready bool
	Phase string
}

func validateConfig(c config, operation string) error {
	if operation == "create" && c.Image == "" {
		return errors.New("HARVESTER_IMAGE is required")
	}
	if operation == "create" && c.SSHPublicKey == "" && c.UserData == "" && c.SSHHost == "" {
		return errors.New("HARVESTER_SSH_PUBLIC_KEY or a cloud-init source (HARVESTER_USER_DATA, HARVESTER_USER_DATA_FILE, HARVESTER_CLOUD_INIT_TEMPLATE) is required when SSH host discovery is enabled")
	}
	if _, err := strconv.Atoi(c.SSHPort); err != nil {
		return fmt.Errorf("HARVESTER_SSH_PORT must be numeric: %w", err)
	}
	if c.ImageType != "raw" && c.ImageType != "qcow2" && c.ImageType != "iso" {
		return errors.New("HARVESTER_IMAGE_TYPE must be raw, qcow2, or iso")
	}
	return nil
}
func buildVMManifest(c config, image imageInfo) (vmManifest, error) {
	cpu, err := strconv.Atoi(c.CPU)
	if err != nil || cpu < 1 {
		return vmManifest{}, errors.New("HARVESTER_CPU must be a positive integer")
	}
	if c.Memory == "" || c.Disk == "" {
		return vmManifest{}, errors.New("HARVESTER_MEMORY and HARVESTER_DISK are required")
	}
	claim := c.VMName + "-rootdisk"
	if image.Format == "iso" {
		claim = c.VMName + "-blankdisk"
	}
	var m vmManifest
	m.APIVersion = "kubevirt.io/v1"
	m.Kind = "VirtualMachine"
	m.Metadata = metadata{Name: c.VMName, Namespace: c.Namespace, Labels: map[string]string{"devsy.sh/provider": "harvester", "devsy.sh/machine-id": c.VMName}}
	if c.CloudInitProfile != "" {
		m.Metadata.Annotations = map[string]string{"devsy.sh/cloud-init-profile": c.CloudInitProfile}
	}
	if c.SSHMode == "managed" {
		m.Metadata.Annotations = managedAnnotations(c)
	}
	m.Spec.RunStrategy = "RerunOnFailure"
	m.Spec.Template.Metadata = metadata{Labels: map[string]string{"devsy.sh/provider": "harvester"}}
	domain := &m.Spec.Template.Spec.Domain
	domain.CPU.Cores = cpu
	domain.Memory.Guest = c.Memory
	domain.Resources.Requests = vmMemoryResource{Memory: c.Memory}
	domain.Resources.Limits = vmMemoryResource{Memory: c.Memory}
	domain.Devices.Disks = []vmDisk{{Name: "rootdisk", Disk: &vmBus{Bus: "virtio"}}}
	domain.Devices.Interfaces = []vmInterface{{Name: "default"}}
	m.Spec.Template.Spec.Networks = []vmNetwork{{Name: "default", Pod: &struct{}{}}}
	if c.Network != "" {
		m.Spec.Template.Spec.Networks = []vmNetwork{{Name: "default", Multus: &vmMultus{NetworkName: c.Network}}}
	}
	m.Spec.Template.Spec.Volumes = []vmVolume{{Name: "rootdisk", PersistentVolumeClaim: &struct {
		ClaimName string `json:"claimName"`
	}{ClaimName: claim}}}
	if image.Format == "iso" {
		m.Spec.Template.Spec.Volumes = append(m.Spec.Template.Spec.Volumes, vmVolume{Name: "installer", PersistentVolumeClaim: &struct {
			ClaimName string `json:"claimName"`
		}{ClaimName: c.VMName + "-image"}})
		domain.Devices.Disks = append(domain.Devices.Disks, vmDisk{Name: "installer", CDROM: &vmBus{Bus: "sata"}})
	}
	userData := c.UserData
	if userData == "" && c.SSHPublicKey != "" {
		userData = "#cloud-config\nusers:\n  - name: " + c.SSHUser + "\n    sudo: ALL=(ALL) NOPASSWD:ALL\n    shell: /bin/bash\n    ssh_authorized_keys:\n      - " + c.SSHPublicKey + "\n"
	}
	if userData != "" {
		cloudInitData := map[string]string{"userData": userData}
		if c.CloudInitProfile != "" || c.SSHMode == "managed" {
			encoded, err := compressedUserData(userData)
			if err != nil {
				return vmManifest{}, err
			}
			cloudInitData = map[string]string{"userDataBase64": encoded}
		}
		m.Spec.Template.Spec.Volumes = append(m.Spec.Template.Spec.Volumes, vmVolume{Name: "cloudinit", CloudInitNoCloud: cloudInitData})
		domain.Devices.Disks = append(domain.Devices.Disks, vmDisk{Name: "cloudinit", Disk: &vmBus{Bus: "virtio"}})
	}
	return m, nil
}
func buildPVCManifest(c config, image imageInfo) (pvcManifest, error) {
	if image.Name == "" || image.StorageClass == "" {
		return pvcManifest{}, errors.New("harvester image metadata must include a storage class")
	}
	claim := c.VMName + "-rootdisk"
	annotations := map[string]string{"harvesterhci.io/imageId": image.Namespace + "/" + image.Name}
	for key, value := range managedAnnotations(c) {
		annotations[key] = value
	}
	if image.Format == "iso" {
		claim = c.VMName + "-image"
	}
	return pvcManifest{
		APIVersion: "v1",
		Kind:       "PersistentVolumeClaim",
		Metadata: metadata{
			Name:        claim,
			Namespace:   c.Namespace,
			Annotations: annotations,
			Labels:      map[string]string{"devsy.sh/provider": "harvester", "devsy.sh/machine-id": c.VMName},
		},
		Spec: pvcSpec{
			AccessModes:      []string{"ReadWriteMany"},
			Resources:        pvcResources{Requests: pvcStorageRequest{Storage: c.Disk}},
			StorageClassName: image.StorageClass,
			VolumeMode:       "Block",
		},
	}, nil
}
func buildBlankPVCManifest(c config, image imageInfo) pvcManifest {
	return pvcManifest{
		APIVersion: "v1",
		Kind:       "PersistentVolumeClaim",
		Metadata: metadata{
			Name:      c.VMName + "-blankdisk",
			Namespace: c.Namespace,
			Labels:    map[string]string{"devsy.sh/provider": "harvester", "devsy.sh/machine-id": c.VMName},
		},
		Spec: pvcSpec{
			AccessModes:      []string{"ReadWriteMany"},
			Resources:        pvcResources{Requests: pvcStorageRequest{Storage: c.Disk}},
			StorageClassName: c.StorageClass,
			VolumeMode:       "Block",
		},
	}
}
func statusForVM(s *vmStatus, notFound bool) string {
	if notFound || s == nil {
		return "NotFound"
	}
	switch strings.ToLower(s.Phase) {
	case "running":
		return "Running"
	case "stopped", "halted":
		return "Stopped"
	default:
		if s.Ready {
			return "Running"
		}
		return "Busy"
	}
}
func isNotFoundError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "not found")
}

func kubectlPath() string {
	if path := os.Getenv("HARVESTER_KUBECTL_PATH"); path != "" && path != "kubectl" {
		return path
	}
	return env("KUBECTL", "kubectl")
}

func requestVMStart(c config) error {
	endpoint := fmt.Sprintf("/apis/subresources.kubevirt.io/v1/namespaces/%s/virtualmachines/%s/start", c.Namespace, c.VMName)
	_, err := kubectlInput(c, []byte("{}"), "replace", "--raw", endpoint, "-f", "-")
	return err
}

func kubectl(c config, args ...string) ([]byte, error) {
	return kubectlInput(c, nil, args...)
}
func kubectlInput(c config, input []byte, args ...string) ([]byte, error) {
	return kubectlInputContext(context.Background(), c, input, args...)
}

func kubectlInputContext(parent context.Context, c config, input []byte, args ...string) ([]byte, error) {
	prefix := []string{}
	if c.Kubeconfig != "" {
		prefix = append(prefix, "--kubeconfig", c.Kubeconfig)
	}
	if c.Context != "" {
		prefix = append(prefix, "--context", c.Context)
	}
	ctx, cancel := context.WithTimeout(parent, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, kubectlPath(), append(prefix, args...)...)
	cmd.WaitDelay = time.Second
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return out, fmt.Errorf("kubectl: %s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return out, nil
}
func createJSON(c config, object any) error {
	payload, err := json.Marshal(object)
	if err != nil {
		return err
	}
	args := []string{}
	if c.Kubeconfig != "" {
		args = append(args, "--kubeconfig", c.Kubeconfig)
	}
	if c.Context != "" {
		args = append(args, "--context", c.Context)
	}
	args = append(args, "create", "-f", "-")
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, kubectlPath(), args...)
	cmd.Stdin = strings.NewReader(string(payload))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
func apply(c config) error {
	c.ContainerCommand = runtimeCommand(c)
	if err := validateSSHOptions(c); err != nil {
		return err
	}
	if c.SSHMode == "managed" {
		progress("Checking managed SSH namespace identity")
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		unlock, err := managedOperation(ctx, &c)
		if err != nil {
			return err
		}
		defer unlock()
	}
	reused, err := reuseExistingVM(&c)
	if err != nil {
		return err
	}
	if !reused {
		if c.SSHMode == "managed" {
			progress("Preparing managed SSH identity")
			if err := prepareManagedSSH(context.Background(), &c, true); err != nil {
				return err
			}
			progress("Managed SSH identity ready")
		}
		if err := provisionVM(c); err != nil {
			return err
		}
	}
	if err := waitForVM(c, "Running"); err != nil {
		return err
	}
	if err := waitForGuest(c); err != nil {
		return err
	}
	progress("Machine ready")
	return nil
}

func reuseExistingVM(c *config) (bool, error) {
	c.ContainerCommand = runtimeCommand(*c)
	out, err := kubectl(*c, "get", "virtualmachine", c.VMName, "-n", c.Namespace, "-o", "json", "--ignore-not-found")
	if err != nil {
		return false, err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return false, nil
	}
	var vm struct {
		Metadata metadata `json:"metadata"`
		Spec     struct {
			Template struct {
				Spec struct {
					Volumes []vmVolume `json:"volumes"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
		Status struct {
			Phase string `json:"printableStatus"`
		} `json:"status"`
	}
	if err := json.Unmarshal(out, &vm); err != nil {
		return false, fmt.Errorf("decode existing VM %s: %w", c.VMName, err)
	}
	if vm.Metadata.Labels["devsy.sh/provider"] != "harvester" || vm.Metadata.Labels["devsy.sh/machine-id"] != c.VMName {
		return false, fmt.Errorf("existing VM %s is not owned by this Harvester machine; refusing to reuse or overwrite it", c.VMName)
	}
	if vm.Metadata.DeletionTimestamp != "" {
		return false, fmt.Errorf("VM %s is deleting; wait for deletion to finish before retrying", c.VMName)
	}
	// Bootstrap options apply only at creation. Reattachment follows the VM's
	// original profile even if the saved provider configuration has changed.
	c.CloudInitProfile = vm.Metadata.Annotations["devsy.sh/cloud-init-profile"]
	if err := restoreManagedSSH(context.Background(), c, vm.Metadata); err != nil {
		return false, err
	}
	if err := validateManagedRootDisk(context.Background(), *c, vm.Metadata, vm.Spec.Template.Spec.Volumes); err != nil {
		return false, err
	}
	if c.CloudInitProfile != "" {
		if _, ok := bootstrapProfiles()[c.CloudInitProfile]; !ok {
			return false, errors.New("existing VM has an unsupported cloud-init profile; use the provider version that created it")
		}
	}
	progress("Reusing existing virtual machine without changing its VM or disk definitions")
	if strings.EqualFold(vm.Status.Phase, "Stopped") || strings.EqualFold(vm.Status.Phase, "Halted") {
		progress("Requesting existing virtual machine start")
		if err := requestVMStart(*c); err != nil {
			return false, err
		}
	}
	return true, nil
}

func ensurePVC(c config, pvc pvcManifest) error {
	out, err := kubectl(c, "get", "pvc", pvc.Metadata.Name, "-n", c.Namespace, "-o", "json", "--ignore-not-found")
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return createJSON(c, pvc)
	}
	var existing struct {
		Metadata metadata `json:"metadata"`
		Spec     struct {
			StorageClassName string   `json:"storageClassName"`
			VolumeMode       string   `json:"volumeMode"`
			AccessModes      []string `json:"accessModes"`
			Resources        struct {
				Requests map[string]string `json:"requests"`
			} `json:"resources"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(out, &existing); err != nil {
		return fmt.Errorf("decode existing disk claim %s: %w", pvc.Metadata.Name, err)
	}
	if existing.Metadata.Labels["devsy.sh/provider"] != "harvester" || existing.Metadata.Labels["devsy.sh/machine-id"] != c.VMName || existing.Metadata.DeletionTimestamp != "" {
		return fmt.Errorf("disk claim %s is not reusable: ownership labels must match and deletion must not be in progress", pvc.Metadata.Name)
	}
	if err := validateManagedBinding(c, existing.Metadata, true); err != nil {
		return err
	}
	if existing.Metadata.Annotations["harvesterhci.io/imageId"] != pvc.Metadata.Annotations["harvesterhci.io/imageId"] ||
		existing.Spec.StorageClassName != pvc.Spec.StorageClassName || existing.Spec.VolumeMode != "Block" ||
		len(existing.Spec.AccessModes) != 1 || existing.Spec.AccessModes[0] != "ReadWriteMany" || !equalQuantity(existing.Spec.Resources.Requests["storage"], c.Disk) {
		return fmt.Errorf("disk claim %s does not match the requested image, storage class, size or mode; refusing to modify it", pvc.Metadata.Name)
	}
	progress("Reusing existing disk claim " + pvc.Metadata.Name + " without changing its definition")
	return nil
}

func equalQuantity(a, b string) bool {
	x, err := resource.ParseQuantity(a)
	if err != nil {
		return false
	}
	y, err := resource.ParseQuantity(b)
	return err == nil && x.Cmp(y) == 0
}

func provisionVM(c config) error {
	progress("Resolving cloud-init configuration")
	userData, err := resolveUserData(c)
	if err != nil {
		return err
	}
	c.UserData = userData
	if err := validateConfig(c, "create"); err != nil {
		return err
	}
	if err := checkNetwork(c); err != nil {
		return err
	}
	progress("Resolving VM image")
	image, err := resolveImage(c)
	if err != nil {
		return err
	}
	if (c.CloudInitProfile != "" || c.SSHMode == "managed") && image.Format == "iso" {
		return errors.New("managed or bundled bootstrap requires an installed cloud image, not an ISO")
	}
	pvc, err := buildPVCManifest(c, image)
	if err != nil {
		return err
	}
	progress("Provisioning root disk")
	if err = ensurePVC(c, pvc); err != nil {
		return err
	}
	if image.Format == "iso" {
		if c.StorageClass == "" {
			return errors.New("HARVESTER_STORAGE_CLASS is required for ISO images")
		}
		if err = ensurePVC(c, buildBlankPVCManifest(c, image)); err != nil {
			return err
		}
	}
	progress("Creating virtual machine")
	vm, err := buildVMManifest(c, image)
	if err != nil {
		return err
	}
	// An intervening create must fail rather than overwrite another VM's spec.
	return createJSON(c, vm)
}
func waitForGuest(c config) error {
	ctx, cancel := context.WithTimeout(context.Background(), guestReadyTimeout(c))
	defer cancel()
	return waitForGuestContext(ctx, c)
}

func waitForGuestContext(ctx context.Context, c config) error {
	engine := "Docker"
	if containerRuntime(c) == "podman" {
		engine = "Podman"
	}
	progress("Waiting for guest IP, SSH and " + engine + " readiness")
	var lastErr, lastMeaningful error
	lastMessage := ""
	ipReported := false
	for ctx.Err() == nil {
		host, err := sshHostContext(ctx, c)
		if err == nil {
			if !ipReported {
				progress("Guest IP available; checking SSH and " + engine)
				ipReported = true
			}
			probeCtx, probeCancel := context.WithTimeout(ctx, 10*time.Second)
			err = probeGuest(probeCtx, c, host)
			probeCancel()
			if err == nil {
				progress("SSH and " + engine + " are ready")
				return nil
			}
			if errors.Is(err, errHostKeyVerification) || errors.Is(err, errBundledBootstrap) {
				return err
			}
		}
		lastErr = err
		if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			lastMeaningful = err
		}
		message := readinessDiagnostic(err)
		if message != lastMessage {
			progress(message)
			lastMessage = message
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	if lastMeaningful != nil {
		lastErr = lastMeaningful
	}
	return fmt.Errorf("timed out waiting for SSH and %s readiness; last failed check: %w", engine, errors.Join(ctx.Err(), lastErr))
}

func readinessDiagnostic(err error) string {
	switch {
	case errors.Is(err, errSSHAuthentication):
		return errSSHAuthentication.Error() + "; retrying while cloud-init may still be configuring access"
	case errors.Is(err, errEngineMissing):
		return errEngineMissing.Error() + "; waiting for bootstrap"
	case errors.Is(err, errEngineMismatch):
		return errEngineMismatch.Error() + "; retrying"
	case errors.Is(err, errEngineAccess):
		return errEngineAccess.Error() + "; retrying"
	case errors.Is(err, errBootstrapPrivilege):
		return errBootstrapPrivilege.Error() + "; waiting for guest user setup"
	case errors.Is(err, context.DeadlineExceeded):
		return "Guest readiness probe reached its deadline; bootstrap or connectivity may still be pending"
	default:
		return "Guest network, SSH or runtime is not ready; retrying"
	}
}

func probeGuest(ctx context.Context, c config, host string) error {
	c, alias, err := sshConnection(ctx, c)
	if err != nil {
		return err
	}
	engine := containerRuntime(c)
	commandWord := quoteGuestWord(runtimeCommand(c))
	pattern := "[Dd]ocker*"
	if engine == "podman" {
		pattern = "[Pp]odman*"
	}
	probe := "command -v " + commandWord + " >/dev/null 2>&1 || exit 43; case \"$(" + commandWord + " --version 2>/dev/null)\" in " + pattern + ") ;; *) exit 46;; esac; " + commandWord + " info >/dev/null 2>&1 || exit 44"
	if c.CloudInitProfile != "" {
		// A terminal bootstrap error must be visible even when Docker was never
		// installed. Use only a reserved exit code, never guest log contents.
		probe = "sudo -n true >/dev/null 2>&1 || exit 45; sudo -n cloud-init status --wait >/dev/null 2>&1 || exit 42; " + probe + "; systemctl is-active --quiet qemu-guest-agent"
		if engine == "docker" {
			probe += " && docker compose version >/dev/null 2>&1 && docker buildx version >/dev/null 2>&1"
		}
	}
	command := sshExecCommand(c, host, alias, probe)
	if command.Err != nil {
		return command.Err
	}
	cmd := exec.CommandContext(ctx, command.Path, command.Args[1:]...)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("SSH/Docker probe: %w", ctx.Err())
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			var diagnostic error
			switch exit.ExitCode() {
			case 43:
				diagnostic = errEngineMissing
			case 44:
				diagnostic = errEngineAccess
			case 45:
				if c.CloudInitProfile != "" {
					diagnostic = errBootstrapPrivilege
				}
			case 46:
				diagnostic = errEngineMismatch
			}
			if diagnostic != nil {
				if c.CloudInitProfile == "" {
					return fmt.Errorf("%w: %s: %w", diagnostic, strings.TrimSpace(string(out)), err)
				}
				return diagnostic
			}
		}
		if c.CloudInitProfile != "" && errors.As(err, &exit) && exit.ExitCode() == 42 {
			return errBundledBootstrap
		}
		if strings.Contains(string(out), "REMOTE HOST IDENTIFICATION HAS CHANGED") || strings.Contains(string(out), "Host key verification failed") {
			if c.CloudInitProfile != "" {
				return errHostKeyVerification
			}
			return fmt.Errorf("%w: %s", errHostKeyVerification, strings.TrimSpace(string(out)))
		}
		if strings.Contains(strings.ToLower(string(out)), "permission denied") && strings.Contains(strings.ToLower(string(out)), "publickey") {
			if c.CloudInitProfile != "" {
				return errSSHAuthentication
			}
			return fmt.Errorf("%w: %s: %w", errSSHAuthentication, strings.TrimSpace(string(out)), err)
		}
		if c.CloudInitProfile != "" {
			return fmt.Errorf("SSH/Docker probe failed; inspect guest cloud-init status and /var/log/cloud-init-output.log: %w", err)
		}
		return fmt.Errorf("SSH/Docker probe: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
func readVMStatus(ctx context.Context, c config) (*vmStatus, error) {
	out, err := kubectlInputContext(ctx, c, nil, "get", "virtualmachine", c.VMName, "-n", c.Namespace, "-o", "json")
	if err != nil {
		return nil, err
	}
	var p struct {
		Status struct {
			Ready           bool   `json:"ready"`
			PrintableStatus string `json:"printableStatus"`
		} `json:"status"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, err
	}
	return &vmStatus{Ready: p.Status.Ready, Phase: p.Status.PrintableStatus}, nil
}
func waitForVM(c config, desired string) error {
	progress("Waiting for virtual machine to become " + desired)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	return waitForVMContext(ctx, c, desired)
}
func waitForVMContext(ctx context.Context, c config, desired string) error {
	lastState := ""
	var lastErr error
	for ctx.Err() == nil {
		status, err := readVMStatus(ctx, c)
		if err == nil {
			lastErr = nil
			state := statusForVM(status, false)
			if status.Phase != lastState {
				progress("Virtual machine state: " + status.Phase)
				lastState = status.Phase
			}
			if state == desired {
				return nil
			}
		}
		if err != nil {
			lastErr = err
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("timed out waiting for VM %s to become %s: %w", c.VMName, desired, errors.Join(ctx.Err(), lastErr))
}
func sshHost(c config) (string, error) {
	return sshHostContext(context.Background(), c)
}

func sshHostContext(ctx context.Context, c config) (string, error) {
	if c.SSHHost != "" {
		return c.SSHHost, nil
	}
	out, err := kubectlInputContext(ctx, c, nil, "get", "vmi", c.VMName, "-n", c.Namespace, "-o", "json")
	if err != nil {
		return "", err
	}
	var p struct {
		Status struct {
			Interfaces []struct {
				Name string `json:"name"`
				IP   string `json:"ipAddress"`
			} `json:"interfaces"`
		} `json:"status"`
	}
	if err = json.Unmarshal(out, &p); err != nil {
		return "", err
	}
	for _, i := range p.Status.Interfaces {
		if i.Name == "default" && i.IP != "" {
			return i.IP, nil
		}
	}
	return "", errors.New("VM has no IP address on its default network yet")
}
func sshCommand(c config, command string) error {
	host, err := sshHost(c)
	if err != nil {
		return err
	}
	return sshRun(c, host, command)
}
func sshRun(c config, host, command string) error {
	c, alias, err := sshConnection(context.Background(), c)
	if err != nil {
		return err
	}
	x := sshExecCommand(c, host, alias, command)
	x.Stdin, x.Stdout, x.Stderr = os.Stdin, os.Stdout, os.Stderr
	return x.Run()
}
func sshHostKeyAlias(ctx context.Context, c config) (string, error) {
	out, err := kubectlInputContext(ctx, c, nil, "get", "virtualmachine", c.VMName, "-n", c.Namespace, "-o", "jsonpath={.metadata.uid}")
	if err != nil {
		return "", fmt.Errorf("resolve VM identity for SSH: %w", err)
	}
	uid := strings.TrimSpace(string(out))
	if uid == "" || strings.IndexFunc(uid, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-'
	}) >= 0 {
		return "", errors.New("VM has no valid UID for SSH host key verification")
	}
	return "devsy-harvester-" + uid, nil
}

func sshExecCommand(c config, host, hostKeyAlias, command string) *exec.Cmd {
	args := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "HostKeyAlias=" + hostKeyAlias, "-o", "CheckHostIP=no", "-p", c.SSHPort}
	if c.SSHKeyPath != "" {
		args = append(args, "-i", c.SSHKeyPath, "-o", "IdentitiesOnly=yes")
	}
	flags, err := splitSSHFlags(c.SSHFlags)
	args = append(args, flags...)
	args = append(args, c.SSHUser+"@"+host, command)
	// This is Devsy's long-lived transport, not a bounded control-plane call.
	cmd := exec.Command("ssh", args...)
	if err != nil {
		cmd.Err = err
	}
	return cmd
}

func ownedMetadata(c config, kind, name string) (*metadata, error) {
	return ownedMetadataContext(context.Background(), c, kind, name)
}

func ownedMetadataContext(ctx context.Context, c config, kind, name string) (*metadata, error) {
	out, err := kubectlInputContext(ctx, c, nil, "get", kind, name, "-n", c.Namespace, "-o", "json", "--ignore-not-found")
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}
	var obj struct {
		Metadata metadata `json:"metadata"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		return nil, err
	}
	m := &obj.Metadata
	if m.Labels["devsy.sh/provider"] != "harvester" || m.Labels["devsy.sh/machine-id"] != c.VMName || m.UID == "" {
		return nil, fmt.Errorf("%s %s is not owned by this Harvester machine or has no UID", kind, name)
	}
	return m, nil
}

func deleteOwned(c config, kind, name, endpoint string) (string, error) {
	return deleteOwnedSnapshot(c, kind, name, endpoint, nil)
}

func deleteOwnedSnapshot(c config, kind, name, endpoint string, expected *string) (string, error) {
	m, err := ownedMetadata(c, kind, name)
	if err != nil || m == nil {
		return "", err
	}
	if m.Annotations[managedModeAnnotation] == "managed" && c.SSHMode != "managed" {
		return "", errors.New("managed resource requires its original managed SSH mode before deletion")
	}
	if expected != nil && m.UID != *expected {
		return "", fmt.Errorf("%s %s was replaced after managed deletion began", kind, name)
	}
	payload, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": map[string]string{"uid": m.UID}, "propagationPolicy": "Foreground"})
	if err != nil {
		return "", err
	}
	_, err = kubectlInput(c, payload, "delete", "--raw", endpoint, "-f", "-")
	if isNotFoundError(err) {
		return "", nil
	}
	return m.UID, err
}

func waitDeleted(c config, kind, name, uid string) error {
	if uid == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	for {
		out, err := kubectlInputContext(ctx, c, nil, "get", kind, name, "-n", c.Namespace, "-o", "json", "--ignore-not-found")
		if err != nil {
			return err
		}
		if len(bytes.TrimSpace(out)) == 0 {
			return nil
		}
		var obj struct {
			Metadata metadata `json:"metadata"`
		}
		if err := json.Unmarshal(out, &obj); err != nil {
			return err
		}
		if obj.Metadata.UID != uid {
			return fmt.Errorf("%s %s was replaced during deletion", kind, name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func deleteMachine(c config) error {
	if len(c.Namespace) > 63 || !kubernetesName.MatchString(c.Namespace) {
		return fmt.Errorf("invalid Harvester namespace %q", c.Namespace)
	}
	if len(c.VMName) > 253 || !kubernetesName.MatchString(c.VMName) {
		return fmt.Errorf("invalid Harvester VM name %q", c.VMName)
	}
	if c.SSHMode == "managed" {
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		unlock, err := managedOperation(ctx, &c)
		if err != nil {
			return err
		}
		defer unlock()
	}
	snapshot, removeKey, err := managedDeletionSnapshot(&c)
	if err != nil {
		return err
	}
	pinned := map[string]string{c.VMName: c.ExpectedVMUID, c.VMName + "-rootdisk": c.ExpectedRootDiskUID}
	for name, expected := range pinned {
		if expected == "" {
			continue
		}
		uid := snapshot[name]
		if snapshot == nil {
			kind := "pvc"
			if name == c.VMName {
				kind = "virtualmachine"
			}
			meta, err := ownedMetadata(c, kind, name)
			if err != nil {
				return err
			}
			if meta != nil {
				uid = meta.UID
			}
		}
		if uid != "" && uid != expected {
			return fmt.Errorf("resource %s differs from the confirmed deletion UID; refusing cleanup", name)
		}
	}
	deleteResource := func(kind, name, endpoint string) (string, error) {
		if expected := pinned[name]; expected != "" {
			return deleteOwnedSnapshot(c, kind, name, endpoint, &expected)
		}
		if snapshot == nil {
			return deleteOwned(c, kind, name, endpoint)
		}
		uid := snapshot[name]
		return deleteOwnedSnapshot(c, kind, name, endpoint, &uid)
	}
	progress("Deleting virtual machine; waiting for VM and dependent resources")
	vmPath := fmt.Sprintf("/apis/kubevirt.io/v1/namespaces/%s/virtualmachines/%s", c.Namespace, c.VMName)
	uid, err := deleteResource("virtualmachine", c.VMName, vmPath)
	if err != nil {
		return err
	}
	if err := waitDeleted(c, "virtualmachine", c.VMName, uid); err != nil {
		return err
	}

	progress("Virtual machine deletion confirmed; deleting disk claims")
	var failures []error
	for _, suffix := range []string{"-rootdisk", "-image", "-blankdisk"} {
		name := c.VMName + suffix
		path := fmt.Sprintf("/api/v1/namespaces/%s/persistentvolumeclaims/%s", c.Namespace, name)
		uid, err := deleteResource("pvc", name, path)
		if err == nil {
			err = waitDeleted(c, "pvc", name, uid)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("delete disk claim %s: %w", name, err))
		}
	}
	if err := errors.Join(failures...); err != nil {
		return err
	}
	if removeKey {
		if err := removeManagedIdentity(c); err != nil {
			return err
		}
		progress("Managed SSH identity removed after confirmed VM and disk deletion")
	}
	progress("VM and PVC deletion confirmed")
	return nil
}

func main() {
	c := load()
	op := "command"
	if len(os.Args) > 1 {
		op = os.Args[1]
	}
	var err error
	switch op {
	case "runtime-command":
		if containerRuntime(c) != "docker" && containerRuntime(c) != "podman" {
			err = errors.New("HARVESTER_CONTAINER_RUNTIME must be docker or podman")
		} else {
			fmt.Print(defaultRuntimeCommand(c))
			return
		}
	case "image-options":
		err = printImageOptions(c, os.Stdout)
	case "images":
		fs := flag.NewFlagSet("images", flag.ContinueOnError)
		asJSON := fs.Bool("json", false, "Output the image catalog as JSON")
		err = fs.Parse(os.Args[2:])
		if err == nil {
			err = printImages(c, *asJSON, os.Stdout)
		}
	case "init":
		progress("Resolving cloud-init configuration")
		err = initializeCloudInit(c)
		if err == nil {
			err = checkNetwork(c)
		}
		if err == nil {
			progress("Checking Harvester API connectivity")
			_, err = kubectl(c, "version", "--request-timeout=10s")
		}
		if err == nil {
			progress("Provider initialized")
		}
	case "create":
		err = apply(c)
	case "delete":
		err = deleteMachine(c)
	case "start":
		progress("Requesting virtual machine start")
		err = requestVMStart(c)
		if err == nil {
			err = waitForVM(c, "Running")
		}
	case "stop":
		progress("Requesting virtual machine shutdown")
		_, err = kubectl(c, "patch", "virtualmachine", c.VMName, "-n", c.Namespace, "--type=merge", "-p", `{"spec":{"runStrategy":"Halted"}}`)
		if err == nil {
			err = waitForVM(c, "Stopped")
		}
	case "status":
		out, e := kubectl(c, "get", "virtualmachine", c.VMName, "-n", c.Namespace, "-o", "json")
		if e != nil {
			if isNotFoundError(e) {
				fmt.Print("NotFound")
				return
			}
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		var p struct {
			Status struct {
				Ready           bool   `json:"ready"`
				PrintableStatus string `json:"printableStatus"`
			} `json:"status"`
		}
		if e = json.Unmarshal(out, &p); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		fmt.Print(statusForVM(&vmStatus{Ready: p.Status.Ready, Phase: p.Status.PrintableStatus}, false))
		return
	case "command":
		fs := flag.NewFlagSet("command", flag.ExitOnError)
		command := fs.String("command", "", "")
		_ = fs.Parse(os.Args[2:])
		err = sshCommand(c, *command)
	default:
		fmt.Fprintln(os.Stderr, "usage: harvester-provider {init|create|delete|start|stop|status|command|images|image-options}")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
