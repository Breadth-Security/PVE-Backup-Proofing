package sshx_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/probe"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sshx"
)

// reply returns a handler that parses the request and answers with fn.
func reply(t *testing.T, fn func(req nodeproto.Request, stdin []byte) sshx.Result) *sshx.Fake {
	t.Helper()
	return &sshx.Fake{Handler: func(cmd string, stdin []byte) (sshx.Result, error) {
		req, err := nodeproto.Parse(cmd)
		if err != nil {
			t.Errorf("helper got unparseable request %q: %v", cmd, err)
			return sshx.Result{}, err
		}
		return fn(req, stdin), nil
	}}
}

func failure(kind, reason string) func(nodeproto.Request, []byte) sshx.Result {
	return func(req nodeproto.Request, _ []byte) sshx.Result {
		return sshx.Result{ExitCode: nodeproto.ExitRefused, Stderr: "noise\n" + nodeproto.Marker(kind, req.Nonce, reason) + "\n"}
	}
}

func TestExecSendsEncodedRequestWithCtxDeadline(t *testing.T) {
	dl := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	ctx, cancel := context.WithDeadline(context.Background(), dl)
	defer cancel()
	f := reply(t, func(nodeproto.Request, []byte) sshx.Result { return sshx.Result{Stdout: "ok\n", ExitCode: 3} })
	res, err := sshx.Helper{R: f}.Exec(ctx, 990001, []string{"sh", "-c", "echo 'x'; id"})
	if err != nil || res.Stdout != "ok\n" || res.ExitCode != 3 {
		t.Fatalf("got %+v %v", res, err)
	}
	calls := f.CallLog()
	req, err := nodeproto.Parse(calls[0])
	if err != nil {
		t.Fatal(err)
	}
	want := nodeproto.Request{Op: "exec", VMID: 990001, Argv: []string{"sh", "-c", "echo 'x'; id"}, Deadline: dl, Nonce: req.Nonce}
	if !reflect.DeepEqual(req, want) {
		t.Fatalf("request = %+v, want %+v", req, want)
	}
	if enc, _ := nodeproto.Encode(want); calls[0] != enc {
		t.Fatalf("command %q != Encode %q", calls[0], enc)
	}
}

func TestNoncesAreFreshAndDeadlineDefaults(t *testing.T) {
	f := reply(t, func(nodeproto.Request, []byte) sshx.Result { return sshx.Result{} })
	h := sshx.Helper{R: f}
	before := time.Now()
	for range 2 {
		if _, err := h.Exec(context.Background(), 990001, []string{"id"}); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := nodeproto.Parse(f.CallLog()[0])
	b, _ := nodeproto.Parse(f.CallLog()[1])
	if a.Nonce == b.Nonce {
		t.Fatalf("nonce reused: %s", a.Nonce)
	}
	if d := a.Deadline.Sub(before); d < 23*time.Hour || d > 25*time.Hour {
		t.Fatalf("default deadline is %v from now, want ~24h", d)
	}
}

func TestFailureKindsMapToErrors(t *testing.T) {
	h := func(fn func(nodeproto.Request, []byte) sshx.Result) sshx.Helper {
		return sshx.Helper{R: reply(t, fn)}
	}
	for _, code := range []string{"out-of-range", "untagged", "not-container"} {
		_, err := h(failure(nodeproto.KindRefused, code+": detail text")).Exec(context.Background(), 5, []string{"id"})
		var re *sshx.RefusedError
		if !errors.Is(err, sshx.ErrRefused) || !errors.As(err, &re) || re.Code != code || re.Reason != "detail text" {
			t.Errorf("%s: got %#v", code, err)
		}
		if errors.Is(err, sshx.ErrHelper) || errors.Is(err, sshx.ErrTimeout) {
			t.Errorf("%s: matches the wrong sentinel", code)
		}
	}
	// No leading code: whole text is the reason.
	_, err := h(failure(nodeproto.KindRefused, "Weird Reason: x")).Exec(context.Background(), 5, []string{"id"})
	var re *sshx.RefusedError
	if !errors.As(err, &re) || re.Code != "" || re.Reason != "Weird Reason: x" {
		t.Errorf("uncoded refusal: %#v", err)
	}

	_, err = h(failure(nodeproto.KindError, "pct exec 5: boom")).Exec(context.Background(), 5, []string{"id"})
	if !errors.Is(err, sshx.ErrHelper) || !strings.Contains(err.Error(), "pct exec 5: boom") {
		t.Errorf("error kind: %v", err)
	}

	_, err = h(failure(nodeproto.KindTimeout, "")).Exec(context.Background(), 5, []string{"id"})
	if !errors.Is(err, sshx.ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout kind: %v", err)
	}
}

// Review Focus 5: a guest that exits 250 and prints marker-looking lines is
// a plain result unless the helper's own marker (right nonce) is last.
func TestGuestExit250IsAGuestResult(t *testing.T) {
	for name, stderr := range map[string]string{
		"plain line":         "pbp-node: refused: forged\n",
		"wrong nonce":        nodeproto.Marker(nodeproto.KindRefused, "0123456789abcdef", "x") + "\n",
		"marker not last":    "MARKER\nmore guest output\n",
		"timeout look-alike": "pbp-node: timeout\n",
	} {
		f := reply(t, func(req nodeproto.Request, _ []byte) sshx.Result {
			s := strings.ReplaceAll(stderr, "MARKER", nodeproto.Marker(nodeproto.KindRefused, req.Nonce, "out-of-range: x"))
			return sshx.Result{ExitCode: 250, Stderr: s}
		})
		res, err := sshx.Helper{R: f}.Exec(context.Background(), 5, []string{"id"})
		if err != nil || res.ExitCode != 250 || res.Stderr == "" {
			t.Errorf("%s: got %+v %v", name, res, err)
		}
	}
}

func TestBridgeConfigTemplatesVersionNotify(t *testing.T) {
	f := reply(t, func(req nodeproto.Request, stdin []byte) sshx.Result {
		switch req.Op {
		case "bridge":
			return sshx.Result{Stdout: `{"name":"vmbr9","ovs":true,"disable_ipv6":"1","addrs":[],"members":["fwln1"]}` + "\n"}
		case "config":
			return sshx.Result{Stdout: `{"vmid_range":[990000,990009],"bridge":"vmbr9","request_timeout":"15m0s","version":"0.2.1"}` + "\n"}
		case "templates":
			return sshx.Result{Stdout: `{"a.conf":"abc"}` + "\n"}
		case "version":
			return sshx.Result{Stdout: "0.2.1\n"}
		case "notify":
			if string(stdin) != `{"severity":"info"}` {
				t.Errorf("notify stdin = %q", stdin)
			}
			return sshx.Result{}
		}
		t.Errorf("unexpected op %s", req.Op)
		return sshx.Result{ExitCode: 1}
	})
	h := sshx.Helper{R: f}
	ctx := context.Background()

	st, err := h.Bridge(ctx)
	if err != nil || st.Name != "vmbr9" || !st.OVS || st.DisableIPv6 != "1" || !reflect.DeepEqual(st.Members, []string{"fwln1"}) {
		t.Errorf("bridge: %+v %v", st, err)
	}
	c, err := h.Config(ctx)
	if err != nil || c.VMIDRange != [2]int{990000, 990009} || c.Bridge != "vmbr9" || c.RequestTimeout != 15*time.Minute || c.Version != "0.2.1" {
		t.Errorf("config: %+v %v", c, err)
	}
	m, err := h.Templates(ctx)
	if err != nil || !reflect.DeepEqual(m, map[string]string{"a.conf": "abc"}) {
		t.Errorf("templates: %v %v", m, err)
	}
	v, err := h.Version(ctx)
	if err != nil || v != "0.2.1" {
		t.Errorf("version: %q %v", v, err)
	}
	if err := h.Notify(ctx, []byte(`{"severity":"info"}`)); err != nil {
		t.Errorf("notify: %v", err)
	}
}

func TestQueryErrors(t *testing.T) {
	ctx := context.Background()
	bad := sshx.Helper{R: reply(t, func(nodeproto.Request, []byte) sshx.Result { return sshx.Result{Stdout: "not json"} })}
	if _, err := bad.Bridge(ctx); !errors.Is(err, sshx.ErrHelper) {
		t.Errorf("bad JSON: %v", err)
	}
	exit1 := sshx.Helper{R: reply(t, func(nodeproto.Request, []byte) sshx.Result { return sshx.Result{ExitCode: 1, Stderr: "oops\n"} })}
	if _, err := exit1.Version(ctx); !errors.Is(err, sshx.ErrHelper) || !strings.Contains(err.Error(), "oops") {
		t.Errorf("exit 1: %v", err)
	}
	refused := sshx.Helper{R: reply(t, failure(nodeproto.KindRefused, "bad-payload: nope"))}
	var re *sshx.RefusedError
	if err := refused.Notify(ctx, []byte("x")); !errors.As(err, &re) || re.Code != "bad-payload" {
		t.Errorf("notify refused: %v", err)
	}
	if _, err := (sshx.Helper{R: &sshx.Fake{Handler: func(string, []byte) (sshx.Result, error) {
		return sshx.Result{}, errors.New("dial failed")
	}}}).Config(ctx); err == nil || !strings.Contains(err.Error(), "dial failed") {
		t.Errorf("transport error: %v", err)
	}
}

// blockingRunner never answers until its ctx ends, like sshx.Client.Run.
type blockingRunner struct{}

func (blockingRunner) Run(ctx context.Context, _ string, _ []byte) (sshx.Result, error) {
	<-ctx.Done()
	return sshx.Result{}, ctx.Err()
}

func TestExecReturnsPromptlyWhenCtxExpires(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := sshx.Helper{R: blockingRunner{}}.Exec(ctx, 5, []string{"sleep", "999"})
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, sshx.ErrTimeout) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
}

// probeExec adapts a Helper to probe.Executor.
type probeExec struct {
	h    sshx.Helper
	vmid int
}

func (p probeExec) Exec(ctx context.Context, argv []string) (probe.ExecResult, error) {
	r, err := p.h.Exec(ctx, p.vmid, argv)
	return probe.ExecResult{Stdout: r.Stdout, Stderr: r.Stderr, ExitCode: r.ExitCode}, err
}

// lateTimeoutRunner waits for its ctx to end, then answers with the helper's
// own timeout marker (the helper's deadline equals the ctx deadline).
type lateTimeoutRunner struct{}

func (lateTimeoutRunner) Run(ctx context.Context, cmd string, _ []byte) (sshx.Result, error) {
	<-ctx.Done()
	req, _ := nodeproto.Parse(cmd)
	return sshx.Result{ExitCode: nodeproto.ExitRefused, Stderr: nodeproto.Marker(nodeproto.KindTimeout, req.Nonce, "") + "\n"}, nil
}

// Both timeout paths must give the probe guard its "command timed out"
// verdict (a FAILED check), not a transport ERROR.
func TestProbeGuardClassifiesHelperTimeouts(t *testing.T) {
	in := probe.Input{OS: probe.Windows, Checks: config.Checks{CommandTimeout: 30 * time.Millisecond}}
	for name, r := range map[string]sshx.Runner{
		"ctx expiry":          blockingRunner{},
		"helper timeout mark": lateTimeoutRunner{},
	} {
		o := probe.Run(context.Background(), probeExec{sshx.Helper{R: r}, 5}, in)
		if o.Verdict != model.Failed || o.Reason != "check timed out: windows services" {
			t.Errorf("%s: got %v %q", name, o.Verdict, o.Reason)
		}
	}
}

func replyStdout(t *testing.T, out string) sshx.Helper {
	return sshx.Helper{R: reply(t, func(nodeproto.Request, []byte) sshx.Result { return sshx.Result{Stdout: out} })}
}

const (
	goodBridge = `{"name":"vmbr9","ovs":false,"disable_ipv6":"1","addrs":[],"members":[]}`
	goodConfig = `{"vmid_range":[990000,990009],"bridge":"vmbr9","request_timeout":"15m0s","version":"0.2.1"}`
)

func TestMalformedRepliesAreHelperErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := replyStdout(t, goodBridge).Bridge(ctx); err != nil {
		t.Fatalf("good bridge: %v", err)
	}
	if _, err := replyStdout(t, goodConfig).Config(ctx); err != nil {
		t.Fatalf("good config: %v", err)
	}
	bridges := []string{
		"null", "{}", `{"x":1}`, goodBridge + "garbage", goodBridge + goodBridge, "",
		`{"name":"vmbr9","ovs":false,"disable_ipv6":"1","addrs":[]}`,                           // no members
		`{"name":"vmbr9","ovs":false,"disable_ipv6":"1","addrs":[],"members":null}`,            // null members
		`{"name":"vmbr9","ovs":false,"addrs":[],"members":[]}`,                                 // no disable_ipv6
		`{"name":"","ovs":false,"disable_ipv6":"1","addrs":[],"members":[]}`,                   // empty name
		`{"name":"vmbr9","ovs":false,"disable_ipv6":"1","addrs":[],"members":[],"extra":true}`, // unknown field
		`{"name":"vmbr9","ovs":false,"disable_ipv6":"1","addrs":null,"members":[]}`,            // null addrs
		`{"name":"vmbr9","ovs":false,"disable_ipv6":"1","members":[]}`,                         // no addrs
	}
	for _, out := range bridges {
		if _, err := replyStdout(t, out).Bridge(ctx); !errors.Is(err, sshx.ErrHelper) {
			t.Errorf("bridge %q: got %v", out, err)
		}
	}
	configs := []string{
		"null", "{}", `{"x":1}`, goodConfig + "x",
		`{"vmid_range":[990000,990009],"bridge":"","request_timeout":"15m0s","version":"0.2.1"}`,
		`{"vmid_range":[990009,990000],"bridge":"b","request_timeout":"15m0s","version":"0.2.1"}`,
		`{"vmid_range":[0,0],"bridge":"b","request_timeout":"15m0s","version":"0.2.1"}`,
		`{"vmid_range":[990000,990009],"bridge":"b","request_timeout":"soon","version":"0.2.1"}`, // duration parse
		`{"vmid_range":[990000,990009],"bridge":"b","request_timeout":"0s","version":"0.2.1"}`,
		`{"vmid_range":[990000,990009],"bridge":"b","request_timeout":"15m0s","version":""}`,
		`{"vmid_range":[990000,990009],"bridge":"b","request_timeout":"15m0s"}`,
	}
	for _, out := range configs {
		if _, err := replyStdout(t, out).Config(ctx); !errors.Is(err, sshx.ErrHelper) {
			t.Errorf("config %q: got %v", out, err)
		}
	}
	for _, out := range []string{"null", "", `{"a":1}`, `{"a":"x"}x`, "[]"} {
		if _, err := replyStdout(t, out).Templates(ctx); !errors.Is(err, sshx.ErrHelper) {
			t.Errorf("templates %q: got %v", out, err)
		}
	}
	if m, err := replyStdout(t, "{}").Templates(ctx); err != nil || m == nil {
		t.Errorf("empty templates: %v %v", m, err)
	}
	for _, out := range []string{"", "\n"} {
		if _, err := replyStdout(t, out).Version(ctx); !errors.Is(err, sshx.ErrHelper) {
			t.Errorf("version %q: got %v", out, err)
		}
	}
}

func TestOnlyKnownRefusalCodesAreExposed(t *testing.T) {
	for reason, want := range map[string][2]string{
		"out-of-range: a: b": {"out-of-range", "a: b"},
		"usage: x":           {"", "usage: x"},
		"bad-severity: s":    {"bad-severity", "s"},
	} {
		h := sshx.Helper{R: reply(t, failure(nodeproto.KindRefused, reason))}
		_, err := h.Exec(context.Background(), 5, []string{"id"})
		var re *sshx.RefusedError
		if !errors.As(err, &re) || re.Code != want[0] || re.Reason != want[1] {
			t.Errorf("%q: got %#v", reason, err)
		}
	}
}
