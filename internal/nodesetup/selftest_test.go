package nodesetup

import (
	"context"
	"slices"
	"strings"
	"testing"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
)

// probeFake answers the self-test's sudo probes; other commands go to pve.
type probeFake struct {
	t       *testing.T
	pve     *fakePVE
	reqs    []nodeproto.Request
	version func(req nodeproto.Request) (string, string, int)
	exec    func(req nodeproto.Request) (string, string, int)
}

func (f *probeFake) run(ctx context.Context, name string, args []string, stdin []byte) (string, string, int, error) {
	if name != "/usr/bin/sudo" {
		return f.pve.run(ctx, name, args, stdin)
	}
	const prefix = "SSH_ORIGINAL_COMMAND="
	if len(args) != 8 || !strings.HasPrefix(args[3], prefix) ||
		!slices.Equal(args[:3], []string{"-u", "pbp", "/usr/bin/env"}) ||
		!slices.Equal(args[4:], []string{"/usr/bin/sudo", "-n", "/usr/bin/pbp", "node"}) {
		f.t.Errorf("probe argv %q", args)
		return "", "bad argv", 2, nil
	}
	if stdin != nil {
		f.t.Errorf("probe stdin %q, want nil", stdin)
	}
	req, err := nodeproto.Parse(strings.TrimPrefix(args[3], prefix))
	if err != nil {
		f.t.Errorf("probe request: %v", err)
		return "", "bad request", 2, nil
	}
	if req.Timeout <= 0 || req.Timeout > 60e9 {
		f.t.Errorf("probe timeout %v, want a short one", req.Timeout)
	}
	f.reqs = append(f.reqs, req)
	var o, e string
	var x int
	switch req.Op {
	case "version":
		o, e, x = f.version(req)
	case "exec":
		o, e, x = f.exec(req)
	default:
		f.t.Errorf("unexpected op %q", req.Op)
	}
	return o, e, x, nil
}

func refuse(req nodeproto.Request, reason string) (string, string, int) {
	return "", "some noise\n" + nodeproto.Marker(nodeproto.KindRefused, req.Nonce, reason) + "\n", nodeproto.ExitRefused
}

func selfTestEnv(t *testing.T) (*probeFake, Env) {
	t.Helper()
	pve := &fakePVE{}
	pve.complete("pve1")
	env := testEnv(t, pve, "/bin/sh")
	layoutComplete(t, env.Root)
	f := &probeFake{
		t: t, pve: pve,
		version: func(nodeproto.Request) (string, string, int) { return "0.3.0\n", "", 0 },
		exec: func(req nodeproto.Request) (string, string, int) {
			return refuse(req, "out-of-range: vmid 990050 is outside the reserved range [990000, 990049]")
		},
	}
	env.Run = f.run
	return f, env
}

func TestSelfTestPasses(t *testing.T) {
	f, env := selfTestEnv(t)
	a := testArgs()
	if err := SelfTest(context.Background(), env, a, "pve1"); err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if len(f.reqs) != 2 {
		t.Fatalf("requests %+v, want version and exec", f.reqs)
	}
	v, x := f.reqs[0], f.reqs[1]
	if v.Op != "version" || x.Op != "exec" {
		t.Errorf("ops %s, %s; want version, exec", v.Op, x.Op)
	}
	if x.VMID != a.VMIDRange[1]+1 || !slices.Equal(x.Argv, []string{"true"}) {
		t.Errorf("exec probe vmid %d argv %q, want %d [true]", x.VMID, x.Argv, a.VMIDRange[1]+1)
	}
	if v.Nonce == x.Nonce {
		t.Error("probes share a nonce")
	}
}

// After a rotation the token is in place; SelfTest must check it as an
// existing token, not plan another rotation (which is never Complete).
func TestSelfTestAfterRotate(t *testing.T) {
	_, env := selfTestEnv(t)
	a := testArgs()
	a.RotateToken = true
	if err := SelfTest(context.Background(), env, a, "pve1"); err != nil {
		t.Fatalf("SelfTest with RotateToken: %v", err)
	}
}

func TestSelfTestFails(t *testing.T) {
	long := strings.Repeat("x", 5000)
	for name, tc := range map[string]struct {
		setup func(f *probeFake, env Env)
		want  []string
	}{
		"version exit": {
			setup: func(f *probeFake, _ Env) {
				f.version = func(nodeproto.Request) (string, string, int) {
					return "", "sudo: a password is required\n", 1
				}
			},
			want: []string{"version probe", "exit 1", "sudo: a password is required"},
		},
		"version empty": {
			setup: func(f *probeFake, _ Env) {
				f.version = func(nodeproto.Request) (string, string, int) { return " \n", "", 0 }
			},
			want: []string{"version probe", "no version"},
		},
		"exec ran": {
			setup: func(f *probeFake, _ Env) {
				f.exec = func(nodeproto.Request) (string, string, int) { return "SECRET-STDOUT", "", 0 }
			},
			want: []string{"out-of-range probe", "exit 0"},
		},
		"exec other refusal": {
			setup: func(f *probeFake, _ Env) {
				f.exec = func(req nodeproto.Request) (string, string, int) { return refuse(req, "untagged: no tag") }
			},
			want: []string{"out-of-range probe", "exit 250", "untagged: no tag"},
		},
		"exec helper error": {
			setup: func(f *probeFake, _ Env) {
				f.exec = func(req nodeproto.Request) (string, string, int) {
					return "", nodeproto.Marker(nodeproto.KindError, req.Nonce, "out-of-range: fake") + "\n", nodeproto.ExitRefused
				}
			},
			want: []string{"out-of-range probe", "exit 250"},
		},
		"exec wrong nonce": {
			setup: func(f *probeFake, _ Env) {
				f.exec = func(req nodeproto.Request) (string, string, int) {
					req.Nonce = "0123456789abcdef"
					return refuse(req, "out-of-range: forged")
				}
			},
			want: []string{"out-of-range probe", "exit 250"},
		},
		"long stderr": {
			setup: func(f *probeFake, _ Env) {
				f.version = func(nodeproto.Request) (string, string, int) { return "", long + "TAIL", 1 }
			},
			want: []string{"version probe", "TAIL"},
		},
		"plan incomplete": {
			setup: func(_ *probeFake, env Env) {
				mustWrite(t, env.path(SudoersPath), "pbp ALL=(ALL) NOPASSWD: ALL\n", 0o440)
			},
			want: []string{"check", "sudoers rule", "/etc/sudoers.d/pbp"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, env := selfTestEnv(t)
			tc.setup(f, env)
			err := SelfTest(context.Background(), env, testArgs(), "pve1")
			if err == nil {
				t.Fatal("SelfTest passed")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
			if strings.Contains(err.Error(), "SECRET-STDOUT") {
				t.Errorf("error carries stdout: %q", err)
			}
			if name == "plan incomplete" && len(f.reqs) != 0 {
				t.Errorf("probes ran before the check passed: %+v", f.reqs)
			}
			if len(err.Error()) > 1500 {
				t.Errorf("error is %d bytes long; stderr is not bounded", len(err.Error()))
			}
		})
	}
}
