package sanitize

import (
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/model"
)

// RawLine is one raw "lxc.*" line of a container config.
type RawLine struct{ Key, Value string }

// KeyDetail is a config key with what it names (a device path, a host
// device), for reasons and logs.
type KeyDetail struct{ Key, Detail string }

// keptDevices are the container devices a copy keeps (issue #141). PVE
// restores devN without a permission check but lets only root@pam remove
// it, so a device that gives the copy nothing of production's is left in
// place. Each is purely virtual, per container:
//   - /dev/net/tun: tun/tap interfaces in the container's own network
//     namespace, which PBP attaches only to its isolated bridge;
//   - /dev/fuse: filesystems the container's own processes serve;
//   - null, zero, full, random, urandom: every container has them anyway.
//
// Left out on purpose: /dev/loop* and /dev/loop-control (loop devices are
// shared with the host, so a copy could reach production's), /dev/kvm (it
// hands the copy hardware virtualisation; it may come on request), and any
// real hardware (GPUs, USB and serial devices, disks), which the copy must
// never share with production.
var keptDevices = map[string]bool{
	"/dev/net/tun": true,
	"/dev/fuse":    true,
	"/dev/null":    true,
	"/dev/zero":    true,
	"/dev/full":    true,
	"/dev/random":  true,
	"/dev/urandom": true,
}

// charDevices are the sources a device-only lxc.mount.entry may bind: an
// entry ending in "/" covers that directory and the paths under it, one
// ending in "*" is a name prefix (no "/" after it), and any other entry is
// exact. It lists what may pass, not what blocks: a bind of a block device
// (/dev/sdb1, /dev/disk/…, /dev/zvol/…, /dev/mapper/…) is data, and the
// copy would be proofed without it, so it ends ERROR as before, as does any
// device class not listed here.
var charDevices = []string{
	"/dev/tty*", "/dev/serial/", "/dev/dri/", "/dev/bus/usb/", "/dev/usb/",
	"/dev/net/tun", "/dev/fuse", "/dev/apex_*", "/dev/accel/", "/dev/video*",
	"/dev/snd/", "/dev/hidraw*", "/dev/input/", "/dev/nvidia*", "/dev/kfd",
	"/dev/ppp", "/dev/i2c-*", "/dev/gpiochip*",
}

// RawLXCLines returns a container config's raw lxc.* lines, as
// ParseConfigText gathers them under "lxc", in config order.
func RawLXCLines(cfg map[string]string) []RawLine {
	var out []RawLine
	for _, line := range strings.Split(cfg["lxc"], "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			out = append(out, RawLine{Key: strings.TrimSpace(k), Value: strings.TrimSpace(v)})
		}
	}
	return out
}

// DeviceOnly reports whether a raw line only grants or bind-mounts
// character devices: a cgroup devices.allow rule (on its own it mounts
// nothing), or an lxc.mount.entry binding a source from charDevices. PVE
// drops raw lines when PBP's token restores; for these, that only takes
// devices away from the copy. Anything else, including devices.deny
// (dropping it would grant more) and lines that cannot be parsed, is not.
func DeviceOnly(l RawLine) bool {
	switch l.Key {
	case "lxc.cgroup2.devices.allow", "lxc.cgroup.devices.allow":
		return true
	case "lxc.mount.entry":
		_, _, ok := deviceMount(l.Value)
		return ok
	}
	return false
}

// deviceMount parses a device-only lxc.mount.entry value into its cleaned
// source and the target as a path inside the container.
func deviceMount(v string) (source, target string, ok bool) {
	f := strings.Fields(v)
	if len(f) < 4 || f[2] != "none" {
		return "", "", false
	}
	opts := strings.Split(f[3], ",")
	if !slices.Contains(opts, "bind") && !slices.Contains(opts, "rbind") {
		return "", "", false
	}
	source = path.Clean(f[0])
	if !charDevice(source) {
		return "", "", false
	}
	return source, path.Clean("/" + f[1]), true
}

// charDevice reports whether a cleaned absolute path is on charDevices.
func charDevice(p string) bool {
	for _, e := range charDevices {
		switch {
		case strings.HasSuffix(e, "/"):
			if p == strings.TrimSuffix(e, "/") || strings.HasPrefix(p, e) {
				return true
			}
		case strings.HasSuffix(e, "*"):
			if rest, ok := strings.CutPrefix(p, strings.TrimSuffix(e, "*")); ok && !strings.Contains(rest, "/") {
				return true
			}
		case p == e:
			return true
		}
	}
	return false
}

// DevicePath returns the device path of a container devN value, written
// bare ("/dev/x,mode=0666") or as "path=/dev/x".
func DevicePath(v string) string {
	fields := strings.Split(v, ",")
	for _, f := range fields {
		if p, ok := strings.CutPrefix(strings.TrimSpace(f), "path="); ok {
			return p
		}
	}
	if first := strings.TrimSpace(fields[0]); !strings.Contains(first, "=") {
		return first
	}
	return ""
}

// KeptDevice reports whether a devN path is one the copy keeps. The match
// is exact: no prefix, glob or path cleaning.
func KeptDevice(p string) bool { return keptDevices[p] }

// KeptDevices lists a container's devN entries the copy keeps, in key
// order, with their paths.
func KeptDevices(cfg map[string]string) []KeyDetail {
	var out []KeyDetail
	for _, k := range indexedKeys(cfg, reDev) {
		if p := DevicePath(cfg[k]); KeptDevice(p) {
			out = append(out, KeyDetail{Key: k, Detail: p})
		}
	}
	return out
}

// droppedDevices returns the pieces PVE's restore leaves out when every raw
// line of a container is device-only, one per distinct line in config
// order: an allow rule as its Detail, a bind by its path inside the
// container (what services there log) with its host source as Detail when
// that differs. It returns nil when any raw line is not device-only: such
// a container is refused before restoring.
func droppedDevices(cfg map[string]string) []model.MissingPiece {
	lines := RawLXCLines(cfg)
	var out []model.MissingPiece
	for _, l := range lines {
		if !DeviceOnly(l) {
			return nil
		}
		p := model.MissingPiece{Kind: "passthrough", Key: l.Key, Detail: l.Value}
		if l.Key == "lxc.mount.entry" {
			src, target, _ := deviceMount(l.Value)
			p.Path, p.Detail = target, ""
			if src != target {
				p.Detail = src
			}
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// indexedKeys returns the keys of cfg that re matches, ordered by name and
// then by their numeric suffix (dev2 before dev10).
func indexedKeys(cfg map[string]string, re interface{ MatchString(string) bool }) []string {
	var keys []string
	for k := range cfg {
		if re.MatchString(k) {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		ni, ii := splitIndex(keys[i])
		nj, ij := splitIndex(keys[j])
		if ni != nj {
			return ni < nj
		}
		return ii < ij
	})
	return keys
}

// splitIndex splits "hostpci12" into "hostpci" and 12.
func splitIndex(k string) (string, int) {
	i := strings.IndexFunc(k, func(r rune) bool { return r >= '0' && r <= '9' })
	if i < 0 {
		return k, -1
	}
	n, _ := strconv.Atoi(k[i:])
	return k[:i], n
}
