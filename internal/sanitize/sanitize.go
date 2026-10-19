package sanitize

import (
	"regexp"
	"sort"
	"strings"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
)

// Plan is the set of config changes that make a restored guest safe.
type Plan struct {
	Set     map[string]string // keys to write (always includes every net key)
	Delete  []string          // keys to remove, sorted
	Removed []model.MissingPiece
	Unsafe  []string // reasons the guest cannot be made safe by config edits
}

var (
	reNet      = regexp.MustCompile(`^net\d+$`)
	rePCIEtc   = regexp.MustCompile(`^(hostpci|usb|parallel|serial)\d+$`)
	reVirtiofs = regexp.MustCompile(`^virtiofs\d+$`)
	reDev      = regexp.MustCompile(`^dev\d+$`)
	reMP       = regexp.MustCompile(`^mp\d+$`)
)

// Sanitize computes the plan for a restored guest config. Disk keys are kept.
//
// HA resources and replication jobs need no handling: both are keyed by VMID
// and the restore uses a fresh VMID, so none can exist for the restored copy
// (design section 6 step 7 is satisfied by construction).
func Sanitize(kind model.GuestKind, restored map[string]string, bridge string) Plan {
	p := Plan{Set: map[string]string{"onboot": "0", "tags": "pbp"}}
	keys := make([]string, 0, len(restored))
	for k := range restored {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		v := restored[k]
		switch {
		case reNet.MatchString(k):
			p.Set[k] = sanitizeNet(v, bridge)
		case k == "startup", k == "hookscript":
			p.Delete = append(p.Delete, k)
		case kind == model.KindQEMU && k == "args":
			p.Unsafe = append(p.Unsafe, "args")
		case kind == model.KindQEMU && rePCIEtc.MatchString(k):
			if strings.TrimSpace(v) != "socket" {
				p.remove(k, model.MissingPiece{Kind: "passthrough", Key: k})
			}
		case kind == model.KindQEMU && reVirtiofs.MatchString(k):
			p.remove(k, model.MissingPiece{Kind: "passthrough", Key: k})
		case kind == model.KindLXC && reDev.MatchString(k):
			p.remove(k, model.MissingPiece{Kind: "passthrough", Key: k})
		case kind == model.KindLXC && reMP.MatchString(k):
			if strings.HasPrefix(firstField(v), "/") {
				path, _ := optValue(v, "mp")
				p.remove(k, model.MissingPiece{Kind: "bind-mount", Key: k, Path: path})
			}
		case kind == model.KindLXC && k == "lxc":
			p.Unsafe = append(p.Unsafe, "raw lxc.* config")
		}
	}
	sort.Strings(p.Delete)
	return p
}

func (p *Plan) remove(key string, piece model.MissingPiece) {
	p.Delete = append(p.Delete, key)
	p.Removed = append(p.Removed, piece)
}

func firstField(s string) string {
	f, _, _ := strings.Cut(s, ",")
	return f
}

// sanitizeNet forces the isolated bridge and drops VLAN/trunk/rate options,
// preserving the order of everything else.
func sanitizeNet(v, bridge string) string {
	var out []string
	seenBridge := false
	for _, f := range strings.Split(v, ",") {
		k, _, _ := strings.Cut(f, "=")
		// A qemu first field is "<model>=MAC", never one of these keys.
		switch k {
		case "tag", "trunks", "rate":
			continue
		case "bridge":
			if seenBridge {
				continue
			}
			seenBridge = true
			out = append(out, "bridge="+bridge)
			continue
		}
		out = append(out, f)
	}
	if !seenBridge {
		out = append(out, "bridge="+bridge)
	}
	return strings.Join(out, ",")
}
