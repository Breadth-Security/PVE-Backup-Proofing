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

	"git.breadthanalytics.com/tdiana/pbv/internal/config"
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
		c.Close()
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
	t.Cleanup(func() { ln.Close(); s.dropConns() })

	khKey := ssh.PublicKey(nil)
	if trustHost {
		khKey, _ = ssh.NewPublicKey(hostPub)
	} else {
		other, _, _ := ed25519.GenerateKey(rand.Reader)
		khKey, _ = ssh.NewPublicKey(other)
	}
	khFile := filepath.Join(dir, "known_hosts")
	os.WriteFile(khFile, []byte(knownhosts.Line([]string{s.addr}, khKey)+"\n"), 0o600)

	blk, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "id")
	os.WriteFile(keyFile, pem.EncodeToMemory(blk), 0o600)

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
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "no")
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
	defer ch.Close()
	for r := range reqs {
		if r.Type != "exec" {
			r.Reply(false, nil)
			continue
		}
		r.Reply(true, nil)
		cmd := string(r.Payload[4:])
		code := byte(0)
		switch {
		case strings.HasPrefix(cmd, "exit3"):
			code = 3
			io.WriteString(ch, "out")
			io.WriteString(ch.Stderr(), "err")
		case strings.HasPrefix(cmd, "cat"):
			io.Copy(ch, ch)
		case strings.HasPrefix(cmd, "sleep"):
			select {}
		default:
			io.WriteString(ch, "ok:"+cmd)
		}
		ch.SendRequest("exit-status", false, []byte{0, 0, 0, code})
		return
	}
}

func TestRunExitCodeAndOutput(t *testing.T) {
	s := newServer(t, true)
	c, err := Dial(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
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
	defer c.Close()
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

func TestRunCancel(t *testing.T) {
	s := newServer(t, true)
	c, err := Dial(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
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

func TestReconnectAfterDrop(t *testing.T) {
	s := newServer(t, true)
	c, err := Dial(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Run(context.Background(), "exit3", nil); err != nil {
		t.Fatal(err)
	}
	s.dropConns()
	time.Sleep(50 * time.Millisecond)
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
	defer c.Close()
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
	r.Run(context.Background(), "b", nil)
	if res.Stdout != "x" || len(f.Calls) != 2 || f.Calls[1] != "b" {
		t.Fatalf("%+v %v", res, f.Calls)
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
	c.Close()
}

func TestDialNoKnownHostsEntry(t *testing.T) {
	s := newServer(t, true)
	empty := filepath.Join(t.TempDir(), "kh")
	os.WriteFile(empty, nil, 0o600)
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
		ln.Close()
		mu.Lock()
		for _, c := range conns {
			c.Close()
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
	t.Cleanup(func() { c.Close() })
	s.dropConns()
	time.Sleep(50 * time.Millisecond)
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
	go c.Run(context.Background(), "exit3", nil) // stuck connecting
	time.Sleep(200 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := c.Run(ctx, "exit3", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("second Run blocked behind stuck connect")
	}
}
