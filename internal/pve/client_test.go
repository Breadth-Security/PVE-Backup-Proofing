package pve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
)

const testToken = "root@pam!pbp"

func fingerprint(srv *httptest.Server) string {
	sum := sha256.Sum256(srv.Certificate().Raw)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	var parts []string
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

func quietTLSServer(h http.Handler) *httptest.Server {
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	return srv
}

func newTestClient(t *testing.T, h http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := quietTLSServer(h)
	t.Cleanup(srv.Close)
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(config.API{URL: srv.URL, TokenID: testToken, TokenSecretFile: secret, TLSFingerprint: fingerprint(srv)})
	if err != nil {
		t.Fatal(err)
	}
	c.pollInterval = time.Millisecond
	return c, srv
}

func TestVersionAndAuthHeader(t *testing.T) {
	var gotAuth, gotPath string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_, _ = w.Write([]byte(`{"data":{"version":"9.2.18","release":"9.2"}}`))
	}))
	v, err := c.Version(context.Background())
	if err != nil || v != "9.2.18" {
		t.Fatalf("Version = %q, %v", v, err)
	}
	if gotAuth != "PVEAPIToken=root@pam!pbp=s3cret" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if gotPath != "/api2/json/version" {
		t.Errorf("path = %q", gotPath)
	}
}

func TestBaseURLNormalisation(t *testing.T) {
	var gotPath string
	srv := quietTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"data":{"version":"9.0.0"}}`))
	}))
	defer srv.Close()
	secret := filepath.Join(t.TempDir(), "s")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{srv.URL + "/", srv.URL + "/api2/json", srv.URL + "/api2/json/"} {
		c, err := New(config.API{URL: u, TokenID: testToken, TokenSecretFile: secret, TLSFingerprint: fingerprint(srv)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Version(context.Background()); err != nil {
			t.Fatal(err)
		}
		if gotPath != "/api2/json/version" {
			t.Errorf("url %q: path = %q", u, gotPath)
		}
	}
}

func TestFingerprintPin(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"data":{"version":"9"}}`)) })
	srv := quietTLSServer(h)
	defer srv.Close()
	secret := filepath.Join(t.TempDir(), "s")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// lower-case, no-colon variants are accepted too
	lower := strings.ToLower(fingerprint(srv))
	c, err := New(config.API{URL: srv.URL, TokenID: testToken, TokenSecretFile: secret, TLSFingerprint: lower})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Version(context.Background()); err != nil {
		t.Errorf("matching fingerprint rejected: %v", err)
	}

	wrong := strings.Repeat("AA:", 31) + "AA"
	c, err = New(config.API{URL: srv.URL, TokenID: testToken, TokenSecretFile: secret, TLSFingerprint: wrong})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Version(context.Background()); err == nil {
		t.Error("wrong fingerprint accepted")
	} else if !strings.Contains(err.Error(), "does not match pinned fingerprint") {
		t.Errorf("error does not mention the pin mismatch: %v", err)
	}
}

func TestNoTLSConfigRejectsSelfSigned(t *testing.T) {
	srv := quietTLSServer(http.NotFoundHandler())
	defer srv.Close()
	secret := filepath.Join(t.TempDir(), "s")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(config.API{URL: srv.URL, TokenID: testToken, TokenSecretFile: secret})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Version(context.Background()); err == nil {
		t.Error("self-signed cert accepted without pin or CA")
	}
}

func TestNewMissingSecretFile(t *testing.T) {
	if _, err := New(config.API{URL: "https://x:8006", TokenID: testToken, TokenSecretFile: "/nonexistent/secret"}); err == nil {
		t.Error("expected error")
	}
}

func TestAPIError(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"data":null,"errors":{"notes":"value too long"}}`))
	}))
	_, err := c.Version(context.Background())
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if ae.Status != 400 || ae.Path != "/version" || !strings.Contains(ae.Message, "notes") || !strings.Contains(ae.Message, "value too long") {
		t.Errorf("APIError = %+v", ae)
	}
}

func TestAPIErrorStatusText(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	_, err := c.Version(context.Background())
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 401 || !strings.Contains(ae.Message, "Unauthorized") {
		t.Fatalf("err = %v", err)
	}
}

func TestStorageStatus(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/pve1/storage/local-lvm/status" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":{"active":1,"enabled":"1","avail":"1000","total":2000,"content":"images,rootdir"}}`))
	}))
	s, err := c.StorageStatus(context.Background(), "pve1", "local-lvm")
	if err != nil {
		t.Fatal(err)
	}
	want := StorageStatus{Active: true, Enabled: true, Avail: 1000, Total: 2000, Content: "images,rootdir"}
	if s != want {
		t.Errorf("got %+v want %+v", s, want)
	}
}

func TestNodeMemory(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/pve1/status" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":{"memory":{"total":100,"available":60,"free":50,"used":40}}}`))
	}))
	m, err := c.NodeMemory(context.Background(), "pve1")
	if err != nil || m != (NodeMemory{Total: 100, Available: 60}) {
		t.Errorf("got %+v, %v", m, err)
	}
}

func TestNetIface(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/pve1/network/vmbr0" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":{"type":"OVSBridge","iface":"vmbr0","ovs_ports":"enp66s0f0 ovsport_host ovsport_dev"}}`))
	}))
	n, err := c.NetIface(context.Background(), "pve1", "vmbr0")
	if err != nil {
		t.Fatal(err)
	}
	want := NetIface{Iface: "vmbr0", Type: "OVSBridge", OVSPorts: "enp66s0f0 ovsport_host ovsport_dev"}
	if n != want {
		t.Errorf("got %+v want %+v", n, want)
	}
}

func TestWaitTaskOK(t *testing.T) {
	polls := 0
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/pve1/tasks/UPID:pve1:0001::qmrestore:100:root@pam:/status" {
			t.Errorf("path = %q", r.URL.Path)
		}
		polls++
		if polls < 3 {
			_, _ = w.Write([]byte(`{"data":{"status":"running"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"OK"}}`))
	}))
	if err := c.WaitTask(context.Background(), "pve1", "UPID:pve1:0001::qmrestore:100:root@pam:"); err != nil {
		t.Fatal(err)
	}
	if polls != 3 {
		t.Errorf("polls = %d", polls)
	}
}

func TestWaitTaskFailureIncludesLog(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"command 'x' failed"}}`))
			return
		}
		var sb strings.Builder
		sb.WriteString(`{"data":[`)
		for i := 1; i <= 30; i++ {
			if i > 1 {
				sb.WriteString(",")
			}
			sb.WriteString(`{"n":` + strconv.Itoa(i) + `,"t":"line ` + strconv.Itoa(i) + `"}`)
		}
		sb.WriteString(`]}`)
		_, _ = w.Write([]byte(sb.String()))
	}))
	err := c.WaitTask(context.Background(), "pve1", "UPID:pve1:1:")
	var te *TaskError
	if !errors.As(err, &te) {
		t.Fatalf("err = %v, want *TaskError", err)
	}
	if te.ExitStatus != "command 'x' failed" || te.UPID != "UPID:pve1:1:" {
		t.Errorf("TaskError = %+v", te)
	}
	if len(te.Log) != 20 || te.Log[0] != "line 11" || te.Log[19] != "line 30" {
		t.Errorf("log = %v", te.Log)
	}
	if !strings.Contains(te.Error(), "command 'x' failed") {
		t.Errorf("Error() = %q", te.Error())
	}
}

func TestWaitTaskContextCancel(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"status":"running"}}`))
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := c.WaitTask(ctx, "pve1", "UPID:x:"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
}

func TestWaitTaskWarningsIsSuccess(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"WARNINGS: 2"}}`))
	}))
	if err := c.WaitTask(context.Background(), "pve1", "UPID:pve1:2:"); err != nil {
		t.Fatalf("WARNINGS treated as failure: %v", err)
	}
}

// dropConn closes the connection without a response (the client sees EOF).
func dropConn(t *testing.T, w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Error(err)
		return
	}
	_ = conn.Close()
}

// Transient poll failures (5xx, dropped connections) while a task runs must
// not abandon the task: WaitTask retries them with backoff.
func TestWaitTaskRetriesTransientErrors(t *testing.T) {
	var polls int
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls++
		switch polls {
		case 1:
			_, _ = w.Write([]byte(`{"data":{"status":"running"}}`))
		case 2, 3:
			http.Error(w, `{"message":"proxy loop"}`, http.StatusBadGateway)
		case 4:
			dropConn(t, w)
		case 5:
			_, _ = w.Write([]byte(`{"data":{"status":"running"}}`))
		case 6, 7, 8, 9, 10: // five in a row, then success
			http.Error(w, "", http.StatusServiceUnavailable)
		default:
			_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"OK"}}`))
		}
	}))
	// Fresh connections only: Go's transport silently retries a GET whose
	// reused connection drops, which would hide the EOF from WaitTask.
	c.http.Transport.(*http.Transport).DisableKeepAlives = true
	var backoffs []int
	c.retryBackoff = func(n int) time.Duration { backoffs = append(backoffs, n); return time.Millisecond }
	if err := c.WaitTask(context.Background(), "pve1", "UPID:pve1:3:"); err != nil {
		t.Fatal(err)
	}
	if polls != 11 {
		t.Errorf("polls = %d", polls)
	}
	if want := []int{1, 2, 3, 1, 2, 3, 4, 5}; !slices.Equal(backoffs, want) {
		t.Errorf("backoff attempts = %v, want %v (count resets after a good poll)", backoffs, want)
	}
}

func TestWaitTaskGivesUpAfterEightRetries(t *testing.T) {
	var polls int
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls++
		http.Error(w, "", http.StatusInternalServerError)
	}))
	c.retryBackoff = func(int) time.Duration { return time.Millisecond }
	err := c.WaitTask(context.Background(), "pve1", "UPID:pve1:4:")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 500 {
		t.Fatalf("err = %v", err)
	}
	if polls != 9 {
		t.Errorf("polls = %d, want 9 (first try + 8 retries)", polls)
	}
}

func TestWaitTaskDoesNotRetryClientErrors(t *testing.T) {
	var polls int
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls++
		http.Error(w, "", http.StatusForbidden)
	}))
	c.retryBackoff = func(int) time.Duration { return time.Millisecond }
	if err := c.WaitTask(context.Background(), "pve1", "UPID:pve1:5:"); err == nil || polls != 1 {
		t.Fatalf("err = %v polls = %d", err, polls)
	}
}

func TestWaitTaskRetryHonoursContext(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "", http.StatusBadGateway)
	}))
	c.retryBackoff = func(int) time.Duration { return time.Hour }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := c.WaitTask(ctx, "pve1", "UPID:x:"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
}

func TestTaskFailure(t *testing.T) {
	err := fmt.Errorf("restore x to 990000: %w", &TaskError{
		UPID:       "UPID:pve1:0001",
		ExitStatus: "unable to restore CT 990000 - unable to parse\nvolume",
		Log:        []string{"a", "b"},
	})
	exit, log, ok := TaskFailure(err)
	if !ok || exit != "unable to restore CT 990000 - unable to parse volume" || !slices.Equal(log, []string{"a", "b"}) {
		t.Errorf("TaskFailure = %q, %q, %v", exit, log, ok)
	}
	if _, _, ok := TaskFailure(errors.New("x")); ok {
		t.Error("plain error reported as a task failure")
	}
}

// A refusal for lack of rights changed nothing; any other failure may have.
// The messages are the ones PVE returned on pve2 for a read-only PBS user.
func TestPermissionDenied(t *testing.T) {
	for err, want := range map[error]bool{
		&APIError{Status: 403, Message: "Permission check failed"}:                                                                                               true,
		&APIError{Status: 500, Message: "403 Forbidden: 403 Forbidden\n"}:                                                                                        true,
		fmt.Errorf("protect: %w", &APIError{Status: 500, Message: "Error: permission check failed - missing Datastore.Modify|Datastore.Backup on /datastore/x"}): true,
		&APIError{Status: 500, Message: "got timeout"}:                                                                                                           false,
		&APIError{Status: 595, Message: "Connection refused"}:                                                                                                    false,
		errors.New("403 Forbidden"):                                                                                                                              false,
	} {
		if got := PermissionDenied(err); got != want {
			t.Errorf("%v: got %v, want %v", err, got, want)
		}
	}
}

// The default backoff rides out about two minutes of failed polls, each wait
// at most 30s (code review 4.5).
func TestDefaultRetryBackoff(t *testing.T) {
	var total time.Duration
	for n := 1; n <= maxPollRetries; n++ {
		d := defaultRetryBackoff(n)
		if d > 30*time.Second {
			t.Errorf("retry %d waits %v", n, d)
		}
		total += d
	}
	if total < 2*time.Minute {
		t.Errorf("total backoff %v, want at least 2m", total)
	}
}

// A status reply without a status is a failed poll, not "still running":
// WaitTask gives up after the retries instead of polling forever (4.4).
func TestWaitTaskEmptyStatusIsAnError(t *testing.T) {
	var polls int
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls++
		_, _ = w.Write([]byte(`{"data":null}`))
	}))
	c.retryBackoff = func(int) time.Duration { return time.Millisecond }
	if err := c.WaitTask(context.Background(), "pve1", "UPID:pve1:4:"); !errors.Is(err, errNoTaskStatus) {
		t.Fatalf("err = %v", err)
	}
	if polls != maxPollRetries+1 {
		t.Errorf("polls = %d", polls)
	}
}

// A long error body is cut to 1 KiB (4.9); redirects are not followed (4.7).
func TestErrorMessageCappedAndNoRedirect(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api2/json/version" {
			http.Redirect(w, r, "https://example.invalid/steal", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"` + strings.Repeat("é", 4000) + `"}`))
	}))
	if _, err := c.Version(context.Background()); err == nil {
		t.Error("redirect followed or accepted")
	}
	_, err := c.StorageStatus(context.Background(), "pve1", "x")
	var ae *APIError
	if !errors.As(err, &ae) || len(ae.Message) > maxErrorMessage+len("…") || !utf8.ValidString(ae.Message) {
		t.Errorf("message %d bytes, valid %v", len(ae.Message), utf8.ValidString(ae.Message))
	}
}

// /access/permissions answers with each privilege's propagate flag: a
// privilege granted without propagation (0) is still held (review M3).
func TestPrivilegesKeyPresence(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("path") != "/vms/990000" {
			t.Errorf("path %q", r.URL.Query().Get("path"))
		}
		_, _ = w.Write([]byte(`{"data":{"/vms/990000":{"VM.Config.Disk":0,"VM.Audit":1},"/other":{"Sys.Modify":1}}}`))
	}))
	p, err := c.Privileges(context.Background(), "/vms/990000")
	if err != nil || !p["VM.Config.Disk"] || !p["VM.Audit"] || p["Sys.Modify"] {
		t.Errorf("privileges %v, %v", p, err)
	}
}
