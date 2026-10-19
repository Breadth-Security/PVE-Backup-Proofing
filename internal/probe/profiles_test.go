package probe_test

import (
	"testing"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
)

func TestUserProfileOverridesBuiltin(t *testing.T) {
	ex := healthy() // listens on 443 and 22 only
	ck := config.Checks{Profiles: []config.Profile{{Name: "nginx", Unit: "nginx.service", CheckSet: config.CheckSet{Ports: []int{8443}}}}}
	o := run(ex, ck)
	if o.Verdict != model.Failed || o.Reason != "check failed: nginx:ports:8443" {
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
