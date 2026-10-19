// Package sshx runs commands on the PVE node over SSH with known_hosts
// enforcement.
package sshx

import (
	"bytes"
	"context"
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
	addr string
	cfg  *ssh.ClientConfig

	mu     sync.Mutex
	conn   *ssh.Client
	closed bool
}

// Dial connects to cfg.Host using key auth and verifies the host key against
// cfg.KnownHostsFile.
func Dial(cfg config.SSH) (*Client, error) {
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
	c := &Client{addr: addr}
	c.cfg = &ssh.ClientConfig{
		User: cfg.User,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(h string, r net.Addr, k ssh.PublicKey) error {
			if err := kh(h, r, k); err != nil {
				return fmt.Errorf("knownhosts: %w", err)
			}
			return nil
		},
		Timeout: dialTimeout,
	}
	conn, err := c.connect()
	if err != nil {
		return nil, err
	}
	c.conn = conn
	return c, nil
}

func (c *Client) connect() (*ssh.Client, error) {
	conn, err := ssh.Dial("tcp", c.addr, c.cfg)
	if err != nil {
		return nil, fmt.Errorf("sshx: dial %s: %w", c.addr, err)
	}
	return conn, nil
}

// session opens a session, reconnecting once if the connection has dropped.
func (c *Client) session() (*ssh.Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("sshx: client closed")
	}
	if c.conn != nil {
		if s, err := c.conn.NewSession(); err == nil {
			return s, nil
		}
		c.conn.Close()
		c.conn = nil
	}
	conn, err := c.connect()
	if err != nil {
		return nil, err
	}
	c.conn = conn
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
	sess, err := c.session()
	if err != nil {
		return Result{}, err
	}
	defer sess.Close()

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
		sess.Close()
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
