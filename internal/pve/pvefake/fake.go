// Package pvefake is a deterministic in-memory pve.API for tests.
package pvefake

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/pve"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/testutil"
)

var _ pve.API = (*Fake)(nil)

const fakeNode = "pve1"

// Fake implements pve.API in memory. Seed the exported fields before use; all
// methods are safe for concurrent use. Direct field access by tests is not
// synchronised, so seed before starting workers and read after they finish.
type Fake struct {
	mu sync.Mutex

	// GuestList holds the guests (not named Guests: that is the API method).
	GuestList     []pve.GuestRef
	Configs       map[int]map[string]string
	Backups       map[string][]model.Backup // keyed by storage
	BackupConfigs map[string]string         // keyed by volid: raw original config
	Storage       map[string]pve.StorageStatus
	Memory        pve.NodeMemory
	Ifaces        map[string]pve.NetIface
	VersionString string
	// Fail maps a method name (e.g. "Restore") to an error returned by it.
	Fail map[string]error
	// FailOn, if set, is asked on every call after Fail: a non-nil error
	// fails that one call. args are the call's arguments as logged, so a
	// test can fail, say, only the second SetGuestConfig of one VMID. It
	// runs with the fake locked: it must not call the fake.
	FailOn func(method string, args []string) error
	// Node is the only node name the fake answers for, like a real
	// cluster; "" means pve1. Calls naming another node fail.
	Node string
	// calls records each call as "Method arg arg...".
	calls testutil.CallLog
}

func New() *Fake {
	return &Fake{
		Configs:       map[int]map[string]string{},
		Backups:       map[string][]model.Backup{},
		BackupConfigs: map[string]string{},
		Storage:       map[string]pve.StorageStatus{},
		Ifaces:        map[string]pve.NetIface{},
		VersionString: "9.2.18",
		Fail:          map[string]error{},
	}
}

// begin records the call and returns the injected failure, if any. Callers hold f.mu.
func (f *Fake) begin(method string, args ...any) error {
	parts := append([]string{method}, make([]string, 0, len(args))...)
	for _, a := range args {
		parts = append(parts, fmt.Sprint(a))
	}
	f.calls.Add(strings.Join(parts, " "))
	if err := f.Fail[method]; err != nil {
		return err
	}
	if f.FailOn != nil {
		return f.FailOn(method, parts[1:])
	}
	return nil
}

// at fails a call naming a node other than the fake's, as the real API
// does. Callers hold f.mu.
func (f *Fake) at(node string) error {
	want := f.Node
	if want == "" {
		want = fakeNode
	}
	if node != want {
		return fmt.Errorf("hostname lookup '%s' failed - no such node", node)
	}
	return nil
}

// splitTags splits a PVE tags value (separated by ';', ',' or spaces).
func splitTags(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == ';' || r == ',' || r == ' ' })
}

// syncListing makes GuestList show a guest's config name and tags, like
// /cluster/resources. Callers hold f.mu.
func (f *Fake) syncListing(vmid int) {
	i := f.find(vmid)
	if i < 0 {
		return
	}
	cfg := f.Configs[vmid]
	f.GuestList[i].Tags = splitTags(cfg["tags"])
	name := cfg["name"]
	if name == "" {
		name = cfg["hostname"]
	}
	f.GuestList[i].Name = name
}

// CallLog returns the calls recorded so far, each as "Method arg arg...".
func (f *Fake) CallLog() []string { return f.calls.Snapshot() }

// ResetCalls forgets the calls recorded so far.
func (f *Fake) ResetCalls() { f.calls.Reset() }

func (f *Fake) Version(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Version"); err != nil {
		return "", err
	}
	return f.VersionString, nil
}

func (f *Fake) ListBackups(ctx context.Context, node, storage string) ([]model.Backup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("ListBackups", node, storage); err != nil {
		return nil, err
	}
	if err := f.at(node); err != nil {
		return nil, err
	}
	out := append([]model.Backup(nil), f.Backups[storage]...)
	for i := range out {
		// Like PBS: the listing shows only the first line ("comment").
		out[i].Notes, _, _ = strings.Cut(out[i].Notes, "\n")
	}
	return out, nil
}

// VolumeNotes returns the full notes of a seeded backup.
func (f *Fake) VolumeNotes(ctx context.Context, node, storage, volid string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("VolumeNotes", node, storage, volid); err != nil {
		return "", err
	}
	if err := f.at(node); err != nil {
		return "", err
	}
	for _, b := range f.Backups[storage] {
		if b.VolID == volid {
			return b.Notes, nil
		}
	}
	return "", fmt.Errorf("volume %s does not exist", volid)
}

func (f *Fake) VolumeProtected(ctx context.Context, node, storage, volid string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("VolumeProtected", node, storage, volid); err != nil {
		return false, err
	}
	if err := f.at(node); err != nil {
		return false, err
	}
	for _, b := range f.Backups[storage] {
		if b.VolID == volid {
			return b.Protected, nil
		}
	}
	return false, fmt.Errorf("volume %s does not exist", volid)
}

func (f *Fake) SetVolumeAttrs(ctx context.Context, node, storage, volid string, notes *string, protected *bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := f.Backups[storage]
	// The log names the backup's guest so tests can assert which guests' backups were written.
	guest := "unknown"
	for _, b := range list {
		if b.VolID == volid {
			guest = b.Key.String()
		}
	}
	if err := f.begin("SetVolumeAttrs", node, storage, volid, "guest="+guest); err != nil {
		return err
	}
	if err := f.at(node); err != nil {
		return err
	}
	if guest == "unknown" {
		return fmt.Errorf("volume %s does not exist", volid)
	}
	for i := range list {
		if list[i].VolID != volid {
			continue
		}
		if notes != nil {
			list[i].Notes = *notes
		}
		if protected != nil {
			list[i].Protected = *protected
		}
	}
	return nil
}

func (f *Fake) StorageStatus(ctx context.Context, node, storage string) (pve.StorageStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("StorageStatus", node, storage); err != nil {
		return pve.StorageStatus{}, err
	}
	if err := f.at(node); err != nil {
		return pve.StorageStatus{}, err
	}
	s, ok := f.Storage[storage]
	if !ok {
		return pve.StorageStatus{}, fmt.Errorf("storage %s not found", storage)
	}
	return s, nil
}

func (f *Fake) NodeMemory(ctx context.Context, node string) (pve.NodeMemory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("NodeMemory", node); err != nil {
		return pve.NodeMemory{}, err
	}
	if err := f.at(node); err != nil {
		return pve.NodeMemory{}, err
	}
	return f.Memory, nil
}

func (f *Fake) NetIface(ctx context.Context, node, iface string) (pve.NetIface, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("NetIface", node, iface); err != nil {
		return pve.NetIface{}, err
	}
	if err := f.at(node); err != nil {
		return pve.NetIface{}, err
	}
	n, ok := f.Ifaces[iface]
	if !ok {
		return pve.NetIface{}, fmt.Errorf("interface %s not found", iface)
	}
	return n, nil
}

// WaitTask returns immediately: fake tasks complete instantly.
func (f *Fake) WaitTask(ctx context.Context, node, upid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("WaitTask", node, upid); err != nil {
		return err
	}
	return f.at(node)
}

func (f *Fake) find(vmid int) int {
	for i, g := range f.GuestList {
		if g.Key.VMID == vmid {
			return i
		}
	}
	return -1
}

func (f *Fake) Guests(ctx context.Context) ([]pve.GuestRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Guests"); err != nil {
		return nil, err
	}
	return append([]pve.GuestRef(nil), f.GuestList...), nil
}

// Restore creates a guest whose config is parsed from BackupConfigs[volid]; like
// the real API it fails for a volid that was not seeded.
func (f *Fake) Restore(ctx context.Context, node string, kind model.GuestKind, vmid int, volid, storage string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Restore", node, kind, vmid, volid, storage); err != nil {
		return "", err
	}
	if err := f.at(node); err != nil {
		return "", err
	}
	if f.find(vmid) >= 0 {
		return "", fmt.Errorf("vmid %d already exists", vmid)
	}
	raw, ok := f.BackupConfigs[volid]
	if !ok {
		return "", fmt.Errorf("volume %s does not exist", volid)
	}
	cfg := parseConfig(raw)
	// Like PVE, the restore creates new volumes named after the new VMID.
	// When the backup has #qmdump#map lines, only the drives they list are
	// restored: a backup=0 drive is commented out, and any other drive
	// line is kept as it is (PVE's restore_update_config_line).
	mapped := qmdumpMap(raw)
	for k, v := range cfg {
		if !reVolKey.MatchString(k) {
			continue
		}
		switch {
		case mapped == nil || mapped[k]:
			cfg[k] = reOwnVol.ReplaceAllString(v, fmt.Sprintf("${1}-%d-", vmid))
		case strings.Contains(","+v+",", ",backup=0,"):
			delete(cfg, k)
		}
	}
	// Like PVE with the client's onboot=0 restore override.
	cfg["onboot"] = "0"
	f.Configs[vmid] = cfg
	f.GuestList = append(f.GuestList, pve.GuestRef{
		Key: model.GuestKey{Kind: kind, VMID: vmid}, Node: node, Status: "stopped",
	})
	f.syncListing(vmid)
	return upid("restore", vmid), nil
}

// reVolKey matches the config keys of disks and volumes; reOwnVol a volume
// name's owner part ("vm-101-", "subvol-101-").
var (
	reVolKey = regexp.MustCompile(`^(scsi|virtio|sata|ide|efidisk|tpmstate|mp)\d+$|^rootfs$`)
	reOwnVol = regexp.MustCompile(`\b(vm|subvol)-\d+-`)
)

// qmdumpMap returns the drive keys a VM backup's "#qmdump#map:<key>:…"
// lines list, or nil when it has none (a container, or a hand-made test
// config).
func qmdumpMap(raw string) map[string]bool {
	var m map[string]bool
	for _, line := range strings.Split(raw, "\n") {
		if rest, ok := strings.CutPrefix(line, "#qmdump#map:"); ok {
			if m == nil {
				m = map[string]bool{}
			}
			k, _, _ := strings.Cut(rest, ":")
			m[k] = true
		}
	}
	return m
}

// parseConfig reads "key: value" lines, skipping '#' comments and stopping at the first '[' section.
func parseConfig(raw string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

func upid(op string, vmid int) string { return fmt.Sprintf("UPID:%s:fake:%s:%d:", fakeNode, op, vmid) }

// config returns the live config of a listed guest, creating an empty one when
// the guest was seeded in GuestList without Configs. Callers hold f.mu.
func (f *Fake) config(vmid int) (map[string]string, error) {
	if f.find(vmid) < 0 {
		return nil, fmt.Errorf("guest %d not found", vmid)
	}
	cfg, ok := f.Configs[vmid]
	if !ok {
		cfg = map[string]string{}
		f.Configs[vmid] = cfg
	}
	return cfg, nil
}

func (f *Fake) GuestConfig(ctx context.Context, node string, kind model.GuestKind, vmid int) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("GuestConfig", node, kind, vmid); err != nil {
		return nil, err
	}
	if err := f.at(node); err != nil {
		return nil, err
	}
	cfg, err := f.config(vmid)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(cfg))
	for k, v := range cfg {
		out[k] = v
	}
	return out, nil
}

func (f *Fake) SetGuestConfig(ctx context.Context, node string, kind model.GuestKind, vmid int, set map[string]string, del []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("SetGuestConfig", node, kind, vmid, "set="+fmt.Sprint(set), "del="+fmt.Sprint(del)); err != nil {
		return err
	}
	if err := f.at(node); err != nil {
		return err
	}
	cfg, err := f.config(vmid)
	if err != nil {
		return err
	}
	for _, k := range del {
		delete(cfg, k)
	}
	for k, v := range set {
		cfg[k] = v
	}
	f.syncListing(vmid)
	return nil
}

func (f *Fake) OriginalConfig(ctx context.Context, node, volid string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("OriginalConfig", node, volid); err != nil {
		return "", err
	}
	if err := f.at(node); err != nil {
		return "", err
	}
	s, ok := f.BackupConfigs[volid]
	if !ok {
		return "", fmt.Errorf("no config for %s", volid)
	}
	return s, nil
}

func (f *Fake) setStatus(op string, method string, node string, kind model.GuestKind, vmid int, status string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(method, node, kind, vmid); err != nil {
		return "", err
	}
	if err := f.at(node); err != nil {
		return "", err
	}
	i := f.find(vmid)
	if i < 0 {
		return "", fmt.Errorf("guest %d not found", vmid)
	}
	f.GuestList[i].Status = status
	return upid(op, vmid), nil
}

func (f *Fake) Start(ctx context.Context, node string, kind model.GuestKind, vmid int) (string, error) {
	return f.setStatus("start", "Start", node, kind, vmid, "running")
}

func (f *Fake) Shutdown(ctx context.Context, node string, kind model.GuestKind, vmid int) (string, error) {
	return f.setStatus("shutdown", "Shutdown", node, kind, vmid, "stopped")
}

func (f *Fake) Stop(ctx context.Context, node string, kind model.GuestKind, vmid int) (string, error) {
	return f.setStatus("stop", "Stop", node, kind, vmid, "stopped")
}

func (f *Fake) Destroy(ctx context.Context, node string, kind model.GuestKind, vmid int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Destroy", node, kind, vmid); err != nil {
		return "", err
	}
	if err := f.at(node); err != nil {
		return "", err
	}
	i := f.find(vmid)
	if i < 0 {
		return "", fmt.Errorf("guest %d not found", vmid)
	}
	if f.GuestList[i].Status == "running" {
		return "", fmt.Errorf("VM %d is running - destroy failed", vmid)
	}
	if f.Configs[vmid]["protection"] == "1" {
		return "", fmt.Errorf("can't remove VM %d - protection mode enabled", vmid)
	}
	f.GuestList = append(f.GuestList[:i], f.GuestList[i+1:]...)
	delete(f.Configs, vmid)
	return upid("destroy", vmid), nil
}

func (f *Fake) GuestStatus(ctx context.Context, node string, kind model.GuestKind, vmid int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("GuestStatus", node, kind, vmid); err != nil {
		return "", err
	}
	if err := f.at(node); err != nil {
		return "", err
	}
	i := f.find(vmid)
	if i < 0 {
		return "", fmt.Errorf("guest %d not found", vmid)
	}
	return f.GuestList[i].Status, nil
}

// The agent methods answer as a healthy Debian guest; use Fail to simulate otherwise.

func (f *Fake) AgentPing(ctx context.Context, node string, vmid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("AgentPing", node, vmid); err != nil {
		return err
	}
	return f.at(node)
}

func (f *Fake) AgentOSInfo(ctx context.Context, node string, vmid int) (pve.OSInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("AgentOSInfo", node, vmid); err != nil {
		return pve.OSInfo{}, err
	}
	if err := f.at(node); err != nil {
		return pve.OSInfo{}, err
	}
	return pve.OSInfo{ID: "debian", Name: "Debian GNU/Linux"}, nil
}

func (f *Fake) AgentExec(ctx context.Context, node string, vmid int, argv []string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("AgentExec", node, vmid, strings.Join(argv, " ")); err != nil {
		return 0, err
	}
	if err := f.at(node); err != nil {
		return 0, err
	}
	return 1, nil
}

func (f *Fake) AgentExecStatus(ctx context.Context, node string, vmid, pid int) (pve.AgentExecStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("AgentExecStatus", node, vmid, pid); err != nil {
		return pve.AgentExecStatus{}, err
	}
	if err := f.at(node); err != nil {
		return pve.AgentExecStatus{}, err
	}
	return pve.AgentExecStatus{Exited: true}, nil
}
