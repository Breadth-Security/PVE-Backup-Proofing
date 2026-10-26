package pve

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/model"
)

func TestListBackups(t *testing.T) {
	body, err := os.ReadFile("testdata/content_pbs.json")
	if err != nil {
		t.Fatal(err)
	}
	var gotQuery string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/pve1/storage/pbs-offsite/content" {
			t.Errorf("path = %q", r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		_, _ = w.Write(body)
	}))
	got, err := c.ListBackups(context.Background(), "pve1", "pbs-offsite")
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != "content=backup" {
		t.Errorf("query = %q", gotQuery)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d", len(got))
	}
	want0 := model.Backup{
		Storage: "pbs-offsite", VolID: "pbs-offsite:backup/ct/101/2026-07-26T06:00:03Z",
		Notes: "web01.example.lan", Key: model.GuestKey{Kind: model.KindLXC, VMID: 101},
		Time: time.Unix(1785045603, 0).UTC(), Size: 43956736778, Protected: false, Encrypted: true,
	}
	if got[0] != want0 {
		t.Errorf("got[0] = %+v\nwant   %+v", got[0], want0)
	}
	if !got[1].Protected || got[1].Size != 48671649419 || !got[1].Time.Equal(time.Unix(1788069609, 0)) {
		t.Errorf("got[1] = %+v", got[1])
	}
	if got[1].Time.Location() != time.UTC {
		t.Error("time not UTC")
	}
}

func TestListBackupsTolerantEncoding(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"volid":"s:backup/vm/5/x","vmid":"5","subtype":"qemu","ctime":"100","size":"7","protected":"1"},
			{"volid":"s:backup/vm/6/x","vmid":6,"subtype":"qemu","ctime":100,"size":7,"protected":false,"encrypted":0},
			{"volid":"s:backup/vm/7/x","vmid":7,"subtype":"qemu","ctime":100,"size":7,"protected":true}]}`))
	}))
	got, err := c.ListBackups(context.Background(), "pve1", "s")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Key.VMID != 5 || got[0].Key.Kind != model.KindQEMU || got[0].Size != 7 || !got[0].Protected {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[1].Protected || got[1].Encrypted {
		t.Errorf("got[1] = %+v", got[1])
	}
	if !got[2].Protected {
		t.Errorf("got[2] = %+v", got[2])
	}
}

func TestSetVolumeAttrs(t *testing.T) {
	var method, escPath, ctype string
	var form map[string][]string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, escPath, ctype = r.Method, r.URL.EscapedPath(), r.Header.Get("Content-Type")
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		form = r.PostForm
		_, _ = w.Write([]byte(`{"data":null}`))
	}))
	notes, prot := "PBP PROVEN", true
	err := c.SetVolumeAttrs(context.Background(), "pve1", "pbs-offsite", "pbs-offsite:backup/ct/101/2026-07-26T06:00:03Z", &notes, &prot)
	if err != nil {
		t.Fatal(err)
	}
	if method != "PUT" {
		t.Errorf("method = %s", method)
	}
	if want := "/api2/json/nodes/pve1/storage/pbs-offsite/content/pbs-offsite:backup%2Fct%2F101%2F2026-07-26T06:00:03Z"; escPath != want {
		t.Errorf("escaped path = %q", escPath)
	}
	if ctype != "application/x-www-form-urlencoded" {
		t.Errorf("content-type = %q", ctype)
	}
	if form["notes"][0] != "PBP PROVEN" || form["protected"][0] != "1" {
		t.Errorf("form = %v", form)
	}
}

func TestSetVolumeAttrsOmitsNil(t *testing.T) {
	var form map[string][]string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		form = r.PostForm
		_, _ = w.Write([]byte(`{"data":null}`))
	}))
	f := false
	if err := c.SetVolumeAttrs(context.Background(), "n", "s", "s:v", nil, &f); err != nil {
		t.Fatal(err)
	}
	if _, ok := form["notes"]; ok || form["protected"][0] != "0" {
		t.Errorf("form = %v", form)
	}
}

// A PBS storage also lists host backups (format "pbs-host", subtype and vmid
// null; seen on pve1). They are not guests and must not become guest "/0".
func TestListBackupsSkipsNonGuestBackups(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"content":"backup","ctime":1788809160,"format":"pbs-host","size":37398067560,"subtype":null,"vmid":null,"volid":"pbs-onsite:backup/host/laptop01/2026-09-07T19:26:00Z"},
			{"volid":"s:backup/ct/110/x","vmid":110,"subtype":"lxc","ctime":100,"size":7}]}`))
	}))
	got, err := c.ListBackups(context.Background(), "pve1", "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != (model.GuestKey{Kind: model.KindLXC, VMID: 110}) {
		t.Fatalf("got %+v", got)
	}
}

// The content listing reports only the first line of a PBS backup's notes
// (its "comment"); the full notes come from the volume's attributes.
func TestVolumeNotes(t *testing.T) {
	var method, escPath string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, escPath = r.Method, r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"data":{"notes":"PBP ERROR 2026-10-19 11:18 · x\nct110\nsecond","protected":false,"size":1}}`))
	}))
	got, err := c.VolumeNotes(context.Background(), "pve1", "pbs-offsite", "pbs-offsite:backup/ct/110/2026-10-19T06:24:17Z")
	if err != nil {
		t.Fatal(err)
	}
	if got != "PBP ERROR 2026-10-19 11:18 · x\nct110\nsecond" {
		t.Errorf("notes = %q", got)
	}
	if want := "/api2/json/nodes/pve1/storage/pbs-offsite/content/pbs-offsite:backup%2Fct%2F110%2F2026-10-19T06:24:17Z"; method != "GET" || escPath != want {
		t.Errorf("%s %s", method, escPath)
	}
}

// PVE reports protected as 0/1 (or a JSON boolean on some versions).
func TestVolumeProtected(t *testing.T) {
	for body, want := range map[string]bool{
		`{"data":{"notes":"x","protected":1,"size":1}}`:    true,
		`{"data":{"notes":"x","protected":true,"size":1}}`: true,
		`{"data":{"notes":"x","protected":0,"size":1}}`:    false,
		`{"data":{"notes":"x","size":1}}`:                  false,
	} {
		var escPath string
		c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			escPath = r.URL.EscapedPath()
			_, _ = w.Write([]byte(body))
		}))
		got, err := c.VolumeProtected(context.Background(), "pve1", "PBS", "PBS:backup/vm/101/2026-10-19T06:00:00Z")
		if err != nil || got != want {
			t.Errorf("%s: got %v, %v; want %v", body, got, err, want)
		}
		if p := "/api2/json/nodes/pve1/storage/PBS/content/PBS:backup%2Fvm%2F101%2F2026-10-19T06:00:00Z"; escPath != p {
			t.Errorf("path %s", escPath)
		}
	}
}
