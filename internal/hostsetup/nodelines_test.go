package hostsetup

import (
	"strings"
	"testing"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
)

func lineCfg() config.Config {
	var c config.Config
	c.Target.VMIDRange = [2]int{990000, 990049}
	c.Target.Bridge = "vmbr99"
	c.Restore.Storage = "local-zfs"
	c.Sources.Storages = []string{"pbs-main", "local-zfs", "pbs-two"}
	return c
}

const testPub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGq5o5f7cJ9n8Kq3QmV0b1qHkCQ0dE1oS2w3o6x8yZ1A pbp"

func TestNodeLinesRelease(t *testing.T) {
	got := NodeLines(lineCfg(), testPub, "0.3.0~rc.1", false)
	base := "https://git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/releases/download/v0.3.0-rc.1/"
	install := `(a=$(dpkg --print-architecture) && d=$(mktemp -d) && cd "$d" && curl -fsSLO "` + base +
		`pbp_0.3.0~rc.1_${a}.deb" && curl -fsSLO "` + base + `SHA256SUMS" && sha256sum -c --ignore-missing SHA256SUMS && apt install -y "./pbp_0.3.0~rc.1_${a}.deb"; rc=$?; rm -rf "$d"; exit $rc)`
	setup := "pbp node setup --vmid-range 990000-990049 --bridge vmbr99 --pubkey '" + testPub +
		"' --storages local-zfs,pbs-main,pbs-two --daemon-version '0.3.0~rc.1' --create-token"
	for _, want := range []string{"On the node, as root:", "\n" + install + "\n", "\n" + setup + "\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Index(got, install) > strings.Index(got, setup) {
		t.Error("install line must come before the setup line")
	}
	if strings.Contains(NodeLines(lineCfg(), testPub, "0.3.0", true), "--create-token") {
		t.Error("--create-token with a token file present")
	}
}

func TestNodeLinesDev(t *testing.T) {
	got := NodeLines(lineCfg(), testPub, "dev", false)
	if !strings.Contains(got, "this PBP is a dev build: install a matching dev build of pbp on the node") {
		t.Errorf("no dev note:\n%s", got)
	}
	if strings.Contains(got, "https://") || strings.Contains(got, "dpkg") {
		t.Errorf("dev build must print no URL:\n%s", got)
	}
	if !strings.Contains(got, "pbp node setup --vmid-range 990000-990049") {
		t.Errorf("no setup line:\n%s", got)
	}
}
