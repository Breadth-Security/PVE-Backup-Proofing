package hostsetup

import (
	"reflect"
	"strings"
	"testing"
)

func TestAskDefaults(t *testing.T) {
	var out strings.Builder
	a, err := Ask(strings.NewReader("pve1.example.lan\n\n\n\npbs-main, pbs-two\n\n\n"), &out, Layout{Dir: "/x"}, "Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	want := Answers{
		Address: "pve1.example.lan", VMIDRange: [2]int{990000, 990049}, Bridge: "vmbr99",
		RestoreStorage: "local-zfs", PBSStorages: []string{"pbs-main", "pbs-two"},
		Schedule: "0 2 * * *", TZ: "Europe/Berlin",
	}
	if !reflect.DeepEqual(a, want) {
		t.Fatalf("got %+v\nwant %+v", a, want)
	}
	for _, p := range []string{"[990000-990049]", "[vmbr99]", "[local-zfs]", `[0 2 * * *]`, "[Europe/Berlin]"} {
		if !strings.Contains(out.String(), p) {
			t.Errorf("prompt lacks default %s:\n%s", p, out.String())
		}
	}
}

func TestAskDebNoTimeZone(t *testing.T) {
	var out strings.Builder
	a, err := Ask(strings.NewReader("10.0.0.5\n\n\n\npbs\n\n"), &out, Layout{Deb: true}, "Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	if a.TZ != "" || a.Address != "10.0.0.5" || strings.Contains(out.String(), "zone") {
		t.Fatalf("deb asks no time zone: %+v\n%s", a, out.String())
	}
}

func TestAskReasks(t *testing.T) {
	in := strings.Join([]string{
		"", "https://pve1:8006", "pve1:22", "pve1", // address: empty, scheme, port, ok
		"5-1", "abc", "990000-990009", // range: rule, syntax, ok
		"-bad", "vmbr99", // bridge
		"bad/x", "", // restore storage
		"", "pbs,1x", "pbs, pbs", // PBS storages: empty, bad id, ok (dedup)
		"nope", "0 3 * * *", // schedule
		"Mars/Base", "", // tz
	}, "\n") + "\n"
	var out strings.Builder
	a, err := Ask(strings.NewReader(in), &out, Layout{Dir: "/x"}, "UTC")
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if a.Address != "pve1" || a.VMIDRange != [2]int{990000, 990009} || a.Bridge != "vmbr99" ||
		a.RestoreStorage != "local-zfs" || !reflect.DeepEqual(a.PBSStorages, []string{"pbs"}) ||
		a.Schedule != "0 3 * * *" || a.TZ != "UTC" {
		t.Fatalf("got %+v", a)
	}
	for _, msg := range []string{
		"need 100 <= lo <= hi < 999999999, got [5, 1]",
		"enter the range as lo-hi",
		"not a valid bridge name",
		"invalid cron expression",
		"unknown time zone",
		"not a valid storage id",
		"without a scheme",
	} {
		if !strings.Contains(out.String(), msg) {
			t.Errorf("output lacks %q:\n%s", msg, out.String())
		}
	}
}

func TestAskEOF(t *testing.T) {
	var out strings.Builder
	if _, err := Ask(strings.NewReader("pve1\n\n"), &out, Layout{Dir: "/x"}, "UTC"); err == nil {
		t.Fatal("want an error on end of input")
	}
	if _, err := Ask(strings.NewReader("pve1\nbad\n"), &out, Layout{Dir: "/x"}, "UTC"); err == nil {
		t.Fatal("want an error on end of input while re-asking")
	}
}
