// Package privs knows which privileges PBP's API token needs on which path,
// and checks the token's effective privileges against them (issue #107).
//
// The token is privilege-separated: PVE grants it only what both it and its
// user hold. A missing grant on /vms or a storage does not always fail a
// call; PVE silently leaves out the backups the caller may not see, so a
// storage just looks empty. Checking the privileges up front names the gap.
package privs

import (
	"context"
	"fmt"
	"strings"
)

// RoleName is the role pbp node setup creates and grants on every path.
const RoleName = "PBPProofer"

// SDNZonePath is where PBP needs SDN.Use: it covers every local bridge and
// VLAN the original guests' NICs name.
const SDNZonePath = "/sdn/zones/localnetwork"

// RolePrivs are the PBPProofer privileges, in the order docs/manual-setup.md
// lists them. Every one is needed on some path in Needs.
var RolePrivs = []string{
	"VM.Allocate", "VM.Audit", "VM.Backup", "VM.Config.HWType", "VM.Config.Network",
	"VM.Config.Options", "VM.PowerMgmt", "VM.GuestAgent.Audit", "VM.GuestAgent.Unrestricted",
	"Datastore.AllocateSpace", "Datastore.Audit", "Sys.Audit", "SDN.Use",
}

// Per-path minimums, as docs/security.md "The API token" lists them.
var (
	vmsPrivs     = []string{"VM.Allocate", "VM.Audit", "VM.Backup", "VM.Config.HWType", "VM.Config.Network", "VM.Config.Options", "VM.PowerMgmt", "VM.GuestAgent.Audit", "VM.GuestAgent.Unrestricted"}
	storagePrivs = []string{"Datastore.AllocateSpace", "Datastore.Audit"}
	nodePrivs    = []string{"Sys.Audit"}
	sdnPrivs     = []string{"SDN.Use"}
)

// Need is the privileges the token needs on one ACL path. Source is the
// storage ID when the path is a source storage that is not also the restore
// storage; "" marks a core path, without which nothing can be proofed.
type Need struct {
	Path   string
	Privs  []string
	Source string
}

// Needs lists what the token needs on node: /vms, the restore storage, each
// other source storage (repeats dropped, in order), the node and the SDN
// zone.
func Needs(node, restoreStorage string, sources []string) []Need {
	out := []Need{{Path: "/vms", Privs: vmsPrivs}, {Path: "/storage/" + restoreStorage, Privs: storagePrivs}}
	seen := map[string]bool{restoreStorage: true}
	for _, s := range sources {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, Need{Path: "/storage/" + s, Privs: storagePrivs, Source: s})
	}
	return append(out, Need{Path: "/nodes/" + node, Privs: nodePrivs}, Need{Path: SDNZonePath, Privs: sdnPrivs})
}

// Privileger reads the calling token's effective privileges on a path
// (pve.API does).
type Privileger interface {
	Privileges(ctx context.Context, path string) (map[string]bool, error)
}

// Missing is a need the token does not fully meet; Lacks is in the need's
// order.
type Missing struct {
	Need
	Lacks []string
}

// Check reads the token's privileges on each need's path and returns the
// needs it does not fully meet. A failed read stops it.
func Check(ctx context.Context, api Privileger, needs []Need) ([]Missing, error) {
	var out []Missing
	for _, n := range needs {
		have, err := api.Privileges(ctx, n.Path)
		if err != nil {
			return nil, fmt.Errorf("read the token's privileges on %s: %w", n.Path, err)
		}
		var lacks []string
		for _, p := range n.Privs {
			if !have[p] {
				lacks = append(lacks, p)
			}
		}
		if len(lacks) > 0 {
			out = append(out, Missing{Need: n, Lacks: lacks})
		}
	}
	return out, nil
}

// Describe says what the token tokenID lacks and how to grant it. The token
// cannot read ACLs, so it cannot tell whether the user or the token misses
// the grant: both need it.
func (m Missing) Describe(tokenID string) string {
	user, _, _ := strings.Cut(tokenID, "!")
	them := "them"
	if len(m.Lacks) == 1 {
		them = "it"
	}
	return fmt.Sprintf("token %s lacks %s on %s; the user %s and its privilege-separated token both need %s: "+
		"pveum acl modify %s --users %s --tokens '%s' --roles %s",
		tokenID, strings.Join(m.Lacks, ", "), m.Path, user, them, m.Path, user, tokenID, RoleName)
}
