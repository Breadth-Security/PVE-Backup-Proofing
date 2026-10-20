package nodesetup

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
)

const testHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"

// testCert returns a fresh self-signed certificate's DER.
func testCert(t *testing.T, cn string) []byte {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func pemOf(typ string, b []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: b}))
}

// fingerprint formats sha256(der) the way `openssl x509 -fingerprint
// -sha256` prints it, written independently of the code under test.
func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

func replyEnv(t *testing.T) Env {
	t.Helper()
	env := testEnv(t, &fakePVE{}, "/bin/sh")
	mustWrite(t, env.path(HostKeyPath), testHostKey+" root@pve1\n", 0o644)
	return env
}

func TestBuildReply(t *testing.T) {
	env := replyEnv(t)
	leaf, issuer := testCert(t, "pve1"), testCert(t, "ca")
	// pveproxy-ssl.pem may hold a chain; the first CERTIFICATE is the leaf.
	mustWrite(t, env.path(PveproxyCertPath), pemOf("EC PARAMETERS", []byte{6, 1, 0})+pemOf("CERTIFICATE", leaf)+pemOf("CERTIFICATE", issuer), 0o640)
	mustWrite(t, env.path(PveCertPath), pemOf("CERTIFICATE", issuer), 0o640)

	a := testArgs()
	a.PubKey = "  " + testKey + "  \n"
	r, err := BuildReply(env, a, "pve1", "0.3.0", testSecret)
	if err != nil {
		t.Fatalf("BuildReply: %v", err)
	}
	want := nodeproto.SetupReply{
		Format: 1, Node: "pve1", HelperVersion: "0.3.0",
		VMIDRange: a.VMIDRange, Bridge: a.Bridge, PubKey: testKey,
		HostKey: testHostKey, APIFingerprint: fingerprint(leaf),
		TokenID: TokenID, TokenSecret: testSecret,
	}
	if r != want {
		t.Errorf("reply\n got %+v\nwant %+v", r, want)
	}
	if len(r.APIFingerprint) != 95 {
		t.Errorf("fingerprint %q is not 32 colon-separated bytes", r.APIFingerprint)
	}

	// Without pveproxy-ssl.pem, pve-ssl.pem; no secret is allowed.
	if err := os.Remove(env.path(PveproxyCertPath)); err != nil {
		t.Fatal(err)
	}
	r, err = BuildReply(env, a, "pve1", "0.3.0", "")
	if err != nil {
		t.Fatalf("BuildReply: %v", err)
	}
	if r.APIFingerprint != fingerprint(issuer) || r.TokenSecret != "" {
		t.Errorf("fallback reply fingerprint %q secret %q, want %q and none", r.APIFingerprint, r.TokenSecret, fingerprint(issuer))
	}
}

func TestBuildReplyErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(env Env)
		want  string
	}{
		"no cert": {func(Env) {}, PveCertPath},
		"no certificate block": {func(env Env) {
			mustWrite(t, env.path(PveCertPath), pemOf("PRIVATE KEY", []byte{1}), 0o640)
		}, "no CERTIFICATE"},
		"no host key": {func(env Env) {
			mustWrite(t, env.path(PveCertPath), pemOf("CERTIFICATE", testCert(t, "x")), 0o640)
			_ = os.Remove(env.path(HostKeyPath))
		}, HostKeyPath},
		"bad host key": {func(env Env) {
			mustWrite(t, env.path(PveCertPath), pemOf("CERTIFICATE", testCert(t, "x")), 0o640)
			mustWrite(t, env.path(HostKeyPath), "ssh-rsa AAAA garbage\n", 0o644)
		}, HostKeyPath},
	} {
		t.Run(name, func(t *testing.T) {
			env := replyEnv(t)
			tc.setup(env)
			_, err := BuildReply(env, testArgs(), "pve1", "0.3.0", testSecret)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want one naming %q", err, tc.want)
			}
			if strings.Contains(err.Error(), testSecret) {
				t.Errorf("error carries the secret: %v", err)
			}
		})
	}
}
