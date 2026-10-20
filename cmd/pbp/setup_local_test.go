package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/hostsetup"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodesetup"
)

// localRig is `sudo pbp setup --deb` in a root terminal on a (fake) Proxmox
// VE node: the deb layout in a temp dir, the fake node behind the node
// setup library, and a probe that sees the node's own host key.
type localRig struct {
	*setupRig
	deb string
}

func newLocalRig(t *testing.T) *localRig {
	t.Helper()
	r := newSetupRig(t)
	deb := t.TempDir()
	t.Cleanup(hostsetup.SetTestHooks(func(string, int, int) error { return nil },
		func(string) (int, error) { return 4242, nil }, deb))
	r.a.onPVENode = func() bool { return true }
	r.a.defaultTZ = func() string { return "UTC" }
	r.a.echoOff = func() (func(), error) { return func() {}, nil }
	r.a.probeHostKey = func(context.Context, string) (ssh.PublicKey, error) {
		b, err := os.ReadFile(filepath.Join(r.f.root, nodesetup.HostKeyPath))
		if err != nil {
			return nil, err
		}
		k, _, _, _, err := ssh.ParseAuthorizedKey(b)
		return k, err
	}
	return &localRig{setupRig: r, deb: deb}
}

// The questions: address (Enter: 127.0.0.1), range, bridge, restore
// storage, PBS storages, schedule (Enter). The deb layout asks no time zone.
const localAnswers = "\n990000-990049\nvmbr99\nlocal-zfs\npbs1\n\n"

func (r *localRig) setup(stdin string) int {
	return r.main(stdin, "setup", "--deb")
}

// One shell: setup asks, sets the node up after its own confirmation, and
// finishes with the reply it got, without printing or pasting anything.
func TestSetupOnNodeOneShell(t *testing.T) {
	r := newLocalRig(t)
	code := r.setup(localAnswers + "y\ny\n")
	out := r.out.String() + r.errOut.String()
	if code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{"Node address (hostname or IP, as the host reaches its SSH and API) [127.0.0.1]",
		"This host is a Proxmox VE node", "Set up this node here? [y/N]", "Apply these changes? [y/N]",
		"Setup is complete. Start PBP: sudo systemctl start pbp"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, never := range []string{nodeproto.ReplyBegin, fakeSecret, msgPasteReply} {
		if strings.Contains(out, never) {
			t.Errorf("output shows %q:\n%s", never, out)
		}
	}
	if b, err := os.ReadFile(filepath.Join(r.deb, "secrets", "pve_token")); err != nil || string(b) != fakeSecret+"\n" {
		t.Errorf("token file %q, %v", b, err)
	}
	cfg, err := os.ReadFile(filepath.Join(r.deb, "config.yaml"))
	if err != nil || !strings.Contains(string(cfg), "host: 127.0.0.1") || !strings.Contains(string(cfg), "https://127.0.0.1:8006") {
		t.Errorf("config does not use 127.0.0.1:\n%s", cfg)
	}
	kh, err := os.ReadFile(filepath.Join(r.deb, "known_hosts"))
	if err != nil || !strings.HasPrefix(string(kh), "127.0.0.1 ssh-ed25519 ") {
		t.Errorf("known_hosts %q, %v", kh, err)
	}
	if !r.f.exists(nodesetup.AuthorizedKeys) {
		t.Error("the node was not set up")
	}
}

// The token already exists on the node and this host has none: setup
// offers to replace it, and with yes finishes with the new secret.
func TestSetupOnNodeReplacesToken(t *testing.T) {
	r := newLocalRig(t)
	r.f.complete()
	code := r.setup(localAnswers + "y\ny\ny\n")
	out := r.out.String() + r.errOut.String()
	if code != exitOK || !strings.Contains(out, "Replace the token? [y/N]") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if strings.Contains(out, fakeSecret) {
		t.Errorf("secret shown:\n%s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(r.deb, "secrets", "pve_token")); string(b) != fakeSecret+"\n" {
		t.Errorf("token file %q", b)
	}

	// No: nothing is rotated and setup stops, saying why.
	r2 := newLocalRig(t)
	r2.f.complete()
	before := len(r2.f.events)
	if code := r2.setup(localAnswers + "y\ny\nn\n"); code == exitOK {
		t.Fatal("finished without a token secret")
	}
	for _, e := range r2.f.events[before:] {
		if strings.Contains(e, "token remove") {
			t.Errorf("token rotated after no: %s", e)
		}
	}
}

// Declining, or a shell where node setup may not run, keeps the two-shell
// flow: the node lines and the paste prompt.
func TestSetupOnNodeTwoShellFallback(t *testing.T) {
	r := newLocalRig(t)
	if code := r.setup(localAnswers + "n\n"); code == exitOK {
		t.Fatal("finished without a reply")
	}
	if out := r.out.String(); !strings.Contains(out, "pbp node setup --vmid-range") || !strings.Contains(out, msgPasteReply) {
		t.Errorf("no node lines or paste prompt after no:\n%s", out)
	}
	if r.f.exists(nodesetup.AuthorizedKeys) {
		t.Error("the node was set up after no")
	}

	r2 := newLocalRig(t)
	t.Setenv("SSH_ORIGINAL_COMMAND", "x")
	if code := r2.setup(localAnswers); code == exitOK {
		t.Fatal("finished without a reply")
	}
	if out := r2.out.String(); !strings.Contains(out, "cannot be set up in this run (SSH_ORIGINAL_COMMAND is set)") ||
		strings.Contains(out, "Set up this node here?") {
		t.Errorf("gate not applied to the offer:\n%s", out)
	}
}

// exists reports whether path exists under the fake node's root.
func (f *fakeNode) exists(path string) bool {
	_, err := os.Stat(filepath.Join(f.root, path))
	return err == nil
}

// complete gives the fake node an existing pbp@pve!proof token, as a node
// set up earlier (by another PBP host, say) has.
func (f *fakeNode) complete() {
	f.users = append(f.users, nodesetup.UserID)
	f.tokens[nodesetup.UserID] = append(f.tokens[nodesetup.UserID], nodesetup.TokenName)
}
