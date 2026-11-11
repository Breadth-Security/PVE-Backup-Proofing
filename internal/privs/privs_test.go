package privs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// stub answers Privileges from a map; a path in fail errors instead.
type stub struct {
	grant map[string][]string
	fail  map[string]error
}

func (s stub) Privileges(_ context.Context, path string) (map[string]bool, error) {
	if err := s.fail[path]; err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, p := range s.grant[path] {
		out[p] = true
	}
	return out, nil
}

// all grants every need in full.
func all(needs []Need) map[string][]string {
	m := map[string][]string{}
	for _, n := range needs {
		m[n.Path] = slices.Clone(n.Privs)
	}
	return m
}

var wantVMs = []string{"VM.Allocate", "VM.Audit", "VM.Backup", "VM.Config.HWType", "VM.Config.Network",
	"VM.Config.Options", "VM.PowerMgmt", "VM.GuestAgent.Audit", "VM.GuestAgent.Unrestricted"}

func TestNeedsOrderAndPrivs(t *testing.T) {
	needs := Needs("pve1", "local-zfs", []string{"pbs", "pbs-sync"})
	var paths, sources []string
	for _, n := range needs {
		paths = append(paths, n.Path)
		sources = append(sources, n.Source)
	}
	wantPaths := []string{"/vms", "/storage/local-zfs", "/storage/pbs", "/storage/pbs-sync", "/nodes/pve1", "/sdn/zones/localnetwork"}
	if !slices.Equal(paths, wantPaths) {
		t.Fatalf("paths = %v, want %v", paths, wantPaths)
	}
	if want := []string{"", "", "pbs", "pbs-sync", "", ""}; !slices.Equal(sources, want) {
		t.Errorf("sources = %q, want %q", sources, want)
	}
	if !slices.Equal(needs[0].Privs, wantVMs) {
		t.Errorf("/vms privs = %v", needs[0].Privs)
	}
	storage := []string{"Datastore.AllocateSpace", "Datastore.Audit"}
	for _, i := range []int{1, 2, 3} {
		if !slices.Equal(needs[i].Privs, storage) {
			t.Errorf("%s privs = %v", needs[i].Path, needs[i].Privs)
		}
	}
	if !slices.Equal(needs[4].Privs, []string{"Sys.Audit"}) || !slices.Equal(needs[5].Privs, []string{"SDN.Use"}) {
		t.Errorf("node %v zone %v", needs[4].Privs, needs[5].Privs)
	}
}

func TestNeedsRestoreStorageAlsoSource(t *testing.T) {
	needs := Needs("pve1", "pbs", []string{"pbs", "pbs", "other"})
	count := map[string]int{}
	for _, n := range needs {
		count[n.Path]++
		switch n.Path {
		case "/storage/pbs":
			if n.Source != "" {
				t.Errorf("/storage/pbs Source = %q, want core", n.Source)
			}
		case "/storage/other":
			if n.Source != "other" {
				t.Errorf("/storage/other Source = %q", n.Source)
			}
		}
	}
	if count["/storage/pbs"] != 1 || count["/storage/other"] != 1 {
		t.Errorf("counts = %v", count)
	}
}

func TestNeedsCoverRolePrivs(t *testing.T) {
	union := map[string]bool{}
	for _, n := range Needs("pve1", "local-zfs", []string{"pbs"}) {
		for _, p := range n.Privs {
			union[p] = true
		}
	}
	if len(union) != len(RolePrivs) {
		t.Errorf("union has %d privileges, RolePrivs %d", len(union), len(RolePrivs))
	}
	for _, p := range RolePrivs {
		if !union[p] {
			t.Errorf("%s in RolePrivs but needed on no path", p)
		}
	}
}

func TestCheckReportsOnlyLacking(t *testing.T) {
	needs := Needs("pve1", "local-zfs", []string{"pbs"})
	g := all(needs)
	g["/vms"] = slices.DeleteFunc(g["/vms"], func(p string) bool { return p == "VM.Backup" })
	g["/storage/pbs"] = []string{"Datastore.Audit"}
	missing, err := Check(context.Background(), stub{grant: g}, needs)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 2 {
		t.Fatalf("missing = %+v", missing)
	}
	if missing[0].Path != "/vms" || !slices.Equal(missing[0].Lacks, []string{"VM.Backup"}) {
		t.Errorf("first = %+v", missing[0])
	}
	if missing[1].Path != "/storage/pbs" || missing[1].Source != "pbs" || !slices.Equal(missing[1].Lacks, []string{"Datastore.AllocateSpace"}) {
		t.Errorf("second = %+v", missing[1])
	}
}

func TestCheckReadError(t *testing.T) {
	needs := Needs("pve1", "local-zfs", nil)
	_, err := Check(context.Background(), stub{grant: all(needs), fail: map[string]error{"/nodes/pve1": errors.New("boom")}}, needs)
	if err == nil || !strings.Contains(err.Error(), "read the token's privileges on /nodes/pve1:") {
		t.Errorf("err = %v", err)
	}
}

func TestDescribeUsesConfiguredToken(t *testing.T) {
	got := Missing{Need{Path: "/vms"}, []string{"VM.Backup", "VM.Audit"}}.Describe("pbp2@pve!night")
	want := "token pbp2@pve!night lacks VM.Backup, VM.Audit on /vms; the user pbp2@pve and its privilege-separated token both need them: " +
		"pveum acl modify /vms --users pbp2@pve --tokens 'pbp2@pve!night' --roles PBPProofer"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestDescribeSinglePrivilege(t *testing.T) {
	got := Missing{Need{Path: "/sdn/zones/localnetwork"}, []string{"SDN.Use"}}.Describe("pbp@pve!proof")
	want := "token pbp@pve!proof lacks SDN.Use on /sdn/zones/localnetwork; the user pbp@pve and its privilege-separated token both need it: " +
		"pveum acl modify /sdn/zones/localnetwork --users pbp@pve --tokens 'pbp@pve!proof' --roles PBPProofer"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}
