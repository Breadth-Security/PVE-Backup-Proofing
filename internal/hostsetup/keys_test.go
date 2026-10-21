package hostsetup

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestEnsureKeyKeeps(t *testing.T) {
	_, owners := fakeOwners(t)
	l := Layout{Dir: t.TempDir()}
	pub, created, err := EnsureKey(l)
	if err != nil || !created {
		t.Fatalf("first EnsureKey: %v created=%v", err, created)
	}
	if !strings.HasPrefix(pub, "ssh-ed25519 AAAA") || !strings.HasSuffix(pub, " pbp") {
		t.Fatalf("pub = %q", pub)
	}
	priv, err := os.ReadFile(l.KeyPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(priv, []byte("-----BEGIN OPENSSH PRIVATE KEY-----")) {
		t.Fatalf("private key not in OpenSSH format: %.40s", priv)
	}
	if _, err := ssh.ParsePrivateKey(priv); err != nil {
		t.Fatal(err)
	}
	if file, _ := os.ReadFile(l.PubKeyPath()); string(file) != pub+"\n" {
		t.Fatalf(".pub = %q", file)
	}
	if m := mode(t, l.KeyPath()); m != 0o400 {
		t.Errorf("key mode %o", m)
	}
	if o := owners()[l.KeyPath()]; o != (owned{65532, 65532}) {
		t.Errorf("key owner %v", o)
	}

	pub2, created, err := EnsureKey(l)
	if err != nil || created || pub2 != pub {
		t.Fatalf("second EnsureKey: %v created=%v pub=%q", err, created, pub2)
	}
	if err := os.Remove(l.PubKeyPath()); err != nil {
		t.Fatal(err)
	}
	pub3, created, err := EnsureKey(l)
	if err != nil || created || pub3 != pub {
		t.Fatalf("EnsureKey without .pub: %v created=%v pub=%q", err, created, pub3)
	}
	if file, _ := os.ReadFile(l.PubKeyPath()); string(file) != pub+"\n" {
		t.Fatalf("re-derived .pub = %q", file)
	}
	if again, _ := os.ReadFile(l.KeyPath()); !bytes.Equal(again, priv) {
		t.Fatal("private key replaced")
	}
}

func TestEnsureKeyMismatchedPub(t *testing.T) {
	fakeOwners(t)
	a, b := Layout{Dir: t.TempDir()}, Layout{Dir: t.TempDir()}
	if _, _, err := EnsureKey(a); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnsureKey(b); err != nil {
		t.Fatal(err)
	}
	other, _ := os.ReadFile(b.PubKeyPath())
	if err := os.Chmod(a.PubKeyPath(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.PubKeyPath(), other, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnsureKey(a); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v", err)
	}
}

// The deb layout's private key: 0640 root:pbp, like its token, so the
// daemon (group pbp) reads it and nobody else does (code review 7.11).
func TestEnsureKeyDebMode(t *testing.T) {
	_, owners := fakeOwners(t)
	l := Layout{Deb: true}
	if _, _, err := EnsureKey(l); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, l.KeyPath()); m != 0o640 {
		t.Errorf("deb key mode %o, want 640", m)
	}
	if o := owners()[l.KeyPath()]; o != (owned{0, 4242}) {
		t.Errorf("deb key owner %v, want root:pbp", o)
	}
}
