package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func init() {
	if os.Getenv("HARVESTER_TEST_TEMPLATE_CHILD") != "1" {
		return
	}
	expected := []string{"--kubeconfig", os.Getenv("HARVESTER_TEST_KUBECONFIG"), "--context", "harvester-test", "get", "configmap", "ubuntu-docker", "-n", "templates", "-o", "json"}
	if !reflect.DeepEqual(os.Args[1:], expected) {
		os.Exit(22)
	}
	if os.Getenv("HARVESTER_TEST_TEMPLATE_ERROR") == "1" {
		os.Exit(1)
	}
	data := map[string]string{"cloudInit": "#cloud-config\npackages: [docker.io]\n"}
	if os.Getenv("HARVESTER_TEST_TEMPLATE_EMPTY") == "1" {
		data = map[string]string{}
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"data": data}); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestCloudInitTemplateUsesConfiguredKubeconfigAndContext(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARVESTER_KUBECTL_PATH", exe)
	t.Setenv("HARVESTER_TEST_TEMPLATE_CHILD", "1")
	path := filepath.Join(t.TempDir(), "kube config.yaml")
	t.Setenv("HARVESTER_TEST_KUBECONFIG", path)
	c := config{Kubeconfig: path, Context: "harvester-test", Namespace: "workspace", CloudInitTemplate: "templates/ubuntu-docker"}
	got, err := resolveUserData(c)
	if err != nil {
		t.Fatal(err)
	}
	if got != "#cloud-config\npackages: [docker.io]\n" {
		t.Fatal("cloud-init did not roundtrip exactly")
	}
	t.Setenv("HARVESTER_TEST_TEMPLATE_EMPTY", "1")
	if _, err := resolveUserData(c); err == nil {
		t.Fatal("missing cloudInit must fail")
	}
	t.Setenv("HARVESTER_TEST_TEMPLATE_ERROR", "1")
	if _, err := resolveUserData(c); err == nil {
		t.Fatal("API error must fail")
	}
}

func TestCloudInitFileAndConflicts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user data.yaml")
	want := "#cloud-config\r\npackages: [docker.io]\r\n"
	if err := os.WriteFile(path, []byte(want), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveUserData(config{UserDataFile: path})
	if err != nil || got != want {
		t.Fatalf("file roundtrip: %v", err)
	}
	for _, c := range []config{
		{UserDataFile: path, UserData: "inline"},
		{CloudInitTemplate: "templates/ubuntu-docker", UserDataFile: path},
		{CloudInitTemplate: "-invalid"},
		{CloudInitTemplate: "a/b/c"},
		{CloudInitTemplate: "/ubuntu-docker"},
		{UserDataFile: path + ".missing"},
	} {
		if _, err := resolveUserData(c); err == nil {
			t.Fatalf("invalid source accepted: %#v", c)
		}
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveUserData(config{UserDataFile: path}); err == nil {
		t.Fatal("empty file accepted")
	}
}
