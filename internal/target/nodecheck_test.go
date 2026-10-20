package target

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sshx"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sshx/helperfake"
)

// nodeHelper answers like a correctly set up helper; mod changes its config
// reply, and exec answers an exec request (nil refuses it as out of range).
type nodeHelper struct {
	version string
	cfg     nodeproto.NodeConfig
	exec    func(req nodeproto.Request) sshx.Result
}

func goodNode() nodeHelper {
	return nodeHelper{version: "0.2.3", cfg: nodeproto.NodeConfig{
		VMIDRange: [2]int{lo, hi}, Bridge: bridge, RequestTimeout: 15 * time.Minute, Version: "0.2.3"}}
}

func (h nodeHelper) fake() *helperfake.Fake {
	return &helperfake.Fake{Handler: func(req nodeproto.Request, _ []byte) (sshx.Result, error) {
		switch req.Op {
		case "version":
			return sshx.Result{Stdout: h.version + "\n"}, nil
		case "config":
			return helperfake.JSON(h.cfg), nil
		case "exec":
			if h.exec != nil {
				return h.exec(req), nil
			}
			return helperfake.Refuse(req, "out-of-range", "container is outside the range"), nil
		}
		return sshx.Result{ExitCode: 127}, nil
	}}
}

func nodeTarget(t *testing.T, h nodeHelper) (*ExistingNode, *helperfake.Fake) {
	t.Helper()
	cfg := testConfig()
	cfg.Restore.ProbeTimeout = 5 * time.Minute
	ssh := h.fake()
	n := NewExistingNode(cfg, nil, sshx.Helper{R: ssh})
	n.SetDaemonVersion("0.2.0")
	return n, ssh
}

func TestCheckNode(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		mod  func(*nodeHelper)
		want []string // substrings of the error; nil = passes
	}{
		{name: "healthy"},
		{name: "incompatible version", mod: func(h *nodeHelper) { h.version = "0.3.0" },
			want: []string{"0.3.0", "0.2.0", "not compatible", `README "Node setup"`}},
		{name: "dev helper, release daemon", mod: func(h *nodeHelper) { h.version = "dev" },
			want: []string{"dev build is compatible only with a dev build", `README "Node setup"`}},
		{name: "vmid range mismatch", mod: func(h *nodeHelper) { h.cfg.VMIDRange = [2]int{lo, hi + 10} },
			want: []string{"vmid_range", "[990000, 990059]", "[990000, 990049]", `README "Node setup"`}},
		{name: "bridge mismatch", mod: func(h *nodeHelper) { h.cfg.Bridge = "vmbr0" },
			want: []string{"bridge", `"vmbr0"`, `"pbpiso0"`, `README "Node setup"`}},
		{name: "helper does not refuse", mod: func(h *nodeHelper) {
			h.exec = func(nodeproto.Request) sshx.Result { return sshx.Result{} }
		}, want: []string{"helper did not refuse an out-of-range exec", `README "Node setup"`}},
		{name: "refused for another reason", mod: func(h *nodeHelper) {
			h.exec = func(req nodeproto.Request) sshx.Result { return helperfake.Refuse(req, "untagged", "x") }
		}, want: []string{"helper did not refuse an out-of-range exec", "untagged"}},
		{name: "helper fails on exec", mod: func(h *nodeHelper) {
			h.exec = func(req nodeproto.Request) sshx.Result { return helperfake.Fail(req, "boom") }
		}, want: []string{"helper did not refuse an out-of-range exec"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := goodNode()
			if tc.mod != nil {
				tc.mod(&h)
			}
			n, _ := nodeTarget(t, h)
			err := n.CheckNode(ctx)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("CheckNode: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("CheckNode succeeded, want error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err %q does not mention %q", err, w)
				}
			}
		})
	}
}

func TestCheckNodeProbesJustAboveTheRange(t *testing.T) {
	n, ssh := nodeTarget(t, goodNode())
	if err := n.CheckNode(context.Background()); err != nil {
		t.Fatal(err)
	}
	var probe []nodeproto.Request
	for _, r := range ssh.Requests() {
		if r.Op == "exec" {
			probe = append(probe, r)
		}
	}
	if len(probe) != 1 || probe[0].VMID != hi+1 || len(probe[0].Argv) != 1 || probe[0].Argv[0] != "true" {
		t.Errorf("exec requests = %+v, want one `true` in %d", probe, hi+1)
	}
}

func TestCheckNodeStopsAtFirstFailure(t *testing.T) {
	h := goodNode()
	h.version = "0.3.0"
	n, ssh := nodeTarget(t, h)
	steps := n.CheckNodeSteps(context.Background())
	if len(steps) != 1 || steps[0].Err == nil || steps[0].Name != "node helper version" {
		t.Errorf("steps = %+v", steps)
	}
	if ops := ssh.Ops(); len(ops) != 1 || ops[0] != "version" {
		t.Errorf("ops = %q, want only version", ops)
	}
}

func TestCheckNodeUnreachable(t *testing.T) {
	cfg := testConfig()
	n := NewExistingNode(cfg, nil, sshx.Helper{R: &helperfake.Fake{Handler: func(nodeproto.Request, []byte) (sshx.Result, error) {
		return sshx.Result{}, errors.New("ssh: connection refused")
	}}})
	err := n.CheckNode(context.Background())
	if err == nil || !strings.Contains(err.Error(), "connection refused") || !strings.Contains(err.Error(), `README "Node setup"`) {
		t.Errorf("err = %v", err)
	}
}

// A request_timeout below the probe timeout is a warning, not a failure.
func TestCheckNodeLowRequestTimeoutWarns(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	h := goodNode()
	h.cfg.RequestTimeout = time.Minute
	n, _ := nodeTarget(t, h)
	steps := n.CheckNodeSteps(context.Background())
	if len(steps) != 4 {
		t.Fatalf("steps = %+v", steps)
	}
	for _, s := range steps {
		if s.Err != nil {
			t.Errorf("step %q failed: %v", s.Name, s.Err)
		}
	}
	if !strings.Contains(steps[2].Detail, "warning") || !strings.Contains(steps[2].Detail, "1m0s") {
		t.Errorf("request_timeout step = %+v", steps[2])
	}
	log := buf.String()
	if !strings.Contains(log, "level=WARN") || !strings.Contains(log, "request_timeout=1m0s") || !strings.Contains(log, "probe_timeout=5m0s") {
		t.Errorf("log = %q", log)
	}

	buf.Reset()
	h.cfg.RequestTimeout = 5 * time.Minute // equal is enough
	n, _ = nodeTarget(t, h)
	if err := n.CheckNode(context.Background()); err != nil || buf.Len() != 0 {
		t.Errorf("err = %v, log = %q", err, buf.String())
	}
}
