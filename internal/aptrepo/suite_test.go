package aptrepo

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var testDate = time.Date(2026, 11, 14, 21, 0, 0, 0, time.UTC)

func testSuite(t *testing.T, debs ...string) Suite {
	t.Helper()
	s := Suite{Name: "stable", Archs: []string{"amd64", "arm64"}, Date: testDate, Packages: map[string][]Stanza{}}
	for _, p := range debs {
		d, err := ReadDeb(p)
		if err != nil {
			t.Fatal(err)
		}
		arch := d.Control.Get("Architecture")
		ix := Index{Stanzas: s.Packages[arch]}
		if _, err := ix.Add(d, "pool/main/p/pbp/"+filepath.Base(p)); err != nil {
			t.Fatal(err)
		}
		s.Packages[arch] = ix.Stanzas
	}
	return s
}

// tree returns every file under root, relative path → content.
func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		rel, _ := filepath.Rel(root, p)
		out[rel] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func hexSum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestWriteSuite(t *testing.T) {
	dir := t.TempDir()
	s := testSuite(t, buildDeb(t, dir, controlFor("0.10.0", "amd64"), ""), buildDeb(t, dir, controlFor("0.10.0", "arm64"), ""))
	root := t.TempDir()
	if err := WriteSuite(root, s); err != nil {
		t.Fatal(err)
	}
	files := tree(t, root)
	rel := files["dists/stable/Release"]
	for _, want := range []string{
		"Origin: PBP\nLabel: PBP\nSuite: stable\nCodename: stable\nDate: Sat, 14 Nov 2026 21:00:00 UTC\n" +
			"Architectures: amd64 arm64\nComponents: main\nAcquire-By-Hash: yes\nSHA256:\n",
	} {
		if !strings.HasPrefix(rel, want) {
			t.Fatalf("Release:\n%s\nwant prefix\n%s", rel, want)
		}
	}
	var listed []string
	for _, ln := range strings.Split(strings.SplitN(rel, "SHA256:\n", 2)[1], "\n") {
		if ln == "" {
			continue
		}
		f := strings.Fields(ln)
		if len(f) != 3 || !strings.HasPrefix(ln, " ") {
			t.Fatalf("hash line %q", ln)
		}
		content, ok := files["dists/stable/"+f[2]]
		if !ok {
			t.Fatalf("Release lists %s, which was not written", f[2])
		}
		if f[0] != hexSum(content) || f[1] != strconv.Itoa(len(content)) {
			t.Errorf("%s: listed %s %s, file %s %d", f[2], f[0], f[1], hexSum(content), len(content))
		}
		byHash := "dists/stable/" + filepath.Dir(f[2]) + "/by-hash/SHA256/" + f[0]
		if files[byHash] != content {
			t.Errorf("%s missing or different", byHash)
		}
		listed = append(listed, f[2])
	}
	want := []string{"main/binary-amd64/Packages", "main/binary-amd64/Packages.gz", "main/binary-arm64/Packages", "main/binary-arm64/Packages.gz"}
	if strings.Join(listed, ",") != strings.Join(want, ",") {
		t.Errorf("listed %v, want %v (sorted by path)", listed, want)
	}
	if !strings.Contains(files["dists/stable/main/binary-arm64/Packages"], "Architecture: arm64\n") {
		t.Error("arm64 Packages lacks its package")
	}
}

func TestWriteSuiteDeterministic(t *testing.T) {
	dir := t.TempDir()
	s := testSuite(t, buildDeb(t, dir, controlFor("0.10.0", "amd64"), ""))
	a, b := t.TempDir(), t.TempDir()
	if err := WriteSuite(a, s); err != nil {
		t.Fatal(err)
	}
	if err := WriteSuite(b, s); err != nil {
		t.Fatal(err)
	}
	ta, tb := tree(t, a), tree(t, b)
	if len(ta) != len(tb) {
		t.Fatalf("%d vs %d files", len(ta), len(tb))
	}
	for k, v := range ta {
		if tb[k] != v {
			t.Errorf("%s differs between runs", k)
		}
	}
	gz := []byte(ta["dists/stable/main/binary-amd64/Packages.gz"])
	// gzip header: bytes 4-7 mtime, flag bit 3 (FNAME) in byte 3.
	if len(gz) < 10 || !bytes.Equal(gz[4:8], []byte{0, 0, 0, 0}) || gz[3]&0x08 != 0 {
		t.Errorf("Packages.gz header carries an mtime or a name: % x", gz[:10])
	}
}

func TestWriteSuiteKeepsOldByHash(t *testing.T) {
	dir := t.TempDir()
	root := t.TempDir()
	first := testSuite(t, buildDeb(t, dir, controlFor("0.10.0", "amd64"), ""))
	if err := WriteSuite(root, first); err != nil {
		t.Fatal(err)
	}
	before := tree(t, root)
	second := testSuite(t, buildDeb(t, dir, controlFor("0.10.0", "amd64"), ""), buildDeb(t, dir, controlFor("0.10.1", "amd64"), ""))
	if err := WriteSuite(root, second); err != nil {
		t.Fatal(err)
	}
	after := tree(t, root)
	for k, v := range before {
		if strings.Contains(k, "/by-hash/") && after[k] != v {
			t.Errorf("by-hash file %s was removed or changed by the next run", k)
		}
	}
	if after["dists/stable/main/binary-amd64/Packages"] == before["dists/stable/main/binary-amd64/Packages"] {
		t.Error("Packages did not change")
	}
}

func TestWriteSuiteEmptyArch(t *testing.T) {
	dir := t.TempDir()
	root := t.TempDir()
	if err := WriteSuite(root, testSuite(t, buildDeb(t, dir, controlFor("0.10.0", "amd64"), ""))); err != nil {
		t.Fatal(err)
	}
	files := tree(t, root)
	p, ok := files["dists/stable/main/binary-arm64/Packages"]
	if !ok || p != "" {
		t.Errorf("arm64 Packages: present %v content %q", ok, p)
	}
	if !strings.Contains(files["dists/stable/Release"], " main/binary-arm64/Packages\n") {
		t.Error("the empty arm64 Packages is not listed in Release")
	}
}
