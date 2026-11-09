package main

import (
	"bytes"
	"compress/gzip"
	"crypto/ecdh"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"
)

//go:embed profiles/ubuntu-24.04-docker-v1.yaml
var ubuntuDockerProfile string

//go:embed profiles/rocky-9-podman-v1.yaml
var rockyPodmanProfile string

type bootstrapProfile struct{ Runtime, UserData string }

func bootstrapProfiles() map[string]bootstrapProfile {
	return map[string]bootstrapProfile{
		"ubuntu-24.04-docker-v1": {Runtime: "docker", UserData: ubuntuDockerProfile},
		"rocky-9-podman-v1":      {Runtime: "podman", UserData: rockyPodmanProfile},
	}
}

func validateBundledProfile(c config, requireKey bool) (bootstrapProfile, error) {
	profile, ok := bootstrapProfiles()[c.CloudInitProfile]
	if !ok {
		return bootstrapProfile{}, errors.New("HARVESTER_CLOUD_INIT_PROFILE is not a supported bundled profile")
	}
	if containerRuntime(c) != profile.Runtime {
		return bootstrapProfile{}, errors.New("selected bootstrap profile requires its matching HARVESTER_CONTAINER_RUNTIME")
	}
	if c.SSHUser == "root" || !regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`).MatchString(c.SSHUser) {
		return bootstrapProfile{}, errors.New("bundled profile requires a valid non-root HARVESTER_SSH_USER")
	}
	if len(c.VMName) > 63 || !regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`).MatchString(c.VMName) {
		return bootstrapProfile{}, errors.New("bundled profile requires HARVESTER_VM_NAME to be a DNS hostname of at most 63 characters")
	}
	if requireKey && !validPublicKey(c.SSHPublicKey) {
		return bootstrapProfile{}, errors.New("bundled profile requires one valid Ed25519, RSA, or ECDSA HARVESTER_SSH_PUBLIC_KEY")
	}
	return profile, nil
}

func renderBundledProfile(c config) (string, error) {
	profile, err := validateBundledProfile(c, true)
	if err != nil {
		return "", err
	}
	t, err := template.New("bootstrap").Funcs(template.FuncMap{"quote": strconv.Quote}).Parse(profile.UserData)
	if err != nil {
		return "", errors.New("invalid embedded cloud-init profile")
	}
	var out bytes.Buffer
	if err = t.Execute(&out, c); err != nil {
		return "", errors.New("could not render embedded cloud-init profile")
	}
	if _, err := compressedUserData(out.String()); err != nil {
		return "", err
	}
	return out.String(), nil
}

func validPublicKey(key string) bool {
	if strings.ContainsAny(key, "\r\n\x00") || len(key) > 16384 {
		return false
	}
	fields := strings.Fields(key)
	if len(fields) < 2 {
		return false
	}
	wire, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return false
	}
	var parts [][]byte
	for len(wire) > 0 {
		if len(wire) < 4 {
			return false
		}
		n := binary.BigEndian.Uint32(wire)
		wire = wire[4:]
		if uint64(n) > uint64(len(wire)) {
			return false
		}
		parts = append(parts, wire[:n])
		wire = wire[n:]
	}
	if len(parts) < 2 || string(parts[0]) != fields[0] {
		return false
	}
	switch fields[0] {
	case "ssh-ed25519":
		return len(parts) == 2 && len(parts[1]) == 32
	case "ssh-rsa":
		if len(parts) != 3 || len(parts[1]) == 0 || len(parts[2]) == 0 || parts[1][0]&0x80 != 0 || parts[2][0]&0x80 != 0 {
			return false
		}
		e, n := new(big.Int).SetBytes(parts[1]), new(big.Int).SetBytes(parts[2])
		return e.IsInt64() && e.Int64() >= 3 && e.Int64() <= 1<<31-1 && e.Bit(0) == 1 && n.BitLen() >= 2048 && n.Bit(0) == 1
	case "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521":
		if len(parts) != 3 || fields[0] != "ecdsa-sha2-"+string(parts[1]) {
			return false
		}
		var curve ecdh.Curve
		switch string(parts[1]) {
		case "nistp256":
			curve = ecdh.P256()
		case "nistp384":
			curve = ecdh.P384()
		case "nistp521":
			curve = ecdh.P521()
		default:
			return false
		}
		_, err := curve.NewPublicKey(parts[2])
		return err == nil
	}
	return false
}

func guestReadyTimeout(c config) time.Duration {
	if c.CloudInitProfile != "" || c.SSHMode == "managed" {
		return 15 * time.Minute
	}
	return 90 * time.Second
}

// KubeVirt limits decoded inline data to 2 KiB. cloud-init recognizes gzip
// user-data by its magic bytes; no Secret or additional cluster resource is needed.
func compressedUserData(data string) (string, error) {
	var out bytes.Buffer
	writer := gzip.NewWriter(&out)
	if _, err := writer.Write([]byte(data)); err != nil {
		return "", errors.New("compress cloud-init user-data")
	}
	if err := writer.Close(); err != nil {
		return "", errors.New("compress cloud-init user-data")
	}
	if out.Len() > 2048 {
		return "", errors.New("cloud-init exceeds KubeVirt's 2 KiB compressed limit; shorten the source or use a smaller public key")
	}
	return base64.StdEncoding.EncodeToString(out.Bytes()), nil
}
