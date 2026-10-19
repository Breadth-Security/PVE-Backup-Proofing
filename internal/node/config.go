// Package node is the restricted `pbp node` helper that runs on the Proxmox
// node as root (through sudo, reached only by an SSH forced command). It is
// PBP's security boundary on the node: it parses the request itself, enforces
// the VMID range, the pbp tag and the bridge from its own config, and never
// invokes a shell.
package node

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultConfigPath is where the helper's config lives on the node.
const DefaultConfigPath = "/etc/pbp/node.yaml"

const (
	defaultRequestTimeout = 15 * time.Minute
	minRequestTimeout     = 30 * time.Second
	// maxVMID is the largest VMID Proxmox accepts (same rule as the daemon).
	maxVMID = 999999999
)

// reBridge accepts Linux interface names; the name is also used in a /proc
// path and as a command argument, so '/', names like ".." and a leading '-'
// (option injection) must never get through.
var reBridge = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,14}$`)

// Config is the helper's view of what PBP may touch on this node.
type Config struct {
	VMIDRange      [2]int
	Bridge         string
	RequestTimeout time.Duration
}

type fileConfig struct {
	VMIDRange      []int   `yaml:"vmid_range"`
	Bridge         string  `yaml:"bridge"`
	RequestTimeout *string `yaml:"request_timeout"`
}

// LoadConfig reads and validates the helper config at path. A missing or
// invalid file is an error; the helper then refuses every request.
func LoadConfig(path string) (Config, error) {
	// #nosec G304 -- path is the fixed node config path (or a test override).
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("node config: %w", err)
	}
	var fc fileConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&fc); err != nil {
		if errors.Is(err, io.EOF) {
			return Config{}, fmt.Errorf("node config %s is empty", path)
		}
		return Config{}, fmt.Errorf("node config %s: %w", path, err)
	}

	var c Config
	if len(fc.VMIDRange) != 2 {
		return Config{}, fmt.Errorf("node config %s: vmid_range: need [lo, hi]", path)
	}
	c.VMIDRange = [2]int{fc.VMIDRange[0], fc.VMIDRange[1]}
	if lo, hi := c.VMIDRange[0], c.VMIDRange[1]; lo < 100 || hi < lo || hi > maxVMID {
		return Config{}, fmt.Errorf("node config %s: vmid_range: need 100 <= lo <= hi <= %d, got [%d, %d]", path, maxVMID, lo, hi)
	}

	if fc.Bridge == "" {
		return Config{}, fmt.Errorf("node config %s: bridge: must be set", path)
	}
	if !reBridge.MatchString(fc.Bridge) {
		return Config{}, fmt.Errorf("node config %s: bridge: %q is not a valid interface name", path, fc.Bridge)
	}
	c.Bridge = fc.Bridge

	c.RequestTimeout = defaultRequestTimeout
	if fc.RequestTimeout != nil {
		d, err := time.ParseDuration(*fc.RequestTimeout)
		if err != nil {
			return Config{}, fmt.Errorf("node config %s: request_timeout: %q is not a duration with a unit (e.g. 15m)", path, *fc.RequestTimeout)
		}
		c.RequestTimeout = d
	}
	if c.RequestTimeout < minRequestTimeout {
		return Config{}, fmt.Errorf("node config %s: request_timeout: must be at least %v, got %v", path, minRequestTimeout, c.RequestTimeout)
	}
	return c, nil
}
