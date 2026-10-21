package config

import (
	"strings"
	"testing"
)

func TestExcludeLoads(t *testing.T) {
	c, err := loadStr(t, minimal+"selection:\n  exclude:\n    guests: [104, CT109, vm5000, 900-999]\n    tags: [no-proof, Keep_Out]\n")
	if err != nil {
		t.Fatal(err)
	}
	ex := c.Selection.Exclude
	if strings.Join(ex.Guests, " ") != "104 CT109 vm5000 900-999" || strings.Join(ex.Tags, " ") != "no-proof Keep_Out" {
		t.Fatalf("exclude = %+v", ex)
	}
	// An absent or empty selection excludes nothing, as before.
	for _, s := range []string{minimal, minimal + "selection: {}\n"} {
		c, err := loadStr(t, s)
		if err != nil || len(c.Selection.Exclude.Guests)+len(c.Selection.Exclude.Tags) != 0 {
			t.Fatalf("%q: exclude %+v err %v", s, c.Selection.Exclude, err)
		}
	}
}

// A malformed rule stops the config from loading, naming the entry.
func TestExcludeInvalid(t *testing.T) {
	for in, want := range map[string]string{
		"guests: [web01]":    `selection.exclude.guests: "web01"`,
		"guests: [VM]":       `selection.exclude.guests: "VM"`,
		"guests: [999-900]":  `selection.exclude.guests: "999-900"`,
		"guests: [0]":        `selection.exclude.guests: "0"`,
		"guests: [LX109]":    `selection.exclude.guests: "LX109"`,
		"tags: ['']":         `selection.exclude.tags: ""`,
		"tags: ['no proof']": `selection.exclude.tags: "no proof"`,
		"include: [1]":       "include",
	} {
		_, err := loadStr(t, minimal+"selection:\n  exclude: {"+in+"}\n")
		if in == "include: [1]" {
			_, err = loadStr(t, minimal+"selection:\n  include: [1]\n")
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want one naming %s", in, err, want)
		}
	}
}

func TestExcludedBy(t *testing.T) {
	ex := Exclude{Guests: []string{"104", "CT109", "vm5000", "900-999"}, Tags: []string{"no-proof"}}
	for _, tc := range []struct {
		kind string
		vmid int
		tags []string
		want string
	}{
		{"qemu", 104, nil, "guests: 104"},
		{"lxc", 104, nil, "guests: 104"},
		{"lxc", 109, nil, "guests: CT109"},
		{"qemu", 109, nil, ""},
		{"qemu", 5000, nil, "guests: vm5000"},
		{"lxc", 5000, nil, ""},
		{"qemu", 900, nil, "guests: 900-999"},
		{"lxc", 999, nil, "guests: 900-999"},
		{"qemu", 1000, nil, ""},
		{"qemu", 200, []string{"web", "no-proof"}, "tag no-proof"},
		{"qemu", 200, []string{"No-Proof"}, "tag no-proof"},
		{"qemu", 200, []string{"web"}, ""},
	} {
		if got := ex.ExcludedBy(tc.kind, tc.vmid, tc.tags); got != tc.want {
			t.Errorf("%s/%d %v: got %q, want %q", tc.kind, tc.vmid, tc.tags, got, tc.want)
		}
	}
}
