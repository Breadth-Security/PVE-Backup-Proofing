package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/pve"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/pve/pvefake"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sshx"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/state"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/target"
)

// destroyLog records the VMID of every Target.Destroy call.
type destroyLog struct {
	target.Target
	mu    sync.Mutex
	calls []int
}

// CheckNode passes: these tests exercise the janitor and range checks on the
// real target, not the node helper (see target's CheckNode tests).
func (t *destroyLog) CheckNode(context.Context) error { return nil }

func (t *destroyLog) Destroy(ctx context.Context, kind model.GuestKind, vmid int, rec target.Recorded) error {
	t.mu.Lock()
	t.calls = append(t.calls, vmid)
	t.mu.Unlock()
	return t.Target.Destroy(ctx, kind, vmid, rec)
}

type janitorFixture struct {
	d       Deps
	api     *pvefake.Fake
	tgt     *destroyLog
	store   *state.Store
	logBuf  *bytes.Buffer
	allowed map[string]bool // volids it may write: seeded in-flight records, ownBackup
}

// newJanitorFixture uses the real existing-node target so that ownership
// checks are the production ones.
func newJanitorFixture(t *testing.T) *janitorFixture {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	cfg.Target.Node = "pve1"
	cfg.Target.Bridge = "vmbr9"
	cfg.Target.VMIDRange = [2]int{990000, 990009}
	cfg.Restore.Storage = "local-zfs"
	api := pvefake.New()
	var logBuf bytes.Buffer
	tgt := &destroyLog{Target: target.NewExistingNode(cfg, api, sshx.Helper{})}
	f := &janitorFixture{api: api, tgt: tgt, store: store, logBuf: &logBuf}
	f.d = Deps{
		Cfg: cfg, API: api, Target: tgt, Store: store,
		Log: slog.New(slog.NewTextHandler(&logBuf, nil)),
		Now: func() time.Time { return now },
	}
	t.Cleanup(func() { assertWritesOnly(t, api.CallLog(), f.allowed) })
	return f
}

// guest adds a running guest; tagged adds the "pbp" tag.
func (f *janitorFixture) guest(kind model.GuestKind, vmid int, tagged bool) {
	ref := pve.GuestRef{Key: model.GuestKey{Kind: kind, VMID: vmid}, Node: "pve1", Status: "running"}
	cfg := map[string]string{"name": "x"}
	if tagged {
		ref.Tags = []string{"pbp"}
		cfg["tags"] = "pbp"
	}
	f.api.GuestList = append(f.api.GuestList, ref)
	f.api.Configs[vmid] = cfg
}

// stoppedGuest adds a stopped untagged qemu guest at vmid named name.
func (f *janitorFixture) stoppedGuest(vmid int, name string) {
	f.guest(model.KindQEMU, vmid, false)
	f.api.GuestList[len(f.api.GuestList)-1].Status = "stopped"
	f.api.Configs[vmid]["name"] = name
}

// backup adds a backup volume on storage pbs with the given protected flag.
func (f *janitorFixture) backup(volid string, protected bool) {
	f.api.Backups["pbs"] = append(f.api.Backups["pbs"], model.Backup{Storage: "pbs", VolID: volid, Protected: protected})
}

func (f *janitorFixture) protected(volid string) bool {
	for _, b := range f.api.Backups["pbs"] {
		if b.VolID == volid {
			return b.Protected
		}
	}
	panic("no backup " + volid)
}

func (f *janitorFixture) inFlight(jobs ...state.Job) {
	err := f.store.Update(func(st *state.State) error {
		for _, j := range jobs {
			st.InFlight[j.TempVMID] = j
			f.allow(j.VolID)
		}
		return nil
	})
	if err != nil {
		panic(err)
	}
}

func (f *janitorFixture) exists(vmid int) bool {
	return slices.ContainsFunc(f.api.GuestList, func(g pve.GuestRef) bool { return g.Key.VMID == vmid })
}

func (f *janitorFixture) destroyed(vmid int) bool {
	return slices.ContainsFunc(f.api.CallLog(), func(c string) bool {
		return strings.HasPrefix(c, "Destroy pve1 ") && strings.HasSuffix(c, fmt.Sprintf(" %d", vmid))
	})
}

func ptr(b bool) *bool { return &b }

func TestJanitor(t *testing.T) {
	f := newJanitorFixture(t)
	// Crashed after start: tagged guest, backup protected by PBP.
	f.guest(model.KindQEMU, 990000, true)
	f.backup("pbs:v0", true)
	// Untagged running guest at a recorded VMID: refused, since PBP never
	// starts a guest it has not tagged (see ownership_test.go for the
	// stopped partial restore the janitor does remove).
	f.guest(model.KindQEMU, 990001, false)
	f.backup("pbs:v1", true)
	// Destroyed but not unprotected.
	f.backup("pbs:v2", true)
	// Crashed after protect, before restore: no guest; backup was already
	// protected by its owner.
	f.backup("pbs:v3", true)
	// A leftover owned guest the state does not know, and a pbp-tagged guest
	// outside the range.
	f.guest(model.KindLXC, 990005, true)
	f.guest(model.KindQEMU, 500, true)
	f.inFlight(
		state.Job{TempVMID: 990000, Kind: model.KindQEMU, VolID: "pbs:v0", Storage: "pbs", ProtectedOrig: ptr(false), Stage: state.StageStarted},
		state.Job{TempVMID: 990001, Kind: model.KindQEMU, VolID: "pbs:v1", Storage: "pbs", ProtectedOrig: ptr(false), Stage: state.StageRestored},
		state.Job{TempVMID: 990002, Kind: model.KindQEMU, VolID: "pbs:v2", Storage: "pbs", ProtectedOrig: ptr(false), Stage: state.StageDestroyed},
		state.Job{TempVMID: 990003, Kind: model.KindLXC, VolID: "pbs:v3", Storage: "pbs", ProtectedOrig: ptr(true), Stage: state.StageProtected},
	)

	err := Janitor(context.Background(), f.d)

	if err == nil || !strings.Contains(err.Error(), "990001") {
		t.Errorf("err = %v, want refusal for 990001", err)
	}
	if f.exists(990000) || !f.destroyed(990000) {
		t.Error("990000 not destroyed")
	}
	if f.protected("pbs:v0") {
		t.Error("pbs:v0 still protected")
	}
	if !f.exists(990001) || f.destroyed(990001) {
		t.Error("untagged 990001 destroyed")
	}
	if !strings.Contains(f.logBuf.String(), "990001") || !strings.Contains(f.logBuf.String(), "level=ERROR") {
		t.Errorf("refusal not logged: %s", f.logBuf)
	}
	if f.protected("pbs:v1") {
		t.Error("pbs:v1 still protected")
	}
	if slices.Contains(f.tgt.calls, 990002) {
		t.Error("Destroy called for a destroyed-stage job")
	}
	if !slices.Contains(f.tgt.calls, 990003) {
		t.Error("protected-stage job's VMID not checked for a guest")
	}
	if f.protected("pbs:v2") {
		t.Error("pbs:v2 still protected")
	}
	if !f.protected("pbs:v3") {
		t.Error("pbs:v3 unprotected although its owner had protected it")
	}
	if f.exists(990005) {
		t.Error("owned leftover 990005 not destroyed")
	}
	if !f.exists(500) || f.destroyed(500) {
		t.Error("guest outside the range destroyed")
	}

	inFlight := f.store.Snapshot().InFlight
	if len(inFlight) != 1 {
		t.Errorf("InFlight = %+v, want only 990001 kept", inFlight)
	}
	if _, ok := inFlight[990001]; !ok {
		t.Error("refused entry 990001 removed")
	}
	for _, c := range f.api.CallLog() {
		if strings.HasPrefix(c, "SetVolumeAttrs") && strings.Contains(c, "v3") {
			t.Errorf("touched pbs:v3: %s", c)
		}
	}
}

func TestJanitorKeepsEntryOnUnprotectFailure(t *testing.T) {
	f := newJanitorFixture(t)
	f.backup("pbs:v0", true)
	f.inFlight(state.Job{TempVMID: 990000, Kind: model.KindQEMU, VolID: "pbs:v0", Storage: "pbs", ProtectedOrig: ptr(false), Stage: state.StageDestroyed})
	f.api.Fail["SetVolumeAttrs"] = errors.New("pbs unreachable")

	if err := Janitor(context.Background(), f.d); err == nil {
		t.Error("want error")
	}
	if _, ok := f.store.Snapshot().InFlight[990000]; !ok {
		t.Error("entry removed although unprotect failed")
	}
}

func TestJanitorEmpty(t *testing.T) {
	f := newJanitorFixture(t)
	f.guest(model.KindQEMU, 101, false)
	if err := Janitor(context.Background(), f.d); err != nil {
		t.Fatal(err)
	}
	if f.logBuf.Len() != 0 {
		t.Errorf("log: %s", f.logBuf)
	}
}

func TestJanitorVanishedBackupCountsAsUnprotected(t *testing.T) {
	for _, msg := range []string{
		"volume 'pbs:v0' does not exist",
		"Backup pbs:v0 Not Found",
		"unable to stat pbs:v0 - No such file or directory",
		"no such volume 'pbs:v0'",
	} {
		t.Run(msg, func(t *testing.T) {
			f := newJanitorFixture(t)
			f.backup("pbs:v0", true)
			f.inFlight(state.Job{TempVMID: 990000, Kind: model.KindQEMU, VolID: "pbs:v0", Storage: "pbs", ProtectedOrig: ptr(false), Stage: state.StageDestroyed})
			f.api.Fail["SetVolumeAttrs"] = errors.New(msg)

			if err := Janitor(context.Background(), f.d); err != nil {
				t.Errorf("err = %v", err)
			}
			if _, ok := f.store.Snapshot().InFlight[990000]; ok {
				t.Error("entry kept although the backup is gone")
			}
			if !strings.Contains(f.logBuf.String(), "pbs:v0") || !strings.Contains(f.logBuf.String(), "no longer exists") {
				t.Errorf("not logged: %s", f.logBuf)
			}
		})
	}
}

func TestJanitorNetworkErrorKeepsEntry(t *testing.T) {
	for _, err := range []error{
		errors.New("dial tcp: lookup pve1: no such host"),
		&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: no such device")},
	} {
		f := newJanitorFixture(t)
		f.backup("pbs:v0", true)
		f.inFlight(state.Job{TempVMID: 990000, Kind: model.KindQEMU, VolID: "pbs:v0", Storage: "pbs", ProtectedOrig: ptr(false), Stage: state.StageDestroyed})
		f.api.Fail["SetVolumeAttrs"] = err
		if Janitor(context.Background(), f.d) == nil {
			t.Errorf("%v: want error", err)
		}
		if _, ok := f.store.Snapshot().InFlight[990000]; !ok {
			t.Errorf("%v: entry removed on a network error", err)
		}
	}
}

func TestJanitorUnrelatedNotFoundKeepsEntry(t *testing.T) {
	for _, msg := range []string{
		"Not Found",
		"404 Not Found",
		"storage 'pbs' does not exist",
		"pbs:v0: storage 'pbs' does not exist",
		"No such file or directory",
	} {
		t.Run(msg, func(t *testing.T) {
			f := newJanitorFixture(t)
			f.backup("pbs:v0", true)
			f.inFlight(state.Job{TempVMID: 990000, Kind: model.KindQEMU, VolID: "pbs:v0", Storage: "pbs", ProtectedOrig: ptr(false), Stage: state.StageDestroyed})
			f.api.Fail["SetVolumeAttrs"] = errors.New(msg)
			if Janitor(context.Background(), f.d) == nil {
				t.Error("want error")
			}
			if _, ok := f.store.Snapshot().InFlight[990000]; !ok {
				t.Error("entry removed although the volume was not said to be gone")
			}
		})
	}
}

func (f *janitorFixture) allow(volid string) {
	if f.allowed == nil {
		f.allowed = map[string]bool{}
	}
	f.allowed[volid] = true
}
