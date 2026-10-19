// Package bridgetest answers the live bridge checks that target.ExistingNode
// runs over SSH, for tests.
package bridgetest

import "git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sshx"

// Commands are the live bridge-state commands the target runs for a bridge.
type Commands struct {
	IPv6, Addr, Master, OVS string
}

// For returns the commands for bridge.
func For(bridge string) Commands {
	q := sshx.Quote(bridge)
	return Commands{
		IPv6:   "cat " + sshx.Quote("/proc/sys/net/ipv6/conf/"+bridge+"/disable_ipv6"),
		Addr:   "ip -j addr show dev " + q,
		Master: "ip -j link show master " + q,
		OVS:    "ovs-vsctl list-ports " + q,
	}
}

// Isolated answers the live bridge checks as an isolated bridge with IPv6
// disabled, no addresses and no members; overrides replace answers. Any
// other command exits 127.
func Isolated(bridge string, overrides map[string]sshx.Result) *sshx.Fake {
	c := For(bridge)
	answers := map[string]sshx.Result{
		c.IPv6:   {Stdout: "1\n"},
		c.Addr:   {Stdout: `[{"ifindex":9,"ifname":"` + bridge + `","flags":["BROADCAST","MULTICAST","UP"],"addr_info":[]}]` + "\n"},
		c.Master: {Stdout: "[]\n"},
		c.OVS:    {Stdout: ""},
	}
	for k, v := range overrides {
		answers[k] = v
	}
	return &sshx.Fake{Handler: func(cmd string, _ []byte) (sshx.Result, error) {
		if r, ok := answers[cmd]; ok {
			return r, nil
		}
		return sshx.Result{ExitCode: 127, Stderr: "unexpected command " + cmd}, nil
	}}
}
