package pve

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
)

func TestListBackups(t *testing.T) {
	body, err := os.ReadFile("testdata/content_pbs.json")
	if err != nil {
		t.Fatal(err)
	}
	var gotQuery string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/pve1/storage/PBS-Baremetal/content" {
			t.Errorf("path = %q", r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		w.Write(body)
	}))
	got, err := c.ListBackups(context.Background(), "pve1", "PBS-Baremetal")
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
		Storage: "PBS-Baremetal", VolID: "PBS-Baremetal:backup/ct/101/2026-07-26T06:00:03Z",
		Notes: "kasm.cic.unifiedlab.lan", Key: model.GuestKey{Kind: model.KindLXC, VMID: 101},
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
		w.Write([]byte(`{"data":[{"volid":"s:backup/vm/5/x","vmid":"5","subtype":"qemu","ctime":"100","size":"7","protected":"1"},
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
		r.ParseForm()
		form = r.PostForm
		w.Write([]byte(`{"data":null}`))
	}))
	notes, prot := "PBP PROVEN", true
	err := c.SetVolumeAttrs(context.Background(), "pve1", "PBS-Baremetal", "PBS-Baremetal:backup/ct/101/2026-07-26T06:00:03Z", &notes, &prot)
	if err != nil {
		t.Fatal(err)
	}
	if method != "PUT" {
		t.Errorf("method = %s", method)
	}
	if want := "/api2/json/nodes/pve1/storage/PBS-Baremetal/content/PBS-Baremetal:backup%2Fct%2F101%2F2026-07-26T06:00:03Z"; escPath != want {
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
		r.ParseForm()
		form = r.PostForm
		w.Write([]byte(`{"data":null}`))
	}))
	f := false
	if err := c.SetVolumeAttrs(context.Background(), "n", "s", "s:v", nil, &f); err != nil {
		t.Fatal(err)
	}
	if _, ok := form["notes"]; ok || form["protected"][0] != "0" {
		t.Errorf("form = %v", form)
	}
}
