package target

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/pve"
)

// reGuestPort matches the interfaces PVE plugs into a bridge for a guest:
// a VM's tap, a container's veth, the firewall bridge's fwpr/fwln pair
// (Linux bridge), and the firewall's OVS internal port fwln<vmid>o<n>
// (PVE::Network compute_fwbr_names). The capture is the guest's VMID.
var reGuestPort = regexp.MustCompile(`^(?:tap(\d+)i|veth(\d+)i|fwpr(\d+)p|fwln(\d+)[io])\d+$`)

// NotIsolated explains why iface is not an isolated bridge ("" if it is).
func NotIsolated(i pve.NetIface) string {
	switch {
	case i.Type != "bridge" && i.Type != "OVSBridge":
		return fmt.Sprintf("is a %q, not a bridge", i.Type)
	case i.BridgePorts != "" && i.BridgePorts != "none":
		return "has ports " + i.BridgePorts
	case i.OVSPorts != "":
		return "has ports " + i.OVSPorts
	case i.Address != "" || i.CIDR != "" || i.Address6 != "" || i.CIDR6 != "":
		return "has an IP address"
	}
	return ""
}

// ForeignInRange returns the keys of the guests in [lo, hi] that do not
// carry the PBP owner tag.
func ForeignInRange(guests []pve.GuestRef, lo, hi int) []string {
	var foreign []string
	for _, g := range guests {
		if g.Key.VMID >= lo && g.Key.VMID <= hi && !slices.Contains(g.Tags, OwnerTag) {
			foreign = append(foreign, g.Key.String())
		}
	}
	return foreign
}

// LiveIsolationProblem judges the bridge's live state, as the helper's
// bridge request reports it, and returns what is wrong ("" if nothing).
// IPv6 must be disabled on it (otherwise the kernel gives it a link-local
// address, through which a proof guest could reach the node's sshd and
// pveproxy, and a restored router's RAs could give the node a SLAAC address
// and route), it must hold no address at all, and its only members may be
// interfaces of guests in [lo, hi]. The helper reports the bridge from its
// own config: it must be this bridge, and of the kind (Linux or OVS) the API
// says it is (apiOVS). unverifiable is true when the state cannot be
// trusted or read, as opposed to being found wrong.
func LiveIsolationProblem(st nodeproto.BridgeState, bridge string, apiOVS bool, lo, hi int) (problem string, unverifiable bool) {
	if st.Name != bridge {
		return fmt.Sprintf("cannot be verified: the node helper checks bridge %q instead", st.Name), true
	}
	if st.OVS != apiOVS {
		return fmt.Sprintf("cannot be verified: the API says OVS bridge = %v, the node says OVS bridge = %v", apiOVS, st.OVS), true
	}
	if st.DisableIPv6 != "1" {
		return fmt.Sprintf("has IPv6 enabled (disable_ipv6 = %q); it gets a link-local address the node can be reached on", st.DisableIPv6), false
	}

	var addrs []struct {
		AddrInfo []struct {
			Local     string `json:"local"`
			PrefixLen int    `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal(st.Addrs, &addrs); err != nil {
		return fmt.Sprintf("cannot be verified: parse ip addr output: %v", err), true
	}
	for _, a := range addrs {
		if len(a.AddrInfo) > 0 {
			ai := a.AddrInfo[0]
			return fmt.Sprintf("has address %s/%d on the node", ai.Local, ai.PrefixLen), false
		}
	}

	// Members are the kernel bridge's, or the OVS bridge's ports.
	var foreign []string
	for _, m := range st.Members {
		if !proofPort(m, lo, hi) {
			foreign = append(foreign, m)
		}
	}
	if len(foreign) > 0 {
		return "has members that are not proof-guest interfaces: " + strings.Join(foreign, ", "), false
	}
	return "", false
}

// proofPort reports whether a bridge member is a guest interface of a VMID
// in [lo, hi].
func proofPort(name string, lo, hi int) bool {
	m := reGuestPort.FindStringSubmatch(name)
	if m == nil {
		return false
	}
	for _, g := range m[1:] {
		if g != "" {
			v, err := strconv.Atoi(g)
			return err == nil && v >= lo && v <= hi
		}
	}
	return false
}
