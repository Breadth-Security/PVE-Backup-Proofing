// Package sshx runs commands on the PVE node over SSH with known_hosts
// enforcement.
package sshx

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"git.breadthanalytics.com/tdiana/pbv/internal/config"
)

const dialTimeout = 15 * time.Second

// Result is the outcome of a remote command. A non-zero ExitCode is not an
// error; only transport failures are.
type Result struct {
	Stdout, Stderr string
	ExitCode       int
}

// Runner runs a command remotely.
type Runner interface {
	Run(ctx context.Context, cmd string, stdin []byte) (Result, error)
}

// Client holds one SSH connection and opens one session per Run. It is safe
// for concurrent use.
type Client struct {
	addr    string
	cfg     *ssh.ClientConfig
	timeout time.Duration // bounds TCP dial plus SSH handshake

	mu         sync.Mutex
	conn       *ssh.Client
	connecting *connectAttempt
	closed     bool
}

// connectAttempt is a shared in-flight (re)connect; it never holds c.mu while
// doing network I/O.
type connectAttempt struct {
	done chan struct{}
	conn *ssh.Client
	err  error
}

// Dial connects to cfg.Host using key auth and verifies the host key against
// cfg.KnownHostsFile.
func Dial(cfg config.SSH) (*Client, error) {
	return dial(context.Background(), cfg, dialTimeout)
}

func dial(ctx context.Context, cfg config.SSH, timeout time.Duration) (*Client, error) {
	keyBytes, err := os.ReadFile(cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("sshx: read key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("sshx: parse key: %w", err)
	}
	kh, err := knownhosts.New(cfg.KnownHostsFile)
	if err != nil {
		return nil, fmt.Errorf("sshx: knownhosts: %w", err)
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	c := &Client{addr: addr, timeout: timeout}
	c.cfg = &ssh.ClientConfig{
		User: cfg.User,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(h string, r net.Addr, k ssh.PublicKey) error {
			if err := kh(h, r, k); err != nil {
				return fmt.Errorf("knownhosts: %w", err)
			}
			return nil
		},
		// Negotiate only key types that known_hosts lists for this host;
		// empty (unknown host) leaves the default so the error is clean.
		HostKeyAlgorithms: knownAlgorithms(kh, addr),
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := c.connect(cctx)
	if err != nil {
		return nil, err
	}
	c.conn = conn
	return c, nil
}

type strAddr string

func (a strAddr) Network() string { return "tcp" }
func (a strAddr) String() string  { return string(a) }

// knownAlgorithms probes the known_hosts callback with a throwaway key; a
// KeyError then lists the entries on file for addr.
func knownAlgorithms(kh ssh.HostKeyCallback, addr string) []string {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil
	}
	probe, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil
	}
	var ke *knownhosts.KeyError
	if err := kh(addr, strAddr(addr), probe); !errors.As(err, &ke) {
		return nil
	}
	var algs []string
	seen := map[string]bool{}
	for _, w := range ke.Want {
		var cand []string
		switch t := w.Key.Type(); t {
		case ssh.KeyAlgoRSA:
			cand = []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
		default:
			cand = []string{t}
		}
		for _, a := range cand {
			if !seen[a] {
				seen[a] = true
				algs = append(algs, a)
			}
		}
	}
	return algs
}

func (c *Client) connect(ctx context.Context) (*ssh.Client, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return nil, fmt.Errorf("sshx: dial %s: %w", c.addr, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		if err := nc.SetDeadline(dl); err != nil {
			_ = nc.Close()
			return nil, fmt.Errorf("sshx: dial %s: set deadline: %w", c.addr, err)
		}
	}
	sc, chans, reqs, err := ssh.NewClientConn(nc, c.addr, c.cfg)
	if err != nil {
		_ = nc.Close()
		return nil, fmt.Errorf("sshx: dial %s: %w", c.addr, err)
	}
	if err := nc.SetDeadline(time.Time{}); err != nil {
		_ = sc.Close()
		return nil, fmt.Errorf("sshx: dial %s: clear deadline: %w", c.addr, err)
	}
	return ssh.NewClient(sc, chans, reqs), nil
}

// ensureConn returns the live connection, starting (or joining) a single
// shared reconnect if there is none. It honours ctx while waiting.
func (c *Client) ensureConn(ctx context.Context) (*ssh.Client, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("sshx: client closed")
	}
	if c.conn != nil {
		conn := c.conn
		c.mu.Unlock()
		return conn, nil
	}
	att := c.connecting
	if att == nil {
		att = &connectAttempt{done: make(chan struct{})}
		c.connecting = att
		go func() {
			cctx, cancel := context.WithTimeout(context.Background(), c.timeout)
			defer cancel()
			conn, err := c.connect(cctx)
			c.mu.Lock()
			att.conn, att.err = conn, err
			c.connecting = nil
			if err == nil {
				if c.closed {
					_ = conn.Close()
					att.conn, att.err = nil, errors.New("sshx: client closed")
				} else {
					c.conn = conn
				}
			}
			c.mu.Unlock()
			close(att.done)
		}()
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-att.done:
		return att.conn, att.err
	}
}

// dropConn discards conn if it is still the current connection.
func (c *Client) dropConn(conn *ssh.Client) {
	c.mu.Lock()
	if c.conn == conn {
		c.conn = nil
	}
	c.mu.Unlock()
	_ = conn.Close()
}

// session opens a session, reconnecting once if the connection has dropped.
func (c *Client) session(ctx context.Context) (*ssh.Session, error) {
	conn, err := c.ensureConn(ctx)
	if err != nil {
		return nil, err
	}
	if s, err := conn.NewSession(); err == nil {
		return s, nil
	}
	c.dropConn(conn)
	conn, err = c.ensureConn(ctx)
	if err != nil {
		return nil, err
	}
	s, err := conn.NewSession()
	if err != nil {
		return nil, fmt.Errorf("sshx: new session: %w", err)
	}
	return s, nil
}

// Run executes cmd in a new session. Cancelling ctx closes the session and
// returns ctx.Err().
func (c *Client) Run(ctx context.Context, cmd string, stdin []byte) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	sess, err := c.session(ctx)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = sess.Close() }()

	var stdout, stderr bytes.Buffer
	sess.Stdout, sess.Stderr = &stdout, &stderr
	if stdin != nil {
		sess.Stdin = bytes.NewReader(stdin)
	}
	if err := sess.Start(cmd); err != nil {
		return Result{}, fmt.Errorf("sshx: start: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()

	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		_ = sess.Close()
		return Result{}, ctx.Err()
	case err := <-done:
		res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
		var ee *ssh.ExitError
		switch {
		case err == nil:
		case errors.As(err, &ee):
			res.ExitCode = ee.ExitStatus()
		default:
			return Result{}, fmt.Errorf("sshx: wait: %w", err)
		}
		return res, nil
	}
}

// Close closes the underlying connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

// Fake is a mutex-safe Runner for other packages' tests.
type Fake struct {
	Handler func(cmd string, stdin []byte) (Result, error)
	Calls   []string

	mu sync.Mutex
}

// Run records cmd and delegates to Handler (zero Result if nil).
func (f *Fake) Run(_ context.Context, cmd string, stdin []byte) (Result, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, cmd)
	h := f.Handler
	f.mu.Unlock()
	if h == nil {
		return Result{}, nil
	}
	return h(cmd, stdin)
}
