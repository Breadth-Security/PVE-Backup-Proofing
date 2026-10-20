package nodesetup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/node"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/pve"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/target"
)

// hostname is a seam for tests.
var hostname = os.Hostname

const ipv6PostUp = "post-up echo 1 > /proc/sys/net/ipv6/conf/%s/disable_ipv6"

// BridgeStanza is the /etc/network/interfaces stanza for an isolated Linux
// bridge, exactly as README "Create the isolated bridge" shows it. Setup
// only prints it; it never edits networking.
func BridgeStanza(bridge string) string {
	return "auto " + bridge + "\n" +
		"iface " + bridge + " inet manual\n" +
		"        bridge-ports none\n" +
		"        bridge-stp off\n" +
		"        bridge-fd 0\n" +
		"        " + fmt.Sprintf(ipv6PostUp, bridge) + "\n"
}

const applyHint = "apply with ifreload -a, then run pbp node setup again"

func missingBridgeError(node, bridge string) error {
	return fmt.Errorf("the bridge %s does not exist on this node. Create it, then run pbp node setup again.\n\n"+
		"In the web UI: Datacenter → %s → System → Network → Create → Linux Bridge (no ports, no IPv4/IPv6).\n"+
		"A bridge created in the UI needs the post-up line added to its stanza in /etc/network/interfaces.\n\n"+
		"Or add this to /etc/network/interfaces:\n\n%s\n%s", bridge, node, BridgeStanza(bridge), applyHint)
}

// isMissing reports whether err says the thing asked for does not exist
// (an HTTP 404, or pvesh's "not found" text).
func isMissing(err error) bool {
	var ae *pve.APIError
	if errors.As(err, &ae) && ae.Status == 404 {
		return true
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "not found") || strings.Contains(m, "does not exist") || strings.Contains(m, "no such")
}

// Check runs the read-only phase-1 checks before setup changes anything and
// returns the PVE node name (the short hostname, confirmed present in
// /nodes). Each error is one plain sentence plus what to do.
func Check(ctx context.Context, env Env, api *pve.Client, sys node.System, a nodeproto.SetupArgs, helperVersion string) (string, error) {
	v, err := api.Version(ctx)
	if err != nil {
		return "", fmt.Errorf("cannot read the Proxmox VE version (%v): run pbp node setup as root on a Proxmox node", err)
	}
	major, merr := strconv.Atoi(strings.SplitN(strings.TrimSpace(v), ".", 2)[0])
	if merr != nil || major < 9 {
		return "", fmt.Errorf("this node runs Proxmox VE %q and PBP needs Proxmox VE 9 or later: upgrade the node first", v)
	}

	if a.DaemonVersion == "" {
		return "", errors.New("the setup line is missing --daemon-version; copy it again from pbp setup")
	}
	if !nodeproto.Compatible(helperVersion, a.DaemonVersion) {
		return "", fmt.Errorf("this node has pbp %s, the PBP host runs %s: install pbp %s on the node first", helperVersion, a.DaemonVersion, a.DaemonVersion)
	}
	if _, err := nodeproto.ParsePubKey(a.PubKey); err != nil {
		return "", fmt.Errorf("the setup line's public key is not usable (%v): copy the setup line again from pbp setup", err)
	}

	host, err := hostname()
	if err != nil {
		return "", fmt.Errorf("cannot read this node's hostname (%v): fix the hostname and run pbp node setup again", err)
	}
	name, _, _ := strings.Cut(host, ".")
	nodes, err := api.Nodes(ctx)
	if err != nil {
		return "", fmt.Errorf("cannot list the cluster nodes (%v): run pbp node setup as root on a Proxmox node", err)
	}
	if !slices.Contains(nodes, name) {
		return "", fmt.Errorf("%q is not in the cluster node list (%s): run pbp node setup on a node that is part of the cluster, with its hostname unchanged", name, strings.Join(nodes, ", "))
	}

	bridge := a.Bridge
	iface, err := api.NetIface(ctx, name, bridge)
	if err != nil {
		if isMissing(err) {
			return "", missingBridgeError(name, bridge)
		}
		return "", fmt.Errorf("cannot read the bridge %s (%v): check the node's network configuration", bridge, err)
	}
	if why := target.NotIsolated(iface); why != "" {
		return "", fmt.Errorf("the bridge %s %s: use a bridge with no ports and no IP address, or fix this one in Datacenter → %s → System → Network", bridge, why, name)
	}
	st, err := sys.BridgeState(ctx, bridge)
	if err != nil {
		return "", fmt.Errorf("cannot read the live state of the bridge %s (%v): check the node's network configuration", bridge, err)
	}
	problem, unverifiable := target.LiveIsolationProblem(st, bridge, iface.Type == "OVSBridge", a.VMIDRange[0], a.VMIDRange[1])
	if problem != "" {
		msg := fmt.Sprintf("the bridge %s %s", bridge, problem)
		if !unverifiable && st.DisableIPv6 != "1" {
			msg += fmt.Sprintf(": add the line %q to its stanza in /etc/network/interfaces, %s", fmt.Sprintf(ipv6PostUp, bridge), applyHint)
		} else {
			msg += ": fix the bridge, then run pbp node setup again"
		}
		return "", errors.New(msg)
	}

	guests, err := api.Guests(ctx)
	if err != nil {
		return "", fmt.Errorf("cannot list the guests (%v): run pbp node setup as root on a Proxmox node", err)
	}
	if foreign := target.ForeignInRange(guests, a.VMIDRange[0], a.VMIDRange[1]); len(foreign) > 0 {
		return "", fmt.Errorf("guests not owned by PBP sit in the VMID range %d-%d (%s): move them out of the range or choose another range with pbp setup",
			a.VMIDRange[0], a.VMIDRange[1], strings.Join(foreign, ", "))
	}

	out, stderr, exit, err := env.Run(ctx, "/usr/sbin/sshd", []string{"-T"}, nil)
	if err != nil || exit != 0 {
		return "", fmt.Errorf("cannot read the sshd configuration (sshd -T: %v %s): fix sshd, then run pbp node setup again", err, strings.TrimSpace(stderr))
	}
	usePAM := false
	for _, l := range strings.Split(out, "\n") {
		if strings.EqualFold(strings.Join(strings.Fields(l), " "), "usepam yes") {
			usePAM = true
		}
	}
	if !usePAM {
		return "", errors.New("sshd has UsePAM off, so the pbp account cannot log in over SSH: set UsePAM yes in /etc/ssh/sshd_config and reload sshd (setup never edits it)")
	}

	for _, s := range a.Storages {
		if _, err := api.StorageStatus(ctx, name, s); err != nil {
			return "", fmt.Errorf("storage %s cannot be read on this node (%v): check the storage name or choose another with pbp setup", s, err)
		}
	}
	return name, nil
}
