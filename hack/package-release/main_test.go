package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) (string, string, string) {
	t.Helper()
	manifest, err := os.ReadFile("../../provider.yaml")
	if err != nil {
		t.Fatal(err)
	}
	version := versionPattern.FindStringSubmatch(string(manifest))
	if len(version) != 2 {
		t.Fatal("missing provider version")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "provider.yaml")
	if err := os.WriteFile(source, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range helpers {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture "+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return source, dir, version[1]
}

func TestPackageRelease(t *testing.T) {
	source, dist, tag := fixture(t)
	if err := packageRelease(source, dist, tag); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(dist, "provider.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	sums, err := os.ReadFile(filepath.Join(dist, "checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range append(append([]string{}, helpers...), "provider.yaml") {
		data, err := os.ReadFile(filepath.Join(dist, name))
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data)
		if !strings.Contains(string(sums), fmt.Sprintf("%x  %s\n", digest, name)) {
			t.Errorf("missing checksum for %s", name)
		}
		if name != "provider.yaml" && !strings.Contains(string(manifest), fmt.Sprintf("/%s\n      checksum: %x", name, digest)) {
			t.Errorf("missing manifest checksum for %s", name)
		}
	}
	if lines := strings.Count(string(sums), "\n"); lines != 6 {
		t.Errorf("got %d checksum lines, want 6", lines)
	}
}

func TestPackageReleaseRejectsInvalidInput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, source, dist string)
		tag    string
	}{
		{"wrong tag", func(*testing.T, string, string) {}, "v9.9.9"},
		{"missing helper", func(t *testing.T, _, dist string) {
			if err := os.Remove(filepath.Join(dist, helpers[0])); err != nil {
				t.Fatal(err)
			}
		}, ""},
		{"empty helper", func(t *testing.T, _, dist string) {
			if err := os.WriteFile(filepath.Join(dist, helpers[0]), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, ""},
		{"wrong repository", func(t *testing.T, source, _ string) {
			data, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			data = []byte(strings.Replace(string(data), "github.com/devsy-org/devsy-provider-harvester/releases", "github.com/example/provider/releases", 1))
			if err := os.WriteFile(source, data, 0o600); err != nil {
				t.Fatal(err)
			}
		}, ""},
		{"extra platform", func(t *testing.T, source, _ string) {
			data, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			data = append(data, []byte("    - os: linux\n      arch: 386\n      path: invalid\n")...)
			if err := os.WriteFile(source, data, 0o600); err != nil {
				t.Fatal(err)
			}
		}, ""},
		{"wrong platform", func(t *testing.T, source, _ string) {
			data, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			data = []byte(strings.Replace(string(data), "arch: amd64\n      path: https://github.com/devsy-org/devsy-provider-harvester", "arch: arm64\n      path: https://github.com/devsy-org/devsy-provider-harvester", 1))
			if err := os.WriteFile(source, data, 0o600); err != nil {
				t.Fatal(err)
			}
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, dist, tag := fixture(t)
			tc.change(t, source, dist)
			if tc.tag != "" {
				tag = tc.tag
			}
			if err := packageRelease(source, dist, tag); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
