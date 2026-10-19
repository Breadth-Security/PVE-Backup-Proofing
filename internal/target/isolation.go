package target

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"git.breadthanalytics.com/tdiana/pbv/internal/pve"
	"git.breadthanalytics.com/tdiana/pbv/internal/sshx"
)

// reGuestPort matches the interfaces PVE plugs into a bridge for a guest:
// a VM's tap, a container's veth, the firewall bridge's fwpr/fwln pair
// (Linux bridge), and the firewall's OVS internal port fwln<vmid>o<n>
// (PVE::Network compute_fwbr_names). The capture is the guest's VMID.
var reGuestPort = regexp.MustCompile(`^(?:tap(\d+)i|veth(\d+)i|fwpr(\d+)p|fwln(\d+)[io])\d+$`)

// checkLiveIsolation verifies the bridge's live state on the node, which the
// API config does not show: IPv6 must be disabled on it (otherwise the
// kernel gives it a link-local address, through which a proof guest could
// reach the node's sshd and pveproxy, and a restored router's RAs could give
// the node a SLAAC address and route), it must hold no address at all, and
// its only members may be interfaces of guests in the reserved range.
//
// A state that cannot be read is reported as ErrNotIsolated (isolation not
// verified); an SSH transport failure is returned as is.
func (n *ExistingNode) checkLiveIsolation(ctx context.Context, iface pve.NetIface) error {
	notIso := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s %s", ErrNotIsolated, n.bridge, fmt.Sprintf(format, a...))
	}
	run := func(cmd string) (string, error) {
		res, err := n.ssh.Run(ctx, cmd, nil)
		if err != nil {
			return "", fmt.Errorf("bridge %s: %s: %w", n.bridge, cmd, err)
		}
		if res.ExitCode != 0 {
			return "", notIso("cannot be verified: %s exited %d: %s", cmd, res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		return res.Stdout, nil
	}

	out, err := run("cat " + sshx.Quote("/proc/sys/net/ipv6/conf/"+n.bridge+"/disable_ipv6"))
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "1" {
		return notIso("has IPv6 enabled (disable_ipv6 = %q); it gets a link-local address the node can be reached on", strings.TrimSpace(out))
	}

	out, err = run("ip -j addr show dev " + sshx.Quote(n.bridge))
	if err != nil {
		return err
	}
	var addrs []struct {
		AddrInfo []struct {
			Local     string `json:"local"`
			PrefixLen int    `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal([]byte(out), &addrs); err != nil {
		return notIso("cannot be verified: parse ip addr output: %v", err)
	}
	for _, a := range addrs {
		if len(a.AddrInfo) > 0 {
			ai := a.AddrInfo[0]
			return notIso("has address %s/%d on the node", ai.Local, ai.PrefixLen)
		}
	}

	var members []string
	if iface.Type == "OVSBridge" {
		// OVS ports are not kernel bridge members; ask OVS.
		out, err = run("ovs-vsctl list-ports " + sshx.Quote(n.bridge))
		if err != nil {
			return err
		}
		members = strings.Fields(out)
	} else {
		out, err = run("ip -j link show master " + sshx.Quote(n.bridge))
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) != "" {
			var links []struct {
				IfName string `json:"ifname"`
			}
			if err := json.Unmarshal([]byte(out), &links); err != nil {
				return notIso("cannot be verified: parse ip link output: %v", err)
			}
			for _, l := range links {
				members = append(members, l.IfName)
			}
		}
	}
	var foreign []string
	for _, m := range members {
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
