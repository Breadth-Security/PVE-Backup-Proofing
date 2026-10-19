package target

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
	"git.breadthanalytics.com/tdiana/pbv/internal/probe"
	"git.breadthanalytics.com/tdiana/pbv/internal/pve"
	"git.breadthanalytics.com/tdiana/pbv/internal/pve/pvefake"
	"git.breadthanalytics.com/tdiana/pbv/internal/sshx"
)

// agentAPI scripts the guest agent on top of pvefake.
type agentAPI struct {
	*pvefake.Fake
	osID      string
	osErr     error
	pingFails int // pings that fail before one succeeds; -1 = never answers
	execErr   error
	statuses  []pve.AgentExecStatus // returned in order; the last repeats

	mu    sync.Mutex
	pings int
	polls int
	argv  [][]string
}

func (a *agentAPI) AgentPing(ctx context.Context, node string, vmid int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pings++
	if a.pingFails < 0 || a.pings <= a.pingFails {
		return errors.New("QEMU guest agent is not running")
	}
	return nil
}

func (a *agentAPI) AgentOSInfo(ctx context.Context, node string, vmid int) (pve.OSInfo, error) {
	return pve.OSInfo{ID: a.osID}, a.osErr
}

func (a *agentAPI) AgentExec(ctx context.Context, node string, vmid int, argv []string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.argv = append(a.argv, argv)
	if a.execErr != nil {
		return 0, a.execErr
	}
	return 42, nil
}

func (a *agentAPI) AgentExecStatus(ctx context.Context, node string, vmid, pid int) (pve.AgentExecStatus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.statuses) == 0 {
		return pve.AgentExecStatus{Exited: true}, nil
	}
	i := min(a.polls, len(a.statuses)-1)
	a.polls++
	return a.statuses[i], nil
}

func runningVM(f *pvefake.Fake, vmid int, agent string) {
	f.GuestList = append(f.GuestList, guest(model.KindQEMU, vmid, "running", "pbp"))
	f.Configs[vmid] = map[string]string{"tags": "pbp"}
	if agent != "" {
		f.Configs[vmid]["agent"] = agent
	}
}

func TestWaitBootedVM(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name  string
		agent string
		api   *agentAPI
		want  BootInfo
		argv  string
	}{
		{name: "agent: 0", agent: "0", want: BootInfo{AgentMissing: true, OS: probe.Unknown}},
		{name: "no agent key", agent: "", want: BootInfo{AgentMissing: true, OS: probe.Unknown}},
		{name: "enabled=0", agent: "enabled=0,fstrim_cloned_disks=1", want: BootInfo{AgentMissing: true, OS: probe.Unknown}},
		{name: "agent never answers", agent: "1", api: &agentAPI{pingFails: -1},
			want: BootInfo{AgentMissing: true, OS: probe.Unknown}},
		{name: "linux after retries", agent: "1,fstrim_cloned_disks=1", api: &agentAPI{osID: "debian", pingFails: 2},
			want: BootInfo{OS: probe.Linux}, argv: "true"},
		{name: "windows", agent: "enabled=1", api: &agentAPI{osID: "mswindows"},
			want: BootInfo{OS: probe.Windows}, argv: "cmd /c exit 0"},
		{name: "exec blocked", agent: "1", api: &agentAPI{osID: "rhel", execErr: errors.New("guest-exec has been disabled by agent policy")},
			want: BootInfo{OS: probe.Linux, ExecBlocked: true}, argv: "true"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := healthyFake()
			runningVM(f, 990000, tc.agent)
			api := tc.api
			if api == nil {
				api = &agentAPI{}
			}
			api.Fake = f
			n := newTarget(t, api, f, nil)
			got, err := n.WaitBooted(ctx, model.KindQEMU, 990000, 200*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			if got.Duration <= 0 {
				t.Error("Duration not set")
			}
			got.Duration = 0
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
			if tc.argv != "" {
				if len(api.argv) == 0 || strings.Join(api.argv[0], " ") != tc.argv {
					t.Errorf("test exec argv = %q, want %q", api.argv, tc.argv)
				}
			}
		})
	}
}

func TestWaitBootedVMNotRunning(t *testing.T) {
	f := healthyFake()
	f.GuestList = []pve.GuestRef{guest(model.KindQEMU, 990000, "stopped", "pbp")}
	f.Configs[990000] = map[string]string{"agent": "1"}
	n := newTarget(t, f, f, nil)
	if _, err := n.WaitBooted(context.Background(), model.KindQEMU, 990000, 20*time.Millisecond); !errors.Is(err, ErrBootFailed) {
		t.Errorf("err = %v, want ErrBootFailed for a guest that never runs", err)
	}
}

// Node and API faults are not boot failures: they must not blame the backup.
func TestWaitBootedFaultsAreNotBootFailures(t *testing.T) {
	ctx := context.Background()
	t.Run("status API failing", func(t *testing.T) {
		f := healthyFake()
		runningVM(f, 990000, "1")
		f.Fail["GuestStatus"] = errors.New("pve api: 500 internal")
		_, err := newTarget(t, f, f, nil).WaitBooted(ctx, model.KindQEMU, 990000, 20*time.Millisecond)
		if err == nil || errors.Is(err, ErrBootFailed) || !strings.Contains(err.Error(), "500") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("vm config unreadable", func(t *testing.T) {
		f := healthyFake()
		runningVM(f, 990000, "1")
		f.Fail["GuestConfig"] = errors.New("pve api: 500 internal")
		_, err := newTarget(t, f, f, nil).WaitBooted(ctx, model.KindQEMU, 990000, time.Second)
		if err == nil || errors.Is(err, ErrBootFailed) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("agent test exec API error", func(t *testing.T) {
		f := healthyFake()
		runningVM(f, 990000, "1")
		api := &agentAPI{Fake: f, osID: "debian", execErr: errors.New("pve api: 500 internal")}
		_, err := newTarget(t, api, f, nil).WaitBooted(ctx, model.KindQEMU, 990000, time.Second)
		if err == nil || errors.Is(err, ErrBootFailed) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("lxc ssh transport failing", func(t *testing.T) {
		f := healthyFake()
		f.GuestList = []pve.GuestRef{guest(model.KindLXC, 990001, "running", "pbp")}
		f.Configs[990001] = map[string]string{"tags": "pbp", "ostype": "debian"}
		ssh := &sshx.Fake{Handler: func(string, []byte) (sshx.Result, error) { return sshx.Result{}, errors.New("ssh: connection refused") }}
		_, err := newTarget(t, f, f, ssh).WaitBooted(ctx, model.KindLXC, 990001, 30*time.Millisecond)
		if err == nil || errors.Is(err, ErrBootFailed) || !strings.Contains(err.Error(), "connection refused") {
			t.Errorf("err = %v", err)
		}
	})
}

// If OS info fails after the agent answered, a working "true" exec means a
// Linux guest; otherwise PBP cannot look inside (BOOTED), never FAILED.
func TestWaitBootedVMOSInfoFails(t *testing.T) {
	ctx := context.Background()
	osErr := errors.New("guest-get-osinfo not supported")
	for _, tc := range []struct {
		name    string
		execErr error
		want    BootInfo
	}{
		{"exec works", nil, BootInfo{OS: probe.Linux}},
		{"exec blocked", errors.New("guest-exec has been disabled"), BootInfo{OS: probe.Unknown, ExecBlocked: true}},
		{"exec fails", errors.New("agent: command failed"), BootInfo{OS: probe.Unknown, ExecBlocked: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := healthyFake()
			runningVM(f, 990000, "1")
			api := &agentAPI{Fake: f, osErr: osErr, execErr: tc.execErr}
			got, err := newTarget(t, api, f, nil).WaitBooted(ctx, model.KindQEMU, 990000, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			got.Duration = 0
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
			if len(api.argv) == 0 || strings.Join(api.argv[0], " ") != "true" {
				t.Errorf("argv = %q", api.argv)
			}
		})
	}
}

func TestWaitBootedNotRunningCancelled(t *testing.T) {
	f := healthyFake()
	f.GuestList = []pve.GuestRef{guest(model.KindQEMU, 990000, "stopped", "pbp")}
	n := newTarget(t, f, f, nil)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	_, err := n.WaitBooted(ctx, model.KindQEMU, 990000, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestWaitBootedLXC(t *testing.T) {
	ctx := context.Background()
	mk := func(ostype string) *pvefake.Fake {
		f := healthyFake()
		f.GuestList = []pve.GuestRef{guest(model.KindLXC, 990001, "running", "pbp")}
		f.Configs[990001] = map[string]string{"tags": "pbp", "ostype": ostype}
		return f
	}
	t.Run("running and exec ok", func(t *testing.T) {
		f := mk("debian")
		attempts := 0
		ssh := &sshx.Fake{Handler: func(cmd string, _ []byte) (sshx.Result, error) {
			attempts++
			if attempts < 3 { // container still booting
				return sshx.Result{ExitCode: 255, Stderr: "not ready"}, nil
			}
			return sshx.Result{}, nil
		}}
		got, err := newTarget(t, f, f, ssh).WaitBooted(ctx, model.KindLXC, 990001, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if got.OS != probe.Linux || got.AgentMissing || got.ExecBlocked {
			t.Errorf("got %+v", got)
		}
		if ssh.CallLog()[0] != "timeout --kill-after=5s 1s pct exec 990001 -- 'true'" {
			t.Errorf("ssh calls = %q", ssh.CallLog())
		}
		for _, c := range ssh.CallLog() {
			if strings.Contains(c, "pct status") {
				t.Errorf("must use API status, not %q", c)
			}
		}
	})
	t.Run("ostype reported", func(t *testing.T) {
		f := mk("alpine")
		got, err := newTarget(t, f, f, &sshx.Fake{}).WaitBooted(ctx, model.KindLXC, 990001, time.Second)
		if err != nil || got.OS != probe.Linux || got.OSType != "alpine" {
			t.Errorf("got %+v, %v", got, err)
		}
	})
	t.Run("unmanaged ostype", func(t *testing.T) {
		f := mk("unmanaged")
		got, err := newTarget(t, f, f, &sshx.Fake{}).WaitBooted(ctx, model.KindLXC, 990001, time.Second)
		if err != nil || got.OS != probe.Unknown {
			t.Errorf("got %+v, %v", got, err)
		}
	})
	t.Run("exec never succeeds", func(t *testing.T) {
		f := mk("debian")
		ssh := &sshx.Fake{Handler: func(string, []byte) (sshx.Result, error) { return sshx.Result{ExitCode: 1}, nil }}
		if _, err := newTarget(t, f, f, ssh).WaitBooted(ctx, model.KindLXC, 990001, 30*time.Millisecond); !errors.Is(err, ErrBootFailed) {
			t.Errorf("err = %v, want ErrBootFailed", err)
		}
	})
}

func TestVMExecutor(t *testing.T) {
	ctx := context.Background()
	f := healthyFake()
	runningVM(f, 990000, "1")
	api := &agentAPI{Fake: f, statuses: []pve.AgentExecStatus{
		{}, {}, {Exited: true, ExitCode: 3, OutData: "out", ErrData: "err"},
	}}
	n := newTarget(t, api, f, nil)
	res, err := n.Executor(model.KindQEMU, 990000).Exec(ctx, []string{"systemctl", "is-system-running"})
	if err != nil {
		t.Fatal(err)
	}
	if res != (probe.ExecResult{Stdout: "out", Stderr: "err", ExitCode: 3}) {
		t.Errorf("res = %+v", res)
	}
	if api.polls != 3 {
		t.Errorf("polls = %d", api.polls)
	}

	for _, msg := range []string{"agent exec Not Allowed", "command blocked by policy", "guest-exec is disabled"} {
		api.execErr = errors.New(msg)
		_, err := n.Executor(model.KindQEMU, 990000).Exec(ctx, []string{"true"})
		if !errors.Is(err, probe.ErrExecBlocked) || !strings.Contains(err.Error(), msg) {
			t.Errorf("%q: err = %v", msg, err)
		}
	}
	api.execErr = errors.New("agent not running")
	if _, err := n.Executor(model.KindQEMU, 990000).Exec(ctx, []string{"true"}); err == nil || errors.Is(err, probe.ErrExecBlocked) {
		t.Errorf("plain error: %v", err)
	}
}

func TestExecutorRefusesOutsideRange(t *testing.T) {
	f := healthyFake()
	ssh := &sshx.Fake{}
	n := newTarget(t, f, f, ssh)
	for _, kind := range []model.GuestKind{model.KindQEMU, model.KindLXC} {
		if _, err := n.Executor(kind, 100).Exec(context.Background(), []string{"true"}); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("%s: err = %v", kind, err)
		}
	}
	if len(ssh.CallLog()) != 0 || len(f.CallLog()) != 0 {
		t.Errorf("calls made: %q %q", ssh.CallLog(), f.CallLog())
	}
}

func TestLXCExecutor(t *testing.T) {
	ctx := context.Background()
	f := healthyFake()
	ssh := &sshx.Fake{Handler: func(cmd string, _ []byte) (sshx.Result, error) {
		if strings.Contains(cmd, "boom") {
			return sshx.Result{}, errors.New("ssh: connection lost")
		}
		return sshx.Result{Stdout: "o", Stderr: "e", ExitCode: 2}, nil
	}}
	n := newTarget(t, f, f, ssh)
	res, err := n.Executor(model.KindLXC, 990002).Exec(ctx, []string{"sh", "-c", "echo 'hi'"})
	if err != nil {
		t.Fatal(err)
	}
	if res != (probe.ExecResult{Stdout: "o", Stderr: "e", ExitCode: 2}) {
		t.Errorf("res = %+v", res)
	}
	if want := `pct exec 990002 -- 'sh' '-c' 'echo '\''hi'\'''`; ssh.CallLog()[0] != want {
		t.Errorf("cmd = %q, want %q", ssh.CallLog()[0], want)
	}
	if _, err := n.Executor(model.KindLXC, 990002).Exec(ctx, []string{"boom"}); err == nil {
		t.Error("transport error not returned")
	}
}

// OpenSSH does not pass a cancelled context on to the remote command, so
// pct exec is bounded on the node by coreutils timeout: the context's
// remaining time rounded up to whole seconds (at least 1s). Without a
// deadline there is no wrapper.
func TestLXCExecutorBoundsRemoteCommand(t *testing.T) {
	f := healthyFake()
	ssh := &sshx.Fake{}
	n := newTarget(t, f, f, ssh)
	ex := n.Executor(model.KindLXC, 990002)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second+500*time.Millisecond)
	defer cancel()
	if _, err := ex.Exec(ctx, []string{"true"}); err != nil {
		t.Fatal(err)
	}
	short, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	if _, err := ex.Exec(short, []string{"true"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Exec(context.Background(), []string{"true"}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"timeout --kill-after=5s 91s pct exec 990002 -- 'true'",
		"timeout --kill-after=5s 1s pct exec 990002 -- 'true'",
		"pct exec 990002 -- 'true'",
	}
	if !slices.Equal(ssh.CallLog(), want) {
		t.Errorf("cmds = %q, want %q", ssh.CallLog(), want)
	}
}

// Agent-exec polling honours its context, so the probe's per-command and
// whole-probe timeouts can cut a stuck command short.
func TestVMExecutorHonoursContext(t *testing.T) {
	f := healthyFake()
	runningVM(f, 990000, "1")
	api := &agentAPI{Fake: f, statuses: []pve.AgentExecStatus{{}}} // never exits
	n := newTarget(t, api, f, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := n.Executor(model.KindQEMU, 990000).Exec(ctx, []string{"sleep", "infinity"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Error("exec polling ignored the context")
	}
}

// pct exec passes its context to the SSH runner.
func TestLXCExecutorPassesContext(t *testing.T) {
	f := healthyFake()
	ctxRunner := ctxSSH{}
	n := newTarget(t, f, f, ctxRunner)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := n.Executor(model.KindLXC, 990002).Exec(ctx, []string{"true"}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

// ctxSSH fails with the context's error once it has ended.
type ctxSSH struct{}

func (ctxSSH) Run(ctx context.Context, cmd string, stdin []byte) (sshx.Result, error) {
	if err := ctx.Err(); err != nil {
		return sshx.Result{}, err
	}
	return sshx.Result{}, nil
}
