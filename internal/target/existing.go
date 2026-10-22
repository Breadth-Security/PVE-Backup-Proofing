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

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/pve"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sanitize"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sshx"
)

// ExistingNode restores onto a PVE node that also runs production guests.
// Restore and config changes go through the API; container exec and the
// bridge's live state go through the restricted `pbp node` helper.
type ExistingNode struct {
	api    pve.API
	helper sshx.Helper
	node   string
	bridge string
	store  string
	lo, hi int

	// probeTimeout is restore.probe_timeout, which the helper's
	// request_timeout should cover.
	probeTimeout time.Duration
	// daemonVersion is what CheckNode requires the helper to match.
	daemonVersion string

	// Poll intervals; fields so tests can shorten them.
	statusPoll  time.Duration // GuestStatus while waiting for "running"
	agentPoll   time.Duration // AgentPing while waiting for the agent
	lxcExecPoll time.Duration // "pct exec true" while a container boots
	execPoll    time.Duration // AgentExecStatus while a command runs
}

var _ Target = (*ExistingNode)(nil)

// NewExistingNode returns the target for cfg's node, reached through api and
// the node's helper.
func NewExistingNode(cfg config.Config, api pve.API, helper sshx.Helper) *ExistingNode {
	return &ExistingNode{
		api:          api,
		helper:       helper,
		node:         cfg.Target.Node,
		bridge:       cfg.Target.Bridge,
		store:        cfg.Restore.Storage,
		lo:           cfg.Target.VMIDRange[0],
		hi:           cfg.Target.VMIDRange[1],
		probeTimeout: cfg.Restore.ProbeTimeout,
		statusPoll:   time.Second,
		agentPoll:    3 * time.Second,
		lxcExecPoll:  2 * time.Second,
		execPoll:     500 * time.Millisecond,
	}
}

// SetDaemonVersion sets the daemon version CheckNode requires the helper to
// be compatible with.
func (n *ExistingNode) SetDaemonVersion(v string) { n.daemonVersion = v }

func (n *ExistingNode) inRange(vmid int) bool { return vmid >= n.lo && vmid <= n.hi }

// guard must precede every API call that names a VMID which could be
// outside the range.
func (n *ExistingNode) guard(vmid int) error {
	if !n.inRange(vmid) {
		return fmt.Errorf("%w: %d not in [%d, %d]", ErrOutOfRange, vmid, n.lo, n.hi)
	}
	return nil
}

func (n *ExistingNode) Prepare(ctx context.Context) error {
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
	if why := NotIsolated(iface); why != "" {
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

	return nil
}

func (n *ExistingNode) CheckRangeClear(ctx context.Context) error {
	guests, err := n.api.Guests(ctx)
	if err != nil {
		return fmt.Errorf("list guests: %w", err)
	}
	foreign := ForeignInRange(guests, n.lo, n.hi)
	if len(foreign) > 0 {
		return fmt.Errorf("%w: %s", ErrForeignInRange, strings.Join(foreign, ", "))
	}
	return nil
}

func (n *ExistingNode) Owned(ctx context.Context) ([]pve.GuestRef, error) {
	guests, err := n.api.Guests(ctx)
	if err != nil {
		return nil, err
	}
	var out []pve.GuestRef
	for _, g := range guests {
		if n.inRange(g.Key.VMID) && slices.Contains(g.Tags, OwnerTag) && n.onNode(g) {
			out = append(out, g)
		}
	}
	return out, nil
}

// onNode reports whether g is on this target's node. A guest with no node
// in the listing counts as this node's.
func (n *ExistingNode) onNode(g pve.GuestRef) bool { return g.Node == "" || g.Node == n.node }

func (n *ExistingNode) StrayTagged(ctx context.Context) ([]pve.GuestRef, error) {
	guests, err := n.api.Guests(ctx)
	if err != nil {
		return nil, err
	}
	var out []pve.GuestRef
	for _, g := range guests {
		// In the range but on another cluster node (a migrated proof
		// guest, or target.node changed) is stray too: Destroy refuses
		// it, so the janitor must not count it as its own (code review
		// 1.7).
		if slices.Contains(g.Tags, OwnerTag) && (!n.inRange(g.Key.VMID) || !n.onNode(g)) {
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
// The tag goes on in the first config call, built from origTags, so no read
// precedes it. Until then the guest is PBP's only by the caller's state
// record (see Destroy).
func (n *ExistingNode) Restore(ctx context.Context, b model.Backup, vmid int, origTags string) error {
	if err := n.guard(vmid); err != nil {
		return notStarted{err}
	}
	kind := b.Key.Kind
	upid, err := n.api.Restore(ctx, n.node, kind, vmid, b.VolID, n.store)
	if err != nil {
		err = fmt.Errorf("restore %s to %d: %w", b.VolID, vmid, err)
		// Only an API error response counts as a refusal (a transport
		// failure may hide an accepted request). Should a pveproxy 5xx
		// follow a forwarded request, its guest is untagged and unrecorded:
		// foreign, never destroyed (§11.2).
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
	// same call, so the guest is destroyable from here on, and onboot again
	// (the restore already overrode it), so a copy that Sanitize never
	// reaches can still not autostart on its production NICs.
	set := map[string]string{"tags": sanitize.AddTag(origTags, OwnerTag), "protection": "0", "onboot": "0"}
	// LXC has no "name"; renaming its hostname would change the guest itself.
	if kind == model.KindQEMU {
		set["name"] = fmt.Sprintf("pbp-%d-%s", b.Key.VMID, b.Time.UTC().Format("200601021504"))
	}
	if err := n.api.SetGuestConfig(ctx, n.node, kind, vmid, set, nil); err != nil {
		return fmt.Errorf("tag restored guest %d: %w", vmid, err)
	}
	return nil
}

func (n *ExistingNode) Sanitize(ctx context.Context, kind model.GuestKind, vmid int) (Sanitized, error) {
	if err := n.guard(vmid); err != nil {
		return Sanitized{}, err
	}
	cfg, err := n.api.GuestConfig(ctx, n.node, kind, vmid)
	if err != nil {
		return Sanitized{}, fmt.Errorf("read config %d: %w", vmid, err)
	}
	plan := sanitize.Sanitize(kind, cfg, n.bridge)
	if len(plan.Unsafe) > 0 {
		return Sanitized{}, fmt.Errorf("%w: %s", ErrUnsafe, strings.Join(plan.Unsafe, ", "))
	}
	removeNotInBackup(&plan, cfg, vmid)
	if err := n.mayRemoveDrives(ctx, plan, cfg, vmid); err != nil {
		return Sanitized{}, err
	}
	if err := n.api.SetGuestConfig(ctx, n.node, kind, vmid, plan.Set, plan.Delete); err != nil {
		return Sanitized{}, fmt.Errorf("sanitize config %d: %w", vmid, err)
	}
	if err := n.verifySanitized(ctx, kind, vmid); err != nil {
		return Sanitized{}, err
	}
	return Sanitized{Removed: plan.Removed, Changes: sanitizeChanges(cfg, plan)}, nil
}

// sanitizeChanges describes the edits plan makes to cfg, sorted by key. A
// key set to the value it already has is not a change.
func sanitizeChanges(cfg map[string]string, plan sanitize.Plan) []string {
	type change struct{ key, line string }
	var cs []change
	for k, v := range plan.Set {
		old, ok := cfg[k]
		if ok && old == v {
			continue
		}
		if !ok {
			old = "(unset)"
		}
		cs = append(cs, change{k, fmt.Sprintf("%s: %s \u2192 %s", k, old, v)})
	}
	for _, k := range plan.Delete {
		cs = append(cs, change{k, fmt.Sprintf("%s removed (%s)", k, cfg[k])})
	}
	slices.SortFunc(cs, func(a, b change) int { return strings.Compare(a.key, b.key) })
	lines := make([]string, len(cs))
	for i, c := range cs {
		lines[i] = c.line
	}
	return lines
}

var reNet = regexp.MustCompile(`^net\d+$`)

// verifySanitized re-reads the live config and checks it is safe to start:
// tagged "pbp", onboot and protection off, every NIC on the isolated bridge with no VLAN, rate
// or MTU options, and nothing left that Sanitize would remove or refuse.
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
	for _, k := range keys {
		if why := foreignDisk(k, cfg[k], vmid); why != "" {
			bad = append(bad, why)
		}
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
// to bridge and no tag/trunks/rate/mtu options.
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
		case "tag", "trunks", "rate", "mtu":
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
// destroys with purge, and confirms the guest is gone. Checks, in order:
// range, existence, kind and node, then the "pbp" tag or else rec's
// leftover rule (stopped, then identity; see notLeftover).
func (n *ExistingNode) Destroy(ctx context.Context, kind model.GuestKind, vmid int, rec Recorded) error {
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
	if !tagged && rec.VolID == "" {
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
		if why := n.notLeftover(ctx, kind, cfg, rec); why != "" {
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

// notLeftover says why the stopped, untagged guest with config cfg is not
// the leftover of the recorded restore rec ("" if it is): it must still be
// locked by the restore ("lock: create") or carry the same non-empty name
// (hostname for a container) as rec's backup. A backup without a name is
// matched by its NICs' MAC addresses instead, which the restore keeps: the
// same non-empty set (code review 1.6b). The name is rec.Name, or read from
// rec.VolID's original config; an unreadable one is a reason (fail safe).
func (n *ExistingNode) notLeftover(ctx context.Context, kind model.GuestKind, cfg map[string]string, rec Recorded) string {
	if cfg["lock"] == "create" {
		return ""
	}
	want := rec.Name
	var orig map[string]string
	if want == "" {
		raw, err := n.api.OriginalConfig(ctx, n.node, rec.VolID)
		if err != nil {
			return fmt.Sprintf("the original config of %s cannot be read (%v)", rec.VolID, err)
		}
		orig = sanitize.ParseConfigText(raw)
		want = sanitize.GuestName(kind, orig)
	}
	got := sanitize.GuestName(kind, cfg)
	if want == "" && orig != nil {
		if have, need := macs(cfg), macs(orig); len(need) > 0 && slices.Equal(have, need) {
			return ""
		}
		return fmt.Sprintf("its name %q does not match the unnamed backup %s, nor do its NIC MAC addresses %v (%v in the backup)",
			got, rec.VolID, macs(cfg), macs(orig))
	}
	if want == "" || got != want {
		return fmt.Sprintf("its name %q does not match %q from %s", got, want, rec.VolID)
	}
	return ""
}

var reMAC = regexp.MustCompile(`(?i)\b[0-9a-f]{2}(?::[0-9a-f]{2}){5}\b`)

// macs returns the sorted, upper-cased MAC addresses of a config's netN
// entries.
func macs(cfg map[string]string) []string {
	var out []string
	for k, v := range cfg {
		if !strings.HasPrefix(k, "net") || strings.TrimLeft(k[3:], "0123456789") != "" || k == "net" {
			continue
		}
		for _, m := range reMAC.FindAllString(v, -1) {
			out = append(out, strings.ToUpper(m))
		}
	}
	slices.Sort(out)
	return out
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

// damageMarkers name damage to the backup itself: only these make a restore
// failure FAILED (code review I6). A chunk missing from the datastore shows
// as a failed read of its .chunks/ path (PBS answers HTTP 400).
var damageMarkers = []string{"corrupt", "bad chunk", "chunk digest", "wrong digest", "digest mismatch",
	"checksum", "failed to decode", "decode error", "unable to parse", "invalid magic", "bad magic",
	"missing chunk", "/.chunks/", "unable to load chunk"}

// ClassifyRestoreError maps a restore failure to a verdict. FAILED, which
// blames the backup, only for known damage to it (corrupt or missing
// chunks, checksum or digest mismatches, undecodable data); anything else
// (the node, PBS, permissions, space, memory, timeouts, connectivity, or an
// error PBP does not recognise) is ERROR. A node-fault marker wins over a
// damage marker (a corrupt-looking message about a missing key is the
// key's fault). nil yields "".
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
	for _, m := range damageMarkers {
		if strings.Contains(text, m) {
			return model.Failed
		}
	}
	return model.Error
}

// reVolKey matches the config keys of a guest's disks and volumes (VM
// drives, EFI and TPM state; a container's rootfs and mount points).
var reVolKey = regexp.MustCompile(`^(scsi|virtio|sata|ide|efidisk|tpmstate|mp)\d+$|^rootfs$`)

// reVolOwner finds the owner VMID in a volume name: vm-<id>-…, subvol-<id>-….
var reVolOwner = regexp.MustCompile(`^(?:vm|subvol)-(\d+)-`)

// foreignDisk says why disk key k with value v may not belong to the
// temporary guest vmid ("" when it does): a host path, or a volume named
// after another VMID. "none", an ISO image or the host's CD drive as a
// CD-ROM, and non-disk keys are not judged.
// PVE's restore always creates the copy's own volumes; this guards against
// a change in that, so the copy can never boot from a production disk
// (code review 1.9).
func foreignDisk(k, v string, vmid int) string {
	if !reVolKey.MatchString(k) {
		return ""
	}
	vol, opts, _ := strings.Cut(v, ",")
	if vol == "none" {
		return ""
	}
	if slices.Contains(strings.Split(opts, ","), "media=cdrom") {
		// An ISO image or the host's drive is read-only media, not a
		// disk; any other volume as a CD-ROM is judged like a disk
		// (#50 review): a production volume must not be attached at all.
		if _, name, ok := strings.Cut(vol, ":"); vol == "cdrom" || (ok && strings.HasPrefix(name, "iso/")) {
			return ""
		}
	}
	if strings.HasPrefix(vol, "/") {
		return fmt.Sprintf("%s is a host path (%s)", k, vol)
	}
	_, name, ok := strings.Cut(vol, ":")
	if !ok {
		return fmt.Sprintf("%s has no storage volume (%s)", k, vol)
	}
	name = name[strings.LastIndex(name, "/")+1:] // linked clone base, dir storage subdir
	m := reVolOwner.FindStringSubmatch(name)
	if m == nil || m[1] != strconv.Itoa(vmid) {
		return fmt.Sprintf("%s is not a volume of guest %d (%s)", k, vmid, vol)
	}
	return ""
}

// removeNotInBackup adds to plan the drives of the restored config that are
// not the copy's own: PVE's restore rewrites only the drives in the backup
// and keeps every other line as it is, so such a line names a production
// volume, such as a leftover efidisk0 on a SeaBIOS VM that vzdump does not
// back up (issue #67). Each is removed and reported as not covered by the
// backup, so the proof goes on without it. rootfs is never removed: a
// container without its root is no proof, and verifySanitized refuses it.
func removeNotInBackup(plan *sanitize.Plan, cfg map[string]string, vmid int) {
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if k == "rootfs" || slices.Contains(plan.Delete, k) || foreignDisk(k, cfg[k], vmid) == "" {
			continue
		}
		plan.Delete = append(plan.Delete, k)
		plan.Removed = append(plan.Removed, model.MissingPiece{Kind: "excluded-disk", Key: k, Detail: "not in the backup"})
	}
	slices.Sort(plan.Delete)
}

// privConfigDisk is the PVE privilege that removing a drive from a guest's
// config needs. PBP's role does not grant it by default (issue #67).
const privConfigDisk = "VM.Config.Disk"

// mayRemoveDrives checks, when plan removes drives (a host disk, or one the
// backup does not hold), that PBP's token may: it needs VM.Config.Disk on
// the copy. Without it the copy cannot be made safe, so the guest ends
// ERROR before anything changes, naming the drives and both fixes.
func (n *ExistingNode) mayRemoveDrives(ctx context.Context, plan sanitize.Plan, cfg map[string]string, vmid int) error {
	var drives []string
	for _, k := range plan.Delete {
		if reVolKey.MatchString(k) {
			vol, _, _ := strings.Cut(cfg[k], ",")
			drives = append(drives, fmt.Sprintf("%s (%s)", k, vol))
		}
	}
	if len(drives) == 0 {
		return nil
	}
	path := "/vms/" + strconv.Itoa(vmid)
	privs, err := n.api.Privileges(ctx, path)
	if err != nil {
		return fmt.Errorf("read the token's privileges on %s: %w", path, err)
	}
	if privs[privConfigDisk] {
		return nil
	}
	return fmt.Errorf("%w: the copy has drives that are not its own: %s; removing them needs %s, which PBP's token lacks: "+
		"remove them from the original guest's config, or grant %s on /vms to PBP's token (docs/security.md)",
		ErrUnsafe, strings.Join(drives, ", "), privConfigDisk, privConfigDisk)
}
