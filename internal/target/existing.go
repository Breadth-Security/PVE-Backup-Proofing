package target

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.breadthanalytics.com/tdiana/pbv/internal/config"
	"git.breadthanalytics.com/tdiana/pbv/internal/model"
	"git.breadthanalytics.com/tdiana/pbv/internal/pve"
	"git.breadthanalytics.com/tdiana/pbv/internal/sanitize"
	"git.breadthanalytics.com/tdiana/pbv/internal/sshx"
)

// ExistingNode restores onto a PVE node that also runs production guests.
// It uses the API only; restore and config changes have no SSH fallback yet.
type ExistingNode struct {
	api    pve.API
	ssh    sshx.Runner
	node   string
	bridge string
	store  string
	lo, hi int

	// Poll intervals; fields so tests can shorten them.
	statusPoll  time.Duration // GuestStatus while waiting for "running"
	agentPoll   time.Duration // AgentPing while waiting for the agent
	lxcExecPoll time.Duration // "pct exec true" while a container boots
	execPoll    time.Duration // AgentExecStatus while a command runs
}

var _ Target = (*ExistingNode)(nil)

func NewExistingNode(cfg config.Config, api pve.API, ssh sshx.Runner) *ExistingNode {
	return &ExistingNode{
		api:         api,
		ssh:         ssh,
		node:        cfg.Target.Node,
		bridge:      cfg.Target.Bridge,
		store:       cfg.Restore.Storage,
		lo:          cfg.Target.VMIDRange[0],
		hi:          cfg.Target.VMIDRange[1],
		statusPoll:  time.Second,
		agentPoll:   3 * time.Second,
		lxcExecPoll: 2 * time.Second,
		execPoll:    500 * time.Millisecond,
	}
}

func (n *ExistingNode) inRange(vmid int) bool { return vmid >= n.lo && vmid <= n.hi }

// guard must precede every API call that names a VMID which could be
// outside the range.
func (n *ExistingNode) guard(vmid int) error {
	if !n.inRange(vmid) {
		return fmt.Errorf("%w: %d not in [%d, %d]", ErrOutOfRange, vmid, n.lo, n.hi)
	}
	return nil
}

func (n *ExistingNode) Prepare(ctx context.Context, recorded map[int]Recorded) error {
	v, err := n.api.Version(ctx)
	if err != nil {
		return fmt.Errorf("pve version: %w", err)
	}
	major, err := strconv.Atoi(strings.SplitN(strings.TrimSpace(v), ".", 2)[0])
	if err != nil || major < 9 {
		return fmt.Errorf("%w: node runs %q", ErrVersion, v)
	}

	iface, err := n.api.NetIface(ctx, n.node, n.bridge)
	if err != nil {
		return fmt.Errorf("bridge %s: %w", n.bridge, err)
	}
	if why := notIsolated(iface); why != "" {
		return fmt.Errorf("%w: %s %s", ErrNotIsolated, n.bridge, why)
	}
	if err := n.checkLiveIsolation(ctx, iface); err != nil {
		return err
	}

	st, err := n.api.StorageStatus(ctx, n.node, n.store)
	if err != nil {
		return fmt.Errorf("restore storage %s: %w", n.store, err)
	}
	if !st.Active || !st.Enabled {
		return fmt.Errorf("restore storage %s is not active and enabled", n.store)
	}
	content := sanitize.SplitList(st.Content)
	for _, c := range []string{"images", "rootdir"} {
		if !slices.Contains(content, c) {
			return fmt.Errorf("restore storage %s does not hold %s content (has %q)", n.store, c, st.Content)
		}
	}

	guests, err := n.api.Guests(ctx)
	if err != nil {
		return fmt.Errorf("list guests: %w", err)
	}
	var foreign []string
	for _, g := range guests {
		if !n.inRange(g.Key.VMID) || slices.Contains(g.Tags, OwnerTag) {
			continue
		}
		if r, ok := recorded[g.Key.VMID]; ok {
			why, err := n.notLeftover(ctx, g, r)
			if err != nil {
				return err
			}
			if why == "" {
				continue // a failed restore's leftover: the janitor removes it
			}
			foreign = append(foreign, fmt.Sprintf("%s (recorded restore, but %s)", g.Key, why))
		} else {
			foreign = append(foreign, g.Key.String())
		}
	}
	if len(foreign) > 0 {
		return fmt.Errorf("%w: %s", ErrForeignInRange, strings.Join(foreign, ", "))
	}
	return nil
}

// notIsolated explains why iface is not an isolated bridge ("" if it is).
func notIsolated(i pve.NetIface) string {
	switch {
	case i.Type != "bridge" && i.Type != "OVSBridge":
		return fmt.Sprintf("is a %q, not a bridge", i.Type)
	case i.BridgePorts != "" && i.BridgePorts != "none":
		return "has ports " + i.BridgePorts
	case i.OVSPorts != "":
		return "has ports " + i.OVSPorts
	case i.Address != "" || i.CIDR != "" || i.Address6 != "" || i.CIDR6 != "":
		return "has an IP address"
	}
	return ""
}

func (n *ExistingNode) Owned(ctx context.Context) ([]pve.GuestRef, error) {
	guests, err := n.api.Guests(ctx)
	if err != nil {
		return nil, err
	}
	var out []pve.GuestRef
	for _, g := range guests {
		if n.inRange(g.Key.VMID) && slices.Contains(g.Tags, OwnerTag) {
			out = append(out, g)
		}
	}
	return out, nil
}

func (n *ExistingNode) StrayTagged(ctx context.Context) ([]pve.GuestRef, error) {
	guests, err := n.api.Guests(ctx)
	if err != nil {
		return nil, err
	}
	var out []pve.GuestRef
	for _, g := range guests {
		if !n.inRange(g.Key.VMID) && slices.Contains(g.Tags, OwnerTag) {
			out = append(out, g)
		}
	}
	return out, nil
}

func (n *ExistingNode) AllocateVMID(ctx context.Context, inUse map[int]bool) (int, error) {
	guests, err := n.api.Guests(ctx)
	if err != nil {
		return 0, err
	}
	exists := map[int]bool{}
	for _, g := range guests {
		exists[g.Key.VMID] = true
	}
	for v := n.lo; v <= n.hi; v++ {
		if !exists[v] && !inUse[v] {
			return v, nil
		}
	}
	return 0, fmt.Errorf("%w [%d, %d]", ErrNoFreeVMID, n.lo, n.hi)
}

// Restore restores b into vmid and, as soon as the restore task succeeds,
// tags the guest "pbp" so it stays destroyable even if Sanitize then fails.
// The tag goes on in the first config call, built from the backup's original
// tags, so no read precedes it. Until then the guest is PBP's only by the
// caller's state record (see DestroyRecorded).
func (n *ExistingNode) Restore(ctx context.Context, b model.Backup, vmid int, orig map[string]string) error {
	if err := n.guard(vmid); err != nil {
		return notStarted{err}
	}
	kind := b.Key.Kind
	upid, err := n.api.Restore(ctx, n.node, kind, vmid, b.VolID, n.store)
	if err != nil {
		err = fmt.Errorf("restore %s to %d: %w", b.VolID, vmid, err)
		// Only an API error response is a definite refusal; a transport
		// failure may hide a request the node accepted. A pveproxy 5xx can
		// also follow a request it forwarded; calling that "not started"
		// fails safe: a leftover is then untagged and unrecorded, so it
		// blocks the run (foreign) instead of being destroyed.
		var ae *pve.APIError
		if errors.As(err, &ae) {
			return notStarted{err}
		}
		return err
	}
	if err := n.api.WaitTask(ctx, n.node, upid); err != nil {
		return fmt.Errorf("restore %s to %d: %w", b.VolID, vmid, err)
	}
	// Add the owner tag to the restored tags (the backup's own): replacing
	// them would remove any registered tag, which needs Sys.Modify on "/".
	// Clear protection (restored from a protected guest's backup) in the
	// same call, so the guest is destroyable from here on.
	set := map[string]string{"tags": sanitize.AddTag(orig["tags"], OwnerTag), "protection": "0"}
	// LXC has no "name"; renaming its hostname would change the guest itself.
	if kind == model.KindQEMU {
		set["name"] = fmt.Sprintf("pbp-%d-%s", b.Key.VMID, b.Time.UTC().Format("200601021504"))
	}
	if err := n.api.SetGuestConfig(ctx, n.node, kind, vmid, set, nil); err != nil {
		return fmt.Errorf("tag restored guest %d: %w", vmid, err)
	}
	return nil
}

func (n *ExistingNode) Sanitize(ctx context.Context, kind model.GuestKind, vmid int, orig map[string]string) ([]model.MissingPiece, error) {
	if err := n.guard(vmid); err != nil {
		return nil, err
	}
	cfg, err := n.api.GuestConfig(ctx, n.node, kind, vmid)
	if err != nil {
		return nil, fmt.Errorf("read config %d: %w", vmid, err)
	}
	plan := sanitize.Sanitize(kind, cfg, n.bridge)
	if len(plan.Unsafe) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrUnsafe, strings.Join(plan.Unsafe, ", "))
	}
	if err := n.api.SetGuestConfig(ctx, n.node, kind, vmid, plan.Set, plan.Delete); err != nil {
		return nil, fmt.Errorf("sanitize config %d: %w", vmid, err)
	}
	if err := n.verifySanitized(ctx, kind, vmid); err != nil {
		return nil, err
	}
	missing := append([]model.MissingPiece(nil), plan.Removed...)
	return append(missing, sanitize.MissingFromOriginal(kind, orig)...), nil
}

var reNet = regexp.MustCompile(`^net\d+$`)

// verifySanitized re-reads the live config and checks it is safe to start:
// tagged "pbp", onboot and protection off, every NIC on the isolated bridge with no VLAN or
// rate options, and nothing left that Sanitize would remove or refuse.
func (n *ExistingNode) verifySanitized(ctx context.Context, kind model.GuestKind, vmid int) error {
	cfg, err := n.api.GuestConfig(ctx, n.node, kind, vmid)
	if err != nil {
		return fmt.Errorf("re-read config %d: %w", vmid, err)
	}
	var bad []string
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if reNet.MatchString(k) && !onlyBridge(cfg[k], n.bridge) {
			bad = append(bad, fmt.Sprintf("%s not on bridge %s (%s)", k, n.bridge, cfg[k]))
		}
	}
	if !slices.Contains(sanitize.SplitList(cfg["tags"]), OwnerTag) {
		bad = append(bad, "missing tag pbp")
	}
	if cfg["onboot"] != "0" {
		bad = append(bad, "onboot not 0")
	}
	if p := cfg["protection"]; p != "" && p != "0" {
		bad = append(bad, "protection not 0")
	}
	plan := sanitize.Sanitize(kind, cfg, n.bridge)
	bad = append(bad, plan.Unsafe...)
	for _, k := range plan.Delete {
		bad = append(bad, k+" still present")
	}
	if len(bad) > 0 {
		return fmt.Errorf("%w: guest %d failed verification: %s", ErrUnsafe, vmid, strings.Join(bad, "; "))
	}
	return nil
}

// onlyBridge reports whether a NIC value has exactly one bridge option equal
// to bridge and no tag/trunks/rate options.
func onlyBridge(v, bridge string) bool {
	bridges := 0
	for _, f := range strings.Split(v, ",") {
		k, val, _ := strings.Cut(f, "=")
		switch k {
		case "bridge":
			if val != bridge {
				return false
			}
			bridges++
		case "tag", "trunks", "rate":
			return false
		}
	}
	return bridges == 1
}

// Start refuses a guest that does not pass verification, so an unsanitized
// guest can never be started even if a caller ignores a Sanitize error.
func (n *ExistingNode) Start(ctx context.Context, kind model.GuestKind, vmid int) error {
	if err := n.guard(vmid); err != nil {
		return err
	}
	if err := n.verifySanitized(ctx, kind, vmid); err != nil {
		return fmt.Errorf("refusing to start: %w", err)
	}
	return n.task(ctx, kind, vmid, "start", n.api.Start)
}

// Destroy hard-stops (proof guests hold nothing worth a clean shutdown),
// destroys with purge, and confirms the guest is gone.
func (n *ExistingNode) Destroy(ctx context.Context, kind model.GuestKind, vmid int) error {
	return n.destroy(ctx, kind, vmid, nil)
}

func (n *ExistingNode) DestroyRecorded(ctx context.Context, kind model.GuestKind, vmid int, volid string) error {
	return n.destroy(ctx, kind, vmid, &Recorded{Kind: kind, VolID: volid})
}

// leftoverNameKey is the config key restore keeps from the backup and PBP
// renames only together with tagging.
func leftoverNameKey(kind model.GuestKind) string {
	if kind == model.KindLXC {
		return "hostname"
	}
	return "name"
}

// notLeftover says why the untagged guest g is not the leftover of the
// recorded restore r ("" if it is): it must be of r's kind, stopped, and
// either still locked by the restore or named as in r's backup. An
// unreadable original config is a reason (fail safe); a failed read of the
// guest's own config is an error.
func (n *ExistingNode) notLeftover(ctx context.Context, g pve.GuestRef, r Recorded) (string, error) {
	if g.Key.Kind != r.Kind || g.Node != n.node {
		return fmt.Sprintf("it is %s on %s, recorded %s on %s", g.Key.Kind, g.Node, r.Kind, n.node), nil
	}
	if g.Status != "stopped" {
		return "it is " + g.Status, nil
	}
	cfg, err := n.api.GuestConfig(ctx, n.node, g.Key.Kind, g.Key.VMID)
	if err != nil {
		return "", fmt.Errorf("read config %d: %w", g.Key.VMID, err)
	}
	return n.leftoverMismatch(ctx, g.Key.Kind, cfg, r.VolID), nil
}

// leftoverMismatch checks an untagged guest's config against the recorded
// backup: "lock: create" (restore still holds it) or the same non-empty
// name/hostname as the backup's original config. It returns "" on a match.
func (n *ExistingNode) leftoverMismatch(ctx context.Context, kind model.GuestKind, cfg map[string]string, volid string) string {
	if cfg["lock"] == "create" {
		return ""
	}
	raw, err := n.api.OriginalConfig(ctx, n.node, volid)
	if err != nil {
		return fmt.Sprintf("the original config of %s cannot be read (%v)", volid, err)
	}
	key := leftoverNameKey(kind)
	want := sanitize.ParseConfigText(raw)[key]
	if want == "" || cfg[key] != want {
		return fmt.Sprintf("its %s %q does not match %q from %s", key, cfg[key], want, volid)
	}
	return ""
}

// destroy removes the guest at vmid if PBP owns it: in range, of the expected
// kind on this node, and tagged "pbp" or, with rec, the stopped leftover of
// the recorded restore (see notLeftover).
func (n *ExistingNode) destroy(ctx context.Context, kind model.GuestKind, vmid int, rec *Recorded) error {
	if err := n.guard(vmid); err != nil {
		return err
	}
	ref, ok, err := n.find(ctx, vmid)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if ref.Key.Kind != kind || ref.Node != n.node {
		return fmt.Errorf("%w: %d is %s on %s, expected %s on %s", ErrNotOwned, vmid, ref.Key.Kind, ref.Node, kind, n.node)
	}
	cfg, err := n.api.GuestConfig(ctx, n.node, kind, vmid)
	if err != nil {
		return fmt.Errorf("read config %d: %w", vmid, err)
	}
	tagged := slices.Contains(sanitize.SplitList(cfg["tags"]), OwnerTag)
	if !tagged && rec == nil {
		return fmt.Errorf("%w: %d has no %q tag", ErrNotOwned, vmid, OwnerTag)
	}
	status, err := n.api.GuestStatus(ctx, n.node, kind, vmid)
	if err != nil {
		return fmt.Errorf("status %d: %w", vmid, err)
	}
	if !tagged {
		// PBP tags a guest before it can be started, so an untagged one
		// that runs is no leftover of PBP's.
		if status != "stopped" {
			return fmt.Errorf("%w: %d has no %q tag and is %s", ErrNotOwned, vmid, OwnerTag, status)
		}
		if why := n.leftoverMismatch(ctx, kind, cfg, rec.VolID); why != "" {
			return fmt.Errorf("%w: %d has no %q tag and %s", ErrNotOwned, vmid, OwnerTag, why)
		}
	}
	// Owned: a protected guest (restored from a protected guest's backup
	// whose tag write failed, or debris from a run before protection was
	// cleared at restore) would refuse destroy.
	if p := cfg["protection"]; p != "" && p != "0" {
		if err := n.api.SetGuestConfig(ctx, n.node, kind, vmid, map[string]string{"protection": "0"}, nil); err != nil {
			return fmt.Errorf("clear protection %d: %w", vmid, err)
		}
	}
	if status != "stopped" {
		if err := n.task(ctx, kind, vmid, "stop", n.api.Stop); err != nil {
			return err
		}
	}
	if err := n.task(ctx, kind, vmid, "destroy", n.api.Destroy); err != nil {
		return err
	}
	if _, still, err := n.find(ctx, vmid); err != nil {
		return fmt.Errorf("confirm %d destroyed: %w", vmid, err)
	} else if still {
		return fmt.Errorf("guest %d still exists after destroy", vmid)
	}
	return nil
}

// task runs a guest operation and waits for its task to finish.
func (n *ExistingNode) task(ctx context.Context, kind model.GuestKind, vmid int, what string, op func(context.Context, string, model.GuestKind, int) (string, error)) error {
	upid, err := op(ctx, n.node, kind, vmid)
	if err != nil {
		return fmt.Errorf("%s %d: %w", what, vmid, err)
	}
	if err := n.api.WaitTask(ctx, n.node, upid); err != nil {
		return fmt.Errorf("%s %d: %w", what, vmid, err)
	}
	return nil
}

func (n *ExistingNode) find(ctx context.Context, vmid int) (pve.GuestRef, bool, error) {
	guests, err := n.api.Guests(ctx)
	if err != nil {
		return pve.GuestRef{}, false, fmt.Errorf("list guests: %w", err)
	}
	for _, g := range guests {
		if g.Key.VMID == vmid {
			return g, true, nil
		}
	}
	return pve.GuestRef{}, false, nil
}

var reHTTPCode = regexp.MustCompile(`\b(401|403|500)\b`)

var errorMarkers = []string{"decrypt", "encryption key", "permission", "no space", "timeout", "timed out", "connection",
	"already exists", "can't lock", "is locked", "error trying to connect", "no route to host", "unreachable",
	"reset by peer", "broken pipe", "not online"}

// ClassifyRestoreError maps a restore failure to a verdict: ERROR when the
// node or PBP is at fault (key, permissions, space, timeouts, connectivity),
// FAILED when the backup itself is bad. nil yields "".
func ClassifyRestoreError(err error) model.Verdict {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.Is(err, ErrOutOfRange) || errors.Is(err, ErrUnsafe) {
		return model.Error
	}
	text := err.Error()
	var te *pve.TaskError
	if errors.As(err, &te) {
		text += "\n" + strings.Join(te.Log, "\n")
	}
	text = strings.ToLower(text)
	for _, m := range errorMarkers {
		if strings.Contains(text, m) {
			return model.Error
		}
	}
	// Status codes as whole numbers only: chunk digests are hex and may
	// contain "500" or "401".
	if reHTTPCode.MatchString(text) {
		return model.Error
	}
	return model.Failed
}
