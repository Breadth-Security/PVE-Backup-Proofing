package aptrepo

import (
	"crypto/sha1" //nolint:gosec // OpenPGP v4 fingerprints are SHA-1 by definition (RFC 4880 12.2)
	"encoding/base64"
	"encoding/hex"
	"html"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The landing page served at the root of the APT repository's domain
// (packaging/apt/index.html) repeats the key's fingerprint and the setup
// commands; these tests keep it from drifting from the committed key and
// from docs/install-apt.md.

const (
	landingPage = "../../packaging/apt/index.html"
	keyringASC  = "../../packaging/apt/pbp-archive-keyring.asc"
	installDoc  = "../../docs/install-apt.md"
)

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// v4Fingerprint returns the fingerprint of the first packet of an armored
// OpenPGP key, which must be a version 4 public key: SHA-1 over 0x99, the
// two-byte body length and the body.
func v4Fingerprint(t *testing.T, armored string) string {
	t.Helper()
	var b64 strings.Builder
	inBody := false
	for _, ln := range strings.Split(armored, "\n") {
		ln = strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(ln, "-----BEGIN PGP PUBLIC KEY BLOCK-----"):
			inBody = false
			continue
		case strings.HasPrefix(ln, "-----END"), strings.HasPrefix(ln, "="):
			inBody = false
			continue
		case ln == "":
			inBody = true // the blank line after the armor headers
			continue
		}
		if inBody {
			b64.WriteString(ln)
		}
	}
	data, err := base64.StdEncoding.DecodeString(b64.String())
	if err != nil || len(data) < 3 {
		t.Fatalf("armored key: %v", err)
	}
	var tag, n, hdr int
	if data[0]&0x40 != 0 { // new packet format
		tag = int(data[0] & 0x3f)
		switch l := int(data[1]); {
		case l < 192:
			n, hdr = l, 2
		case l < 224:
			n, hdr = (l-192)<<8+int(data[2])+192, 3
		default:
			t.Fatalf("unsupported packet length octet %d", l)
		}
	} else { // old packet format
		tag = int(data[0]>>2) & 0x0f
		switch data[0] & 3 {
		case 0:
			n, hdr = int(data[1]), 2
		case 1:
			n, hdr = int(data[1])<<8|int(data[2]), 3
		default:
			t.Fatalf("unsupported old-format length type %d", data[0]&3)
		}
	}
	if tag != 6 || len(data) < hdr+n || data[hdr] != 4 {
		t.Fatalf("first packet is not a v4 public key (tag %d)", tag)
	}
	h := sha1.New() //nolint:gosec // see the import
	h.Write([]byte{0x99, byte(n >> 8), byte(n)})
	h.Write(data[hdr : hdr+n])
	return strings.ToUpper(hex.EncodeToString(h.Sum(nil)))
}

func TestLandingPageFingerprint(t *testing.T) {
	want := v4Fingerprint(t, readFile(t, keyringASC))
	m := regexp.MustCompile(`id="fingerprint"[^>]*>([^<]*)<`).FindStringSubmatch(readFile(t, landingPage))
	if m == nil {
		t.Fatal(`the landing page has no element with id="fingerprint"`)
	}
	if got := strings.Join(strings.Fields(m[1]), ""); got != want {
		t.Errorf("landing page fingerprint %s, committed key %s", got, want)
	}
}

func TestLandingPageSetupMatchesDocs(t *testing.T) {
	doc := readFile(t, installDoc)
	start := strings.Index(doc, "curl -fsSLo /usr/share/keyrings/")
	end := strings.Index(doc, "apt update && apt install pbp\n")
	if start < 0 || end < start {
		t.Fatal("docs/install-apt.md: setup commands not found")
	}
	want := doc[start : end+len("apt update && apt install pbp")]
	m := regexp.MustCompile(`(?s)<code id="setup">(.*?)</code>`).FindStringSubmatch(readFile(t, landingPage))
	if m == nil {
		t.Fatal(`the landing page has no <code id="setup">`)
	}
	if got := strings.TrimSpace(html.UnescapeString(m[1])); got != want {
		t.Errorf("landing page setup commands differ from docs/install-apt.md:\n%s\nwant\n%s", got, want)
	}
}

func TestLandingPageSelfContained(t *testing.T) {
	page := strings.ToLower(readFile(t, landingPage))
	for _, bad := range []string{"<script", "<link", "<img", "<iframe", "src=", "@import", "url("} {
		if strings.Contains(page, bad) {
			t.Errorf("the landing page contains %q: it must load nothing", bad)
		}
	}
	if !strings.Contains(page, `http-equiv="content-security-policy" content="default-src 'none'`) {
		t.Error("the landing page lacks its default-src 'none' Content-Security-Policy")
	}
}
