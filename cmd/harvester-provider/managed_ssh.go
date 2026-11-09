package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

type managedIdentity struct{ KeyPath, PublicKey, Fingerprint string }

var managedSlotPattern = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

func splitSSHFlags(input string) ([]string, error) {
	var result []string
	var word strings.Builder
	var quote rune
	started := false
	for _, r := range input {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, started = r, true
		case unicode.IsSpace(r):
			if started {
				result = append(result, word.String())
				word.Reset()
				started = false
			}
		default:
			word.WriteRune(r)
			started = true
		}
	}
	if quote != 0 {
		return nil, errors.New("HARVESTER_SSH_FLAGS contains an unclosed quote")
	}
	if started {
		result = append(result, word.String())
	}
	return result, nil
}

func managedIdentityAt(root, slot string, create bool) (managedIdentity, error) {
	if !managedSlotPattern.MatchString(slot) {
		return managedIdentity{}, errors.New("invalid managed SSH state slot")
	}
	if err := secureManagedDirectory(root); err != nil {
		return managedIdentity{}, err
	}
	dir := filepath.Join(root, slot)
	if !create {
		if _, err := os.Lstat(dir); err != nil {
			return managedIdentity{}, errors.New("managed SSH identity is missing; recover the original client key before reconnecting")
		}
	}
	if err := secureManagedDirectory(dir); err != nil {
		return managedIdentity{}, err
	}
	path := filepath.Join(dir, "id_rsa")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		if !create {
			return managedIdentity{}, errors.New("managed SSH identity is missing; recover the original client key before reconnecting")
		}
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return managedIdentity{}, fmt.Errorf("generate managed SSH identity: %w", err)
		}
		file, err := os.CreateTemp(dir, ".identity-")
		if err != nil {
			return managedIdentity{}, err
		}
		defer func() { _ = os.Remove(file.Name()) }()
		if err := pem.Encode(file, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}); err != nil {
			_ = file.Close()
			return managedIdentity{}, err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return managedIdentity{}, err
		}
		if err := file.Close(); err != nil {
			return managedIdentity{}, err
		}
		if err := os.Rename(file.Name(), path); err != nil {
			return managedIdentity{}, err
		}
	} else if err != nil {
		return managedIdentity{}, err
	}
	if err := checkManagedFile(path); err != nil {
		return managedIdentity{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return managedIdentity{}, errors.New("cannot read managed SSH private key")
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "RSA PRIVATE KEY" || len(rest) != 0 {
		return managedIdentity{}, errors.New("managed SSH identity is corrupt; refusing to replace it")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil || key.N.BitLen() < 2048 {
		return managedIdentity{}, errors.New("managed SSH identity is invalid; refusing to replace it")
	}
	wire := appendSSHString(nil, []byte("ssh-rsa"))
	for _, n := range []*big.Int{big.NewInt(int64(key.E)), key.N} {
		b := n.Bytes()
		if len(b) > 0 && b[0]&0x80 != 0 {
			b = append([]byte{0}, b...)
		}
		wire = appendSSHString(wire, b)
	}
	fingerprint := sha256.Sum256(wire)
	return managedIdentity{path, "ssh-rsa " + base64.StdEncoding.EncodeToString(wire), "SHA256:" + base64.RawStdEncoding.EncodeToString(fingerprint[:])}, nil
}

func appendSSHString(dst, value []byte) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(value)))
	return append(dst, value...)
}

func checkManagedFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("managed SSH state must be a regular file, not a symlink")
	}
	return checkManagedPermissions(path, info)
}

func lockManagedSlot(ctx context.Context, root, slot string) (func(), error) {
	if !managedSlotPattern.MatchString(slot) {
		return nil, errors.New("invalid managed SSH lock slot")
	}
	if err := secureManagedDirectory(root); err != nil {
		return nil, err
	}
	path := filepath.Join(root, slot+".lock")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("managed SSH lock must be a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := checkManagedFile(path); err != nil {
		_ = file.Close()
		return nil, err
	}
	for {
		locked, err := tryManagedLock(file)
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		if locked {
			return func() { releaseManagedLock(file); _ = file.Close() }, nil
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, fmt.Errorf("managed SSH machine is busy: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}
