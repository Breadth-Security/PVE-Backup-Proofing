package aptrepo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// arMember is one member of an ar archive.
type arMember struct {
	name string
	data []byte
}

// writeAr returns an ar archive (the .deb container format) of members.
func writeAr(members []arMember) []byte {
	var b bytes.Buffer
	b.WriteString("!<arch>\n")
	for _, m := range members {
		fmt.Fprintf(&b, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", m.name, 0, 0, 0, "100644", len(m.data))
		b.Write(m.data)
		if len(m.data)%2 == 1 {
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}

// tarGz returns a gzipped tar holding files (name → content).
func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// controlFor is a control file for pbp at version and arch.
func controlFor(version, arch string) string {
	return "Package: pbp\nVersion: " + version + "\nSection: admin\nPriority: optional\nArchitecture: " + arch +
		"\nMaintainer: PBP <pbp@example.com>\nInstalled-Size: 12000\nDepends: openssh-client\n" +
		"Description: PVE Backup Proofing\n Restores backups and proves they work.\n .\n Second paragraph.\n"
}

// buildDeb writes a .deb with control text to dir and returns its path.
// An extra byte in data makes a different package with the same control.
func buildDeb(t *testing.T, dir, control, extra string) string {
	t.Helper()
	ver, arch := field(control, "Version"), field(control, "Architecture")
	p := filepath.Join(dir, "pbp_"+ver+"_"+arch+".deb")
	deb := writeAr([]arMember{
		{"debian-binary", []byte("2.0\n")},
		{"control.tar.gz", tarGz(t, map[string]string{"./control": control})},
		{"data.tar.gz", tarGz(t, map[string]string{"./usr/bin/pbp": "binary" + extra})},
	})
	if err := os.WriteFile(p, deb, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// field is a control field's value, for building test file names.
func field(control, name string) string {
	for _, ln := range strings.Split(control, "\n") {
		if v, ok := strings.CutPrefix(ln, name+": "); ok {
			return v
		}
	}
	return ""
}

func sha256File(t *testing.T, p string) (string, int64) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), int64(len(b))
}

func TestReadDeb(t *testing.T) {
	p := buildDeb(t, t.TempDir(), controlFor("0.10.0", "amd64"), "")
	d, err := ReadDeb(p)
	if err != nil {
		t.Fatal(err)
	}
	if d.Control.Get("Package") != "pbp" || d.Control.Get("Version") != "0.10.0" || d.Control.Get("Architecture") != "amd64" {
		t.Errorf("control %+v", d.Control)
	}
	if got := d.Control.Get("Description"); got != "PVE Backup Proofing\n Restores backups and proves they work.\n .\n Second paragraph." {
		t.Errorf("multi-line Description %q", got)
	}
	sum, size := sha256File(t, p)
	if d.SHA256 != sum || d.Size != size {
		t.Errorf("SHA256 %s size %d, want %s %d", d.SHA256, d.Size, sum, size)
	}
}

func TestReadDebRejects(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct{ name, path, want string }{
		{"not ar", write("a.deb", []byte("PK\x03\x04 a zip")), "not a .deb"},
		{"no control", write("b.deb", writeAr([]arMember{{"debian-binary", []byte("2.0\n")}, {"data.tar.gz", tarGz(t, nil)}})), "no control.tar"},
		{"xz control", write("c.deb", writeAr([]arMember{{"debian-binary", []byte("2.0\n")}, {"control.tar.xz", []byte("xz")}})), "control.tar.xz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadDeb(tc.path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
