package run

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.breadthanalytics.com/tdiana/pbv/internal/discovery"
	"git.breadthanalytics.com/tdiana/pbv/internal/model"
	"git.breadthanalytics.com/tdiana/pbv/internal/sanitize"
	"git.breadthanalytics.com/tdiana/pbv/internal/state"
	"git.breadthanalytics.com/tdiana/pbv/internal/target"
)

// notifyTimeout bounds sending the run summary. It runs on a fresh context so
// a shutdown still reports the run.
const notifyTimeout = 2 * time.Minute

// Memory assumed for a guest whose backed-up config has no usable "memory"
// key, in MiB (the PVE defaults).
const (
	defaultMemLXC  = 512
	defaultMemQEMU = 2048
)

// SKIPPED reasons.
const (
	reasonAlreadyProven = "already proven"
	reasonDeadline      = "run deadline"
	reasonCancelled     = "run cancelled"
	reasonNoMemory      = "insufficient node memory"
	reasonNoVMID        = "no free VMID"
)

// Options select what a run does. Trigger is "schedule" or "manual";
// OnlyVMID, when non-zero, limits the run to that guest. RunID, when set,
// names the run instead of the default derived from its start time and
// trigger, so a caller can open the run's log file before the run starts.
type Options struct {
	Trigger  string
	OnlyVMID int
	RunID    string
}

// Run performs one proofing run (design section 6): preflight, discovery,
// gated proofing with Cfg.Workers workers, then history and notification.
// It never panics and always returns a summary.
func Run(ctx context.Context, d Deps, opt Options) model.RunSummary {
	log := logger(d).With("trigger", opt.Trigger)
	start := d.Now()
	s := model.RunSummary{
		ID:      opt.RunID,
		Trigger: opt.Trigger,
		Started: start,
	}
	if s.ID == "" {
		s.ID = start.UTC().Format("20060102T150405Z") + "-" + opt.Trigger
	}
	log = log.With("run", s.ID)
	log.Info("run started")

	var runErrs []string
	rec := state.RunRecord{ID: s.ID, Trigger: s.Trigger, Outcome: "running", Started: start}
	if err := d.Store.AppendRun(rec, d.Cfg.Logging.RetainRuns); err != nil {
		log.Error("record run start", "err", err)
		runErrs = append(runErrs, "record run: "+err.Error())
	} else {
		results, errs := guarded(log, func() ([]model.GuestResult, []string) {
			if err := Preflight(ctx, d); err != nil {
				log.Error("preflight failed", "err", err)
				return nil, []string{"preflight: " + err.Error()}
			}
			return execute(ctx, d, opt, start, log)
		})
		s.Results = results
		runErrs = append(runErrs, errs...)
	}
	s.RunError = strings.Join(runErrs, "; ")
	s.Ended = d.Now()
	finish(d, log, s, rec)
	return s
}

// guarded runs fn, turning a panic into a run error.
func guarded(log *slog.Logger, fn func() ([]model.GuestResult, []string)) (res []model.GuestResult, errs []string) {
	defer func() {
		if p := recover(); p != nil {
			log.Error("panic during run", "panic", p, "stack", string(debug.Stack()))
			errs = append(errs, fmt.Sprintf("internal error: %v", p))
		}
	}()
	return fn()
}

// finish logs the summary, notifies and records the run's outcome.
func finish(d Deps, log *slog.Logger, s model.RunSummary, rec state.RunRecord) {
	for _, r := range s.Results {
		log.Info("guest result", "guest", r.Backup.Key.String(), "volid", r.Backup.VolID,
			"verdict", r.Verdict, "reason", r.Reason, "temp_vmid", r.TempVMID,
			"restore", r.RestoreDuration, "boot", r.BootDuration,
			"failed_units", r.FailedUnits, "ignored_units", r.IgnoredUnits)
	}
	sev := s.Severity()
	outcome := "ok"
	switch {
	case s.RunError != "":
		outcome = "error"
	case sev != "info":
		outcome = "problems"
	}
	if d.Cfg.Notify.When == "always" || sev != "info" {
		err := func() (err error) {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("panic: %v", p)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
			defer cancel()
			return d.Notifier.Send(ctx, s)
		}()
		if err != nil {
			log.Error("send notification failed", "err", err)
			outcome = "error"
		}
	}
	log.Info("run finished", "outcome", outcome, "severity", sev, "run_error", s.RunError)

	rec.Outcome, rec.Ended, rec.Counts = outcome, s.Ended, s.Counts()
	if err := d.Store.AppendRun(rec, d.Cfg.Logging.RetainRuns); err != nil {
		log.Error("record run outcome", "err", err)
	}
}

// execute discovers the backups to proof and proofs them. It returns the
// results in priority order and any run-level errors.
func execute(ctx context.Context, d Deps, opt Options, start time.Time, log *slog.Logger) ([]model.GuestResult, []string) {
	queue, errs := discover(ctx, d, opt, log)
	if queue == nil {
		return nil, errs
	}
	p := &pool{d: d, log: log, inUse: map[int]bool{}}
	results := p.run(ctx, queue, start)
	return results, append(errs, p.errs...)
}

// discover lists every source storage and returns the newest backup per
// guest in proofing order. A storage that cannot be listed is reported and
// skipped; if none can be listed the queue is nil.
func discover(ctx context.Context, d Deps, opt Options, log *slog.Logger) ([]model.Backup, []string) {
	var all []model.Backup
	var errs []string
	listed := 0
	for _, st := range d.Cfg.Sources.Storages {
		bs, err := d.API.ListBackups(ctx, d.Cfg.Target.Node, st)
		if err != nil {
			log.Error("list backups failed", "storage", st, "err", err)
			errs = append(errs, fmt.Sprintf("storage %s: %v", st, err))
			continue
		}
		listed++
		all = append(all, bs...)
	}
	if listed == 0 {
		return nil, errs
	}
	newest := discovery.NewestPerGuest(all)
	if opt.OnlyVMID != 0 {
		var only []model.Backup
		for _, b := range newest {
			if b.Key.VMID == opt.OnlyVMID {
				only = append(only, b)
			}
		}
		newest = only
	}
	lastProven := map[model.GuestKey]time.Time{}
	snap := d.Store.Snapshot()
	for _, b := range newest {
		if t, ok := snap.LastProven[b.Key.String()]; ok {
			lastProven[b.Key] = t
		}
	}
	return append([]model.Backup{}, discovery.Order(newest, lastProven)...), errs
}

// pool admits jobs one at a time in priority order and proofs them on up to
// Cfg.Workers goroutines. mu guards the VMIDs and the space and memory
// reserved by admitted jobs that have not finished.
type pool struct {
	d   Deps
	log *slog.Logger

	mu       sync.Mutex
	inUse    map[int]bool
	spaceRes int64 // bytes on the restore storage
	ramRes   int64 // bytes of node memory
	errs     []string
}

// lease is what an admitted job holds until it ends.
type lease struct {
	vmid       int
	space, ram int64
}

func (p *pool) run(ctx context.Context, queue []model.Backup, start time.Time) []model.GuestResult {
	d := p.d
	results := make([]model.GuestResult, len(queue))
	workers := max(d.Cfg.Workers, 1)
	slots := make(chan struct{}, workers)
	var wg sync.WaitGroup
	// Even if the dispatcher panics, never return while jobs still run.
	defer wg.Wait()

	for i, b := range queue {
		if discovery.AlreadyProven(b, d.Cfg.Restore.Reproof) {
			results[i] = p.skip(b, reasonAlreadyProven)
			continue
		}
		// Wait for a free worker; the gates below then see current figures.
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			results[i] = p.skip(b, reasonCancelled)
			continue
		}
		if ctx.Err() != nil {
			<-slots
			results[i] = p.skip(b, reasonCancelled)
			continue
		}
		if limit := d.Cfg.Restore.MaxRunDuration; limit > 0 && !d.Now().Before(start.Add(limit)) {
			<-slots
			results[i] = p.skip(b, reasonDeadline)
			continue
		}
		l, res := p.admitGuarded(ctx, b)
		if res != nil {
			<-slots
			results[i] = *res
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			defer p.release(l)
			defer func() {
				if r := recover(); r != nil {
					p.log.Error("panic in job", "guest", b.Key.String(), "panic", r, "stack", string(debug.Stack()))
					results[i] = model.GuestResult{Backup: b, TempVMID: l.vmid, Verdict: model.Error,
						Reason: fmt.Sprintf("internal error: %v", r)}
					p.addErr(fmt.Sprintf("internal error: %v", r))
				}
			}()
			results[i] = proofOne(ctx, d, b, l.vmid)
		}()
	}
	wg.Wait()
	return results
}

func (p *pool) skip(b model.Backup, reason string) model.GuestResult {
	p.log.Info("guest skipped", "guest", b.Key.String(), "volid", b.VolID, "reason", reason)
	return model.GuestResult{Backup: b, Verdict: model.Skipped, Reason: reason}
}

func (p *pool) addErr(msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.errs = append(p.errs, msg)
}

// admitGuarded is admit with a panic turned into an ERROR result and a run
// error.
func (p *pool) admitGuarded(ctx context.Context, b model.Backup) (l lease, res *model.GuestResult) {
	defer func() {
		if r := recover(); r != nil {
			p.log.Error("panic while admitting job", "guest", b.Key.String(), "panic", r, "stack", string(debug.Stack()))
			msg := fmt.Sprintf("internal error: %v", r)
			p.addErr(msg)
			res = &model.GuestResult{Backup: b, Verdict: model.Error, Reason: msg}
		}
	}()
	return p.admit(ctx, b)
}

// admit runs the capacity gates for b (design section 6 step 4) and, if it
// passes, reserves its space, memory and a VMID. Otherwise it returns the
// SKIPPED or ERROR result for b.
func (p *pool) admit(ctx context.Context, b model.Backup) (lease, *model.GuestResult) {
	d := p.d
	node, store := d.Cfg.Target.Node, d.Cfg.Restore.Storage
	fail := func(v model.Verdict, reason string) (lease, *model.GuestResult) {
		if v == model.Error && ctx.Err() != nil {
			// The failure is most likely the cancellation itself.
			v, reason = model.Skipped, reasonCancelled
		}
		if v == model.Skipped {
			r := p.skip(b, reason)
			return lease{}, &r
		}
		p.log.Error("guest not proofed", "guest", b.Key.String(), "volid", b.VolID, "reason", reason)
		return lease{}, &model.GuestResult{Backup: b, Verdict: v, Reason: reason}
	}

	raw, err := d.API.OriginalConfig(ctx, node, b.VolID)
	if err != nil {
		return fail(model.Error, "cannot read backup config: "+err.Error())
	}
	needRAM := guestMemoryMiB(b.Key.Kind, raw) * (1 << 20) * 110 / 100

	p.mu.Lock()
	defer p.mu.Unlock()

	st, err := d.API.StorageStatus(ctx, node, store)
	if err != nil {
		return fail(model.Error, fmt.Sprintf("cannot read storage %s: %v", store, err))
	}
	margin := st.Total * int64(d.Cfg.Restore.SpaceMarginPercent) / 100
	if b.Size > st.Avail-p.spaceRes-margin {
		return fail(model.Skipped, "insufficient space on "+store)
	}

	mem, err := d.API.NodeMemory(ctx, node)
	if err != nil {
		return fail(model.Error, "cannot read node memory: "+err.Error())
	}
	if needRAM > mem.Available-p.ramRes {
		return fail(model.Skipped, reasonNoMemory)
	}

	vmid, err := d.Target.AllocateVMID(ctx, p.inUse)
	if errors.Is(err, target.ErrNoFreeVMID) {
		return fail(model.Skipped, reasonNoVMID)
	}
	if err != nil {
		return fail(model.Error, "allocate VMID: "+err.Error())
	}
	p.inUse[vmid] = true
	p.spaceRes += b.Size
	p.ramRes += needRAM
	return lease{vmid: vmid, space: b.Size, ram: needRAM}, nil
}

func (p *pool) release(l lease) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.inUse, l.vmid)
	p.spaceRes -= l.space
	p.ramRes -= l.ram
}

// guestMemoryMiB reads the "memory" key (MiB) from a backed-up guest config,
// accepting PVE's "current=<n>[,...]" form, and falls back to the PVE default
// for the guest kind.
func guestMemoryMiB(kind model.GuestKind, raw string) int64 {
	v := sanitize.ParseConfigText(raw)["memory"]
	v, _, _ = strings.Cut(v, ",")
	v = strings.TrimPrefix(strings.TrimSpace(v), "current=")
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
		return n
	}
	if kind == model.KindLXC {
		return defaultMemLXC
	}
	return defaultMemQEMU
}

func logger(d Deps) *slog.Logger {
	if d.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return d.Log
}
