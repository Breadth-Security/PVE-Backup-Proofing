package hostsetup

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	pbp "git.breadthanalytics.com/tdiana/PVE-Backup-Proofing"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/config"
)

// TokenID is the API token setup creates on the node.
const TokenID = "pbp@pve!proof" // #nosec G101 -- a token id, not its secret

// checksComment replaces the example's sample profiles and per-guest checks.
const checksComment = "Profiles (checks.profiles) and per-guest checks (checks.guests):\nsee config.example.yaml in the PBP repository."

// WriteConfig writes config.yaml from the embedded example with the answers
// filled in and target.node and target.api.tls_fingerprint left empty for
// the node's reply. The example's comments are kept; its sample profiles and
// per-guest checks are removed. For Docker it also creates logs/ and writes
// .env with TZ.
func WriteConfig(l Layout, a Answers) error {
	data, err := configYAML(l, a)
	if err != nil {
		return err
	}
	if err := l.WritePlainFile(l.ConfigPath(), data); err != nil {
		return err
	}
	if l.Deb {
		return nil
	}
	if err := l.ensureDir(l.LogsDir(), 0o750); err != nil {
		return err
	}
	return l.WritePlainFile(l.EnvPath(), []byte("TZ="+a.TZ+"\n"))
}

// configYAML renders the example with the answers applied.
func configYAML(l Layout, a Answers) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(pbp.ExampleConfig, &doc); err != nil {
		return nil, fmt.Errorf("parse embedded example config: %w", err)
	}
	root := doc.Content[0]
	host := a.Address
	if ip := net.ParseIP(a.Address); ip != nil && strings.Contains(a.Address, ":") {
		host = "[" + a.Address + "]"
	}
	set := []struct {
		path, value string
	}{
		{"schedule", a.Schedule},
		{"target.node", ""},
		{"target.api.url", "https://" + host + ":8006"},
		{"target.api.token_id", TokenID},
		{"target.api.token_secret_file", l.InConfigTokenPath()},
		{"target.api.tls_fingerprint", ""},
		{"target.ssh.host", a.Address},
		{"target.ssh.user", "pbp"},
		{"target.ssh.key_file", l.InConfigKeyPath()},
		{"target.ssh.known_hosts_file", l.InConfigKnownHostsPath()},
		{"target.bridge", a.Bridge},
		{"restore.storage", a.RestoreStorage},
	}
	for _, s := range set {
		n, err := lookup(root, s.path)
		if err != nil {
			return nil, err
		}
		setString(n, s.value)
	}
	port, err := lookup(root, "target.ssh.port")
	if err != nil {
		return nil, err
	}
	setInt(port, 22)
	vr, err := lookup(root, "target.vmid_range")
	if err != nil {
		return nil, err
	}
	setSeq(vr, intNode(a.VMIDRange[0]), intNode(a.VMIDRange[1]))
	st, err := lookup(root, "sources.storages")
	if err != nil {
		return nil, err
	}
	var items []*yaml.Node
	for _, s := range a.PBSStorages {
		items = append(items, strNode(s))
	}
	setSeq(st, items...)

	api, _ := lookup(root, "target.api")
	removeKey(api, "ca_file")
	checksKey, checks := keyValue(root, "checks")
	if checks == nil {
		return nil, errors.New("embedded example config has no checks section")
	}
	removeKey(checks, "profiles")
	removeKey(checks, "guests")
	checksKey.HeadComment = checksComment

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("render config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("render config: %w", err)
	}
	return spaceSections(buf.Bytes()), nil
}

// spaceSections puts back the blank lines yaml.v3 drops when re-encoding: one
// before each top-level key (and its head comment), except between two
// one-line keys such as schedule and workers.
func spaceSections(b []byte) []byte {
	var out []string
	seenKey := false
	for _, ln := range strings.Split(string(b), "\n") {
		if ln != "" && !strings.ContainsRune(" #-", rune(ln[0])) {
			start := len(out)
			for start > 0 && strings.HasPrefix(out[start-1], "#") {
				start--
			}
			prev := ""
			if start > 0 {
				prev = out[start-1]
			}
			oneLine := func(l string) bool { _, v, _ := strings.Cut(l, ":"); return strings.TrimSpace(v) != "" }
			adjacent := start == len(out) && prev != "" && !strings.ContainsRune(" #-", rune(prev[0])) && oneLine(prev) && oneLine(ln)
			if seenKey && prev != "" && !adjacent {
				out = slices.Insert(out, start, "")
			}
			seenKey = true
		}
		out = append(out, ln)
	}
	return []byte(strings.Join(out, "\n"))
}

// keyValue returns the key and value nodes of key in mapping n.
func keyValue(n *yaml.Node, key string) (*yaml.Node, *yaml.Node) {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i], n.Content[i+1]
		}
	}
	return nil, nil
}

// lookup returns the value node at a dotted path.
func lookup(root *yaml.Node, path string) (*yaml.Node, error) {
	n := root
	for _, part := range strings.Split(path, ".") {
		_, n = keyValue(n, part)
		if n == nil {
			return nil, fmt.Errorf("embedded example config has no %s", path)
		}
	}
	return n, nil
}

// removeKey drops key and its value from mapping n, if present.
func removeKey(n *yaml.Node, key string) {
	if n == nil || n.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			n.Content = append(n.Content[:i], n.Content[i+2:]...)
			return
		}
	}
}

// setString sets a scalar to a string value, keeping its comments; a
// double-quoted scalar stays double-quoted.
func setString(n *yaml.Node, v string) {
	n.Kind, n.Tag, n.Value = yaml.ScalarNode, "!!str", v
	if n.Style != yaml.DoubleQuotedStyle {
		n.Style = 0
	}
}

func setInt(n *yaml.Node, v int) {
	n.Kind, n.Tag, n.Value, n.Style = yaml.ScalarNode, "!!int", strconv.Itoa(v), 0
}

// setSeq replaces a sequence's items, keeping its style and comments.
func setSeq(n *yaml.Node, items ...*yaml.Node) {
	n.Kind, n.Tag, n.Content = yaml.SequenceNode, "!!seq", items
}

func intNode(v int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(v)}
}

func strNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

// errConfigDir is returned when config.yaml is a directory: Docker creates
// one at a bind-mount source that does not exist.
var errConfigDir = errors.New("config.yaml is a directory (Docker creates one when the file is missing); remove it and run pbp setup again")

// pendingFingerprint is a syntactically valid placeholder used only to
// validate a pending config in memory.
var pendingFingerprint = strings.TrimSuffix(strings.Repeat("00:", 32), ":")

// LoadState reads the layout's config.yaml. A missing file gives an error
// wrapping os.ErrNotExist (the caller asks the questions). A config written by
// WriteConfig and not yet finished (target.node, tls_fingerprint and ca_file
// all empty) is validated with placeholders for those values and returned with
// pending true and the real, empty values. Anything else must pass
// config.Load.
func LoadState(l Layout) (cfg config.Config, pending bool, err error) {
	path := l.ConfigPath()
	fi, err := os.Stat(path)
	if err != nil {
		return config.Config{}, false, fmt.Errorf("read config: %w", err)
	}
	if fi.IsDir() {
		return config.Config{}, false, errConfigDir
	}
	// #nosec G304 -- the layout's own config path
	data, err := os.ReadFile(path)
	if err != nil {
		return config.Config{}, false, fmt.Errorf("read config: %w", err)
	}
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil || len(doc.Content) == 0 || !isPending(doc.Content[0]) {
		cfg, err := config.Load(path)
		return cfg, false, err
	}
	root := doc.Content[0]
	if err := fill(root, "target.node", "pending"); err != nil {
		return config.Config{}, false, err
	}
	if err := fill(root, "target.api.tls_fingerprint", pendingFingerprint); err != nil {
		return config.Config{}, false, err
	}
	filled, err := yaml.Marshal(&doc)
	if err != nil {
		return config.Config{}, false, fmt.Errorf("config %s: %w", path, err)
	}
	cfg, err = config.Parse(filled)
	if err != nil {
		return config.Config{}, false, fmt.Errorf("%s: %w", path, err)
	}
	cfg.Target.Node, cfg.Target.API.TLSFingerprint = "", ""
	return cfg, true, nil
}

// isPending reports whether target.node, tls_fingerprint and ca_file are all
// empty or absent: the state WriteConfig leaves before the node's reply.
func isPending(root *yaml.Node) bool {
	empty := func(path string) bool {
		n, err := lookup(root, path)
		return err != nil || (n.Kind == yaml.ScalarNode && (n.Value == "" || n.Tag == "!!null"))
	}
	return empty("target.node") && empty("target.api.tls_fingerprint") && empty("target.api.ca_file")
}

// fill sets a string at a dotted path, creating missing mapping keys.
func fill(root *yaml.Node, path, value string) error {
	n := root
	for _, part := range strings.Split(path, ".") {
		if n.Kind != yaml.MappingNode {
			return fmt.Errorf("invalid config: %s is not a mapping", path)
		}
		_, v := keyValue(n, part)
		if v == nil {
			v = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			n.Content = append(n.Content, strNode(part), v)
		}
		n = v
	}
	n.Kind, n.Tag, n.Value, n.Style, n.Content = yaml.ScalarNode, "!!str", value, 0, nil
	return nil
}
