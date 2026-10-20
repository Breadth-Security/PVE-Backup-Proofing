package hostsetup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
)

// HostKeyProbe connects to addr ("host:port") and returns the SSH host key
// the server there presents, without logging in.
type HostKeyProbe func(ctx context.Context, addr string) (ssh.PublicKey, error)

// ErrWrongAddress marks a target.ssh.host that reaches another SSH server
// than the node that printed the reply.
var ErrWrongAddress = errors.New("the address does not reach that node")

const probeTimeout = 10 * time.Second

var errProbeDone = errors.New("host key captured")

// ProbeHostKey is the production HostKeyProbe: a TCP connection and the
// start of an SSH handshake, stopped as soon as the server has shown its
// ed25519 host key. No key is offered and nothing is logged in.
func ProbeHostKey(ctx context.Context, addr string) (ssh.PublicKey, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	var key ssh.PublicKey
	cfg := &ssh.ClientConfig{
		User:              "pbp",
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			key = k
			return errProbeDone
		},
	}
	_, _, _, err = ssh.NewClientConn(conn, addr, cfg)
	if key != nil {
		return key, nil
	}
	if err == nil {
		err = errors.New("no host key received")
	}
	return nil, err
}

// FinishChecked is Finish after making sure cfg's SSH address reaches the
// node that printed r: probe asks the server at target.ssh.host for its
// host key, which must be the reply's. A different key means the address
// (through DNS or /etc/hosts) reaches another machine. Then nothing is
// written but the token secret (PVE shows it only once, and it is right
// whatever the address), and the error, matching ErrWrongAddress, names the
// address, what it resolves to and the node's own addresses. A server that
// cannot be reached is no proof either way: warning says so and Finish goes
// ahead, and the check after it reports the failure. A nil probe skips the
// address check.
func FinishChecked(ctx context.Context, l Layout, cfg config.Config, r nodeproto.SetupReply, probe HostKeyProbe) (needRotate bool, warning string, err error) {
	if probe != nil {
		if err := checkReply(l, cfg, r); err != nil {
			return false, "", fmt.Errorf("node reply refused: %w", err)
		}
		want, _, _, _, err := ssh.ParseAuthorizedKey([]byte(r.HostKey))
		if err != nil {
			return false, "", errors.New("node reply refused: host key is not valid")
		}
		host, addr := sshAddr(cfg)
		got, perr := probe(ctx, addr)
		switch {
		case perr != nil:
			warning = fmt.Sprintf("could not reach %s to confirm it is node %s (%v); the check below shows whether it works", addr, r.Node, perr)
		case !bytes.Equal(got.Marshal(), want.Marshal()):
			if r.TokenSecret != "" {
				if err := l.WriteSecretFile(l.TokenPath(), []byte(r.TokenSecret+"\n")); err != nil {
					return false, "", fmt.Errorf("write token secret: %w", err)
				}
			}
			return false, "", wrongAddress(host, addr, r, l)
		}
	}
	needRotate, err = Finish(l, cfg, r)
	return needRotate, warning, err
}

// sshAddr returns target.ssh.host and "host:port" for it.
func sshAddr(cfg config.Config) (host, addr string) {
	port := cfg.Target.SSH.Port
	if port == 0 {
		port = 22
	}
	host = cfg.Target.SSH.Host
	return host, net.JoinHostPort(host, strconv.Itoa(port))
}

// wrongAddress explains an address that reaches another SSH server.
func wrongAddress(host, addr string, r nodeproto.SetupReply, l Layout) error {
	resolved := ""
	if net.ParseIP(host) == nil {
		if ips, err := net.DefaultResolver.LookupHost(context.Background(), host); err == nil && len(ips) > 0 {
			resolved = " (" + host + " resolves to " + strings.Join(ips, ", ") + ")"
		}
	}
	var own []string
	for _, a := range strings.Fields(r.Addresses) {
		if net.ParseIP(a) != nil { // the reply is the node's word; print only addresses
			own = append(own, a)
		}
	}
	hint := "an address of that node"
	if len(own) > 0 {
		hint = "one of its addresses (" + strings.Join(own, ", ") + ")"
	}
	saved := ""
	if r.TokenSecret != "" {
		saved = " The token secret was saved to " + l.TokenPath() + "."
	}
	return fmt.Errorf("%w: %s%s answers SSH with a different host key than node %s, which printed this reply, so it is another machine. "+
		"Nothing else was written.%s Set target.ssh.host and the host in target.api.url in %s to %s "+
		"(or 127.0.0.1 when PBP runs on the node itself), then run pbp setup again",
		ErrWrongAddress, addr, resolved, r.Node, saved, l.ConfigPath(), hint)
}
