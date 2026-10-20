package nodesetup

import (
	"context"
	"fmt"
	"strings"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
)

const (
	sudoBin = "/usr/bin/sudo"
	envBin  = "/usr/bin/env"
	pbpBin  = "/usr/bin/pbp"
	// probeTimeout is the time each self-test request allows the helper.
	probeTimeout = 30 * time.Second
	// maxProbeStderr bounds the stderr a self-test error quotes; the tail is
	// kept, since the helper's marker is the last line.
	maxProbeStderr = 512
)

// SelfTest proves the setup through the path the daemon uses, minus SSH:
// as pbp, with SSH_ORIGINAL_COMMAND set, through the installed sudo rule.
// A version request must succeed and print a version; an exec just above
// the range must be refused with out-of-range. Then BuildPlan must find
// every item correct. The error names the failing probe or items.
func SelfTest(ctx context.Context, env Env, a nodeproto.SetupArgs, nodeName string) error {
	out, stderr, exit, nonce, err := probe(ctx, env, nodeproto.Request{Op: "version"})
	switch {
	case err != nil:
		return fmt.Errorf("self-test: version probe: %w", err)
	case exit != 0:
		return fmt.Errorf("self-test: version probe: exit %d: %s", exit, boundStderr(stderr))
	case strings.TrimSpace(out) == "":
		return fmt.Errorf("self-test: version probe: exit 0 but no version printed: %s", boundStderr(stderr))
	}

	vmid := a.VMIDRange[1] + 1
	_, stderr, exit, nonce, err = probe(ctx, env, nodeproto.Request{Op: "exec", VMID: vmid, Argv: []string{"true"}})
	if err != nil {
		return fmt.Errorf("self-test: out-of-range probe (exec %d): %w", vmid, err)
	}
	kind, reason, ok := nodeproto.HelperFailure(exit, stderr, nonce)
	code, _, _ := strings.Cut(reason, ":")
	if !ok || kind != nodeproto.KindRefused || strings.TrimSpace(code) != "out-of-range" {
		return fmt.Errorf("self-test: out-of-range probe (exec %d) was not refused with out-of-range: exit %d: %s",
			vmid, exit, boundStderr(stderr))
	}

	p, err := BuildPlan(ctx, env, a, nodeName)
	if err != nil {
		return fmt.Errorf("self-test: check: %w", err)
	}
	if !p.Complete() {
		var bad []string
		for _, it := range p.Items {
			if it.Status != Correct {
				bad = append(bad, fmt.Sprintf("%s (%s): %s", it.Name, it.Path, it.Status))
			}
		}
		return fmt.Errorf("self-test: check: not as setup left it: %s", strings.Join(bad, "; "))
	}
	return nil
}

// probe runs req as the daemon would: `sudo -u pbp env
// SSH_ORIGINAL_COMMAND=<req> sudo -n /usr/bin/pbp node`, with a fresh nonce,
// which it returns. No shell is involved.
func probe(ctx context.Context, env Env, req nodeproto.Request) (stdout, stderr string, exit int, nonce string, err error) {
	req.Nonce, req.Timeout = nodeproto.NewNonce(), probeTimeout
	s, err := nodeproto.Encode(req)
	if err != nil {
		return "", "", 0, "", err
	}
	args := []string{"-u", PbpUser, envBin, "SSH_ORIGINAL_COMMAND=" + s, sudoBin, "-n", pbpBin, "node"}
	stdout, stderr, exit, err = env.Run(ctx, sudoBin, args, nil)
	return stdout, stderr, exit, req.Nonce, err
}

// boundStderr trims stderr and keeps at most its last maxProbeStderr bytes.
func boundStderr(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(no stderr)"
	}
	if len(s) > maxProbeStderr {
		s = "..." + strings.ToValidUTF8(s[len(s)-maxProbeStderr:], "")
	}
	return s
}
