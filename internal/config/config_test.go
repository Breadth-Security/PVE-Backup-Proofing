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
		c.Notify.When != "always" || c.Logging.RetainRuns != 30 || c.Target.SSH.Port != 22 ||
		c.Restore.ProbeTimeout != 5*time.Minute || c.Checks.CommandTimeout != time.Minute {
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
	if len(c.Checks.Guests) == 0 || len(c.Checks.Profiles) == 0 || len(c.Checks.Profiles[0].Ports) == 0 {
		t.Fatalf("example config lacks guests/profiles/ports: %+v", c.Checks)
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
	if c.Target.Type != "existing-node" || c.Target.SSH.User != "pbp" || c.Logging.Dir != "/var/log/pbp" || c.Restore.BootTimeout != 5*time.Minute {
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

const RS = "restore: {storage: local-zfs}"

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
		{"boot timeout short", RS, "restore: {storage: local-zfs, boot_timeout: 29s}", "restore.boot_timeout"},
		{"boot timeout unitless", RS, "restore: {storage: local-zfs, boot_timeout: 300}", "restore.boot_timeout"},
		{"max run negative", RS, "restore: {storage: local-zfs, max_run_duration: -1h}", "restore.max_run_duration"},
		{"max run unitless", RS, "restore: {storage: local-zfs, max_run_duration: 3600}", "restore.max_run_duration"},
		{"margin high", RS, "restore: {storage: local-zfs, space_margin_percent: 101}", "restore.space_margin_percent"},
		{"margin negative", RS, "restore: {storage: local-zfs, space_margin_percent: -1}", "restore.space_margin_percent"},
		{"retain runs", "sources:", "logging: {retain_runs: 0}\nsources:", "logging.retain_runs"},
		{"probe timeout short", RS, "restore: {storage: local-zfs, probe_timeout: 29s}", "restore.probe_timeout"},
		{"probe timeout unitless", RS, "restore: {storage: local-zfs, probe_timeout: 600}", "restore.probe_timeout"},
		{"command timeout short", "sources:", "checks: {command_timeout: 500ms}\nsources:", "checks.command_timeout"},
		{"command timeout unitless", "sources:", "checks: {command_timeout: 60}\nsources:", "checks.command_timeout"},
		{"range too high", "[990000, 990049]", "[990000, 1000000000]", "target.vmid_range"},
		{"range hi at max", "[990000, 990049]", "[990000, 999999999]", "target.vmid_range"},
		{"profile no name", "sources:", "checks: {profiles: [{unit: x.service}]}\nsources:", "checks.profiles[0].name"},
		{"profile no unit", "sources:", "checks: {profiles: [{name: x}]}\nsources:", "checks.profiles[0].unit"},
		{"profile duplicate", "sources:", "checks: {profiles: [{name: x, unit: a}, {name: x, unit: b}]}\nsources:", "checks.profiles[1].name"},
		{"profile port zero", "sources:", "checks: {profiles: [{name: x, unit: a, ports: [80, 0]}]}\nsources:", "checks.profiles[0].ports[1]"},
		{"profile port high", "sources:", "checks: {profiles: [{name: x, unit: a, ports: [65536]}]}\nsources:", "checks.profiles[0].ports[0]"},
		{"guest empty match", "sources:", "checks: {guests: [{match: {}, services: [a]}]}\nsources:", "checks.guests[0].match"},
		{"guest vmid negative", "sources:", "checks: {guests: [{match: {vmid: -1, name: x}}]}\nsources:", "checks.guests[0].match.vmid"},
		{"guest vmid too high", "sources:", "checks: {guests: [{match: {vmid: 1000000000}}]}\nsources:", "checks.guests[0].match.vmid"},
		{"guest port", "sources:", "checks: {guests: [{match: {vmid: 5}, ports: [70000]}]}\nsources:", "checks.guests[0].ports[0]"},
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

func TestRootSSHUserIsRejected(t *testing.T) {
	for _, user := range []string{"root", `"root"`} {
		_, err := loadStr(t, strings.Replace(minimal, "ssh: {known_hosts_file: /kh}", "ssh: {user: "+user+", known_hosts_file: /kh}", 1))
		want := `target.ssh.user: root is no longer supported; set up the pbp node helper (README "Node setup")`
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("user %s: err = %v, want it to contain %q", user, err, want)
		}
	}
	c, err := loadStr(t, strings.Replace(minimal, "ssh: {known_hosts_file: /kh}", "ssh: {user: backup, known_hosts_file: /kh}", 1))
	if err != nil || c.Target.SSH.User != "backup" {
		t.Errorf("another user: %v, %+v", err, c.Target.SSH)
	}
}

func TestEmptyConfigFile(t *testing.T) {
	for name, body := range map[string]string{"empty": "", "comments only": "# nothing\n"} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "c.yaml")
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(p)
			if err == nil || err.Error() != "config "+p+" is empty" {
				t.Fatalf("got %v", err)
			}
		})
	}
}

// A null list (key present, no value) falls back to the default; only an
// explicit [] means none.
func TestNullIgnoreListsUseDefaults(t *testing.T) {
	c, err := loadStr(t, minimal+"checks:\n  ignore_units:\n  windows_ignore_services: ~\n")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Checks.IgnoreUnits, DefaultIgnoreUnits()) || !reflect.DeepEqual(c.Checks.WindowsIgnoreServices, DefaultWindowsIgnore()) {
		t.Fatalf("null lists must default: %+v", c.Checks)
	}
}

// A profile or guest with no checks is valid (the unit is just required active).
func TestChecklessProfileAndGuestValid(t *testing.T) {
	if _, err := loadStr(t, minimal+"checks: {profiles: [{name: x, unit: a}], guests: [{match: {name: web}}, {match: {vmid: 999999999}}]}\n"); err != nil {
		t.Fatal(err)
	}
}

// A duration without a unit must be rejected with an error naming the key,
// and so must one under 1s (almost certainly a unit typo).
func TestUnitlessDurationMessage(t *testing.T) {
	_, err := loadStr(t, strings.Replace(minimal, RS, "restore: {storage: local-zfs, boot_timeout: 300}", 1))
	if err == nil || !strings.Contains(err.Error(), "restore.boot_timeout: 300 has no unit") {
		t.Fatalf("err = %v", err)
	}
	_, err = loadStr(t, strings.Replace(minimal, RS, "restore: {storage: local-zfs, max_run_duration: 500ms}", 1))
	if err == nil || !strings.Contains(err.Error(), "restore.max_run_duration: must be >= 1s, got 500ms") {
		t.Fatalf("err = %v", err)
	}
}

func TestTimeoutDefaults(t *testing.T) {
	c, err := loadStr(t, minimal)
	if err != nil {
		t.Fatal(err)
	}
	if c.Restore.ProbeTimeout != 5*time.Minute || c.Checks.CommandTimeout != time.Minute {
		t.Fatalf("probe_timeout %v command_timeout %v", c.Restore.ProbeTimeout, c.Checks.CommandTimeout)
	}
	c, err = loadStr(t, strings.Replace(minimal, RS, "restore: {storage: local-zfs, probe_timeout: 30s}", 1)+"checks: {command_timeout: 1s}\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Restore.ProbeTimeout != 30*time.Second || c.Checks.CommandTimeout != time.Second {
		t.Fatalf("probe_timeout %v command_timeout %v", c.Restore.ProbeTimeout, c.Checks.CommandTimeout)
	}
}

func TestValidBounds(t *testing.T) {
	c, err := loadStr(t, strings.Replace(minimal, RS,
		"restore: {storage: local-zfs, boot_timeout: 30s, max_run_duration: 0s, space_margin_percent: 0}", 1)+
		"logging: {retain_runs: 1}\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Restore.BootTimeout != 30*time.Second || c.Restore.MaxRunDuration != 0 || c.Restore.SpaceMarginPercent != 0 || c.Logging.RetainRuns != 1 {
		t.Fatalf("%+v", c)
	}
	if _, err := loadStr(t, strings.Replace(minimal, RS, "restore: {storage: local-zfs, space_margin_percent: 100}", 1)); err != nil {
		t.Fatal(err)
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

// The refusal probe uses vmid_range[1]+1, so hi stops one short of the
// largest VMID Proxmox accepts.
func TestVMIDRangeTop(t *testing.T) {
	c, err := loadStr(t, strings.Replace(minimal, "[990000, 990049]", "[990000, 999999998]", 1))
	if err != nil {
		t.Fatal(err)
	}
	if c.Target.VMIDRange[1] != 999999998 {
		t.Errorf("hi = %d, want 999999998", c.Target.VMIDRange[1])
	}
}
