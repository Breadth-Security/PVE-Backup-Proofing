package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildDeb writes a minimal .deb (ar: debian-binary, control.tar.gz,
// data.tar.gz) for pbp at version and arch into dir.
func buildDeb(t *testing.T, dir, version, arch string) string {
	t.Helper()
	return buildDebNamed(t, dir, "pbp", version, arch)
}

// buildDebNamed is buildDeb for the package name pkg.
func buildDebNamed(t *testing.T, dir, pkg, version, arch string) string {
	t.Helper()
	tgz := func(name, content string) []byte {
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		tw := tar.NewWriter(zw)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(content))
		_ = tw.Close()
		_ = zw.Close()
		return b.Bytes()
	}
	members := []struct {
		name string
		data []byte
	}{
		{"debian-binary", []byte("2.0\n")},
		{"control.tar.gz", tgz("./control", "Package: "+pkg+"\nVersion: "+version+"\nArchitecture: "+arch+"\nDescription: PBP\n")},
		{"data.tar.gz", tgz("./usr/bin/pbp", "bin")},
	}
	var b bytes.Buffer
	b.WriteString("!<arch>\n")
	for _, m := range members {
		fmt.Fprintf(&b, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", m.name, 0, 0, 0, "100644", len(m.data))
		b.Write(m.data)
		if len(m.data)%2 == 1 {
			b.WriteByte('\n')
		}
	}
	p := filepath.Join(dir, "pbp_"+version+"_"+arch+".deb")
	if err := os.WriteFile(p, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAptindexCLI(t *testing.T) {
	src, root := t.TempDir(), t.TempDir()
	debs := []string{buildDeb(t, src, "0.10.0", "amd64"), buildDeb(t, src, "0.10.0", "arm64")}
	args := append([]string{"-root", root, "-date", "Sat, 14 Nov 2026 21:00:00 UTC", "-suite", "stable"}, debs...)

	var out bytes.Buffer
	if err := run(args, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "added pbp_0.10.0_amd64.deb\nadded pbp_0.10.0_arm64.deb\n" {
		t.Errorf("first run printed %q", out.String())
	}
	for _, f := range []string{"pool/main/p/pbp/pbp_0.10.0_amd64.deb", "pool/main/p/pbp/pbp_0.10.0_arm64.deb", "dists/stable/Release"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	pk := filepath.Join(root, "dists/stable/main/binary-amd64/Packages")
	first, err := os.ReadFile(pk)
	if err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := run(args, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "present pbp_0.10.0_amd64.deb\npresent pbp_0.10.0_arm64.deb\n" {
		t.Errorf("second run printed %q", out.String())
	}
	again, _ := os.ReadFile(pk)
	if !bytes.Equal(first, again) {
		t.Error("Packages changed on a rerun with the same debs")
	}

	out.Reset()
	newer := buildDeb(t, src, "0.10.1", "amd64")
	if err := run(append(args[:6:6], newer), &out); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(pk)
	if !strings.Contains(string(got), "Version: 0.10.0\n") || !strings.Contains(string(got), "Version: 0.10.1\n") {
		t.Errorf("Packages after adding 0.10.1 keeps both versions:\n%s", got)
	}

	if err := run([]string{"-root", root, "-date", "yesterday"}, &out); err == nil {
		t.Error("a bad -date must be an error")
	}
}

// A package name from a .deb's control file becomes a path in the pool, so
// one that is not a Debian package name is refused before anything is
// written (gosec G703).
func TestAptindexRejectsBadPackageName(t *testing.T) {
	src, root := t.TempDir(), t.TempDir()
	p := buildDebNamed(t, src, "../../escape", "0.10.0", "amd64")
	var out bytes.Buffer
	err := run([]string{"-root", root, "-date", "Sat, 14 Nov 2026 21:00:00 UTC", p}, &out)
	if err == nil || !strings.Contains(err.Error(), "not a valid Debian package name") {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("wrote %v", entries)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escape")); err == nil {
		t.Error("wrote outside the repository tree")
	}
}
