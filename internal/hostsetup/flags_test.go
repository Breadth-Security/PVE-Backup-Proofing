package hostsetup

import (
	"reflect"
	"strings"
	"testing"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/config"
)

func TestFromFlagsDefaults(t *testing.T) {
	a, err := FromFlags(Flags{Storages: "pbs-main"}, Layout{Deb: true}, "Europe/Berlin", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	want := Answers{Address: "127.0.0.1", VMIDRange: [2]int{990000, 990049}, Bridge: "vmbr99", RestoreStorage: "local-zfs",
		PBSStorages: []string{"pbs-main"}, Schedule: "0 2 * * *", TZ: ""}
	if !reflect.DeepEqual(a, want) {
		t.Errorf("got %+v\nwant %+v", a, want)
	}
	// Docker layout: the time zone defaults to the host's.
	a, err = FromFlags(Flags{Storages: "pbs-main", Address: "pve1.example.lan"}, Layout{Dir: "/x"}, "Europe/Berlin", "")
	if err != nil || a.TZ != "Europe/Berlin" || a.Address != "pve1.example.lan" {
		t.Errorf("docker defaults: %+v %v", a, err)
	}
	// Given values are used, storages deduplicated as the question does.
	a, err = FromFlags(Flags{Address: "10.0.0.5", VMIDRange: "990100-990109", Bridge: "vmbr77", RestoreStorage: "local-lvm",
		Storages: "pbs-a, pbs-b,pbs-a", Schedule: "30 1 * * *", TZ: "UTC"}, Layout{Dir: "/x"}, "Europe/Berlin", "")
	want = Answers{Address: "10.0.0.5", VMIDRange: [2]int{990100, 990109}, Bridge: "vmbr77", RestoreStorage: "local-lvm",
		PBSStorages: []string{"pbs-a", "pbs-b"}, Schedule: "30 1 * * *", TZ: "UTC"}
	if err != nil || !reflect.DeepEqual(a, want) {
		t.Errorf("given values: %+v %v", a, err)
	}
}

func TestFromFlagsRequired(t *testing.T) {
	if _, err := FromFlags(Flags{}, Layout{Deb: true}, "", "127.0.0.1"); err == nil || !strings.Contains(err.Error(), "--storages is required") {
		t.Errorf("no storages: %v", err)
	}
	if _, err := FromFlags(Flags{Storages: "pbs"}, Layout{Deb: true}, "", ""); err == nil || !strings.Contains(err.Error(), "--address is required") {
		t.Errorf("no address off the node: %v", err)
	}
}

func TestFromFlagsInvalid(t *testing.T) {
	base := Flags{Address: "pve1.example.lan", Storages: "pbs"}
	cases := []struct {
		name string
		mod  func(*Flags)
		want []string
	}{
		{"address", func(f *Flags) { f.Address = "https://x" }, []string{"--address", "without a scheme"}},
		{"range", func(f *Flags) { f.VMIDRange = "5-1" }, []string{"--vmid-range", "need 100 <= lo <= hi"}},
		{"range syntax", func(f *Flags) { f.VMIDRange = "abc" }, []string{"--vmid-range", "enter the range as lo-hi"}},
		{"bridge", func(f *Flags) { f.Bridge = "-x" }, []string{"--bridge", "not a valid bridge name"}},
		{"restore", func(f *Flags) { f.RestoreStorage = "local-" }, []string{"--restore-storage", "not a valid storage ID"}},
		{"storages", func(f *Flags) { f.Storages = "pbs,1x" }, []string{"--storages", "not a valid storage ID"}},
		{"schedule", func(f *Flags) { f.Schedule = "nope" }, []string{"--schedule", "invalid cron expression"}},
		{"tz", func(f *Flags) { f.TZ = "Local" }, []string{"--tz", "unknown time zone"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := base
			tc.mod(&f)
			_, err := FromFlags(f, Layout{Dir: "/x"}, "UTC", "")
			if err == nil {
				t.Fatal("want an error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err %q lacks %q", err, w)
				}
			}
		})
	}
	if _, err := FromFlags(Flags{Storages: "pbs", TZ: "UTC"}, Layout{Deb: true}, "", "127.0.0.1"); err == nil || !strings.Contains(err.Error(), "--tz") {
		t.Errorf("--tz on the deb layout: %v", err)
	}
}

func TestConflicts(t *testing.T) {
	var cfg config.Config
	cfg.Schedule = "0 2 * * *"
	cfg.Target.SSH.Host = "pve1.example.lan"
	cfg.Target.VMIDRange = [2]int{990000, 990049}
	cfg.Target.Bridge = "vmbr99"
	cfg.Restore.Storage = "local-zfs"
	cfg.Sources.Storages = []string{"pbs-a", "pbs-b"}

	if got := Conflicts(cfg, Flags{Bridge: "vmbr77"}); !reflect.DeepEqual(got, []string{"--bridge vmbr77 differs from target.bridge vmbr99 in config.yaml"}) {
		t.Errorf("bridge: %q", got)
	}
	same := Flags{Address: "pve1.example.lan", VMIDRange: "990000-990049", Bridge: "vmbr99", RestoreStorage: "local-zfs",
		Storages: "pbs-a, pbs-b", Schedule: "0 2 * * *", TZ: "Mars/Base"}
	if got := Conflicts(cfg, same); got != nil {
		t.Errorf("equal flags (TZ not compared): %q", got)
	}
	if got := Conflicts(cfg, Flags{}); got != nil {
		t.Errorf("no flags: %q", got)
	}
	all := Flags{Address: "10.0.0.5", VMIDRange: "990100-990109", Bridge: "vmbr7", RestoreStorage: "local-lvm", Storages: "pbs-c", Schedule: "1 2 * * *"}
	got := Conflicts(cfg, all)
	for _, key := range []string{"target.ssh.host", "target.vmid_range", "target.bridge", "restore.storage", "sources.storages", "schedule"} {
		found := false
		for _, g := range got {
			found = found || strings.Contains(g, " "+key+" ")
		}
		if !found {
			t.Errorf("no conflict for %s in %q", key, got)
		}
	}
}
