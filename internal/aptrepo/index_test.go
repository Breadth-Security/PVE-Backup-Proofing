package aptrepo

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

func TestParseFormatRoundTrip(t *testing.T) {
	in := "Package: pbp\nVersion: 0.10.0\nArchitecture: amd64\nDescription: PVE Backup Proofing\n Restores backups.\n .\n More.\nSHA256: aa\n\n" +
		"Package: pbp\nVersion: 0.10.1\nArchitecture: amd64\nSHA256: bb\n"
	st, err := ParsePackages(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 2 || st[1].Get("Version") != "0.10.1" {
		t.Fatalf("stanzas %+v", st)
	}
	if got := string(FormatPackages(st)); got != in {
		t.Errorf("round trip:\n%q\nwant\n%q", got, in)
	}
	if st, err := ParsePackages(strings.NewReader("")); err != nil || len(st) != 0 {
		t.Errorf("empty input: %v %v", st, err)
	}
	if _, err := ParsePackages(strings.NewReader(" continuation first\n")); err == nil {
		t.Error("a continuation line without a field must be an error")
	}
}

func TestIndexAdd(t *testing.T) {
	dir := t.TempDir()
	deb, err := ReadDeb(buildDeb(t, dir, controlFor("0.10.0", "amd64"), ""))
	if err != nil {
		t.Fatal(err)
	}
	var ix Index
	added, err := ix.Add(deb, "pool/main/p/pbp/pbp_0.10.0_amd64.deb")
	if err != nil || !added {
		t.Fatalf("add: %v %v", added, err)
	}
	if len(ix.Stanzas) != 1 {
		t.Fatalf("stanzas %d", len(ix.Stanzas))
	}
	s := ix.Stanzas[0]
	n := len(deb.Control)
	if len(s) != n+3 || s[n].Name != "Filename" || s[n+1].Name != "Size" || s[n+2].Name != "SHA256" {
		t.Fatalf("stanza tail %+v", s[n-1:])
	}
	if s.Get("Filename") != "pool/main/p/pbp/pbp_0.10.0_amd64.deb" || s.Get("Size") != strconv.FormatInt(deb.Size, 10) || s.Get("SHA256") != deb.SHA256 {
		t.Errorf("stanza %+v", s)
	}
	if !bytes.Contains(FormatPackages(ix.Stanzas), []byte("Package: pbp\n")) {
		t.Error("formatted index lacks the package")
	}

	added, err = ix.Add(deb, "pool/main/p/pbp/pbp_0.10.0_amd64.deb")
	if err != nil || added || len(ix.Stanzas) != 1 {
		t.Errorf("same deb again: added %v err %v stanzas %d", added, err, len(ix.Stanzas))
	}

	other, err := ReadDeb(buildDeb(t, t.TempDir(), controlFor("0.10.0", "amd64"), "x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Add(other, "pool/main/p/pbp/pbp_0.10.0_amd64.deb"); err == nil || !strings.Contains(err.Error(), "already published with a different SHA256") {
		t.Errorf("changed file: err = %v", err)
	}

	arm, err := ReadDeb(buildDeb(t, dir, controlFor("0.10.0", "arm64"), ""))
	if err != nil {
		t.Fatal(err)
	}
	if added, err := ix.Add(arm, "pool/main/p/pbp/pbp_0.10.0_arm64.deb"); err != nil || !added {
		t.Errorf("other arch, same version: %v %v", added, err)
	}

	rc, err := ReadDeb(buildDeb(t, dir, controlFor("0.11.0~rc.1", "amd64"), ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Add(rc, "pool/main/p/pbp/pbp_0.11.0~rc.1_amd64.deb"); err == nil || !strings.Contains(err.Error(), "pre-release") {
		t.Errorf("pre-release: err = %v", err)
	}
}
