package hostsetup

import (
	"os"
	"os/exec"
	"path/filepath"
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
	base := "https://github.com/BreadthAnalytics/proxmox-backup-proofing/releases/download/v0.3.0-rc.1/"
	// GitHub serves the pre-release's "~" as "."; the file is saved under
	// the name SHA256SUMS signs.
	install := `(a=$(dpkg --print-architecture) && d=$(mktemp -d) && cd "$d" && curl -fsSL -o "pbp_0.3.0~rc.1_${a}.deb" "` + base +
		`pbp_0.3.0.rc.1_${a}.deb" && curl -fsSLO "` + base + `SHA256SUMS" && curl -fsSLO "` + base + `SHA256SUMS.sig" && echo '` +
		ReleaseSigner + `' > allowed_signers && ssh-keygen -Y verify -f allowed_signers -I pbp-release -n pbp-release -s SHA256SUMS.sig < SHA256SUMS` +
		` && sha256sum -c --ignore-missing SHA256SUMS && apt install -y "./pbp_0.3.0~rc.1_${a}.deb"; rc=$?; rm -rf "$d"; exit $rc)`
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

// A final release has no "~": its file is fetched under its own name.
func TestNodeLinesFinalRelease(t *testing.T) {
	got := NodeLines(lineCfg(), testPub, "0.8.1", true)
	want := `curl -fsSLO "https://github.com/BreadthAnalytics/proxmox-backup-proofing/releases/download/v0.8.1/pbp_0.8.1_${a}.deb" && `
	if !strings.Contains(got, want) {
		t.Errorf("missing %q in:\n%s", want, got)
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

// The key built into PBP is the one the repository publishes in
// allowed_signers, for release files and for git.
func TestReleaseSignerMatchesRepository(t *testing.T) {
	b, err := os.ReadFile("../../allowed_signers")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "\n"+ReleaseSigner+"\n") {
		t.Errorf("allowed_signers lacks %q", ReleaseSigner)
	}
	key := strings.SplitN(ReleaseSigner, `" `, 2)[1]
	if !strings.Contains(string(b), `namespaces="git" `+key) {
		t.Error("allowed_signers lacks the git entry for the same key")
	}
}

// The verify command the install line and the docs give accepts a
// signature made the way packaging/sign-release.sh makes it, and rejects a
// changed SHA256SUMS or another namespace.
func TestVerifyCommand(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not installed")
	}
	dir := t.TempDir()
	run := func(cmd string) error {
		c := exec.Command("sh", "-c", cmd)
		c.Dir = dir
		out, err := c.CombinedOutput()
		if err != nil {
			t.Logf("%s: %s", cmd, out)
		}
		return err
	}
	if err := run(`ssh-keygen -q -t ed25519 -N "" -f key && printf 'abc  pbp_1_amd64.deb\n' > SHA256SUMS`); err != nil {
		t.Fatal(err)
	}
	pub, err := os.ReadFile(filepath.Join(dir, "key.pub"))
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(string(pub))
	signer := strings.Replace(ReleaseSigner, strings.SplitN(ReleaseSigner, `" `, 2)[1], f[0]+" "+f[1], 1)
	// Written exactly as the install line writes it.
	if err := run("echo '" + signer + "' > allowed_signers"); err != nil {
		t.Fatal(err)
	}
	if err := run("ssh-keygen -q -Y sign -f key -n pbp-release SHA256SUMS && " + VerifyCommand); err != nil {
		t.Fatalf("good signature rejected: %v", err)
	}
	if err := run("echo x >> SHA256SUMS"); err != nil {
		t.Fatal(err)
	}
	if err := run(VerifyCommand); err == nil {
		t.Error("changed SHA256SUMS accepted")
	}
	if err := run("sed -i '$d' SHA256SUMS && rm SHA256SUMS.sig && ssh-keygen -q -Y sign -f key -n git SHA256SUMS"); err != nil {
		t.Fatal(err)
	}
	if err := run(VerifyCommand); err == nil {
		t.Error("signature in the git namespace accepted")
	}
}
