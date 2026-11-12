package probe_test

import (
	"testing"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/probe"
)

func TestUserProfileOverridesBuiltin(t *testing.T) {
	ex := healthy() // listens on 443 and 22 only
	ck := config.Checks{Profiles: []config.Profile{{Name: "nginx", Unit: "nginx.service", CheckSet: config.CheckSet{Ports: []int{8443}}}}}
	o := run(ex, ck)
	if o.Verdict != model.Failed || o.Reason != "check failed: nginx (port 8443)" {
		t.Fatalf("got %+v", o)
	}
	ex[ssC] = out("LISTEN 0 1 *:8443 *:*\n")
	if o = run(ex, ck); o.Verdict != model.Proven {
		t.Fatalf("got %+v", o)
	}
}

func TestPortParsing(t *testing.T) {
	ex := healthy()
	ex[ssC] = out("LISTEN 0 4096 127.0.0.1%lo:53 0.0.0.0:*\nLISTEN 0 1 *:80 *:*\n")
	if o := run(ex, config.Checks{}); o.Verdict != model.Proven {
		t.Fatalf("got %+v", o)
	}
}

func TestMalformedGlobNeverMatches(t *testing.T) {
	ck := config.Checks{
		Profiles: []config.Profile{{Name: "x", Unit: "[bad", CheckSet: config.CheckSet{Commands: []string{"false"}}}},
	}
	o := run(healthy(), ck)
	if o.Verdict != model.Proven || len(o.Checks) != 1 || o.Checks[0].Name != "nginx:ports:80,443" {
		t.Fatalf("malformed profile unit must match nothing: %+v", o)
	}
	ex := healthy()
	ex[failedC] = out("a.service loaded failed failed A\n")
	o = run(ex, config.Checks{IgnoreUnits: []string{"[bad"}})
	if o.Verdict != model.Failed || o.Reason != "failed: a.service" || len(o.IgnoredUnits) != 0 {
		t.Fatalf("got %+v", o)
	}
}

// TestEnableGuestAgent: a checks.guests entry with enable_guest_agent turns
// the agent on for the guests it matches, by VMID, name or tag (#123).
func TestEnableGuestAgent(t *testing.T) {
	c := config.Checks{Guests: []config.GuestChecks{
		{Match: config.GuestMatch{VMID: 502}, EnableGuestAgent: true},
		{Match: config.GuestMatch{Name: "web"}, EnableGuestAgent: true},
		{Match: config.GuestMatch{Tag: "legacy"}, EnableGuestAgent: true},
		{Match: config.GuestMatch{VMID: 600}, CheckSet: config.CheckSet{Services: []string{"x"}}},
	}}
	for _, tc := range []struct {
		g    probe.GuestInfo
		want bool
	}{
		{probe.GuestInfo{VMID: 502}, true},
		{probe.GuestInfo{VMID: 503, Name: "web"}, true},
		{probe.GuestInfo{VMID: 504, Tags: []string{"prod", "legacy"}}, true},
		{probe.GuestInfo{VMID: 600}, false}, // matched, but the entry does not set it
		{probe.GuestInfo{VMID: 505, Name: "db", Tags: []string{"prod"}}, false},
	} {
		if got := probe.EnableGuestAgent(c, tc.g); got != tc.want {
			t.Errorf("EnableGuestAgent(%+v) = %v, want %v", tc.g, got, tc.want)
		}
	}
	if probe.EnableGuestAgent(config.Checks{}, probe.GuestInfo{VMID: 502}) {
		t.Error("no entries: want false")
	}
}
