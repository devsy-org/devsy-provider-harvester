package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSSHFlagQuoting(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  []string
	}{
		{`-i "C:\Users\jyorko\.ssh\id_ed25519_vision" -o IdentitiesOnly=yes`, []string{"-i", `C:\Users\jyorko\.ssh\id_ed25519_vision`, "-o", "IdentitiesOnly=yes"}},
		{`-i "C:\Users\Josh Yorko\.ssh\key"`, []string{"-i", `C:\Users\Josh Yorko\.ssh\key`}},
		{`-o 'ProxyCommand=ssh jump -W %h:%p'`, []string{"-o", "ProxyCommand=ssh jump -W %h:%p"}},
		{`-i C:\Users\jyorko\.ssh\key`, []string{"-i", `C:\Users\jyorko\.ssh\key`}},
	} {
		got, err := splitSSHFlags(tc.input)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("flag grouping changed argv: %q, %v", got, err)
		}
	}
	if _, err := splitSSHFlags(`-i "unfinished`); err == nil {
		t.Fatal("unbalanced quotes accepted")
	}
}

func TestManagedIdentityPersistsAndIsOpenSSHReadable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "keys")
	a, err := managedIdentityAt(root, "slot", true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := managedIdentityAt(root, "slot", false)
	if err != nil || a != b || !validPublicKey(a.PublicKey) {
		t.Fatalf("identity was not reusable: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(a.KeyPath)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("private key must be owner-only")
		}
	}
	if path, err := exec.LookPath("ssh-keygen"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, path, "-y", "-f", a.KeyPath).Output()
		fields := strings.Fields(string(out))
		if err != nil || len(fields) < 2 || strings.Join(fields[:2], " ") != a.PublicKey {
			t.Fatalf("OpenSSH cannot read generated identity: %v", err)
		}
	} else if os.Getenv("HARVESTER_REQUIRE_OPENSSH_TEST") == "1" {
		t.Fatal("OpenSSH ssh-keygen is required for native key-readability qualification")
	}
}

func TestManagedIdentityNeverReplacesMissingOrCorruptState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "keys")
	if _, err := managedIdentityAt(root, "slot", false); err == nil {
		t.Fatal("missing existing identity was generated")
	}
	a, err := managedIdentityAt(root, "slot", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.KeyPath, []byte("invalid private state"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := managedIdentityAt(root, "slot", true); err == nil {
		t.Fatal("corrupt existing identity was replaced")
	}
	data, err := os.ReadFile(a.KeyPath)
	if err != nil || string(data) != "invalid private state" {
		t.Fatal("corrupt state was overwritten")
	}
}

func TestManagedIdentityRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows symlinks require privileges; ACL behavior has separate platform tests")
	}
	root := filepath.Join(t.TempDir(), "keys")
	a, err := managedIdentityAt(root, "slot", true)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(a.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "other-key")
	if err := os.WriteFile(target, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(a.KeyPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, a.KeyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := managedIdentityAt(root, "slot", false); err == nil {
		t.Fatal("symlinked credential accepted")
	}
}

func TestManagedSlotLockSerializesIdentityCreation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "keys")
	var wg sync.WaitGroup
	fingerprints := make(chan string, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			unlock, err := lockManagedSlot(ctx, root, "slot")
			if err != nil {
				t.Error(err)
				return
			}
			defer unlock()
			identity, err := managedIdentityAt(root, "slot", true)
			if err != nil {
				t.Error(err)
				return
			}
			fingerprints <- identity.Fingerprint
		}()
	}
	wg.Wait()
	close(fingerprints)
	first := ""
	for value := range fingerprints {
		if first == "" {
			first = value
		}
		if value != first {
			t.Fatal("concurrent calls created different credentials")
		}
	}
}
