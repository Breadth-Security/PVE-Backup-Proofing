package hostsetup

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// probeReturning is a HostKeyProbe that answers with the host key string
// k (as the reply carries it) or err.
func probeReturning(t *testing.T, k string, err error) (HostKeyProbe, *string) {
	t.Helper()
	var asked string
	return func(_ context.Context, addr string) (ssh.PublicKey, error) {
		asked = addr
		if err != nil {
			return nil, err
		}
		pk, _, _, _, perr := ssh.ParseAuthorizedKey([]byte(k))
		if perr != nil {
			t.Fatal(perr)
		}
		return pk, nil
	}, &asked
}

func TestFinishCheckedRightAddress(t *testing.T) {
	fakeOwners(t)
	l, cfg, pub := pendingHost(t)
	probe, asked := probeReturning(t, testHostKey(t, 0), nil)
	need, warn, err := FinishChecked(context.Background(), l, cfg, goodReply(t, pub, "s3cret"), probe)
	if err != nil || need || warn != "" {
		t.Fatalf("need=%v warn=%q err=%v", need, warn, err)
	}
	if *asked != "pve1.example.lan:22" {
		t.Errorf("probed %q, want pve1.example.lan:22", *asked)
	}
	if !l.HaveToken() || !exists(l.KnownHostsPath()) {
		t.Error("files not written")
	}
}

// The address reaches another SSH server (another host key): nothing is
// written but the token secret, and the error explains it.
func TestFinishCheckedWrongAddress(t *testing.T) {
	for _, secret := range []string{"s3cret", ""} {
		name := "with a new token secret"
		if secret == "" {
			name = "token already on this host"
		}
		t.Run(name, func(t *testing.T) {
			fakeOwners(t)
			l, cfg, pub := pendingHost(t)
			if secret == "" { // a host that already has its token
				if err := l.WriteSecretFile(l.TokenPath(), []byte("old\n")); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshot(t, l.Dir)
			r := goodReply(t, pub, secret)
			r.Addresses = "10.0.0.1 fd00::1 not-an-ip"
			probe, _ := probeReturning(t, testHostKey(t, 7), nil)
			need, _, err := FinishChecked(context.Background(), l, cfg, r, probe)
			if need || !errors.Is(err, ErrWrongAddress) {
				t.Fatalf("need=%v err=%v, want ErrWrongAddress", need, err)
			}
			msg := err.Error()
			for _, want := range []string{"pve1.example.lan:22", "different host key", "10.0.0.1, fd00::1", "127.0.0.1", l.ConfigPath()} {
				if !strings.Contains(msg, want) {
					t.Errorf("error lacks %q: %s", want, msg)
				}
			}
			if wantWritten := map[bool]string{true: "Only the token secret was written", false: "Nothing was written."}[secret != ""]; !strings.Contains(msg, wantWritten) {
				t.Errorf("error lacks %q: %s", wantWritten, msg)
			}
			if strings.Contains(msg, "not-an-ip") || strings.Contains(msg, "s3cret") {
				t.Errorf("error carries reply text it should not: %s", msg)
			}
			after := snapshot(t, l.Dir)
			if secret == "" {
				if !reflect.DeepEqual(after, before) {
					t.Error("files changed although nothing should be written")
				}
				return
			}
			if b, _ := os.ReadFile(l.TokenPath()); string(b) != secret+"\n" {
				t.Errorf("token secret not saved: %q", b)
			}
			if exists(l.KnownHostsPath()) {
				t.Error("known_hosts written for the wrong address")
			}
		})
	}
}

// An address nothing answers on proves nothing: Finish goes ahead with a
// warning, and the check after it reports the failure.
func TestFinishCheckedUnreachable(t *testing.T) {
	fakeOwners(t)
	l, cfg, pub := pendingHost(t)
	probe, _ := probeReturning(t, "", errors.New("connection refused"))
	need, warn, err := FinishChecked(context.Background(), l, cfg, goodReply(t, pub, "s3cret"), probe)
	if err != nil || need || !strings.Contains(warn, "connection refused") {
		t.Fatalf("need=%v warn=%q err=%v", need, warn, err)
	}
	if !exists(l.KnownHostsPath()) {
		t.Error("known_hosts not written")
	}
}

// ProbeHostKey reads a real SSH server's host key without logging in.
func TestProbeHostKey(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	srv := &ssh.ServerConfig{NoClientAuth: false, PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
		t.Error("the probe offered a key")
		return nil, errors.New("no")
	}}
	srv.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _, _, _ = ssh.NewServerConn(c, srv); _ = c.Close() }()
		}
	}()
	got, err := ProbeHostKey(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Marshal()) != string(signer.PublicKey().Marshal()) {
		t.Error("probe returned another key")
	}

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := closed.Addr().String()
	_ = closed.Close()
	if _, err := ProbeHostKey(context.Background(), addr); err == nil {
		t.Error("probe of a closed port succeeded")
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
