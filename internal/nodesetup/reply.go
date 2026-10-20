package nodesetup

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
)

// Files the reply block is built from.
const (
	HostKeyPath      = "/etc/ssh/ssh_host_ed25519_key.pub"
	PveproxyCertPath = "/etc/pve/local/pveproxy-ssl.pem" // a custom API certificate...
	PveproxyKeyPath  = "/etc/pve/local/pveproxy-ssl.key" // ...served only together with its key
	PveCertPath      = "/etc/pve/local/pve-ssl.pem"      // the node's own certificate
)

// ReplyFormat is the reply block's format version.
const ReplyFormat = 1

// BuildReply gathers what the host needs: the values setup applied, the
// node's ed25519 SSH host key, the API certificate's SHA-256 fingerprint and
// the token id, with tokenSecret (empty when no token was created). Errors
// never carry the secret.
func BuildReply(env Env, a nodeproto.SetupArgs, nodeName, helperVersion, tokenSecret string) (nodeproto.SetupReply, error) {
	pub, err := nodeproto.ParsePubKey(a.PubKey)
	if err != nil {
		return nodeproto.SetupReply{}, fmt.Errorf("public key: %w", err)
	}
	host, err := hostKey(env)
	if err != nil {
		return nodeproto.SetupReply{}, err
	}
	fp, err := apiFingerprint(env)
	if err != nil {
		return nodeproto.SetupReply{}, err
	}
	// Addresses only help the host explain a wrong address; a failure to
	// list them is not a reason to stop.
	var addrs []string
	if env.Addrs != nil {
		addrs, _ = env.Addrs()
	}
	return nodeproto.SetupReply{
		Addresses:      strings.Join(addrs, " "),
		Format:         ReplyFormat,
		Node:           nodeName,
		HelperVersion:  helperVersion,
		VMIDRange:      a.VMIDRange,
		Bridge:         a.Bridge,
		PubKey:         pub,
		HostKey:        host,
		APIFingerprint: fp,
		TokenID:        TokenID,
		TokenSecret:    tokenSecret,
	}, nil
}

// hostKey returns the node's ed25519 host key as "ssh-ed25519 <base64>",
// without its comment.
func hostKey(env Env) (string, error) {
	b, err := os.ReadFile(env.path(HostKeyPath))
	if err != nil {
		return "", fmt.Errorf("SSH host key: %w", err)
	}
	k, _, _, _, err := ssh.ParseAuthorizedKey(b)
	if err != nil {
		return "", fmt.Errorf("SSH host key %s: %w", HostKeyPath, err)
	}
	if k.Type() != ssh.KeyAlgoED25519 {
		return "", fmt.Errorf("SSH host key %s: type %s, want %s", HostKeyPath, k.Type(), ssh.KeyAlgoED25519)
	}
	return k.Type() + " " + base64.StdEncoding.EncodeToString(k.Marshal()), nil
}

// apiFingerprint is the SHA-256 of the first certificate of the file
// pveproxy serves, as uppercase hex bytes joined by ':' (the form openssl
// prints). pveproxy serves pveproxy-ssl.pem only when it and
// pveproxy-ssl.key are both regular files (PVE::Service::pveproxy uses
// `-f` on both), else pve-ssl.pem.
func apiFingerprint(env Env) (string, error) {
	path := PveCertPath
	if regularFile(env.path(PveproxyCertPath)) && regularFile(env.path(PveproxyKeyPath)) {
		path = PveproxyCertPath
	}
	b, err := os.ReadFile(env.path(path))
	if err != nil {
		return "", fmt.Errorf("API certificate: %w", err)
	}
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return "", fmt.Errorf("API certificate %s: no CERTIFICATE block", path)
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		if _, err := x509.ParseCertificate(blk.Bytes); err != nil {
			return "", fmt.Errorf("API certificate %s: %w", path, err)
		}
		sum := sha256.Sum256(blk.Bytes)
		hexes := make([]string, len(sum))
		for i, c := range sum {
			hexes[i] = fmt.Sprintf("%02X", c)
		}
		return strings.Join(hexes, ":"), nil
	}
}

// regularFile is perl's -f: path (symlinks followed) is a regular file.
func regularFile(path string) bool {
	fi, err := os.Stat(path) // #nosec G703 -- a fixed PVE certificate path under env.Root
	return err == nil && fi.Mode().IsRegular()
}
