package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var versionPattern = regexp.MustCompile(`(?m)^version: (v[0-9]+\.[0-9]+\.[0-9]+)$`)
var entryPattern = regexp.MustCompile(`(?m)^    - os:`)
var helperPattern = regexp.MustCompile(`(?m)^    - os: (linux|darwin|windows)\n      arch: (amd64|arm64)\n      path: (https://[^\r\n]*/harvester-provider-[^\r\n]+)$`)

var helpers = []string{
	"harvester-provider-darwin-amd64",
	"harvester-provider-darwin-arm64",
	"harvester-provider-linux-amd64",
	"harvester-provider-linux-arm64",
	"harvester-provider-windows-amd64.exe",
}

func packageRelease(source, dist, tag string) error {
	manifest, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	if bytes.Contains(manifest, []byte("\r\n")) {
		manifest = bytes.ReplaceAll(manifest, []byte("\r\n"), []byte("\n"))
	}
	version := versionPattern.FindStringSubmatch(string(manifest))
	if len(version) != 2 || version[1] != tag {
		return fmt.Errorf("release tag must match provider version")
	}

	sections := strings.Split(string(manifest), "\n  HARVESTER_PROVIDER:\n")
	if len(sections) != 2 || len(entryPattern.FindAllString(sections[1], -1)) != len(helpers) {
		return fmt.Errorf("expected one helper section with %d platform entries", len(helpers))
	}
	matches := helperPattern.FindAllStringSubmatchIndex(string(manifest), -1)
	if len(matches) != len(helpers) {
		return fmt.Errorf("expected %d platform binaries, got %d", len(helpers), len(matches))
	}
	allowed := make(map[string]bool, len(helpers))
	for _, name := range helpers {
		allowed[name] = true
	}
	prefix := "https://github.com/devsy-org/devsy-provider-harvester/releases/download/" + tag + "/"
	checksums := make(map[string][sha256.Size]byte, len(helpers)+1)
	parts := make([]string, 0, 2*len(matches)+1)
	last := 0
	for _, match := range matches {
		osName := string(manifest[match[2]:match[3]])
		arch := string(manifest[match[4]:match[5]])
		url := string(manifest[match[6]:match[7]])
		name := strings.TrimPrefix(url, prefix)
		expected := "harvester-provider-" + osName + "-" + arch
		if osName == "windows" {
			expected += ".exe"
		}
		if !strings.HasPrefix(url, prefix) || !allowed[name] || name != expected {
			return fmt.Errorf("unexpected or duplicate helper URL %q", url)
		}
		if strings.HasPrefix(string(manifest[match[1]:]), "\n      checksum:") {
			return fmt.Errorf("helper %s already has a checksum", name)
		}
		delete(allowed, name)
		digest, err := hashHelper(filepath.Join(dist, name))
		if err != nil {
			return fmt.Errorf("hash helper %s: %w", name, err)
		}
		checksums[name] = digest
		parts = append(parts, string(manifest[last:match[1]]), fmt.Sprintf("\n      checksum: %x", digest))
		last = match[1]
	}
	parts = append(parts, string(manifest[last:]))
	result := []byte(strings.Join(parts, ""))
	checksums["provider.yaml"] = sha256.Sum256(result)
	if err := os.WriteFile(filepath.Join(dist, "provider.yaml"), result, 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	names := make([]string, 0, len(checksums))
	for name := range checksums {
		names = append(names, name)
	}
	sort.Strings(names)
	lines := make([]string, 0, len(names))
	for _, name := range names {
		lines = append(lines, fmt.Sprintf("%x  %s\n", checksums[name], name))
	}
	if err := os.WriteFile(filepath.Join(dist, "checksums.txt"), []byte(strings.Join(lines, "")), 0o644); err != nil {
		return fmt.Errorf("write checksums: %w", err)
	}
	return nil
}

func hashHelper(path string) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	file, err := os.Open(path)
	if err != nil {
		return digest, err
	}
	hash := sha256.New()
	size, readErr := io.Copy(hash, file)
	closeErr := file.Close()
	if readErr != nil {
		return digest, readErr
	}
	if closeErr != nil {
		return digest, closeErr
	}
	if size == 0 {
		return digest, fmt.Errorf("file is empty")
	}
	_ = hash.Sum(digest[:0])
	return digest, nil
}

func main() {
	if len(os.Args) != 2 {
		_, _ = fmt.Fprintln(os.Stderr, "usage: package-release <tag>")
		os.Exit(2)
	}
	if err := packageRelease("provider.yaml", "dist", os.Args[1]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
