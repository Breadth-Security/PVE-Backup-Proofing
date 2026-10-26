package hostsetup

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"gopkg.in/yaml.v3"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/nodeproto"
)

var (
	// reFingerprint is the form the node prints: 32 uppercase hex bytes
	// joined by ':'.
	reFingerprint = regexp.MustCompile(`^[0-9A-F]{2}(:[0-9A-F]{2}){31}$`)
	reNodeName    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,62}$`)
)

// Finish applies the node's reply to the layout: it writes the token secret
// (when the reply carries one, replacing any old file: the node made a new
// token, so the old secret is dead), replaces known_hosts with the node's
// host key, and sets target.node and target.api.tls_fingerprint in
// config.yaml (the fingerprint only when target.api.ca_file is not set).
//
// The reply is checked first and nothing is written if it is refused: its
// range, bridge and public key must be this host's, its token id
// pbp@pve!proof, its host key ssh-ed25519 and its fingerprint 32 uppercase
// hex bytes. needRotate is true, and nothing is written, when the reply has
// no secret and the host has no token file: the token exists on the node
// and its secret cannot be read back. Errors never carry the secret.
func Finish(l Layout, cfg config.Config, r nodeproto.SetupReply) (needRotate bool, err error) {
	if err := checkReply(l, cfg, r); err != nil {
		return false, fmt.Errorf("node reply refused: %w", err)
	}
	if r.TokenSecret == "" && !l.HaveToken() {
		return true, nil
	}
	hostKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(r.HostKey))
	if err != nil {
		return false, errors.New("node reply refused: host key is not valid")
	}
	port := cfg.Target.SSH.Port
	if port == 0 {
		port = 22
	}
	addr := knownhosts.Normalize(net.JoinHostPort(cfg.Target.SSH.Host, strconv.Itoa(port)))
	knownHosts := knownhosts.Line([]string{addr}, hostKey) + "\n"
	cfgData, err := finishedConfig(l, cfg, r)
	if err != nil {
		return false, err
	}

	if r.TokenSecret != "" {
		if err := l.WriteSecretFile(l.TokenPath(), []byte(r.TokenSecret+"\n")); err != nil {
			return false, fmt.Errorf("write token secret: %w", err)
		}
	}
	if err := l.WritePlainFile(l.KnownHostsPath(), []byte(knownHosts)); err != nil {
		return false, fmt.Errorf("write known_hosts: %w", err)
	}
	if err := l.WritePlainFile(l.ConfigPath(), cfgData); err != nil {
		return false, fmt.Errorf("write config: %w", err)
	}
	return false, nil
}

// Changes lists the config values the reply replaces, as "key: old → new",
// for values that are already set and differ.
func Changes(cfg config.Config, r nodeproto.SetupReply) []string {
	var out []string
	if old := cfg.Target.Node; old != "" && old != r.Node {
		out = append(out, "target.node: "+old+" → "+r.Node)
	}
	if old := cfg.Target.API.TLSFingerprint; cfg.Target.API.CAFile == "" && old != "" && old != r.APIFingerprint {
		out = append(out, "target.api.tls_fingerprint: "+old+" → "+r.APIFingerprint)
	}
	return out
}

// checkReply refuses a reply that does not belong to this host's config or
// carries a malformed value.
func checkReply(l Layout, cfg config.Config, r nodeproto.SetupReply) error {
	if r.TokenID != TokenID {
		return fmt.Errorf("token id %q, want %s", r.TokenID, TokenID)
	}
	if r.VMIDRange != cfg.Target.VMIDRange {
		return fmt.Errorf("VMID range %d-%d does not match this host's %d-%d",
			r.VMIDRange[0], r.VMIDRange[1], cfg.Target.VMIDRange[0], cfg.Target.VMIDRange[1])
	}
	if r.Bridge != cfg.Target.Bridge {
		return fmt.Errorf("bridge %q does not match this host's %q", r.Bridge, cfg.Target.Bridge)
	}
	// #nosec G304 -- the layout's own public key
	ours, err := os.ReadFile(l.PubKeyPath())
	if err != nil {
		return fmt.Errorf("read this host's public key: %w", err)
	}
	theirs, err := nodeproto.ParsePubKey(r.PubKey)
	if err != nil {
		return fmt.Errorf("public key: %w", err)
	}
	if keyPart(theirs) != keyPart(strings.TrimSpace(string(ours))) {
		return errors.New("the node authorized a different public key than this host's " + l.PubKeyPath())
	}
	if !reNodeName.MatchString(r.Node) {
		return fmt.Errorf("node name %q is not valid", r.Node)
	}
	hk, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(r.HostKey))
	if err != nil || strings.TrimSpace(string(rest)) != "" {
		return errors.New("the node's SSH host key is missing or not valid")
	}
	if hk.Type() != ssh.KeyAlgoED25519 {
		return fmt.Errorf("the node's SSH host key is %s, want %s", hk.Type(), ssh.KeyAlgoED25519)
	}
	if !reFingerprint.MatchString(r.APIFingerprint) {
		return errors.New("the API certificate fingerprint is not 32 uppercase hex bytes joined by ':'")
	}
	if s := r.TokenSecret; s != "" && (len(s) > 256 || strings.IndexFunc(s, func(c rune) bool { return c <= ' ' || c > '~' }) >= 0) {
		return errors.New("the token secret is not valid")
	}
	return nil
}

// finishedConfig is config.yaml with target.node and (unless ca_file is set)
// target.api.tls_fingerprint set from the reply. Only those two values are
// edited in the YAML tree; the result is re-parsed and must equal cfg with
// just those two values changed.
func finishedConfig(l Layout, cfg config.Config, r nodeproto.SetupReply) ([]byte, error) {
	path := l.ConfigPath()
	// #nosec G304 -- the layout's own config path
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil || len(doc.Content) == 0 {
		return nil, fmt.Errorf("%s: cannot parse as YAML", path)
	}
	root := doc.Content[0]
	want := cfg
	want.Target.Node = r.Node
	if err := setPath(root, "target.node", r.Node); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Target.API.CAFile == "" {
		want.Target.API.TLSFingerprint = r.APIFingerprint
		if err := setPath(root, "target.api.tls_fingerprint", r.APIFingerprint); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if n, _ := lookup(root, "target.api.tls_fingerprint"); n != nil {
			n.Style = yaml.DoubleQuotedStyle
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("render config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("render config: %w", err)
	}
	out := spaceSections(buf.Bytes())
	got, err := config.Parse(out)
	if err != nil {
		return nil, fmt.Errorf("%s with the node's values: %w", path, err)
	}
	if !reflect.DeepEqual(got, want) {
		return nil, fmt.Errorf("%s: editing target.node and tls_fingerprint would change other values; config left unchanged", path)
	}
	return out, nil
}

// setPath sets the string at a dotted path, keeping an existing scalar's
// comments, or adds the missing keys.
func setPath(root *yaml.Node, path, value string) error {
	if n, err := lookup(root, path); err == nil && n.Kind == yaml.ScalarNode {
		setString(n, value)
		return nil
	}
	return fill(root, path, value)
}
