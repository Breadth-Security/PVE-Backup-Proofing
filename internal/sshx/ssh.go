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
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/testutil"
)

const dialTimeout = 15 * time.Second

// Keepalives find a connection that died without a FIN or RST (a NAT or
// firewall dropping it while idle through a long restore): without them the
// next session would wait about 15 minutes for TCP to give up (code review
// I15). Like OpenSSH's ServerAliveInterval 15 / ServerAliveCountMax 3, a
// connection that leaves keepaliveMisses intervals in a row unanswered is
// dropped (about 45 s), and the next Run reconnects. One late answer does
// not drop the connection every proof in flight shares. A var for tests.
var keepaliveInterval = 15 * time.Second

const keepaliveMisses = 3

// maxOutput bounds each of stdout and stderr that Run collects. The node
// helper caps a guest's output below this, so only a broken or hostile node
// reaches it (code review I14). A var for tests.
var maxOutput = 16 << 20

// ErrNoSession: the node never answered: the session did not open, or the
// command's start was not acknowledged, before ctx ended. A healthy sshd
// answers both in milliseconds, so this is the connection's fault, not the
// command's. It wraps ctx's error.
var ErrNoSession = errors.New("sshx: no answer from the node")

// ErrOutputTooLarge: the command printed more than maxOutput on stdout or
// stderr; it was killed and its output dropped.
var ErrOutputTooLarge = errors.New("sshx: command output too large")

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

	keepEvery time.Duration

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
	c := &Client{addr: addr, timeout: timeout, keepEvery: keepaliveInterval}
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
	conn := ssh.NewClient(sc, chans, reqs)
	go c.keepalive(conn)
	return conn, nil
}

// keepalive sends a keepalive request every keepEvery until conn closes,
// and drops conn when one stays unanswered for keepaliveMisses intervals.
// Any answer counts: OpenSSH replies "failure" to the unknown request type.
func (c *Client) keepalive(conn *ssh.Client) {
	closed := make(chan struct{})
	go func() { _ = conn.Wait(); close(closed) }()
	t := time.NewTicker(c.keepEvery)
	defer t.Stop()
	var pending chan error // the request in flight, if unanswered
	misses := 0
	for {
		select {
		case <-closed:
			return
		case <-t.C:
		}
		if pending != nil {
			select {
			case err := <-pending:
				if err != nil {
					c.dropConn(conn)
					return
				}
				misses = 0
			default:
				if misses++; misses >= keepaliveMisses {
					c.dropConn(conn)
					return
				}
				continue
			}
		}
		pending = make(chan error, 1)
		go func(reply chan<- error) {
			_, _, err := conn.SendRequest("keepalive@openssh.com", true, nil)
			reply <- err
		}(pending)
	}
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
		// #nosec G118 -- the shared reconnect is joined by many callers, so it must outlive any single caller's ctx; it is bounded by c.timeout
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

// newSession opens a session on conn, giving up when ctx ends: on a
// half-open connection the channel-open reply never comes (code review
// I15). A session that opens after that is closed.
func newSession(ctx context.Context, conn *ssh.Client) (*ssh.Session, error) {
	type opened struct {
		s   *ssh.Session
		err error
	}
	ch := make(chan opened, 1)
	go func() {
		s, err := conn.NewSession()
		ch <- opened{s, err}
	}()
	select {
	case o := <-ch:
		return o.s, o.err
	case <-ctx.Done():
		go func() {
			if o := <-ch; o.s != nil {
				_ = o.s.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// session opens a session, reconnecting once if the connection has dropped.
func (c *Client) session(ctx context.Context) (*ssh.Session, error) {
	conn, err := c.ensureConn(ctx)
	if err != nil {
		return nil, err
	}
	s, err := newSession(ctx, conn)
	if err == nil {
		return s, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// Any NewSession error is treated as a dropped connection (the ssh
	// package does not distinguish a dead transport from a refused channel):
	// reconnect once, and let the second failure surface.
	c.dropConn(conn)
	conn, err = c.ensureConn(ctx)
	if err != nil {
		return nil, err
	}
	s, err = newSession(ctx, conn)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("sshx: new session: %w", err)
	}
	return s, nil
}

// capBuffer collects up to max bytes; past that it drops the rest and
// closes over (once).
type capBuffer struct {
	buf  bytes.Buffer
	max  int
	over chan struct{}
	once *sync.Once
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.max {
		b.once.Do(func() { close(b.over) })
		return len(p), nil
	}
	return b.buf.Write(p)
}

// Run sends cmd verbatim in a new session. Cancelling ctx closes the
// session and returns ctx.Err(); OpenSSH does not pass that on to the remote
// process, so a command must bound itself on the node (Helper requests
// carry their remaining time for the helper to enforce). Output past
// maxOutput on either stream ends the command with ErrOutputTooLarge.
func (c *Client) Run(ctx context.Context, cmd string, stdin []byte) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	sess, err := c.session(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, fmt.Errorf("%w: %w", ErrNoSession, ctx.Err())
		}
		return Result{}, err
	}
	defer func() { _ = sess.Close() }()

	over, once := make(chan struct{}), &sync.Once{}
	stdout := &capBuffer{max: maxOutput, over: over, once: once}
	stderr := &capBuffer{max: maxOutput, over: over, once: once}
	sess.Stdout, sess.Stderr = stdout, stderr
	if stdin != nil {
		sess.Stdin = bytes.NewReader(stdin)
	}
	// Start waits for the server's reply too, so it runs under the same
	// select as the command (code review I15).
	type waited struct {
		started bool
		err     error
	}
	done := make(chan waited, 1)
	var started atomic.Bool
	go func() {
		if err := sess.Start(cmd); err != nil {
			done <- waited{err: err}
			return
		}
		started.Store(true)
		done <- waited{started: true, err: sess.Wait()}
	}()

	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		_ = sess.Close()
		if !started.Load() {
			return Result{}, fmt.Errorf("%w: %w", ErrNoSession, ctx.Err())
		}
		return Result{}, ctx.Err()
	case <-over:
		_ = sess.Signal(ssh.SIGKILL)
		_ = sess.Close()
		return Result{}, fmt.Errorf("%w: more than %d bytes", ErrOutputTooLarge, maxOutput)
	case w := <-done:
		if !w.started {
			return Result{}, fmt.Errorf("sshx: start: %w", w.err)
		}
		select {
		case <-over:
			return Result{}, fmt.Errorf("%w: more than %d bytes", ErrOutputTooLarge, maxOutput)
		default:
		}
		res := Result{Stdout: stdout.buf.String(), Stderr: stderr.buf.String()}
		var ee *ssh.ExitError
		switch {
		case w.err == nil:
		case errors.As(w.err, &ee):
			res.ExitCode = ee.ExitStatus()
		default:
			return Result{}, fmt.Errorf("sshx: wait: %w", w.err)
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

	calls testutil.CallLog
}

// Run records cmd and delegates to Handler (zero Result if nil).
func (f *Fake) Run(_ context.Context, cmd string, stdin []byte) (Result, error) {
	f.calls.Add(cmd)
	if f.Handler == nil {
		return Result{}, nil
	}
	return f.Handler(cmd, stdin)
}

// CallLog returns the commands run so far, in order.
func (f *Fake) CallLog() []string { return f.calls.Snapshot() }
