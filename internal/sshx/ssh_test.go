package sshx

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
)

type testServer struct {
	addr  string
	cfg   config.SSH
	mu    sync.Mutex
	conns []net.Conn
	ln    net.Listener
}

func (s *testServer) dropConns() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.conns = nil
}

// newServer starts an in-process SSH server. If trustHost is false the
// known_hosts file lists a different key than the server presents.
func newServer(t *testing.T, trustHost bool) *testServer {
	t.Helper()
	return newServerKeys(t, false, trustHost)
}

// newServerKeys optionally adds an ecdsa host key alongside ed25519; the
// known_hosts file only ever lists the ed25519 key.
func newServerKeys(t *testing.T, ecdsaToo, trustHost bool) *testServer {
	t.Helper()
	dir := t.TempDir()

	hostPub, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
	clientPub, clientPriv, _ := ed25519.GenerateKey(rand.Reader)
	authorized, _ := ssh.NewPublicKey(clientPub)

	scfg := &ssh.ServerConfig{
		PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if string(k.Marshal()) == string(authorized.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("unauthorized")
		},
	}
	scfg.AddHostKey(hostSigner)
	if ecdsaToo {
		ek, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		es, _ := ssh.NewSignerFromKey(ek)
		scfg.AddHostKey(es)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &testServer{addr: ln.Addr().String(), ln: ln}
	t.Cleanup(func() { _ = ln.Close(); s.dropConns() })

	khKey := ssh.PublicKey(nil)
	if trustHost {
		khKey, _ = ssh.NewPublicKey(hostPub)
	} else {
		other, _, _ := ed25519.GenerateKey(rand.Reader)
		khKey, _ = ssh.NewPublicKey(other)
	}
	khFile := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(khFile, []byte(knownhosts.Line([]string{s.addr}, khKey)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	blk, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "id")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(blk), 0o600); err != nil {
		t.Fatal(err)
	}

	host, port, _ := net.SplitHostPort(s.addr)
	p := 0
	for _, ch := range port {
		p = p*10 + int(ch-'0')
	}
	s.cfg = config.SSH{Host: host, Port: p, User: "root", KeyFile: keyFile, KnownHostsFile: khFile}

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns = append(s.conns, c)
			s.mu.Unlock()
			go serve(c, scfg)
		}
	}()
	return s
}

func serve(c net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer func() { _ = sc.Close() }()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "no")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go handleSession(ch, creqs)
	}
}

func handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer func() { _ = ch.Close() }()
	for r := range reqs {
		if r.Type != "exec" {
			_ = r.Reply(false, nil)
			continue
		}
		_ = r.Reply(true, nil)
		cmd := string(r.Payload[4:])
		inner := cmd
		code := byte(0)
		switch {
		case strings.HasPrefix(inner, "exit3"):
			code = 3
			_, _ = io.WriteString(ch, "out")
			_, _ = io.WriteString(ch.Stderr(), "err")
		case strings.HasPrefix(inner, "flood"):
			chunk := []byte(strings.Repeat("y\n", 16<<10))
			for {
				if _, err := ch.Write(chunk); err != nil {
					return
				}
			}
		case strings.HasPrefix(inner, "cat"):
			_, _ = io.Copy(ch, ch)
		case strings.HasPrefix(inner, "sleep"):
			for range reqs { // block until the client closes the session
			}
			return
		default:
			_, _ = io.WriteString(ch, "ok:"+cmd)
		}
		_, _ = ch.SendRequest("exit-status", false, []byte{0, 0, 0, code})
		return
	}
}

func TestRunExitCodeAndOutput(t *testing.T) {
	s := newServer(t, true)
	c, err := Dial(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	res, err := c.Run(context.Background(), "exit3", nil)
	if err != nil {
		t.Fatalf("non-zero exit must not be error: %v", err)
	}
	if res.ExitCode != 3 || res.Stdout != "out" || res.Stderr != "err" {
		t.Fatalf("got %+v", res)
	}
}

func TestRunStdin(t *testing.T) {
	s := newServer(t, true)
	c, err := Dial(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	res, err := c.Run(context.Background(), "cat", []byte("hello"))
	if err != nil || res.Stdout != "hello" || res.ExitCode != 0 {
		t.Fatalf("got %+v err %v", res, err)
	}
}

func TestDialUnknownHost(t *testing.T) {
	s := newServer(t, false)
	_, err := Dial(s.cfg)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "knownhosts") {
		t.Fatalf("error should mention knownhosts: %v", err)
	}
}

// Client.Run sends the command verbatim, deadline or not: the node's
// forced command parses it as a helper request, and the helper enforces the
// request's own deadline.
func TestRunSendsCommandVerbatim(t *testing.T) {
	s := newServer(t, true)
	c, err := Dial(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	const cmd = `exec --timeout-ms 1 --nonce 0123456789abcdef 990001 -- ["sh","-c","echo 'x'"]`
	if res, err := c.Run(context.Background(), cmd, nil); err != nil || res.Stdout != "ok:"+cmd {
		t.Errorf("no deadline: %+v, %v", res, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if res, err := c.Run(ctx, cmd, nil); err != nil || res.Stdout != "ok:"+cmd {
		t.Errorf("deadline: %+v, %v", res, err)
	}
}

func TestRunCancel(t *testing.T) {
	s := newServer(t, true)
	c, err := Dial(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err = c.Run(ctx, "sleep", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancel took too long")
	}
	// connection still usable afterwards
	if res, err := c.Run(context.Background(), "exit3", nil); err != nil || res.ExitCode != 3 {
		t.Fatalf("after cancel: %+v %v", res, err)
	}
}

// A deadline (the probe's per-command timeout) stops a running command.
func TestRunDeadline(t *testing.T) {
	s := newServer(t, true)
	c, err := Dial(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Run(ctx, "sleep", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("deadline took too long")
	}
}

// waitFor polls cond until it holds, failing the test after five seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

// waitConnLost waits until c's connection has noticed the server dropping it.
func waitConnLost(t *testing.T, c *Client) {
	t.Helper()
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	lost := make(chan struct{})
	go func() { _ = conn.Wait(); close(lost) }()
	select {
	case <-lost:
	case <-time.After(5 * time.Second):
		t.Fatal("connection drop not noticed")
	}
}

func TestReconnectAfterDrop(t *testing.T) {
	s := newServer(t, true)
	c, err := Dial(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Run(context.Background(), "exit3", nil); err != nil {
		t.Fatal(err)
	}
	s.dropConns()
	waitConnLost(t, c)
	res, err := c.Run(context.Background(), "exit3", nil)
	if err != nil || res.ExitCode != 3 {
		t.Fatalf("reconnect failed: %+v %v", res, err)
	}
}

func TestConcurrentRuns(t *testing.T) {
	s := newServer(t, true)
	c, err := Dial(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := c.Run(context.Background(), "exit3", nil)
			if err != nil || res.ExitCode != 3 {
				t.Errorf("got %+v %v", res, err)
			}
		}()
	}
	wg.Wait()
}

func TestFake(t *testing.T) {
	f := &Fake{Handler: func(cmd string, stdin []byte) (Result, error) {
		return Result{Stdout: "x", ExitCode: 1}, nil
	}}
	var r Runner = f
	res, _ := r.Run(context.Background(), "a", nil)
	if _, err := r.Run(context.Background(), "b", nil); err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "x" || len(f.CallLog()) != 2 || f.CallLog()[1] != "b" {
		t.Fatalf("%+v %v", res, f.CallLog())
	}
	if _, err := (&Fake{}).Run(context.Background(), "z", nil); err != nil {
		t.Fatal(err)
	}
}

func TestDialKnownHostsOnlyEd25519WithEcdsaServer(t *testing.T) {
	s := newServerKeys(t, true, true)
	c, err := Dial(s.cfg)
	if err != nil {
		t.Fatalf("should negotiate ed25519 per known_hosts: %v", err)
	}
	_ = c.Close()
}

func TestDialNoKnownHostsEntry(t *testing.T) {
	s := newServer(t, true)
	empty := filepath.Join(t.TempDir(), "kh")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.KnownHostsFile = empty
	_, err := Dial(s.cfg)
	if err == nil || !strings.Contains(err.Error(), "knownhosts") {
		t.Fatalf("want knownhosts error, got %v", err)
	}
}

// silentListener accepts TCP connections and never speaks.
func silentListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	return ln.Addr().String()
}

func TestDialHandshakeTimeout(t *testing.T) {
	s := newServer(t, true)
	_, port, _ := net.SplitHostPort(silentListener(t))
	s.cfg.Port, _ = strconv.Atoi(port)
	start := time.Now()
	_, err := dial(context.Background(), s.cfg, 300*time.Millisecond)
	if err == nil {
		t.Fatal("expected error")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("handshake not bounded: %v", time.Since(start))
	}
}

// stuckClient returns a connected client whose next reconnect hangs.
func stuckClient(t *testing.T) *Client {
	t.Helper()
	s := newServer(t, true)
	c, err := dial(context.Background(), s.cfg, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	s.dropConns()
	waitConnLost(t, c)
	c.addr = silentListener(t)
	return c
}

func TestRunCtxDuringReconnect(t *testing.T) {
	c := stuckClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Run(ctx, "exit3", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("ctx ignored during reconnect")
	}
}

func TestRunNotBlockedByStuckConnect(t *testing.T) {
	c := stuckClient(t)
	first, cancelFirst := context.WithCancel(context.Background())
	firstDone := make(chan struct{})
	go func() { defer close(firstDone); _, _ = c.Run(first, "exit3", nil) }() // stuck connecting
	defer func() { cancelFirst(); <-firstDone }()
	waitFor(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.connecting != nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Run(ctx, "exit3", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("second Run blocked behind stuck connect")
	}
}

// A command that floods its output is cut off at maxOutput: Run returns an
// error instead of buffering without bound (code review I14).
func TestRunOutputCapped(t *testing.T) {
	old := maxOutput
	maxOutput = 1 << 20
	defer func() { maxOutput = old }()
	s := newServer(t, true)
	c, err := Dial(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = c.Run(ctx, "flood", nil)
	if !errors.Is(err, ErrOutputTooLarge) {
		t.Fatalf("got %v, want ErrOutputTooLarge", err)
	}
	// The connection is still usable.
	if res, err := c.Run(ctx, "exit3", nil); err != nil || res.ExitCode != 3 {
		t.Fatalf("after flood: %+v %v", res, err)
	}
}

// freezeProxy forwards TCP between the client and target until frozen;
// then it stops passing bytes without closing anything, like a NAT that
// dropped the connection silently (a half-open connection).
type freezeProxy struct {
	addr   string
	frozen chan struct{}
	once   sync.Once
}

func (p *freezeProxy) freeze() { p.once.Do(func() { close(p.frozen) }) }

func newFreezeProxy(t *testing.T, target string) *freezeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &freezeProxy{addr: ln.Addr().String(), frozen: make(chan struct{})}
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
	})
	pipe := func(dst, src net.Conn) {
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			select {
			case <-p.frozen:
				return // swallow it and everything after
			default:
			}
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s, err := net.Dial("tcp", target)
			if err != nil {
				_ = c.Close()
				continue
			}
			mu.Lock()
			conns = append(conns, c, s)
			mu.Unlock()
			go pipe(s, c)
			go pipe(c, s)
		}
	}()
	return p
}

// frozenClient returns a client connected through a proxy that is then
// frozen.
func frozenClient(t *testing.T) (*Client, *freezeProxy) {
	t.Helper()
	s := newServer(t, true)
	p := newFreezeProxy(t, s.addr)
	kh, err := os.ReadFile(s.cfg.KnownHostsFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.cfg
	cfg.KnownHostsFile = filepath.Join(t.TempDir(), "known_hosts")
	kh = []byte(strings.Replace(string(kh), knownhosts.Normalize(s.addr), knownhosts.Normalize(p.addr), 1))
	if err := os.WriteFile(cfg.KnownHostsFile, kh, 0o600); err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(p.addr)
	cfg.Port, _ = strconv.Atoi(port)
	c, err := dial(context.Background(), cfg, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Run(context.Background(), "exit3", nil); err != nil {
		t.Fatal(err)
	}
	p.freeze()
	return c, p
}

// On a half-open connection, opening the session blocks on a reply that
// never comes; Run still returns when ctx ends (code review I15).
func TestRunHonoursCtxOnHalfOpenConnection(t *testing.T) {
	c, _ := frozenClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Run(ctx, "exit3", nil)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrNoSession) {
		t.Fatalf("got %v, want ErrNoSession wrapping the deadline", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("Run blocked %v past its deadline", el)
	}
}

// Keepalives find a half-open connection and drop it, so the next Run
// reconnects instead of waiting on it (code review I15).
func TestKeepaliveDropsDeadConnection(t *testing.T) {
	old := keepaliveInterval
	keepaliveInterval = 20 * time.Millisecond
	defer func() { keepaliveInterval = old }()
	c, _ := frozenClient(t)
	waitFor(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.conn == nil
	})
}

// Keepalives leave a healthy connection alone.
func TestKeepaliveKeepsLiveConnection(t *testing.T) {
	old := keepaliveInterval
	keepaliveInterval = 10 * time.Millisecond
	defer func() { keepaliveInterval = old }()
	s := newServer(t, true)
	c, err := Dial(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	// 50 intervals: dropped after 3 unanswered unless replies count.
	time.Sleep(500 * time.Millisecond)
	c.mu.Lock()
	same := c.conn == conn
	c.mu.Unlock()
	if !same {
		t.Fatal("healthy connection dropped")
	}
}

// A command that started and then ran out of time is the command's own
// timeout, not ErrNoSession.
func TestRunStartedCommandTimeoutIsNotNoSession(t *testing.T) {
	s := newServer(t, true)
	c, err := Dial(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = c.Run(ctx, "sleep", nil)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrNoSession) {
		t.Fatalf("got %v, want a plain deadline", err)
	}
}
