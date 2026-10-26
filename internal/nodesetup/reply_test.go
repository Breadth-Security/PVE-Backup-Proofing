package nodesetup

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/nodeproto"
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
	mustWrite(t, env.path(PveproxyKeyPath), "key\n", 0o600)

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

	// pveproxy serves pveproxy-ssl.pem only with its key; a stray .pem, or
	// no .pem, means pve-ssl.pem. No secret is allowed.
	for _, rm := range []string{PveproxyKeyPath, PveproxyCertPath} {
		if err := os.Remove(env.path(rm)); err != nil {
			t.Fatal(err)
		}
		r, err = BuildReply(env, a, "pve1", "0.3.0", "")
		if err != nil {
			t.Fatalf("BuildReply without %s: %v", rm, err)
		}
		if r.APIFingerprint != fingerprint(issuer) || r.TokenSecret != "" {
			t.Errorf("without %s: fingerprint %q secret %q, want %q and none", rm, r.APIFingerprint, r.TokenSecret, fingerprint(issuer))
		}
	}
}

// TestBuildReplyOpensslFixture checks the fingerprint against the value
// `openssl x509 -noout -fingerprint -sha256 -in testdata/api-cert.crt`
// printed (OpenSSL 3.6.5) for a fixed certificate.
func TestBuildReplyOpensslFixture(t *testing.T) {
	const want = "46:C1:0B:4F:57:92:57:99:F6:BF:9E:58:81:3E:52:A0:FF:7D:D0:4E:A0:BE:57:1F:F2:4D:32:33:68:B0:48:98"
	cert, err := os.ReadFile("testdata/api-cert.crt")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{PveproxyCertPath, PveCertPath} {
		env := replyEnv(t)
		mustWrite(t, env.path(path), string(cert), 0o640)
		if path == PveproxyCertPath {
			mustWrite(t, env.path(PveproxyKeyPath), "key\n", 0o600)
			mustWrite(t, env.path(PveCertPath), pemOf("CERTIFICATE", testCert(t, "other")), 0o640)
		}
		r, err := BuildReply(env, testArgs(), "pve1", "0.3.0", "")
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if r.APIFingerprint != want {
			t.Errorf("%s: fingerprint %s, want %s", path, r.APIFingerprint, want)
		}
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

// The reply lists the node's addresses when Env can, and a failure to list
// them does not stop the reply.
func TestBuildReplyAddresses(t *testing.T) {
	env := replyEnv(t)
	mustWrite(t, env.path(PveCertPath), pemOf("CERTIFICATE", testCert(t, "pve1")), 0o640)
	env.Addrs = func() ([]string, error) { return []string{"10.0.0.1", "fd00::1"}, nil }
	r, err := BuildReply(env, testArgs(), "pve1", "0.3.0", "")
	if err != nil || r.Addresses != "10.0.0.1 fd00::1" {
		t.Fatalf("addresses %q err %v", r.Addresses, err)
	}
	env.Addrs = func() ([]string, error) { return nil, errors.New("no netlink") }
	if r, err := BuildReply(env, testArgs(), "pve1", "0.3.0", ""); err != nil || r.Addresses != "" {
		t.Fatalf("addresses %q err %v", r.Addresses, err)
	}
}
