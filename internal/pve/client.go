// Package pve is a client for the Proxmox VE REST API.
package pve

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
)

const (
	apiSuffix        = "/api2/json"
	taskLogTail      = 20
	defaultPollEvery = 2 * time.Second
	// maxPollRetries is how many consecutive transient failures polling a
	// task's status WaitTask retries before giving up.
	maxPollRetries = 5
)

// defaultRetryBackoff waits 1s, 2s, 4s, 8s, 16s before retry n (1-based).
func defaultRetryBackoff(n int) time.Duration { return time.Second << (n - 1) }

// Client talks to one PVE node/cluster endpoint using an API token.
type Client struct {
	http         *http.Client
	base         string
	auth         string
	pollInterval time.Duration
	// retryBackoff is the wait before the nth consecutive retry of a
	// transient task-poll failure; a field so tests can shorten it.
	retryBackoff func(n int) time.Duration
}

// APIError is returned for any non-2xx response.
type APIError struct {
	Status  int
	Path    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("pve api %s: %d: %s", e.Path, e.Status, e.Message)
}

// TaskError is returned by WaitTask when a task finishes with a non-OK status.
type TaskError struct {
	UPID, ExitStatus string
	Log              []string
}

func (e *TaskError) Error() string {
	msg := fmt.Sprintf("task %s failed: %s", e.UPID, e.ExitStatus)
	if len(e.Log) > 0 {
		msg += "\n" + strings.Join(e.Log, "\n")
	}
	return msg
}

// New builds a client from the API configuration. TLS is verified either by
// pinning the leaf certificate's SHA-256 fingerprint or against CAFile.
func New(cfg config.API) (*Client, error) {
	secret, err := os.ReadFile(cfg.TokenSecretFile)
	if err != nil {
		return nil, fmt.Errorf("read token secret: %w", err)
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case cfg.TLSFingerprint != "":
		want := normalizeFingerprint(cfg.TLSFingerprint)
		// Chain verification is replaced by the pin below.
		tlsCfg.InsecureSkipVerify = true
		// VerifyConnection (unlike VerifyPeerCertificate) also runs on resumed sessions.
		tlsCfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no server certificate presented")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if got := fmt.Sprintf("%x", sum[:]); got != want {
				return fmt.Errorf("server certificate fingerprint %s does not match pinned fingerprint", got)
			}
			return nil
		}
	case cfg.CAFile != "":
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA file %s contains no certificates", cfg.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	base := strings.TrimRight(cfg.URL, "/")
	base = strings.TrimSuffix(base, apiSuffix) + apiSuffix
	return &Client{
		http:         &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}, Timeout: 5 * time.Minute},
		base:         base,
		auth:         "PVEAPIToken=" + cfg.TokenID + "=" + strings.TrimSpace(string(secret)),
		pollInterval: defaultPollEvery,
		retryBackoff: defaultRetryBackoff,
	}, nil
}

func normalizeFingerprint(s string) string {
	return strings.ToLower(strings.NewReplacer(":", "", " ", "").Replace(s))
}

// do performs a request. path must already be escaped and start with "/".
// Responses are unwrapped from {"data": ...} into out (if non-nil).
func (c *Client) do(ctx context.Context, method, path string, query, form url.Values, out any) error {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.auth)
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{Status: resp.StatusCode, Path: path, Message: errorMessage(resp, raw)}
	}
	if out == nil {
		return nil
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("pve api %s: decode response: %w", path, err)
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("pve api %s: decode data: %w", path, err)
	}
	return nil
}

func errorMessage(resp *http.Response, raw []byte) string {
	msg := strings.TrimSpace(strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)))
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	var b struct {
		Message string            `json:"message"`
		Errors  map[string]string `json:"errors"`
	}
	if json.Unmarshal(raw, &b) != nil {
		return msg
	}
	if b.Message != "" {
		msg += ": " + b.Message
	}
	keys := make([]string, 0, len(b.Errors))
	for k := range b.Errors {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		msg += fmt.Sprintf("; %s: %s", k, b.Errors[k])
	}
	return msg
}

// Version returns the PVE version string, e.g. "9.2.18".
func (c *Client) Version(ctx context.Context) (string, error) {
	var v struct {
		Version string `json:"version"`
	}
	if err := c.do(ctx, http.MethodGet, "/version", nil, nil, &v); err != nil {
		return "", err
	}
	return v.Version, nil
}

// WaitTask polls a task until it stops. Exit status OK or "WARNINGS: n" is
// success; anything else yields *TaskError. A transient poll failure (a
// network error or a 5xx) does not mean the task failed: it is retried with
// backoff, up to maxPollRetries times in a row.
func (c *Client) WaitTask(ctx context.Context, node, upid string) error {
	base := "/nodes/" + url.PathEscape(node) + "/tasks/" + url.PathEscape(upid)
	failures := 0
	for {
		var st struct {
			Status     string `json:"status"`
			ExitStatus string `json:"exitstatus"`
		}
		wait := c.pollInterval
		if err := c.do(ctx, http.MethodGet, base+"/status", nil, nil, &st); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !transient(err) || failures >= maxPollRetries {
				return err
			}
			failures++
			wait = c.retryBackoff(failures)
		} else {
			failures = 0
			if st.Status == "stopped" {
				if st.ExitStatus == "OK" || strings.HasPrefix(st.ExitStatus, "WARNINGS:") {
					return nil
				}
				return &TaskError{UPID: upid, ExitStatus: st.ExitStatus, Log: c.taskLogTail(ctx, base)}
			}
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// transient reports whether a request error may go away on retry: a server
// error (5xx) or a failure below HTTP (connection refused or reset, EOF, a
// timeout, an undecodable body). A 4xx is a real answer.
func transient(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status >= 500
	}
	return true
}

// taskLogTail is best effort: a failure to fetch the log must not mask the task error.
func (c *Client) taskLogTail(ctx context.Context, base string) []string {
	var lines []struct {
		T string `json:"t"`
	}
	if err := c.do(ctx, http.MethodGet, base+"/log", url.Values{"limit": {"0"}}, nil, &lines); err != nil {
		return nil
	}
	if len(lines) > taskLogTail {
		lines = lines[len(lines)-taskLogTail:]
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.T
	}
	return out
}
