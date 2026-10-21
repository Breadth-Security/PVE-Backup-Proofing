package config

import (
	"regexp"
	"strconv"
	"strings"
)

// Selection scopes which guests a run proofs.
type Selection struct {
	Exclude Exclude `yaml:"exclude"`
}

// Exclude lists guests never to proof (issue #32).
type Exclude struct {
	// Guests are VMIDs ("104", either kind), labels ("VM104", "CT109") or
	// inclusive VMID ranges ("900-999").
	Guests []string `yaml:"guests"`
	// Tags exclude a guest whose live config or backed-up config carries
	// any of them (compared case-insensitively, as PVE does).
	Tags []string `yaml:"tags"`
}

var (
	reGuestRule = regexp.MustCompile(`^(?i)(vm|ct)?([0-9]+)$`)
	reRangeRule = regexp.MustCompile(`^([0-9]+)-([0-9]+)$`)
	// PVE's tag format: letters, digits and - _ + . , starting with a
	// letter, digit or underscore.
	reTag = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_\-+.]*$`)
)

// guestRule is one parsed Guests entry: kind "" matches both kinds.
type guestRule struct {
	kind   string
	lo, hi int
}

func parseGuestRule(s string) (guestRule, bool) {
	if m := reRangeRule.FindStringSubmatch(s); m != nil {
		lo, err1 := strconv.Atoi(m[1])
		hi, err2 := strconv.Atoi(m[2])
		return guestRule{lo: lo, hi: hi}, err1 == nil && err2 == nil && lo >= 1 && lo <= hi
	}
	m := reGuestRule.FindStringSubmatch(s)
	if m == nil {
		return guestRule{}, false
	}
	id, err := strconv.Atoi(m[2])
	r := guestRule{lo: id, hi: id}
	switch strings.ToLower(m[1]) {
	case "vm":
		r.kind = "qemu"
	case "ct":
		r.kind = "lxc"
	}
	return r, err == nil && id >= 1
}

func (e Exclude) validate(bad reporter) {
	for _, g := range e.Guests {
		if _, ok := parseGuestRule(g); !ok {
			bad("selection.exclude.guests", "%q is not a VMID (104), a label (VM104, CT104) or a range (900-999)", g)
		}
	}
	for _, t := range e.Tags {
		if !reTag.MatchString(t) {
			bad("selection.exclude.tags", "%q is not a valid Proxmox tag", t)
		}
	}
}

// ExcludedBy returns the rule that excludes the guest of kind ("qemu" or
// "lxc") and vmid carrying tags, as "guests: <entry>" or "tag <tag>", or ""
// if none does. The config must have been validated.
func (e Exclude) ExcludedBy(kind string, vmid int, tags []string) string {
	for _, g := range e.Guests {
		r, ok := parseGuestRule(g)
		if ok && (r.kind == "" || r.kind == kind) && vmid >= r.lo && vmid <= r.hi {
			return "guests: " + g
		}
	}
	for _, t := range e.Tags {
		for _, have := range tags {
			if strings.EqualFold(t, have) {
				return "tag " + t
			}
		}
	}
	return ""
}
