package hostsetup

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/nodeproto"
)

// testHostKey is a fixed ed25519 host key as the node's reply carries it.
func testHostKey(t *testing.T, seed byte) string {
	t.Helper()
	pk := ed25519.NewKeyFromSeed(make([]byte, 32)).Public().(ed25519.PublicKey)
	if seed != 0 {
		s := make([]byte, 32)
		s[0] = seed
		pk = ed25519.NewKeyFromSeed(s).Public().(ed25519.PublicKey)
	}
	k, err := ssh.NewPublicKey(pk)
	if err != nil {
		t.Fatal(err)
	}
	return k.Type() + " " + base64.StdEncoding.EncodeToString(k.Marshal())
}

// pendingHost writes a fresh Docker layout: config (pending) and key.
func pendingHost(t *testing.T) (Layout, config.Config, string) {
	t.Helper()
	l := Layout{Dir: t.TempDir()}
	if err := WriteConfig(l, testAnswers()); err != nil {
		t.Fatal(err)
	}
	pub, _, err := EnsureKey(l)
	if err != nil {
		t.Fatal(err)
	}
	cfg, pending, err := LoadState(l)
	if err != nil || !pending {
		t.Fatalf("LoadState: pending=%v err=%v", pending, err)
	}
	return l, cfg, pub
}

func goodReply(t *testing.T, pub, secret string) nodeproto.SetupReply {
	return nodeproto.SetupReply{Format: 1, Node: "pve1", HelperVersion: "0.3.0",
		VMIDRange: [2]int{990000, 990009}, Bridge: "vmbr77", PubKey: pub,
		HostKey: testHostKey(t, 0), APIFingerprint: fp, TokenID: TokenID, TokenSecret: secret}
}

func parseFile(t *testing.T, p string) config.Config {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Parse(b)
	if err != nil {
		t.Fatalf("parse %s: %v", p, err)
	}
	return c
}

func TestFinishWritesFiles(t *testing.T) {
	_, owners := fakeOwners(t)
	l, cfg, pub := pendingHost(t)
	before, err := os.ReadFile(l.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	r := goodReply(t, pub, "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0")
	if lines := Changes(cfg, r); len(lines) != 0 {
		t.Errorf("Changes on a pending config = %q, want none", lines)
	}
	need, err := Finish(l, cfg, r)
	if err != nil || need {
		t.Fatalf("Finish: need=%v err=%v", need, err)
	}

	kh, err := os.ReadFile(l.KnownHostsPath())
	if err != nil || string(kh) != "pve1.example.lan "+testHostKey(t, 0)+"\n" {
		t.Errorf("known_hosts = %q, %v", kh, err)
	}
	tok, err := os.ReadFile(l.TokenPath())
	if err != nil || string(tok) != "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0\n" {
		t.Errorf("token = %q, %v", tok, err)
	}
	for p, m := range map[string]os.FileMode{l.KnownHostsPath(): 0o444, l.TokenPath(): 0o400, l.ConfigPath(): 0o444} {
		if got := mode(t, p); got != m {
			t.Errorf("mode of %s = %o, want %o", p, got, m)
		}
		if o := owners()[p]; o != (owned{65532, 65532}) {
			t.Errorf("owner of %s = %v", p, o)
		}
	}

	// Exactly the two values changed: compare with the old file plus them.
	got := parseFile(t, l.ConfigPath())
	old := string(before)
	old = strings.Replace(old, `node: ""`, "node: pve1", 1)
	old = strings.Replace(old, `tls_fingerprint: ""`, `tls_fingerprint: "`+fp+`"`, 1)
	want, err := config.Parse([]byte(old))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("config after Finish:\n got %+v\nwant %+v", got, want)
	}
	if _, pending, err := LoadState(l); err != nil || pending {
		t.Errorf("after Finish: pending=%v err=%v", pending, err)
	}
	after, _ := os.ReadFile(l.ConfigPath())
	if !strings.Contains(string(after), "# cron, in the container's TZ") {
		t.Errorf("comments lost:\n%s", after)
	}

	// A second reply (re-pair): new node name, fingerprint and host key,
	// with a fresh secret, replaces all of them; Changes names old -> new.
	cfg2 := parseFile(t, l.ConfigPath())
	fp2 := strings.Replace(fp, "11", "AB", 1)
	r2 := goodReply(t, pub, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	r2.Node, r2.APIFingerprint, r2.HostKey = "pve2", fp2, testHostKey(t, 9)
	ch := Changes(cfg2, r2)
	if len(ch) != 2 || ch[0] != "target.node: pve1 → pve2" || ch[1] != "target.api.tls_fingerprint: "+fp+" → "+fp2 {
		t.Errorf("Changes = %q", ch)
	}
	if need, err := Finish(l, cfg2, r2); err != nil || need {
		t.Fatalf("second Finish: need=%v err=%v", need, err)
	}
	c3 := parseFile(t, l.ConfigPath())
	if c3.Target.Node != "pve2" || c3.Target.API.TLSFingerprint != fp2 {
		t.Errorf("after re-pair: %+v", c3.Target)
	}
	if tok, _ := os.ReadFile(l.TokenPath()); string(tok) != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee\n" {
		t.Errorf("existing token file not replaced by the new secret: %q", tok)
	}
	if kh, _ := os.ReadFile(l.KnownHostsPath()); string(kh) != "pve1.example.lan "+testHostKey(t, 9)+"\n" {
		t.Errorf("known_hosts not replaced wholesale: %q", kh)
	}

	// No secret, token file present: nothing to rotate, token kept.
	r3 := goodReply(t, pub, "")
	if need, err := Finish(l, parseFile(t, l.ConfigPath()), r3); err != nil || need {
		t.Fatalf("Finish without secret: need=%v err=%v", need, err)
	}
	if tok, _ := os.ReadFile(l.TokenPath()); string(tok) != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee\n" {
		t.Errorf("token changed without a secret: %q", tok)
	}
}

func TestFinishDebAndPort(t *testing.T) {
	_, owners := fakeOwners(t)
	l := Layout{Deb: true}
	a := testAnswers()
	a.TZ = ""
	if err := WriteConfig(l, a); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(l.ConfigPath())
	_ = os.Chmod(l.ConfigPath(), 0o600)
	if err := os.WriteFile(l.ConfigPath(), []byte(strings.Replace(string(raw), "port: 22", "port: 2222", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	pub, _, err := EnsureKey(l)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := LoadState(l)
	if err != nil {
		t.Fatal(err)
	}
	if need, err := Finish(l, cfg, goodReply(t, pub, "s3cret")); err != nil || need {
		t.Fatalf("Finish: need=%v err=%v", need, err)
	}
	kh, _ := os.ReadFile(l.KnownHostsPath())
	if string(kh) != "[pve1.example.lan]:2222 "+testHostKey(t, 0)+"\n" {
		t.Errorf("known_hosts = %q", kh)
	}
	for p, m := range map[string]os.FileMode{l.KnownHostsPath(): 0o640, l.TokenPath(): 0o640, l.ConfigPath(): 0o640} {
		if got := mode(t, p); got != m {
			t.Errorf("mode of %s = %o, want %o", p, got, m)
		}
		if o := owners()[p]; o != (owned{0, 4242}) {
			t.Errorf("owner of %s = %v", p, o)
		}
	}
}

// With target.api.ca_file set, TLS is left alone: no fingerprint is added.
func TestFinishCAFileKeepsTLS(t *testing.T) {
	fakeOwners(t)
	l := Layout{Dir: t.TempDir()}
	cfgText := `schedule: "0 2 * * *"
target:
  node: old-name
  api:
    url: https://pve1:8006
    token_id: pbp@pve!proof
    token_secret_file: /run/secrets/pve_token
    ca_file: /etc/pbp/ca.pem   # our CA
  ssh: {host: pve1, key_file: /run/secrets/pve_ssh_key, known_hosts_file: /etc/pbp/known_hosts}
  bridge: vmbr77
  vmid_range: [990000, 990009]
sources: {storages: [pbs-main]}
restore: {storage: local-zfs}
`
	if err := os.WriteFile(l.ConfigPath(), []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := l.WriteSecretFile(l.TokenPath(), []byte("old\n")); err != nil {
		t.Fatal(err)
	}
	pub, _, err := EnsureKey(l)
	if err != nil {
		t.Fatal(err)
	}
	cfg, pending, err := LoadState(l)
	if err != nil || pending {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	r := goodReply(t, pub, "")
	if ch := Changes(cfg, r); len(ch) != 1 || ch[0] != "target.node: old-name → pve1" {
		t.Errorf("Changes = %q", ch)
	}
	if need, err := Finish(l, cfg, r); err != nil || need {
		t.Fatalf("Finish: need=%v err=%v", need, err)
	}
	got := parseFile(t, l.ConfigPath())
	if got.Target.API.TLSFingerprint != "" || got.Target.API.CAFile != "/etc/pbp/ca.pem" || got.Target.Node != "pve1" {
		t.Errorf("target.api = %+v node %q", got.Target.API, got.Target.Node)
	}
	want, _ := config.Parse([]byte(strings.Replace(cfgText, "node: old-name", "node: pve1", 1)))
	if !reflect.DeepEqual(got, want) {
		t.Errorf("config changed beyond target.node:\n got %+v\nwant %+v", got, want)
	}
}

// snapshot records every file under dir with its content.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[p] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFinishRefusesMismatch(t *testing.T) {
	fakeOwners(t)
	l, cfg, pub := pendingHost(t)
	_, otherPub, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	sp, _ := ssh.NewPublicKey(otherPub.Public())
	other := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp)))
	// A real, parseable host key of another type, so the type check (not
	// the parser) is what refuses it.
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ssh.NewPublicKey(&ec.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	ecdsaHostKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(ecKey)))
	cases := map[string]func(*nodeproto.SetupReply){
		"range":            func(r *nodeproto.SetupReply) { r.VMIDRange = [2]int{990000, 990049} },
		"bridge":           func(r *nodeproto.SetupReply) { r.Bridge = "vmbr99" },
		"pubkey":           func(r *nodeproto.SetupReply) { r.PubKey = other + " pbp" },
		"empty host key":   func(r *nodeproto.SetupReply) { r.HostKey = "" },
		"garbled host key": func(r *nodeproto.SetupReply) { r.HostKey = "ssh-rsa AAAAB3NzaC1yc2E=" },
		"ecdsa host key":   func(r *nodeproto.SetupReply) { r.HostKey = ecdsaHostKey },
		"bad fingerprint":  func(r *nodeproto.SetupReply) { r.APIFingerprint = strings.ToLower(fp) },
		"short fp":         func(r *nodeproto.SetupReply) { r.APIFingerprint = "AB:CD" },
		"token id":         func(r *nodeproto.SetupReply) { r.TokenID = "root@pam!x" },
		"empty node":       func(r *nodeproto.SetupReply) { r.Node = "" },
		"node with spaces": func(r *nodeproto.SetupReply) { r.Node = "pve 1" },
		"secret newline":   func(r *nodeproto.SetupReply) { r.TokenSecret = "a\nb" },
	}
	before := snapshot(t, l.Dir)
	for name, mut := range cases {
		r := goodReply(t, pub, "s3cret")
		mut(&r)
		need, err := Finish(l, cfg, r)
		if err == nil || need {
			t.Errorf("%s: need=%v err=%v, want refusal", name, need, err)
		}
		if name == "ecdsa host key" && (err == nil || !strings.Contains(err.Error(), "want ssh-ed25519")) {
			t.Errorf("%s: err %v, want the type check's refusal", name, err)
		}
		if err != nil && strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%s: error leaks the secret: %v", name, err)
		}
		if after := snapshot(t, l.Dir); !reflect.DeepEqual(after, before) {
			t.Errorf("%s: files changed on refusal", name)
		}
	}
}

func TestFinishNeedRotate(t *testing.T) {
	fakeOwners(t)
	l, cfg, pub := pendingHost(t)
	before := snapshot(t, l.Dir)
	need, err := Finish(l, cfg, goodReply(t, pub, ""))
	if err != nil || !need {
		t.Fatalf("need=%v err=%v, want needRotate", need, err)
	}
	if after := snapshot(t, l.Dir); !reflect.DeepEqual(after, before) {
		t.Error("files changed when a rotation is needed")
	}
	if _, err := os.Stat(l.TokenPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("token file: %v", err)
	}
}
