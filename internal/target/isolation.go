package target

import (
	"context"
	"errors"
	"fmt"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/pve"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/sshx"
)

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
	if problem, _ := LiveIsolationProblem(st, n.bridge, iface.Type == "OVSBridge", n.lo, n.hi); problem != "" {
		return notIso("%s", problem)
	}
	return nil
}
