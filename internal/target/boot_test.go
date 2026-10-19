package target

import (
	"context"
	"errors"
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
	return pve.OSInfo{ID: a.osID}, nil
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
	if _, err := n.WaitBooted(context.Background(), model.KindQEMU, 990000, 20*time.Millisecond); err == nil {
		t.Error("want error for guest that never runs")
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
		if ssh.Calls[0] != "pct exec 990001 -- 'true'" {
			t.Errorf("ssh calls = %q", ssh.Calls)
		}
		for _, c := range ssh.Calls {
			if strings.Contains(c, "pct status") {
				t.Errorf("must use API status, not %q", c)
			}
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
		if _, err := newTarget(t, f, f, ssh).WaitBooted(ctx, model.KindLXC, 990001, 30*time.Millisecond); err == nil {
			t.Error("want timeout error")
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
	if len(ssh.Calls) != 0 || len(f.CallLog()) != 0 {
		t.Errorf("calls made: %q %q", ssh.Calls, f.CallLog())
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
	if want := `pct exec 990002 -- 'sh' '-c' 'echo '\''hi'\'''`; ssh.Calls[0] != want {
		t.Errorf("cmd = %q, want %q", ssh.Calls[0], want)
	}
	if _, err := n.Executor(model.KindLXC, 990002).Exec(ctx, []string{"boom"}); err == nil {
		t.Error("transport error not returned")
	}
}
