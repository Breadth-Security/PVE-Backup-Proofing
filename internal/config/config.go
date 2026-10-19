// Package config loads, defaults and validates the PBP YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Schedule  string         `yaml:"schedule"`
	Workers   int            `yaml:"workers"`
	Target    Target         `yaml:"target"`
	Sources   Sources        `yaml:"sources"`
	Selection map[string]any `yaml:"selection"`
	Restore   Restore        `yaml:"restore"`
	Checks    Checks         `yaml:"checks"`
	Notify    Notify         `yaml:"notify"`
	Logging   Logging        `yaml:"logging"`
}

type Target struct {
	Type      string `yaml:"type"`
	Node      string `yaml:"node"`
	Bridge    string `yaml:"bridge"`
	API       API    `yaml:"api"`
	SSH       SSH    `yaml:"ssh"`
	VMIDRange [2]int `yaml:"vmid_range"`
}

type API struct {
	URL             string `yaml:"url"`
	TokenID         string `yaml:"token_id"`
	TokenSecretFile string `yaml:"token_secret_file"`
	TLSFingerprint  string `yaml:"tls_fingerprint"`
	CAFile          string `yaml:"ca_file"`
}

type SSH struct {
	Host           string `yaml:"host"`
	User           string `yaml:"user"`
	KeyFile        string `yaml:"key_file"`
	KnownHostsFile string `yaml:"known_hosts_file"`
	Port           int    `yaml:"port"`
}

type Sources struct {
	Storages []string `yaml:"storages"`
}

type Restore struct {
	Storage            string        `yaml:"storage"`
	SpaceMarginPercent int           `yaml:"space_margin_percent"`
	BootTimeout        time.Duration `yaml:"boot_timeout"`
	// ProbeTimeout bounds all checks inside one booted guest.
	ProbeTimeout   time.Duration `yaml:"probe_timeout"`
	Reproof        bool          `yaml:"reproof"`
	MaxRunDuration time.Duration `yaml:"max_run_duration"`
}

type Checks struct {
	// CommandTimeout bounds each command PBP runs inside a guest.
	CommandTimeout        time.Duration `yaml:"command_timeout"`
	IgnoreUnits           []string      `yaml:"ignore_units"`
	WindowsIgnoreServices []string      `yaml:"windows_ignore_services"`
	Profiles              []Profile     `yaml:"profiles"`
	Guests                []GuestChecks `yaml:"guests"`
}

type CheckSet struct {
	Services []string `yaml:"services"`
	Ports    []int    `yaml:"ports"`
	Commands []string `yaml:"commands"`
}

type Profile struct {
	Name     string `yaml:"name"`
	Unit     string `yaml:"unit"`
	CheckSet `yaml:",inline"`
}

type GuestMatch struct {
	VMID int    `yaml:"vmid"`
	Name string `yaml:"name"`
	Tag  string `yaml:"tag"`
}

type GuestChecks struct {
	Match    GuestMatch `yaml:"match"`
	CheckSet `yaml:",inline"`
}

type Notify struct {
	When string `yaml:"when"`
}

type Logging struct {
	Dir        string `yaml:"dir"`
	RetainRuns int    `yaml:"retain_runs"`
}

// defaults returns the config that absent keys fall back to. Decoding YAML on
// top of it means only keys that are present override a default, so an
// explicit zero is kept (and validated) rather than replaced. The ignore
// lists are defaulted after decoding instead (see orDefault).
func defaults() Config {
	return Config{
		Workers: 1,
		Target:  Target{Type: "existing-node", SSH: SSH{User: "root", Port: 22}},
		Restore: Restore{SpaceMarginPercent: 10, BootTimeout: 5 * time.Minute, ProbeTimeout: 5 * time.Minute},
		Checks:  Checks{CommandTimeout: time.Minute},
		Notify:  Notify{When: "always"},
		Logging: Logging{Dir: "/var/log/pbp", RetainRuns: 30},
	}
}

// Load reads the YAML file at path, applies defaults and validates it.
func Load(path string) (Config, error) {
	// #nosec G304 -- path is the operator-supplied --config flag, $PBP_CONFIG or the default; reading it is the function's purpose
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if err := unitlessDurations(data); err != nil {
		return Config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
	c := defaults()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		if errors.Is(err, io.EOF) {
			return Config{}, fmt.Errorf("config %s is empty", path)
		}
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	c.Checks.IgnoreUnits = orDefault(c.Checks.IgnoreUnits, DefaultIgnoreUnits)
	c.Checks.WindowsIgnoreServices = orDefault(c.Checks.WindowsIgnoreServices, DefaultWindowsIgnore)
	if err := c.validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return c, nil
}

// orDefault returns list, or def() when list is nil: the key is absent or
// null. Only an explicit [] (empty, not nil) means "none".
func orDefault(list []string, def func() []string) []string {
	if list == nil {
		return def()
	}
	return list
}

func (c Config) validate() error {
	var errs []error
	bad := func(key, format string, a ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", key, fmt.Sprintf(format, a...)))
	}
	if _, err := cron.ParseStandard(c.Schedule); err != nil {
		bad("schedule", "invalid cron expression %q: %v", c.Schedule, err)
	}
	if c.Workers < 1 {
		bad("workers", "must be >= 1, got %d", c.Workers)
	}
	t := c.Target
	if t.Type != "existing-node" {
		bad("target.type", "must be \"existing-node\", got %q", t.Type)
	}
	if t.Node == "" {
		bad("target.node", "required")
	}
	if t.Bridge == "" {
		bad("target.bridge", "required")
	}
	if lo, hi := t.VMIDRange[0], t.VMIDRange[1]; lo < 100 || hi < lo || hi > maxVMID {
		bad("target.vmid_range", "need 100 <= lo <= hi <= %d, got [%d, %d]", maxVMID, lo, hi)
	}
	if t.API.TLSFingerprint == "" && t.API.CAFile == "" {
		bad("target.api.tls_fingerprint", "set tls_fingerprint or target.api.ca_file (skip-verify is not supported)")
	}
	if t.SSH.KnownHostsFile == "" {
		bad("target.ssh.known_hosts_file", "required")
	}
	if len(c.Sources.Storages) == 0 {
		bad("sources.storages", "at least one storage is required")
	}
	if c.Restore.Storage == "" {
		bad("restore.storage", "required")
	}
	r := c.Restore
	if r.SpaceMarginPercent < 0 || r.SpaceMarginPercent > 100 {
		bad("restore.space_margin_percent", "must be 0..100, got %d", r.SpaceMarginPercent)
	}
	duration(bad, "restore.boot_timeout", r.BootTimeout, 30*time.Second)
	duration(bad, "restore.probe_timeout", r.ProbeTimeout, 30*time.Second)
	duration(bad, "checks.command_timeout", c.Checks.CommandTimeout, time.Second)
	if r.MaxRunDuration != 0 {
		duration(bad, "restore.max_run_duration", r.MaxRunDuration, time.Second)
	}
	if c.Logging.RetainRuns < 1 {
		bad("logging.retain_runs", "must be >= 1, got %d", c.Logging.RetainRuns)
	}
	if c.Notify.When != "always" && c.Notify.When != "problems" {
		bad("notify.when", "must be \"always\" or \"problems\", got %q", c.Notify.When)
	}
	c.validateChecks(bad)
	return errors.Join(errs...)
}

// reporter records one validation error against a config key path.
type reporter func(key, format string, a ...any)

// maxVMID is the largest VMID Proxmox accepts.
const maxVMID = 999999999

// validateChecks validates the user profiles and per-guest checks. A profile
// or guest with no checks is valid: a profile then only requires its unit to
// be active.
func (c Config) validateChecks(bad reporter) {
	seen := map[string]bool{}
	for i, p := range c.Checks.Profiles {
		key := fmt.Sprintf("checks.profiles[%d]", i)
		switch {
		case p.Name == "":
			bad(key+".name", "required")
		case seen[p.Name]:
			bad(key+".name", "duplicate profile name %q", p.Name)
		default:
			seen[p.Name] = true
		}
		if p.Unit == "" {
			bad(key+".unit", "required")
		}
		p.validate(bad, key)
	}
	for i, g := range c.Checks.Guests {
		key := fmt.Sprintf("checks.guests[%d]", i)
		if m := g.Match; m.VMID == 0 && m.Name == "" && m.Tag == "" {
			bad(key+".match", "set at least one of vmid, name or tag")
		} else if m.VMID < 0 || m.VMID > maxVMID {
			bad(key+".match.vmid", "must be 1..%d, got %d", maxVMID, m.VMID)
		}
		g.validate(bad, key)
	}
}

// validate checks that every port is a valid TCP port, naming key in errors.
func (cs CheckSet) validate(bad reporter, key string) {
	for j, port := range cs.Ports {
		if port < 1 || port > 65535 {
			bad(fmt.Sprintf("%s.ports[%d]", key, j), "must be 1..65535, got %d", port)
		}
	}
}

// duration validates a duration key against a minimum (at least 1s: a
// smaller value is almost certainly a unit typo).
func duration(bad reporter, key string, d, min time.Duration) {
	if d < min {
		bad(key, "must be >= %s, got %s (check the unit)", min, d)
	}
}

// durationKeys are the config keys holding durations, as dotted paths.
var durationKeys = []string{"restore.boot_timeout", "restore.probe_timeout", "restore.max_run_duration", "checks.command_timeout"}

// unitlessDurations names every duration key written as a bare number
// ("300"), which has no unit. The YAML decoder rejects these with only a
// line number; this names the key.
func unitlessDurations(data []byte) error {
	var root yaml.Node
	if yaml.Unmarshal(data, &root) != nil || len(root.Content) == 0 {
		return nil // the real decode reports the syntax error
	}
	var errs []error
	for _, key := range durationKeys {
		n := root.Content[0]
		for _, part := range strings.Split(key, ".") {
			n = mapValue(n, part)
		}
		if n != nil && n.Kind == yaml.ScalarNode && (n.Tag == "!!int" || n.Tag == "!!float") {
			errs = append(errs, fmt.Errorf("%s: %s has no unit; durations need one, e.g. 5m or 90s", key, n.Value))
		}
	}
	return errors.Join(errs...)
}

// mapValue returns the value node of key in mapping n, or nil.
func mapValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// EffectiveProfiles returns the built-in profiles with same-Name user profiles
// replacing them in place, followed by the remaining user profiles.
func (c Checks) EffectiveProfiles() []Profile {
	out := BuiltinProfiles()
	idx := map[string]int{}
	for i, p := range out {
		idx[p.Name] = i
	}
	for _, p := range c.Profiles {
		if i, ok := idx[p.Name]; ok {
			out[i] = p
		} else {
			idx[p.Name] = len(out)
			out = append(out, p)
		}
	}
	return out
}

// ResolvePath picks the config path: flag, then $PBP_CONFIG, then the default.
func ResolvePath(flag string) string {
	if flag != "" {
		return flag
	}
	if env := os.Getenv("PBP_CONFIG"); env != "" {
		return env
	}
	return "/etc/pbp/config.yaml"
}
