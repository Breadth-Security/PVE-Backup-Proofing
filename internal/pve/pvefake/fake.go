// Package pvefake is a deterministic in-memory pve.API for tests.
package pvefake

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
	"git.breadthanalytics.com/tdiana/pbv/internal/pve"
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
	// Calls records each call as "Method arg arg...".
	Calls []string
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
	f.Calls = append(f.Calls, strings.Join(parts, " "))
	return f.Fail[method]
}

func (f *Fake) CallLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Calls...)
}

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
	return append([]model.Backup(nil), f.Backups[storage]...), nil
}

func (f *Fake) SetVolumeAttrs(ctx context.Context, node, storage, volid string, notes *string, protected *bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("SetVolumeAttrs", node, storage, volid); err != nil {
		return err
	}
	list := f.Backups[storage]
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
	return f.Memory, nil
}

func (f *Fake) NetIface(ctx context.Context, node, iface string) (pve.NetIface, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("NetIface", node, iface); err != nil {
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
	return f.begin("WaitTask", node, upid)
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

// Restore creates a guest whose config is parsed from BackupConfigs[volid].
func (f *Fake) Restore(ctx context.Context, node string, kind model.GuestKind, vmid int, volid, storage string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Restore", node, kind, vmid, volid, storage); err != nil {
		return "", err
	}
	if f.find(vmid) >= 0 {
		return "", fmt.Errorf("vmid %d already exists", vmid)
	}
	cfg := parseConfig(f.BackupConfigs[volid])
	f.Configs[vmid] = cfg
	f.GuestList = append(f.GuestList, pve.GuestRef{
		Key: model.GuestKey{Kind: kind, VMID: vmid}, Node: fakeNode, Name: cfg["name"], Status: "stopped",
	})
	return upid("restore", vmid), nil
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

func (f *Fake) GuestConfig(ctx context.Context, node string, kind model.GuestKind, vmid int) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("GuestConfig", node, kind, vmid); err != nil {
		return nil, err
	}
	cfg, ok := f.Configs[vmid]
	if !ok {
		return nil, fmt.Errorf("guest %d not found", vmid)
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
	if err := f.begin("SetGuestConfig", node, kind, vmid); err != nil {
		return err
	}
	cfg, ok := f.Configs[vmid]
	if !ok {
		return fmt.Errorf("guest %d not found", vmid)
	}
	for _, k := range del {
		delete(cfg, k)
	}
	for k, v := range set {
		cfg[k] = v
	}
	return nil
}

func (f *Fake) OriginalConfig(ctx context.Context, node, volid string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("OriginalConfig", node, volid); err != nil {
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
	i := f.find(vmid)
	if i < 0 {
		return "", fmt.Errorf("guest %d not found", vmid)
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
	return f.begin("AgentPing", node, vmid)
}

func (f *Fake) AgentOSInfo(ctx context.Context, node string, vmid int) (pve.OSInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("AgentOSInfo", node, vmid); err != nil {
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
	return 1, nil
}

func (f *Fake) AgentExecStatus(ctx context.Context, node string, vmid, pid int) (pve.AgentExecStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("AgentExecStatus", node, vmid, pid); err != nil {
		return pve.AgentExecStatus{}, err
	}
	return pve.AgentExecStatus{Exited: true}, nil
}
