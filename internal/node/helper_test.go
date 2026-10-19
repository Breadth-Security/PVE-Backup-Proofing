package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
)

const testNonce = "0123456789abcdef"

var testCfg = Config{VMIDRange: [2]int{990000, 990009}, Bridge: "vmbr99", RequestTimeout: 15 * time.Minute}

type execCall struct {
	VMID  int
	Argv  []string
	Stdin string
}

// fakeSystem records calls. Guests maps VMID to its `pct config`; a VMID not
// in the map behaves like a VM or a missing guest (PctConfig fails).
type fakeSystem struct {
	mu        sync.Mutex
	Guests    map[int]map[string]string
	ExecFn    func(ctx context.Context, stdout, stderr io.Writer) (int, error)
	Bridge    nodeproto.BridgeState
	BridgeErr error
	NotifyErr error
	Hashes    map[string]string

	configCalls []int
	execCalls   []execCall
	bridgeCalls []string
	notified    [][]byte
}

func (f *fakeSystem) CTConfig(ctx context.Context, vmid int) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configCalls = append(f.configCalls, vmid)
	if c, ok := f.Guests[vmid]; ok {
		return c, nil
	}
	return nil, fmt.Errorf("open /etc/pve/lxc/%d.conf: no such file or directory", vmid)
}

func (f *fakeSystem) PctExec(ctx context.Context, vmid int, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	in, _ := io.ReadAll(stdin)
	f.mu.Lock()
	f.execCalls = append(f.execCalls, execCall{vmid, argv, string(in)})
	fn := f.ExecFn
	f.mu.Unlock()
	if fn == nil {
		return 0, nil
	}
	return fn(ctx, stdout, stderr)
}

func (f *fakeSystem) BridgeState(ctx context.Context, bridge string) (nodeproto.BridgeState, error) {
	f.bridgeCalls = append(f.bridgeCalls, bridge)
	return f.Bridge, f.BridgeErr
}

func (f *fakeSystem) Notify(ctx context.Context, payload []byte) error {
	f.notified = append(f.notified, payload)
	return f.NotifyErr
}

func (f *fakeSystem) TemplateHashes() (map[string]string, error) { return f.Hashes, nil }

func newFake() *fakeSystem {
	return &fakeSystem{Guests: map[int]map[string]string{
		990001: {"hostname": "proof-1", "tags": "pbp", "arch": "amd64"},
		990002: {"hostname": "untagged"},
		990003: {"hostname": "other-tags", "tags": "prod;pbpx;web"},
		990004: {"hostname": "multi-tags", "tags": "keep;pbp"},
		101:    {"hostname": "production", "tags": "pbp"},
	}}
}

type result struct {
	exit           int
	stdout, stderr string
}

func serve(t *testing.T, sys System, request, stdin string) result {
	t.Helper()
	var out, errb bytes.Buffer
	exit := Serve(context.Background(), testCfg, sys, "0.2.0", request, strings.NewReader(stdin), &out, &errb)
	return result{exit, out.String(), errb.String()}
}

func req(t *testing.T, r nodeproto.Request) string {
	t.Helper()
	if r.Nonce == "" {
		r.Nonce = testNonce
	}
	if r.Deadline.IsZero() {
		r.Deadline = time.Now().Add(time.Minute)
	}
	s, err := nodeproto.Encode(r)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func future() string { return fmt.Sprint(time.Now().Add(time.Minute).UnixMilli()) }

func lastLine(s string) string {
	s = strings.TrimRight(s, "\n")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func assertHelperFailure(t *testing.T, r result, kind string) {
	t.Helper()
	if r.exit != nodeproto.ExitRefused {
		t.Errorf("exit = %d, want %d (stderr %q)", r.exit, nodeproto.ExitRefused, r.stderr)
	}
	k, _, ok := nodeproto.HelperFailure(r.exit, r.stderr, testNonce)
	if !ok || k != kind {
		t.Errorf("last stderr line %q is not a %s marker for nonce %s", lastLine(r.stderr), kind, testNonce)
	}
	if !strings.HasSuffix(r.stderr, "\n") {
		t.Errorf("stderr %q does not end in a newline", r.stderr)
	}
}

// Review Focus 1: injection through argv or VMID, malformed requests.
func TestServeRefusesMalformedRequests(t *testing.T) {
	dl := future()
	for _, tc := range []struct{ name, request string }{
		{"shell in vmid", "exec --deadline " + dl + " --nonce " + testNonce + " 990001;id -- [\"id\"]"},
		{"vmid too long", "exec --deadline " + dl + " --nonce " + testNonce + " 9900010000 -- [\"id\"]"},
		{"negative vmid", "exec --deadline " + dl + " --nonce " + testNonce + " -1 -- [\"id\"]"},
		{"unquoted argv", "exec --deadline " + dl + " --nonce " + testNonce + " 990001 -- sh -c 'id'; reboot"},
		{"newline in argv", "exec --deadline " + dl + " --nonce " + testNonce + " 990001 -- [\"id\n\"]"},
		{"extra args on version", "version --deadline " + dl + " --nonce " + testNonce + " extra"},
		{"unknown op", "install-templates --deadline " + dl + " --nonce " + testNonce},
		{"shell op", "sh --deadline " + dl + " --nonce " + testNonce},
		{"no deadline", "version --nonce " + testNonce},
		{"empty argv", "exec --deadline " + dl + " --nonce " + testNonce + " 990001 -- []"},
		{"non-string argv", "exec --deadline " + dl + " --nonce " + testNonce + " 990001 -- [1]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			r := serve(t, f, tc.request, "")
			assertHelperFailure(t, r, nodeproto.KindRefused)
			if len(f.execCalls) != 0 || len(f.configCalls) != 0 {
				t.Errorf("pct called: exec %v config %v", f.execCalls, f.configCalls)
			}
		})
	}
}

func TestServeRefusesUnparseableWithoutNonce(t *testing.T) {
	for _, request := range []string{"", "garbage", "exec --deadline 1 --nonce XYZ", "exec 990001 -- [\"id\"]\nreboot", `{"op":"exec","vmid":990001,"argv":["id"],"extra":1}`} {
		f := newFake()
		r := serve(t, f, request, "")
		if r.exit != nodeproto.ExitRefused {
			t.Errorf("%q: exit %d", request, r.exit)
		}
		if !strings.HasPrefix(lastLine(r.stderr), "pbp-node: refused: ") {
			t.Errorf("%q: last line %q", request, lastLine(r.stderr))
		}
		if strings.Count(r.stderr, "\n") != 1 {
			t.Errorf("%q: marker is not a single line: %q", request, r.stderr)
		}
		if len(f.execCalls) != 0 {
			t.Errorf("%q: pct exec called", request)
		}
	}
}

// A request rejected by Parse whose nonce is still syntactically present
// gets a nonce-carrying marker, so the daemon sees a proper refusal.
func TestServeRefusalUsesSyntacticNonce(t *testing.T) {
	r := serve(t, newFake(), "exec --deadline "+future()+" --nonce "+testNonce+" 990001;id -- [\"id\"]", "")
	assertHelperFailure(t, r, nodeproto.KindRefused)
}

// Review Focus 2: out of range, untagged, not an LXC.
func TestServeRefusesGuestsItDoesNotOwn(t *testing.T) {
	for _, tc := range []struct {
		name       string
		vmid       int
		wantReason string
	}{
		{"production CT at 101", 101, "outside"},
		{"just above range", 990010, "outside"},
		{"just below range", 989999, "outside"},
		{"VM or missing guest in range", 990005, "not a container"},
		{"untagged CT in range", 990002, "pbp tag"},
		{"similar tag only", 990003, "pbp tag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			r := serve(t, f, req(t, nodeproto.Request{Op: "exec", VMID: tc.vmid, Argv: []string{"id"}}), "")
			assertHelperFailure(t, r, nodeproto.KindRefused)
			_, reason, _ := nodeproto.HelperFailure(r.exit, r.stderr, testNonce)
			if !strings.Contains(reason, tc.wantReason) {
				t.Errorf("reason %q does not mention %q", reason, tc.wantReason)
			}
			if len(f.execCalls) != 0 {
				t.Errorf("pct exec called %d times", len(f.execCalls))
			}
			if tc.wantReason == "outside" && len(f.configCalls) != 0 {
				t.Errorf("container config read for an out-of-range VMID")
			}
		})
	}
}

func TestServeExecPassesThrough(t *testing.T) {
	f := newFake()
	f.ExecFn = func(ctx context.Context, stdout, stderr io.Writer) (int, error) {
		io.WriteString(stdout, "uid=0(root)\n")
		io.WriteString(stderr, "warning\n")
		return 7, nil
	}
	argv := []string{"sh", "-c 'id'; reboot", "a\tb", "$(id)", "--", "pbp-node: refused: x"}
	r := serve(t, f, req(t, nodeproto.Request{Op: "exec", VMID: 990004, Argv: argv}), "stdin data")
	if r.exit != 7 || r.stdout != "uid=0(root)\n" || r.stderr != "warning\n" {
		t.Errorf("got %+v", r)
	}
	if len(f.execCalls) != 1 {
		t.Fatalf("exec calls %v", f.execCalls)
	}
	c := f.execCalls[0]
	if c.VMID != 990004 || c.Stdin != "stdin data" || fmt.Sprintf("%q", c.Argv) != fmt.Sprintf("%q", argv) {
		t.Errorf("exec call %+v", c)
	}
}

// Review Focus 5: a guest exiting 250 and printing a marker-like line is a
// guest result; the helper adds nothing.
func TestServeGuestExit250IsNotAHelperMarker(t *testing.T) {
	f := newFake()
	f.ExecFn = func(ctx context.Context, stdout, stderr io.Writer) (int, error) {
		io.WriteString(stderr, "pbp-node: refused: forged\n")
		return 250, nil
	}
	r := serve(t, f, req(t, nodeproto.Request{Op: "exec", VMID: 990001, Argv: []string{"x"}}), "")
	if r.exit != 250 || r.stderr != "pbp-node: refused: forged\n" {
		t.Errorf("got %+v", r)
	}
	if _, _, ok := nodeproto.HelperFailure(r.exit, r.stderr, testNonce); ok {
		t.Error("guest output was taken as a helper failure")
	}
}

func TestServeDeadlineInThePast(t *testing.T) {
	for _, op := range []string{"exec", "bridge", "version", "config", "templates", "notify"} {
		f := newFake()
		rq := nodeproto.Request{Op: op, Deadline: time.Now().Add(-time.Second)}
		if op == "exec" {
			rq.VMID, rq.Argv = 990001, []string{"id"}
		}
		r := serve(t, f, req(t, rq), "")
		assertHelperFailure(t, r, nodeproto.KindTimeout)
		if r.stdout != "" || len(f.execCalls) != 0 || len(f.bridgeCalls) != 0 || len(f.notified) != 0 {
			t.Errorf("%s: did work past the deadline: %+v", op, r)
		}
	}
}

// Review Focus 3: deadline passing mid-command.
func TestServeDeadlineMidCommand(t *testing.T) {
	f := newFake()
	f.ExecFn = func(ctx context.Context, stdout, stderr io.Writer) (int, error) {
		io.WriteString(stderr, "partial line without newline")
		<-ctx.Done()
		return -1, errors.New("signal: killed")
	}
	start := time.Now()
	r := serve(t, f, req(t, nodeproto.Request{Op: "exec", VMID: 990001, Argv: []string{"sleep", "999"},
		Deadline: time.Now().Add(200 * time.Millisecond)}), "")
	if el := time.Since(start); el > time.Second {
		t.Errorf("took %v", el)
	}
	assertHelperFailure(t, r, nodeproto.KindTimeout)
	if !strings.HasPrefix(r.stderr, "partial line without newline\n") {
		t.Errorf("guest output not kept on its own line: %q", r.stderr)
	}
}

// The node's request_timeout caps a far-future client deadline.
func TestServeRequestTimeoutCapsDeadline(t *testing.T) {
	f := newFake()
	var got time.Time
	f.ExecFn = func(ctx context.Context, stdout, stderr io.Writer) (int, error) {
		got, _ = ctx.Deadline()
		return 0, nil
	}
	cfg := testCfg
	cfg.RequestTimeout = time.Minute
	rq := req(t, nodeproto.Request{Op: "exec", VMID: 990001, Argv: []string{"id"}, Deadline: time.Now().Add(time.Hour)})
	Serve(context.Background(), cfg, f, "0.2.0", rq, strings.NewReader(""), io.Discard, io.Discard)
	if d := time.Until(got); d > time.Minute || d < 50*time.Second {
		t.Errorf("effective deadline in %v, want about 1m", d)
	}
}

func TestServeCancelledContextIsTimeout(t *testing.T) {
	f := newFake()
	ctx, cancel := context.WithCancel(context.Background())
	f.ExecFn = func(c context.Context, stdout, stderr io.Writer) (int, error) {
		cancel()
		<-c.Done()
		return -1, errors.New("killed")
	}
	var out, errb bytes.Buffer
	exit := Serve(ctx, testCfg, f, "0.2.0", req(t, nodeproto.Request{Op: "exec", VMID: 990001, Argv: []string{"id"}}), strings.NewReader(""), &out, &errb)
	assertHelperFailure(t, result{exit, out.String(), errb.String()}, nodeproto.KindTimeout)
}

func TestServeExecSystemError(t *testing.T) {
	f := newFake()
	f.ExecFn = func(ctx context.Context, stdout, stderr io.Writer) (int, error) {
		return -1, errors.New("exec: \"pct\": not found\nsecond line")
	}
	r := serve(t, f, req(t, nodeproto.Request{Op: "exec", VMID: 990001, Argv: []string{"id"}}), "")
	assertHelperFailure(t, r, nodeproto.KindError)
	if strings.Count(r.stderr, "\n") != 1 {
		t.Errorf("multi-line marker: %q", r.stderr)
	}
}

func TestServeBridge(t *testing.T) {
	f := newFake()
	f.Bridge = nodeproto.BridgeState{Name: "vmbr99", DisableIPv6: "1", Addrs: json.RawMessage(`[]`), Members: []string{"veth990001i0"}}
	r := serve(t, f, req(t, nodeproto.Request{Op: "bridge"}), "")
	if r.exit != 0 || r.stderr != "" {
		t.Fatalf("got %+v", r)
	}
	if len(f.bridgeCalls) != 1 || f.bridgeCalls[0] != "vmbr99" {
		t.Errorf("bridge calls %v", f.bridgeCalls)
	}
	var got nodeproto.BridgeState
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "vmbr99" || got.DisableIPv6 != "1" || len(got.Members) != 1 {
		t.Errorf("got %+v", got)
	}
}

func TestServeBridgeError(t *testing.T) {
	f := newFake()
	f.BridgeErr = errors.New("read disable_ipv6: no such file")
	r := serve(t, f, req(t, nodeproto.Request{Op: "bridge"}), "")
	assertHelperFailure(t, r, nodeproto.KindError)
}

func TestServeNotify(t *testing.T) {
	f := newFake()
	in := `{"severity":"warning","data":{"title":"x"},"fields":{"type":"other","hostname":"pve1"}}`
	r := serve(t, f, req(t, nodeproto.Request{Op: "notify"}), in)
	if r.exit != 0 || r.stderr != "" {
		t.Fatalf("got %+v", r)
	}
	if len(f.notified) != 1 {
		t.Fatalf("notified %d times", len(f.notified))
	}
	var p struct {
		Severity string            `json:"severity"`
		Data     map[string]any    `json:"data"`
		Fields   map[string]string `json:"fields"`
	}
	if err := json.Unmarshal(f.notified[0], &p); err != nil {
		t.Fatal(err)
	}
	if p.Severity != "warning" || p.Fields["type"] != "pbp" || p.Fields["hostname"] != "pve1" || p.Data["title"] != "x" {
		t.Errorf("payload %s", f.notified[0])
	}
}

func TestServeNotifyNoFieldsGetsType(t *testing.T) {
	f := newFake()
	r := serve(t, f, req(t, nodeproto.Request{Op: "notify"}), `{"severity":"info","data":{}}`)
	if r.exit != 0 {
		t.Fatalf("got %+v", r)
	}
	if !strings.Contains(string(f.notified[0]), `"type":"pbp"`) {
		t.Errorf("payload %s", f.notified[0])
	}
}

func TestServeNotifyRefusals(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"debug severity", `{"severity":"debug","data":{},"fields":{}}`},
		{"empty severity", `{"data":{},"fields":{}}`},
		{"not json", `severity=info`},
		{"unknown key", `{"severity":"info","data":{},"template":"evil"}`},
		{"data not object", `{"severity":"info","data":"x"}`},
		{"non-string field", `{"severity":"info","data":{},"fields":{"type":1}}`},
		{"trailing data", `{"severity":"info","data":{}} {}`},
		{"too large", `{"severity":"info","data":{"x":"` + strings.Repeat("a", maxNotifyPayload) + `"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			r := serve(t, f, req(t, nodeproto.Request{Op: "notify"}), tc.in)
			assertHelperFailure(t, r, nodeproto.KindRefused)
			if len(f.notified) != 0 {
				t.Error("Notify called")
			}
		})
	}
}

// A notify whose stdin never delivers must not outlive the deadline.
func TestServeNotifyStdinBlocksPastDeadline(t *testing.T) {
	f := newFake()
	pr, pw := io.Pipe()
	defer pw.Close()
	var out, errb bytes.Buffer
	start := time.Now()
	exit := Serve(context.Background(), testCfg, f, "0.2.0",
		req(t, nodeproto.Request{Op: "notify", Deadline: time.Now().Add(200 * time.Millisecond)}), pr, &out, &errb)
	if el := time.Since(start); el > time.Second {
		t.Errorf("took %v", el)
	}
	assertHelperFailure(t, result{exit, out.String(), errb.String()}, nodeproto.KindTimeout)
	if len(f.notified) != 0 {
		t.Error("Notify called")
	}
}

func TestServeNotifyError(t *testing.T) {
	f := newFake()
	f.NotifyErr = errors.New("ERROR: could not render template")
	r := serve(t, f, req(t, nodeproto.Request{Op: "notify"}), `{"severity":"error","data":{}}`)
	assertHelperFailure(t, r, nodeproto.KindError)
	_, reason, _ := nodeproto.HelperFailure(r.exit, r.stderr, testNonce)
	if !strings.Contains(reason, "could not render template") {
		t.Errorf("reason %q", reason)
	}
}

func TestServeConfig(t *testing.T) {
	r := serve(t, newFake(), req(t, nodeproto.Request{Op: "config"}), "")
	if r.exit != 0 {
		t.Fatalf("got %+v", r)
	}
	var got nodeproto.NodeConfig
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatal(err)
	}
	want := nodeproto.NodeConfig{VMIDRange: testCfg.VMIDRange, Bridge: "vmbr99", RequestTimeout: 15 * time.Minute, Version: "0.2.0"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if !strings.Contains(r.stdout, `"request_timeout":"15m0s"`) {
		t.Errorf("stdout %s", r.stdout)
	}
}

func TestServeTemplates(t *testing.T) {
	f := newFake()
	f.Hashes = map[string]string{"pbp-body.txt.hbs": "abc", "pbp-subject.txt.hbs": ""}
	r := serve(t, f, req(t, nodeproto.Request{Op: "templates"}), "")
	if r.exit != 0 {
		t.Fatalf("got %+v", r)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["pbp-body.txt.hbs"] != "abc" || got["pbp-subject.txt.hbs"] != "" {
		t.Errorf("got %v", got)
	}
}

func TestServeVersion(t *testing.T) {
	r := serve(t, newFake(), req(t, nodeproto.Request{Op: "version"}), "")
	if r.exit != 0 || r.stdout != "0.2.0\n" || r.stderr != "" {
		t.Errorf("got %+v", r)
	}
}
