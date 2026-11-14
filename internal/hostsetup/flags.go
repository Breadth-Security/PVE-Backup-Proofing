package hostsetup

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/config"
)

// Flags are setup's answers given as flags, for an unattended run (issue
// #131). An empty field takes the default the question offers.
type Flags struct {
	Address, VMIDRange, Bridge, RestoreStorage, Storages, Schedule, TZ string
}

// FromFlags turns f into Answers through the same checks as the questions,
// with the same defaults. defaultAddr is the address offered on the node
// itself ("" elsewhere, where --address is required); --storages is always
// required. An error names the flag.
func FromFlags(f Flags, l Layout, defaultTZ, defaultAddr string) (Answers, error) {
	bad := func(flag string, err error) (Answers, error) {
		return Answers{}, fmt.Errorf("--%s: %w", flag, err)
	}
	or := func(v, def string) string {
		if v == "" {
			return def
		}
		return v
	}
	var a Answers
	a.Address = or(f.Address, defaultAddr)
	if a.Address == "" {
		return Answers{}, errors.New("--address is required (the node's hostname or IP, as this host reaches its SSH and API)")
	}
	if err := validAddress(a.Address); err != nil {
		return bad("address", err)
	}
	var err error
	if a.VMIDRange, err = parseRange(or(f.VMIDRange, defaultRange)); err != nil {
		return bad("vmid-range", err)
	}
	a.Bridge = or(f.Bridge, defaultBridge)
	if err := checkBridge(a.Bridge); err != nil {
		return bad("bridge", err)
	}
	a.RestoreStorage = or(f.RestoreStorage, defaultRestore)
	if err := validStorage(a.RestoreStorage); err != nil {
		return bad("restore-storage", err)
	}
	if strings.TrimSpace(f.Storages) == "" {
		return Answers{}, errors.New("--storages is required (the PBS storages to prove, comma-separated)")
	}
	if a.PBSStorages, err = parseStorages(f.Storages); err != nil {
		return bad("storages", err)
	}
	if a.Schedule, err = parseSchedule(or(f.Schedule, defaultSchedule)); err != nil {
		return bad("schedule", err)
	}
	if l.Deb {
		if f.TZ != "" {
			return bad("tz", errors.New("only for the Docker layout: a .deb host's schedule uses the host's time zone"))
		}
		return a, nil
	}
	a.TZ = or(f.TZ, defaultTZ)
	if err := checkTZ(a.TZ); err != nil {
		return bad("tz", err)
	}
	return a, nil
}

// Conflicts lists each flag in f that is set and differs from cfg, as
// "--bridge vmbr77 differs from target.bridge vmbr99 in config.yaml". The
// time zone is not compared: it lives in the Docker .env, not config.yaml.
// A flag that does not parse is left to FromFlags.
func Conflicts(cfg config.Config, f Flags) []string {
	var out []string
	diff := func(flag, given, key, have string) {
		if given != "" && given != have {
			out = append(out, fmt.Sprintf("--%s %s differs from %s %s in config.yaml", flag, given, key, have))
		}
	}
	diff("address", f.Address, "target.ssh.host", cfg.Target.SSH.Host)
	if f.VMIDRange != "" {
		have := fmt.Sprintf("%d-%d", cfg.Target.VMIDRange[0], cfg.Target.VMIDRange[1])
		if r, err := parseRange(f.VMIDRange); err != nil || r != cfg.Target.VMIDRange {
			diff("vmid-range", f.VMIDRange, "target.vmid_range", have)
		}
	}
	diff("bridge", f.Bridge, "target.bridge", cfg.Target.Bridge)
	diff("restore-storage", f.RestoreStorage, "restore.storage", cfg.Restore.Storage)
	if f.Storages != "" {
		if sts, err := parseStorages(f.Storages); err != nil || !slices.Equal(sts, cfg.Sources.Storages) {
			diff("storages", f.Storages, "sources.storages", strings.Join(cfg.Sources.Storages, ","))
		}
	}
	if f.Schedule != "" {
		if s, err := parseSchedule(f.Schedule); err != nil || s != cfg.Schedule {
			diff("schedule", fmt.Sprintf("%q", f.Schedule), "schedule", fmt.Sprintf("%q", cfg.Schedule))
		}
	}
	return out
}
