package pve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/pbv/internal/config"
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
		w.Write([]byte(`{"data":{"version":"9.2.18","release":"9.2"}}`))
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
		w.Write([]byte(`{"data":{"version":"9.0.0"}}`))
	}))
	defer srv.Close()
	secret := filepath.Join(t.TempDir(), "s")
	os.WriteFile(secret, []byte("x"), 0o600)
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
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"data":{"version":"9"}}`)) })
	srv := quietTLSServer(h)
	defer srv.Close()
	secret := filepath.Join(t.TempDir(), "s")
	os.WriteFile(secret, []byte("x"), 0o600)

	// lower-case, no-colon variants are accepted too
	lower := strings.ToLower(fingerprint(srv))
	c, _ := New(config.API{URL: srv.URL, TokenID: testToken, TokenSecretFile: secret, TLSFingerprint: lower})
	if _, err := c.Version(context.Background()); err != nil {
		t.Errorf("matching fingerprint rejected: %v", err)
	}

	wrong := strings.Repeat("AA:", 31) + "AA"
	c, _ = New(config.API{URL: srv.URL, TokenID: testToken, TokenSecretFile: secret, TLSFingerprint: wrong})
	if _, err := c.Version(context.Background()); err == nil {
		t.Error("wrong fingerprint accepted")
	} else if !strings.Contains(err.Error(), "pinned fingerprint") {
		t.Errorf("error does not mention the pin mismatch: %v", err)
	}
}

func TestNoTLSConfigRejectsSelfSigned(t *testing.T) {
	srv := quietTLSServer(http.NotFoundHandler())
	defer srv.Close()
	secret := filepath.Join(t.TempDir(), "s")
	os.WriteFile(secret, []byte("x"), 0o600)
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
		w.Write([]byte(`{"data":null,"errors":{"notes":"value too long"}}`))
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
		w.Write([]byte(`{"data":{"active":1,"enabled":"1","avail":"1000","total":2000,"content":"images,rootdir"}}`))
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
		w.Write([]byte(`{"data":{"memory":{"total":100,"available":60,"free":50,"used":40}}}`))
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
		w.Write([]byte(`{"data":{"type":"OVSBridge","iface":"vmbr0","ovs_ports":"enp66s0f0 ovsport_host ovsport_dev"}}`))
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
			w.Write([]byte(`{"data":{"status":"running"}}`))
			return
		}
		w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"OK"}}`))
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
			w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"command 'x' failed"}}`))
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
		w.Write([]byte(sb.String()))
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
		w.Write([]byte(`{"data":{"status":"running"}}`))
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := c.WaitTask(ctx, "pve1", "UPID:x:"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
}

func TestWaitTaskWarningsIsSuccess(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"WARNINGS: 2"}}`))
	}))
	if err := c.WaitTask(context.Background(), "pve1", "UPID:pve1:2:"); err != nil {
		t.Fatalf("WARNINGS treated as failure: %v", err)
	}
}
