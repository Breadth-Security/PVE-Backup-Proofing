// Package bridgetest answers the `pbp node` helper's bridge request that
// target.ExistingNode sends to check the isolated bridge's live state, for
// tests.
package bridgetest

import (
	"encoding/json"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sshx"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sshx/helperfake"
)

// State is the helper's bridge reply for an isolated Linux bridge: IPv6
// disabled, no addresses and no members.
func State(bridge string) nodeproto.BridgeState {
	return nodeproto.BridgeState{
		Name:        bridge,
		DisableIPv6: "1",
		Addrs:       json.RawMessage(`[{"ifindex":9,"ifname":"` + bridge + `","flags":["BROADCAST","MULTICAST","UP"],"addr_info":[]}]`),
		Members:     []string{},
	}
}

// Isolated answers the helper's bridge request with State(bridge), changed
// by each mod in turn. Any other request exits 127.
func Isolated(bridge string, mods ...func(*nodeproto.BridgeState)) *helperfake.Fake {
	st := State(bridge)
	for _, m := range mods {
		m(&st)
	}
	return &helperfake.Fake{Handler: func(req nodeproto.Request, _ []byte) (sshx.Result, error) {
		if req.Op == "bridge" {
			return helperfake.JSON(st), nil
		}
		return sshx.Result{ExitCode: 127, Stderr: "unexpected request " + req.Op}, nil
	}}
}
