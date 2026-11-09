package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	managedModeAnnotation        = "devsy.sh/ssh-mode"
	managedFingerprintAnnotation = "devsy.sh/ssh-fingerprint"
	managedUserAnnotation        = "devsy.sh/ssh-user"
	managedRuntimeAnnotation     = "devsy.sh/container-runtime"
	managedDigestAnnotation      = "devsy.sh/cloud-init-sha256"
	profileAnnotation            = "devsy.sh/cloud-init-profile"
	managedPublicKeyMarker       = "{{HARVESTER_SSH_PUBLIC_KEY}}"
)

func containerRuntime(c config) string {
	if c.ContainerRuntime == "" {
		return "docker"
	}
	return c.ContainerRuntime
}

func validateSSHOptions(c config) error {
	if c.SSHMode != "" && c.SSHMode != "manual" && c.SSHMode != "managed" {
		return errors.New("HARVESTER_SSH_MODE must be manual or managed")
	}
	if containerRuntime(c) != "docker" && containerRuntime(c) != "podman" {
		return errors.New("HARVESTER_CONTAINER_RUNTIME must be docker or podman")
	}
	flags, err := splitSSHFlags(c.SSHFlags)
	if err != nil {
		return err
	}
	for i, arg := range flags {
		if strings.HasPrefix(arg, "-i") || strings.EqualFold(arg, "IdentityFile") || strings.HasPrefix(strings.ToLower(arg), "identityfile=") {
			if c.SSHMode == "managed" {
				return errors.New("managed SSH selects its own identity; clear -i/IdentityFile from HARVESTER_SSH_FLAGS")
			}
			path := strings.TrimPrefix(arg, "-i")
			if arg == "-i" || strings.EqualFold(arg, "IdentityFile") {
				if i+1 >= len(flags) {
					return errors.New("HARVESTER_SSH_FLAGS identity option requires a path")
				}
				path = flags[i+1]
			} else if strings.Contains(arg, "=") {
				_, path, _ = strings.Cut(arg, "=")
			}
			if _, err := os.Stat(path); err != nil {
				return errors.New("SSH identity file is not accessible; check HARVESTER_SSH_FLAGS on this client")
			}
		}
	}
	if c.SSHKeyPath != "" {
		if c.SSHMode == "managed" {
			return errors.New("managed SSH selects its own identity; clear HARVESTER_SSH_KEY_PATH")
		}
		if _, err := os.Stat(c.SSHKeyPath); err != nil {
			return errors.New("HARVESTER_SSH_KEY_PATH is not accessible on this client")
		}
	}
	return nil
}

func initializeCloudInit(c config) error {
	if err := validateSSHOptions(c); err != nil {
		return err
	}
	if c.SSHMode != "managed" {
		_, err := resolveUserData(c)
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	if _, err := managedNamespaceUID(ctx, c); err != nil {
		return err
	}
	custom := c.UserData != "" || c.UserDataFile != "" || c.CloudInitTemplate != ""
	if custom {
		c.SSHPublicKey = ""
		_, err := resolveUserData(c)
		return err
	}
	if c.CloudInitProfile == "" {
		c.CloudInitProfile = "ubuntu-24.04-docker-v1"
	}
	_, err := validateBundledProfile(c, false)
	return err
}

func canonicalStatePath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var missing []string
	for {
		real, err := filepath.EvalSymlinks(path)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				real = filepath.Join(real, missing[i])
			}
			return real, nil
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(path) == path {
			return "", err
		}
		missing = append(missing, filepath.Base(path))
		path = filepath.Dir(path)
	}
}

func managedStateRoot(c config) (string, error) {
	root := c.ManagedSSHDir
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".ssh", "devsy-harvester")
	}
	canonical, err := canonicalStatePath(root)
	if err != nil {
		return "", err
	}
	for _, variable := range []string{"PROVIDER_FOLDER", "MACHINE_FOLDER", "WORKSPACE_FOLDER"} {
		if value := os.Getenv(variable); value != "" {
			dir, err := canonicalStatePath(value)
			if err != nil {
				return "", err
			}
			rel, err := filepath.Rel(dir, canonical)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return "", errors.New("managed SSH credentials must be outside exported provider, machine and workspace directories")
			}
		}
	}
	// Validate the requested directory itself rather than accepting a symlink alias.
	if info, err := os.Lstat(root); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("managed SSH directory must not be a symlink")
	}
	return canonical, nil
}

func managedNamespaceUID(ctx context.Context, c config) (string, error) {
	out, err := kubectlInputContext(ctx, c, nil, "get", "namespace", c.Namespace, "-o", "jsonpath={.metadata.uid}")
	if err != nil {
		return "", fmt.Errorf("managed SSH requires get permission on namespace %s: %w", c.Namespace, err)
	}
	uid := strings.TrimSpace(string(out))
	if uid == "" || strings.IndexFunc(uid, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-')
	}) >= 0 {
		return "", errors.New("managed SSH namespace has no valid UID")
	}
	return uid, nil
}

func managedLocation(ctx context.Context, c *config) error {
	if c.ManagedSlot != "" {
		return nil
	}
	uid, err := managedNamespaceUID(ctx, *c)
	if err != nil {
		return err
	}
	root, err := managedStateRoot(*c)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(uid + "\x00" + c.VMName))
	c.ManagedSSHDir, c.ManagedSlot = root, hex.EncodeToString(sum[:])
	return nil
}

func managedOperation(ctx context.Context, c *config) (func(), error) {
	if err := managedLocation(ctx, c); err != nil {
		return nil, err
	}
	return lockManagedSlot(ctx, c.ManagedSSHDir, c.ManagedSlot)
}

func prepareManagedSSH(ctx context.Context, c *config, create bool) error {
	if err := managedLocation(ctx, c); err != nil {
		return err
	}
	var disks []metadata
	if create {
		for _, suffix := range []string{"-rootdisk", "-image", "-blankdisk"} {
			out, err := kubectl(*c, "get", "pvc", c.VMName+suffix, "-n", c.Namespace, "-o", "json", "--ignore-not-found")
			if err != nil {
				return err
			}
			if len(bytes.TrimSpace(out)) == 0 {
				continue
			}
			var disk struct {
				Metadata metadata `json:"metadata"`
			}
			if err := json.Unmarshal(out, &disk); err != nil {
				return errors.New("cannot decode existing disk identity")
			}
			if disk.Metadata.Labels["devsy.sh/provider"] != "harvester" || disk.Metadata.Labels["devsy.sh/machine-id"] != c.VMName || disk.Metadata.Annotations[managedModeAnnotation] != "managed" {
				return errors.New("existing disk does not belong to this managed SSH machine; refusing to change it")
			}
			disks = append(disks, disk.Metadata)
		}
	}
	identity, err := managedIdentityAt(c.ManagedSSHDir, c.ManagedSlot, create && len(disks) == 0)
	if err != nil {
		return err
	}
	c.SSHKeyPath, c.SSHPublicKey, c.ManagedFingerprint = identity.KeyPath, identity.PublicKey, identity.Fingerprint
	if create && c.CloudInitProfile == "" && c.UserData == "" && c.UserDataFile == "" && c.CloudInitTemplate == "" {
		c.CloudInitProfile = "ubuntu-24.04-docker-v1"
	}
	for _, disk := range disks {
		if err := validateManagedBinding(*c, disk, false); err != nil {
			return err
		}
	}
	return nil
}

func managedAnnotations(c config) map[string]string {
	if c.SSHMode != "managed" {
		return nil
	}
	digest := sha256.Sum256([]byte(c.UserData))
	return map[string]string{
		managedModeAnnotation: "managed", managedFingerprintAnnotation: c.ManagedFingerprint,
		managedUserAnnotation: c.SSHUser, managedRuntimeAnnotation: containerRuntime(c),
		profileAnnotation: c.CloudInitProfile, managedDigestAnnotation: hex.EncodeToString(digest[:]),
	}
}

func validateManagedBinding(c config, meta metadata, checkDigest bool) error {
	if meta.Annotations[managedModeAnnotation] != "managed" {
		if c.SSHMode == "managed" {
			return errors.New("existing resource uses manual SSH; managed bootstrap applies only to new machines")
		}
		return nil
	}
	if c.SSHMode != "managed" {
		return errors.New("existing resource requires its original managed SSH mode")
	}
	for key, value := range managedAnnotations(c) {
		if key == managedDigestAnnotation && !checkDigest {
			continue
		}
		if meta.Annotations[key] != value {
			return fmt.Errorf("existing resource requires its original managed SSH key, user, runtime and bootstrap settings (%s differs)", key)
		}
	}
	return nil
}

func restoreManagedSSH(ctx context.Context, c *config, meta metadata) error {
	if meta.Annotations[managedModeAnnotation] != "managed" {
		return validateManagedBinding(*c, meta, false)
	}
	if c.SSHMode != "managed" {
		return errors.New("existing VM requires its original managed SSH mode")
	}
	if err := prepareManagedSSH(ctx, c, false); err != nil {
		return err
	}
	return validateManagedBinding(*c, meta, false)
}

func sshConnection(ctx context.Context, c config) (config, string, error) {
	if c.SSHMode != "managed" {
		alias, err := sshHostKeyAlias(ctx, c)
		return c, alias, err
	}
	out, err := kubectlInputContext(ctx, c, nil, "get", "virtualmachine", c.VMName, "-n", c.Namespace, "-o", "json")
	if err != nil {
		return c, "", fmt.Errorf("resolve managed VM identity for SSH: %w", err)
	}
	var vm struct {
		Metadata metadata `json:"metadata"`
	}
	if err := json.Unmarshal(out, &vm); err != nil {
		return c, "", err
	}
	if vm.Metadata.UID == "" || vm.Metadata.Labels["devsy.sh/provider"] != "harvester" || vm.Metadata.Labels["devsy.sh/machine-id"] != c.VMName {
		return c, "", errors.New("SSH target is not the owned managed VM")
	}
	c.CloudInitProfile = vm.Metadata.Annotations[profileAnnotation]
	if err := restoreManagedSSH(ctx, &c, vm.Metadata); err != nil {
		return c, "", err
	}
	return c, "devsy-harvester-" + vm.Metadata.UID, nil
}

func managedDeletionSnapshot(c *config) (map[string]string, bool, error) {
	if c.SSHMode != "managed" {
		return nil, false, nil
	}
	resources := []struct{ kind, name string }{{"virtualmachine", c.VMName}, {"pvc", c.VMName + "-rootdisk"}, {"pvc", c.VMName + "-image"}, {"pvc", c.VMName + "-blankdisk"}}
	snapshot := map[string]string{}
	var identities []metadata
	for _, resource := range resources {
		meta, err := ownedMetadata(*c, resource.kind, resource.name)
		if err != nil {
			return nil, false, err
		}
		snapshot[resource.name] = ""
		if meta != nil {
			snapshot[resource.name] = meta.UID
			identities = append(identities, *meta)
		}
	}
	if len(identities) == 0 {
		return snapshot, false, nil
	}
	c.CloudInitProfile = identities[0].Annotations[profileAnnotation]
	if err := prepareManagedSSH(context.Background(), c, false); err != nil {
		return nil, false, err
	}
	for _, meta := range identities {
		if err := validateManagedBinding(*c, meta, false); err != nil {
			return nil, false, err
		}
	}
	return snapshot, true, nil
}

func removeManagedIdentity(c config) error {
	for _, resource := range []struct{ kind, name string }{{"virtualmachine", c.VMName}, {"pvc", c.VMName + "-rootdisk"}, {"pvc", c.VMName + "-image"}, {"pvc", c.VMName + "-blankdisk"}} {
		meta, err := ownedMetadata(c, resource.kind, resource.name)
		if err != nil {
			return err
		}
		if meta != nil {
			return errors.New("resource appeared during managed cleanup; retaining the SSH identity")
		}
	}
	identity, err := managedIdentityAt(c.ManagedSSHDir, c.ManagedSlot, false)
	if err != nil {
		return err
	}
	if identity.Fingerprint != c.ManagedFingerprint {
		return errors.New("managed SSH identity changed during deletion; refusing local cleanup")
	}
	if err := os.Remove(identity.KeyPath); err != nil {
		return err
	}
	// Leave stable lock files and any unrelated directory contents in place.
	_ = os.Remove(filepath.Dir(identity.KeyPath))
	return nil
}
