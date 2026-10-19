// Package config loads, defaults and validates the PBP YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
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
	Reproof            bool          `yaml:"reproof"`
	MaxRunDuration     time.Duration `yaml:"max_run_duration"`
}

type Checks struct {
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
// explicit zero or empty list is kept (and validated) rather than replaced.
func defaults() Config {
	return Config{
		Workers: 1,
		Target:  Target{Type: "existing-node", SSH: SSH{User: "root", Port: 22}},
		Restore: Restore{SpaceMarginPercent: 10, BootTimeout: 5 * time.Minute},
		Checks: Checks{
			IgnoreUnits:           DefaultIgnoreUnits(),
			WindowsIgnoreServices: DefaultWindowsIgnore(),
		},
		Notify:  Notify{When: "always"},
		Logging: Logging{Dir: "/var/log/pbp", RetainRuns: 30},
	}
}

// Load reads the YAML file at path, applies defaults and validates it.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	c := defaults()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return c, nil
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
	if lo, hi := t.VMIDRange[0], t.VMIDRange[1]; lo < 100 || hi < lo {
		bad("target.vmid_range", "need 100 <= lo <= hi, got [%d, %d]", lo, hi)
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
	if c.Notify.When != "always" && c.Notify.When != "problems" {
		bad("notify.when", "must be \"always\" or \"problems\", got %q", c.Notify.When)
	}
	return errors.Join(errs...)
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
