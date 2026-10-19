package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadExampleConfig(t *testing.T) {
	c, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.Workers != 1 || c.Restore.SpaceMarginPercent != 10 || c.Restore.BootTimeout != 5*time.Minute ||
		c.Notify.When != "always" || c.Logging.RetainRuns != 30 || c.Target.SSH.Port != 22 {
		t.Fatalf("unexpected values: %+v", c)
	}
	found := false
	for _, u := range c.Checks.IgnoreUnits {
		if u == "systemd-networkd-wait-online.service" {
			found = true
		}
	}
	if !found {
		t.Fatal("default ignore unit missing")
	}
	if !reflect.DeepEqual(c.Checks.IgnoreUnits, DefaultIgnoreUnits()) || !reflect.DeepEqual(c.Checks.WindowsIgnoreServices, DefaultWindowsIgnore()) {
		t.Fatal("example lists drift from defaults")
	}
	if c.Target.VMIDRange != [2]int{990000, 990049} || c.Checks.Guests[0].Match.VMID != 105 || c.Checks.Profiles[0].Ports[0] != 8080 {
		t.Fatalf("nested decode wrong: %+v", c)
	}
}

const minimal = `
schedule: "0 2 * * *"
target:
  node: pve1
  bridge: vmbr-pbp
  vmid_range: [990000, 990049]
  api: {tls_fingerprint: "AB"}
  ssh: {known_hosts_file: /kh}
sources: {storages: [pbs-main]}
restore: {storage: local-zfs}
`

func loadStr(t *testing.T, s string) (Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestDefaultsAndAbsence(t *testing.T) {
	c, err := loadStr(t, minimal)
	if err != nil {
		t.Fatal(err)
	}
	if c.Target.Type != "existing-node" || c.Target.SSH.User != "root" || c.Logging.Dir != "/var/log/pbp" || c.Restore.BootTimeout != 5*time.Minute {
		t.Fatalf("defaults: %+v", c)
	}
	if !reflect.DeepEqual(c.Checks.IgnoreUnits, DefaultIgnoreUnits()) {
		t.Fatal("absent ignore_units must default")
	}
	c, err = loadStr(t, strings.Replace(minimal, "restore: {storage: local-zfs}", "restore: {storage: local-zfs, boot_timeout: 90s}", 1)+"checks: {ignore_units: [], windows_ignore_services: [foo]}\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Checks.IgnoreUnits) != 0 || !reflect.DeepEqual(c.Checks.WindowsIgnoreServices, []string{"foo"}) {
		t.Fatalf("explicit lists must be exact: %+v", c.Checks)
	}
	if c.Restore.BootTimeout != 90*time.Second {
		t.Fatalf("duration: %v", c.Restore.BootTimeout)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct{ name, from, to, key string }{
		{"node", "node: pve1", "node: ''", "target.node"},
		{"bridge", "bridge: vmbr-pbp", "bridge: ''", "target.bridge"},
		{"storage", "restore: {storage: local-zfs}", "restore: {}", "restore.storage"},
		{"sources", "storages: [pbs-main]", "storages: []", "sources.storages"},
		{"range lo", "[990000, 990049]", "[99, 990049]", "target.vmid_range"},
		{"range hi", "[990000, 990049]", "[990000, 989999]", "target.vmid_range"},
		{"workers", "schedule:", "workers: 0\nschedule:", "workers"},
		{"type", "target:\n", "target:\n  type: provisioned-host\n", "target.type"},
		{"cron", `"0 2 * * *"`, `"nope"`, "schedule"},
		{"notify", "sources:", "notify: {when: sometimes}\nsources:", "notify.when"},
		{"tls", `api: {tls_fingerprint: "AB"}`, "api: {}", "target.api.tls_fingerprint"},
		{"known hosts", "known_hosts_file: /kh", "known_hosts_file: ''", "target.ssh.known_hosts_file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(minimal, tc.from) {
				t.Fatalf("bad case: %q not in minimal", tc.from)
			}
			_, err := loadStr(t, strings.Replace(minimal, tc.from, tc.to, 1))
			if err == nil {
				t.Fatalf("want error naming %q, got nil", tc.key)
			}
			// Inspect only the validation output: Load's wrapper includes the
			// file path, which contains the test name.
			inner := errors.Unwrap(err)
			if inner == nil {
				t.Fatalf("error not from validation: %v", err)
			}
			found := false
			for _, line := range strings.Split(inner.Error(), "\n") {
				if strings.HasPrefix(line, tc.key+": ") {
					found = true
				}
			}
			if !found {
				t.Fatalf("want validation line starting %q, got %v", tc.key+": ", inner)
			}
		})
	}
}

func TestEffectiveProfiles(t *testing.T) {
	b := BuiltinProfiles()
	c := Checks{Profiles: []Profile{
		{Name: "nginx", Unit: "nginx.service", CheckSet: CheckSet{Ports: []int{8443}}},
		{Name: "myapp", Unit: "myapp*.service"},
	}}
	got := c.EffectiveProfiles()
	if len(got) != len(b)+1 {
		t.Fatalf("len %d want %d", len(got), len(b)+1)
	}
	for _, p := range got {
		if p.Name == "nginx" && !reflect.DeepEqual(p.Ports, []int{8443}) {
			t.Fatalf("nginx not overridden: %+v", p)
		}
	}
	if got[len(got)-1].Name != "myapp" {
		t.Fatal("extra profile must be appended")
	}
}

func TestResolvePath(t *testing.T) {
	t.Setenv("PBP_CONFIG", "")
	if ResolvePath("") != "/etc/pbp/config.yaml" {
		t.Fatal("default")
	}
	t.Setenv("PBP_CONFIG", "/env.yaml")
	if ResolvePath("") != "/env.yaml" || ResolvePath("/flag.yaml") != "/flag.yaml" {
		t.Fatal("precedence")
	}
}
