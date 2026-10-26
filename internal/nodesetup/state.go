// Package nodesetup is `pbp node setup`: it brings a Proxmox node to the
// state docs/manual-setup.md "Node setup" describes. This file is the single source of
// truth for that state; plan.go compares it with the node.
package nodesetup

import (
	"os"
	"strconv"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/node"
)

// SudoersText is /etc/sudoers.d/pbp, exactly as docs/manual-setup.md "Node setup" step 5 shows it.
const SudoersText = "Defaults:pbp env_reset\nDefaults:pbp env_keep = \"SSH_ORIGINAL_COMMAND SSH_CONNECTION\"\npbp ALL=(root) NOPASSWD: /usr/bin/pbp node\n"

// PVE identities for the API token (docs/manual-setup.md "Create an API token").
const (
	RoleName  = "PBPProofer"
	UserID    = "pbp@pve"
	TokenName = "proof"
	TokenID   = UserID + "!" + TokenName
)

// RolePrivs are the PBPProofer privileges, in the order docs/manual-setup.md lists them.
var RolePrivs = []string{
	"VM.Allocate", "VM.Audit", "VM.Backup", "VM.Config.HWType", "VM.Config.Network",
	"VM.Config.Options", "VM.PowerMgmt", "VM.GuestAgent.Audit", "VM.GuestAgent.Unrestricted",
	"Datastore.AllocateSpace", "Datastore.Audit", "Sys.Audit", "SDN.Use",
}

// Paths and fixed values of the node's end state.
const (
	PbpUser         = "pbp"
	PbpShell        = "/bin/sh"
	PbpHome         = "/var/lib/pbp"
	PbpHomeMode     = os.FileMode(0o755)
	EtcDir          = "/etc/pbp"
	NodeYAMLPath    = node.DefaultConfigPath
	SSHDir          = PbpHome + "/.ssh"
	AuthorizedKeys  = SSHDir + "/authorized_keys"
	SudoersPath     = "/etc/sudoers.d/pbp"
	TemplateDir     = node.DefaultTemplateDir
	forcedCmdPrefix = `restrict,command="sudo /usr/bin/pbp node" `
)

// AuthorizedKeysLine is the one authorized_keys line for pubKey (no newline).
func AuthorizedKeysLine(pubKey string) string {
	return forcedCmdPrefix + pubKey
}

// NodeYAML is the helper config setup writes.
func NodeYAML(lo, hi int, bridge string) []byte {
	return []byte("vmid_range: [" + strconv.Itoa(lo) + ", " + strconv.Itoa(hi) + "]\nbridge: " + bridge + "\nrequest_timeout: 15m\n")
}

// ACLPaths are the paths the user and the token get PBPProofer on.
func ACLPaths(nodeName string, storages []string) []string {
	out := []string{"/vms"}
	for _, s := range storages {
		out = append(out, "/storage/"+s)
	}
	return append(out, "/nodes/"+nodeName, "/sdn/zones/localnetwork")
}
