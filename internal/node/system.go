package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/report"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sanitize"
)

// DefaultTemplateDir is where PVE looks for notification templates.
const DefaultTemplateDir = "/etc/pve/notification-templates/default"

// notifyScript sends one notification through PVE's own notification system.
// The payload (already validated by Serve) arrives on stdin; the template
// name is fixed to pbp.
const notifyScript = `use PVE::Notify; use JSON; my $in = decode_json(do { local $/; <STDIN> }); PVE::Notify::notify($in->{severity}, 'pbp', $in->{data}, $in->{fields});`

const (
	// waitDelay bounds how long a killed command may keep its output pipes
	// open (e.g. a grandchild that escaped the process group).
	waitDelay = 5 * time.Second
	// maxCaptured bounds the output captured from a helper-internal command.
	maxCaptured = 4 << 20
	// ovsTimeout keeps ovs-vsctl from waiting forever for a dead ovsdb.
	ovsTimeout = "--timeout=10"
)

// System is everything the helper does to the node. Serve validates the
// request first; a System runs the command it is told to.
type System interface {
	// PctConfig returns `pct config <vmid>` as key/value pairs. It fails for
	// a VM or a missing guest.
	PctConfig(ctx context.Context, vmid int) (map[string]string, error)
	// PctExec runs `pct exec <vmid> -- <argv>` with the streams passed
	// through and returns the guest command's exit code.
	PctExec(ctx context.Context, vmid int, argv []string, stdin io.Reader, stdout, stderr io.Writer) (exit int, err error)
	BridgeState(ctx context.Context, bridge string) (nodeproto.BridgeState, error)
	// Notify hands a validated payload to PVE::Notify.
	Notify(ctx context.Context, payload []byte) error
	// TemplateHashes returns the sha256 (hex) of each installed template
	// file named in report.Templates(); a missing file maps to "".
	TemplateHashes() (map[string]string, error)
}

// runFunc runs a helper-internal command to completion and captures its
// output. err is set only when the command could not be run or was killed,
// not for a non-zero exit.
type runFunc func(ctx context.Context, name string, args []string, stdin []byte) (stdout, stderr string, exit int, err error)

// RealSystem is the System that runs pct, ip, ovs-vsctl and perl.
type RealSystem struct {
	TemplateDir string
	ProcRoot    string
	run         runFunc
}

var _ System = (*RealSystem)(nil)

// NewSystem returns the System for a real Proxmox node.
func NewSystem() *RealSystem {
	return &RealSystem{TemplateDir: DefaultTemplateDir, ProcRoot: "/proc", run: capture}
}

// errNotFound marks a command whose binary does not exist.
var errNotFound = exec.ErrNotFound

func command(ctx context.Context, name string, args []string) *exec.Cmd {
	// #nosec G204 -- name is a fixed binary; args are validated list elements, never a shell string.
	cmd := exec.CommandContext(ctx, name, args...)
	// Own process group so the whole tree can be killed on expiry.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = waitDelay
	return cmd
}

// stream runs name with the streams passed through, returning its exit code.
// When ctx is done the command's whole process group is killed and err is
// non-nil.
func stream(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd := command(ctx, name, args)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return waitExit(ctx, cmd.Run())
}

func waitExit(ctx context.Context, err error) (int, error) {
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ee.Exited() {
			return ee.ExitCode(), nil
		}
		return -1, err
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

type limitedBuffer struct {
	bytes.Buffer
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := maxCaptured - b.Len(); room < len(p) {
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

func capture(ctx context.Context, name string, args []string, stdin []byte) (string, string, int, error) {
	var out, errb limitedBuffer
	cmd := command(ctx, name, args)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.Stdout, cmd.Stderr = &out, &errb
	exit, err := waitExit(ctx, cmd.Run())
	return out.String(), errb.String(), exit, err
}

func pctExecArgs(vmid int, argv []string) []string {
	return append([]string{"exec", strconv.Itoa(vmid), "--"}, argv...)
}

// PctExec implements System.
func (s *RealSystem) PctExec(ctx context.Context, vmid int, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	return stream(ctx, "pct", pctExecArgs(vmid, argv), stdin, stdout, stderr)
}

// runOK runs a command and fails unless it exits 0.
func (s *RealSystem) runOK(ctx context.Context, name string, args ...string) (string, error) {
	out, errOut, exit, err := s.run(ctx, name, args, nil)
	cmd := strings.Join(append([]string{name}, args...), " ")
	if err != nil {
		return "", fmt.Errorf("%s: %w", cmd, err)
	}
	if exit != 0 {
		return "", fmt.Errorf("%s exited %d: %s", cmd, exit, strings.TrimSpace(errOut))
	}
	return out, nil
}

// PctConfig implements System.
func (s *RealSystem) PctConfig(ctx context.Context, vmid int) (map[string]string, error) {
	out, err := s.runOK(ctx, "pct", "config", strconv.Itoa(vmid))
	if err != nil {
		return nil, err
	}
	return sanitize.ParseConfigText(out), nil
}

// BridgeState implements System: disable_ipv6, `ip -j addr` and the members
// (kernel bridge members, or OVS ports when the bridge is an OVS bridge).
func (s *RealSystem) BridgeState(ctx context.Context, bridge string) (nodeproto.BridgeState, error) {
	st := nodeproto.BridgeState{Name: bridge, Members: []string{}}

	p := filepath.Join(s.ProcRoot, "sys/net/ipv6/conf", bridge, "disable_ipv6")
	// #nosec G304 -- bridge is from the validated node config (an interface name).
	b, err := os.ReadFile(p)
	if err != nil {
		return st, fmt.Errorf("bridge %s: read disable_ipv6: %w", bridge, err)
	}
	st.DisableIPv6 = strings.TrimSpace(string(b))

	_, errOut, exit, err := s.run(ctx, "ovs-vsctl", []string{ovsTimeout, "br-exists", bridge}, nil)
	switch {
	case errors.Is(err, errNotFound):
		st.OVS = false // Open vSwitch is not installed
	case err != nil:
		return st, fmt.Errorf("bridge %s: ovs-vsctl br-exists: %w", bridge, err)
	case exit == 0:
		st.OVS = true
	case exit == 2:
		st.OVS = false
	default:
		return st, fmt.Errorf("bridge %s: ovs-vsctl br-exists exited %d: %s", bridge, exit, strings.TrimSpace(errOut))
	}

	out, err := s.runOK(ctx, "ip", "-j", "addr", "show", "dev", bridge)
	if err != nil {
		return st, fmt.Errorf("bridge %s: %w", bridge, err)
	}
	out = strings.TrimSpace(out)
	if !json.Valid([]byte(out)) {
		return st, fmt.Errorf("bridge %s: ip -j addr output is not JSON", bridge)
	}
	st.Addrs = json.RawMessage(out)

	if st.OVS {
		out, err = s.runOK(ctx, "ovs-vsctl", ovsTimeout, "list-ports", bridge)
		if err != nil {
			return st, fmt.Errorf("bridge %s: %w", bridge, err)
		}
		st.Members = append(st.Members, strings.Fields(out)...)
		return st, nil
	}
	out, err = s.runOK(ctx, "ip", "-j", "link", "show", "master", bridge)
	if err != nil {
		return st, fmt.Errorf("bridge %s: %w", bridge, err)
	}
	if strings.TrimSpace(out) != "" {
		var links []struct {
			IfName string `json:"ifname"`
		}
		if err := json.Unmarshal([]byte(out), &links); err != nil {
			return st, fmt.Errorf("bridge %s: parse ip link output: %w", bridge, err)
		}
		for _, l := range links {
			st.Members = append(st.Members, l.IfName)
		}
	}
	return st, nil
}

// Notify implements System. PVE::Notify reports a target it could not
// notify (including a template that failed to render) as an "ERROR:" line
// on stderr and still exits 0, so such lines fail the call too.
func (s *RealSystem) Notify(ctx context.Context, payload []byte) error {
	_, errOut, exit, err := s.run(ctx, "perl", []string{"-e", notifyScript}, payload)
	if err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	if exit != 0 {
		return fmt.Errorf("notify: perl exited %d: %s", exit, strings.TrimSpace(errOut))
	}
	var errs []string
	for _, line := range strings.Split(errOut, "\n") {
		if l := strings.TrimSpace(line); strings.HasPrefix(l, "ERROR:") {
			errs = append(errs, l)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("notify: %s", strings.Join(errs, "; "))
	}
	return nil
}

// TemplateHashes implements System.
func (s *RealSystem) TemplateHashes() (map[string]string, error) {
	out := map[string]string{}
	for name := range report.Templates() {
		// #nosec G304 -- name comes from the embedded template set.
		b, err := os.ReadFile(filepath.Join(s.TemplateDir, name))
		if errors.Is(err, fs.ErrNotExist) {
			out[name] = ""
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", name, err)
		}
		sum := sha256.Sum256(b)
		out[name] = hex.EncodeToString(sum[:])
	}
	return out, nil
}
