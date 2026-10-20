package target

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/pve"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sshx"
)

// reGuestPort matches the interfaces PVE plugs into a bridge for a guest:
// a VM's tap, a container's veth, the firewall bridge's fwpr/fwln pair
// (Linux bridge), and the firewall's OVS internal port fwln<vmid>o<n>
// (PVE::Network compute_fwbr_names). The capture is the guest's VMID.
var reGuestPort = regexp.MustCompile(`^(?:tap(\d+)i|veth(\d+)i|fwpr(\d+)p|fwln(\d+)[io])\d+$`)

// checkLiveIsolation verifies the bridge's live state on the node, which the
// API config does not show, as the helper's bridge request reports it: IPv6
// must be disabled on it (otherwise the kernel gives it a link-local
// address, through which a proof guest could reach the node's sshd and
// pveproxy, and a restored router's RAs could give the node a SLAAC address
// and route), it must hold no address at all, and its only members may be
// interfaces of guests in the reserved range. The helper reports the bridge
// from its own config: it must be this bridge, and of the kind (Linux or
// OVS) the API says it is.
//
// A state the helper cannot read, or replies with unusably, is reported as
// ErrNotIsolated (isolation not verified); an SSH transport failure or a
// timeout is returned as is.
func (n *ExistingNode) checkLiveIsolation(ctx context.Context, iface pve.NetIface) error {
	notIso := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s %s", ErrNotIsolated, n.bridge, fmt.Sprintf(format, a...))
	}
	st, err := n.helper.Bridge(ctx)
	if err != nil {
		if errors.Is(err, sshx.ErrRefused) || errors.Is(err, sshx.ErrHelper) {
			return notIso("cannot be verified: %v", err)
		}
		return fmt.Errorf("bridge %s: %w", n.bridge, err)
	}
	if st.Name != n.bridge {
		return notIso("cannot be verified: the node helper checks bridge %q instead", st.Name)
	}
	if apiOVS := iface.Type == "OVSBridge"; st.OVS != apiOVS {
		return notIso("cannot be verified: the API says OVS bridge = %v, the node says OVS bridge = %v", apiOVS, st.OVS)
	}

	if st.DisableIPv6 != "1" {
		return notIso("has IPv6 enabled (disable_ipv6 = %q); it gets a link-local address the node can be reached on", st.DisableIPv6)
	}

	var addrs []struct {
		AddrInfo []struct {
			Local     string `json:"local"`
			PrefixLen int    `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal(st.Addrs, &addrs); err != nil {
		return notIso("cannot be verified: parse ip addr output: %v", err)
	}
	for _, a := range addrs {
		if len(a.AddrInfo) > 0 {
			ai := a.AddrInfo[0]
			return notIso("has address %s/%d on the node", ai.Local, ai.PrefixLen)
		}
	}

	// Members are the kernel bridge's, or the OVS bridge's ports.
	var foreign []string
	for _, m := range st.Members {
		if !n.proofPort(m) {
			foreign = append(foreign, m)
		}
	}
	if len(foreign) > 0 {
		return notIso("has members that are not proof-guest interfaces: %s", strings.Join(foreign, ", "))
	}
	return nil
}

// proofPort reports whether a bridge member is a guest interface of a VMID
// in the reserved range.
func (n *ExistingNode) proofPort(name string) bool {
	m := reGuestPort.FindStringSubmatch(name)
	if m == nil {
		return false
	}
	for _, g := range m[1:] {
		if g != "" {
			v, err := strconv.Atoi(g)
			return err == nil && n.inRange(v)
		}
	}
	return false
}
