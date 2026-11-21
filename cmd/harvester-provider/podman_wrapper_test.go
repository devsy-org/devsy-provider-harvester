package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func init() {
	if os.Getenv("HARVESTER_TEST_WRAPPER_CHILD") != "1" {
		return
	}
	if os.Getenv("HARVESTER_TEST_WRAPPER_SLEEP") == "1" {
		time.Sleep(2 * time.Second)
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"args": os.Args[1:], "stdin": string(data), "home": os.Getenv("HOME"), "remote": os.Getenv("CONTAINER_HOST"), "xdg": os.Getenv("XDG_RUNTIME_DIR")})
	if os.Getenv("HARVESTER_TEST_WRAPPER_FAIL") == "1" {
		os.Exit(17)
	}
	os.Exit(0)
}

func wrapperFixture(t *testing.T, missingSudo bool) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the guest wrapper is POSIX; native Windows client paths are tested separately")
	}
	dir := t.TempDir()
	id := filepath.Join(dir, "id")
	sudo := filepath.Join(dir, "sudo")
	wrapper := filepath.Join(dir, "runtime")
	for path, body := range map[string]string{
		id:   "#!/bin/sh\nprintf '%s\\n' \"${HARVESTER_TEST_WRAPPER_UID:-0}\"\n",
		sudo: "#!/bin/sh\n[ \"$1\" = -n ] || exit 99\nshift\n[ \"$1\" = -- ] || exit 99\nshift\nHARVESTER_TEST_WRAPPER_UID=0\nexport HARVESTER_TEST_WRAPPER_UID\nexec \"$@\"\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if missingSudo {
		if err := os.Remove(sudo); err != nil {
			t.Fatal(err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := strings.NewReplacer("/usr/bin/id", quoteGuestWord(id), "/usr/bin/sudo", quoteGuestWord(sudo), "/usr/bin/podman", quoteGuestWord(executable), rootfulPodmanCommand, quoteGuestWord(wrapper)).Replace(podmanWrapperScript())
	if strings.Contains(script, "/usr/bin/sudo") || strings.Contains(script, "/usr/bin/podman") {
		t.Fatal("fixture would call a host privilege or engine binary")
	}
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return wrapper
}

func TestPodmanWrapperPreservesArgumentsInputAndRootEnvironment(t *testing.T) {
	for _, uid := range []string{"0", "1000"} {
		t.Run(uid, func(t *testing.T) {
			wrapper := wrapperFixture(t, false)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			args := []string{"exec", "container", "argument with spaces", "literal'quote", "$not-expanded"}
			cmd := exec.CommandContext(ctx, wrapper, args...)
			cmd.Env = append(os.Environ(), "HARVESTER_TEST_WRAPPER_CHILD=1", "HARVESTER_TEST_WRAPPER_UID="+uid, "CONTAINER_HOST=ssh://wrong-engine", "XDG_RUNTIME_DIR=/wrong-user", "GORACE=atexit_sleep_ms=0")
			cmd.Stdin = strings.NewReader("stdin\npayload")
			out, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Args   []string `json:"args"`
				Input  string   `json:"stdin"`
				Home   string   `json:"home"`
				Remote string   `json:"remote"`
				XDG    string   `json:"xdg"`
			}
			if err := json.Unmarshal(out, &result); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(result.Args, append([]string{"--remote=false"}, args...)) || result.Input != "stdin\npayload" || result.Home != "/root" || result.Remote != "" || result.XDG != "" {
				t.Fatalf("wrapper changed arguments, input or engine environment: %+v", result)
			}
		})
	}
}

func TestPodmanWrapperPreservesFailureAndDeadline(t *testing.T) {
	wrapper := wrapperFixture(t, false)
	for _, delayed := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		cmd := exec.CommandContext(ctx, wrapper, "info")
		cmd.Env = append(os.Environ(), "HARVESTER_TEST_WRAPPER_CHILD=1", "HARVESTER_TEST_WRAPPER_FAIL=1", "GORACE=atexit_sleep_ms=0")
		if delayed {
			cmd.Env = append(cmd.Env, "HARVESTER_TEST_WRAPPER_SLEEP=1")
		}
		err := cmd.Run()
		if err == nil || (!delayed && cmd.ProcessState.ExitCode() != 17) || (delayed && ctx.Err() == nil) {
			t.Fatalf("wrapper lost child status or cancellation: %v", err)
		}
		cancel()
	}
}

func TestPodmanWrapperFailsWithoutNoninteractiveSudo(t *testing.T) {
	wrapper := wrapperFixture(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, wrapper, "info")
	cmd.Env = append(os.Environ(), "HARVESTER_TEST_WRAPPER_UID=1000")
	if err := cmd.Run(); err == nil {
		t.Fatal("wrapper succeeded without its required privilege helper")
	}
}

func TestRuntimeCommandDefaultsAndGuestQuoting(t *testing.T) {
	c := config{ContainerRuntime: "podman", CloudInitProfile: "rocky-9-podman-v1"}
	if defaultRuntimeCommand(c) != rootfulPodmanCommand {
		t.Fatal("preset does not select its rootful runtime wrapper")
	}
	c.CloudInitProfile = ""
	if defaultRuntimeCommand(c) != "podman" || quoteGuestWord("path with ' quote") != `'path with '\'' quote'` {
		t.Fatal("custom runtime default or shell argument quoting changed")
	}
}
