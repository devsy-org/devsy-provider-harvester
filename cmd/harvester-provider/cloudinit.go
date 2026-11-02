package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Source contents are never included in errors or normal provider output.
func resolveUserData(c config) (string, error) {
	sources := 0
	for _, s := range []string{c.UserData, c.UserDataFile, c.CloudInitTemplate, c.CloudInitProfile} {
		if s != "" {
			sources++
		}
	}
	if sources > 1 {
		return "", errors.New("choose only one of HARVESTER_USER_DATA, HARVESTER_USER_DATA_FILE, HARVESTER_CLOUD_INIT_TEMPLATE, or HARVESTER_CLOUD_INIT_PROFILE")
	}
	if sources == 0 {
		return "", nil
	}
	data := c.UserData
	if c.CloudInitProfile != "" {
		var err error
		data, err = renderBundledProfile(c)
		if err != nil {
			return "", err
		}
	}
	if c.UserDataFile != "" {
		content, err := os.ReadFile(c.UserDataFile)
		if err != nil {
			return "", fmt.Errorf("read HARVESTER_USER_DATA_FILE: %w", err)
		}
		data = string(content)
	}
	if c.CloudInitTemplate != "" {
		parts := strings.Split(c.CloudInitTemplate, "/")
		ns, name := c.Namespace, ""
		switch len(parts) {
		case 1:
			name = parts[0]
		case 2:
			ns, name = parts[0], parts[1]
		default:
			return "", errors.New("HARVESTER_CLOUD_INIT_TEMPLATE must be NAME or NAMESPACE/NAME")
		}
		dnsLabel := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
		if len(ns) > 63 || !dnsLabel.MatchString(ns) || len(name) > 253 {
			return "", errors.New("invalid cloud-init template namespace/name")
		}
		for _, label := range strings.Split(name, ".") {
			if len(label) > 63 || !dnsLabel.MatchString(label) {
				return "", errors.New("invalid cloud-init template name")
			}
		}
		out, err := kubectl(c, "get", "configmap", name, "-n", ns, "-o", "json")
		if err != nil {
			return "", fmt.Errorf("read cloud-init template %s/%s: %w", ns, name, err)
		}
		var template struct {
			Data map[string]string `json:"data"`
		}
		if err := json.Unmarshal(out, &template); err != nil {
			return "", errors.New("cloud-init template returned invalid ConfigMap JSON")
		}
		data = template.Data["cloudInit"]
		if strings.TrimSpace(data) == "" {
			return "", fmt.Errorf("cloud-init template %s/%s has no non-empty data.cloudInit", ns, name)
		}
	}
	if strings.TrimSpace(data) == "" {
		return "", errors.New("selected cloud-init source is empty")
	}
	if len(data) > 65536 {
		return "", errors.New("cloud-init user-data exceeds 64 KiB")
	}
	return data, nil
}
