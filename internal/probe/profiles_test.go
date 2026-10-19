package probe_test

import (
	"testing"

	"git.breadthanalytics.com/tdiana/pbv/internal/config"
	"git.breadthanalytics.com/tdiana/pbv/internal/model"
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
	ex := healthy()
	ck := config.Checks{
		IgnoreUnits: []string{"[bad"},
		Profiles:    []config.Profile{{Name: "x", Unit: "[bad", CheckSet: config.CheckSet{Commands: []string{"false"}}}},
	}
	ex[failedC] = out("a.service loaded failed failed A\n")
	o := run(ex, ck)
	if o.Verdict != model.Failed || o.Reason != "failed: a.service" {
		t.Fatalf("got %+v", o)
	}
}
