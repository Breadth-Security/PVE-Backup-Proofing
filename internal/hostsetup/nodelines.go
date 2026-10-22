package hostsetup

import (
	"slices"
	"strings"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
)

// releaseURL is where a release's .deb files, SHA256SUMS and its signature
// are published.
const releaseURL = "https://git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/releases/download/"

// ReleaseSigner is the allowed_signers line for the key that signs each
// release's SHA256SUMS (issue #37), the same as in the repository's
// allowed_signers file. It is built into PBP so the install line it prints
// does not trust a key fetched from the release page it verifies.
const ReleaseSigner = `pbp-release namespaces="pbp-release" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMdDxN4XOaHKQMSYXUFAWFRLES54peOduNiltN3U1a86`

// VerifyCommand checks SHA256SUMS.sig against the allowed_signers file.
const VerifyCommand = "ssh-keygen -Y verify -f allowed_signers -I pbp-release -n pbp-release -s SHA256SUMS.sig < SHA256SUMS"

// SetupArgs are the `pbp node setup` arguments for cfg: the range, bridge and
// pub key, the restore storage then the PBS storages (repeats dropped), and
// --create-token when the host has no token secret yet.
func SetupArgs(cfg config.Config, pub, version string, haveToken bool) nodeproto.SetupArgs {
	var storages []string
	for _, s := range append([]string{cfg.Restore.Storage}, cfg.Sources.Storages...) {
		if s != "" && !slices.Contains(storages, s) {
			storages = append(storages, s)
		}
	}
	return nodeproto.SetupArgs{
		VMIDRange:     cfg.Target.VMIDRange,
		Bridge:        cfg.Target.Bridge,
		PubKey:        pub,
		DaemonVersion: version,
		Storages:      storages,
		CreateToken:   !haveToken,
	}
}

// NodeLines renders what the admin runs on the node: the install line for
// this daemon's release (or a note for a dev build), then the setup line.
func NodeLines(cfg config.Config, pub, version string, haveToken bool) string {
	var b strings.Builder
	b.WriteString("On the node, as root:\n\n")
	if v := version; v != "" && !strings.Contains(v, "dev") {
		base := releaseURL + "v" + strings.ReplaceAll(v, "~", "-") + "/"
		deb := "pbp_" + v + "_${a}.deb"
		b.WriteString("1. Install pbp " + v + " (signature and checksum verified):\n\n")
		// A subshell, so the admin's shell keeps its directory and the
		// download dir is removed whatever happens. The signature over
		// SHA256SUMS is checked with the key built into this PBP, then the
		// .deb against SHA256SUMS.
		b.WriteString(`(a=$(dpkg --print-architecture) && d=$(mktemp -d) && cd "$d" && curl -fsSLO "` + base + deb +
			`" && curl -fsSLO "` + base + `SHA256SUMS" && curl -fsSLO "` + base + `SHA256SUMS.sig" && echo '` + ReleaseSigner +
			`' > allowed_signers && ` + VerifyCommand + ` && sha256sum -c --ignore-missing SHA256SUMS && apt install -y "./` + deb +
			`"; rc=$?; rm -rf "$d"; exit $rc)` + "\n\n")
	} else {
		b.WriteString("1. Note: this PBP is a dev build: install a matching dev build of pbp on the node.\n\n")
	}
	b.WriteString("2. Set up the node:\n\n")
	b.WriteString(SetupArgs(cfg, pub, version, haveToken).Line() + "\n")
	return b.String()
}
