package run

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/discovery"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/report"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sanitize"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/state"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/target"
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
	reasonPrivileged    = "PBP cannot restore privileged containers (see " + docsToken + " for options)"
	reasonNoVMID        = "no free VMID for the test copy (target.vmid_range is full)"
	reasonTemplate      = "template (not bootable)"
	reasonStopping      = "PBP stopping"
)

// Options select what a run does. Trigger is "schedule" or "manual";
// OnlyVMID, when non-zero, limits the run to that guest. RunID, when set,
// names the run instead of the default derived from its start time and
// trigger, so a caller can open the run's log file before the run starts.
// LogDir is the run's log folder, passed on to the summary.
type Options struct {
	Trigger  string
	OnlyVMID int
	RunID    string
	LogDir   string
	// Stop, when it closes, ends admission: guests not yet started are
	// SKIPPED, while proofs in flight go on under the run's context. The
	// daemon closes it on shutdown (code review I3); nil never closes.
	Stop <-chan struct{}
}

// NewRunID names a run started at t: its UTC start time to the millisecond,
// so two runs in the same second differ, and its trigger. The ID is safe in a
// file name.
func NewRunID(t time.Time, trigger string) string {
	return t.UTC().Format("20060102T150405.000Z") + "-" + trigger
}

// Run performs one proofing run (design section 6): preflight, discovery,
// gated proofing with Cfg.Workers workers, then history and notification.
// It never panics and always returns a summary.
func Run(ctx context.Context, d Deps, opt Options) model.RunSummary {
	log := logger(d)
	start := d.Now()
	s := model.RunSummary{
		ID:      opt.RunID,
		Trigger: opt.Trigger,
		Started: start,
		LogDir:  opt.LogDir,
	}
	if s.ID == "" {
		s.ID = NewRunID(start, opt.Trigger)
	}
	log.Info(fmt.Sprintf("run started (%s) · PBP %s · node %s", opt.Trigger, d.Version, d.Cfg.Target.Node))

	var runErrs []string
	var envErrs []string
	// This run holds the lock, so a run still recorded as running was
	// interrupted, e.g. a killed manual run while the daemon kept going.
	for _, r := range markInterrupted(d) {
		msg := "previous run " + r.ID + " was interrupted (PBP stopped or crashed before it finished); the janitor cleans up its guests"
		log.Warn(msg)
		envErrs = append(envErrs, msg)
	}
	rec := state.RunRecord{ID: s.ID, Trigger: s.Trigger, Outcome: state.OutcomeRunning, Started: start}
	if err := d.Store.AppendRun(rec, d.Cfg.Logging.RetainRuns); err != nil {
		log.Error("record run start", "err", err)
		runErrs = append(runErrs, "record run: "+err.Error())
	} else {
		results, errs := guarded(log, func() ([]model.GuestResult, []string) {
			t0 := d.Now()
			if err := Preflight(ctx, d); err != nil {
				log.Error("preflight failed", "err", err)
				return nil, []string{"preflight: " + err.Error()}
			}
			log.Info("preflight ok (" + report.Dur(d.Now().Sub(t0)) + ")")
			envErrs = append(envErrs, strayTaggedErrors(ctx, d, log)...)
			return execute(ctx, d, opt, start, log)
		})
		s.Results = results
		runErrs = append(runErrs, errs...)
	}
	s.EnvironmentErrors = envErrs
	s.RunError = strings.Join(runErrs, "; ")
	s.Ended = d.Now()
	finish(d, log, s, rec)
	return s
}

// strayTaggedErrors reports guests tagged "pbp" outside the VMID range. PBP
// never touches them; they are environment errors in the run summary. A
// failed check is itself reported and the run continues.
func strayTaggedErrors(ctx context.Context, d Deps, log *slog.Logger) []string {
	stray, err := d.Target.StrayTagged(ctx)
	if err != nil {
		log.Warn("could not check for stray pbp-tagged guests", "err", err)
		return []string{"could not check for stray pbp-tagged guests: " + err.Error()}
	}
	lo, hi := d.Cfg.Target.VMIDRange[0], d.Cfg.Target.VMIDRange[1]
	var out []string
	for _, g := range stray {
		kind := "VM"
		if g.Key.Kind == model.KindLXC {
			kind = "CT"
		}
		who := fmt.Sprintf("%s %d", kind, g.Key.VMID)
		if g.Name != "" {
			who += " (" + g.Name + ")"
		}
		msg := fmt.Sprintf("%s is tagged pbp but outside the PBP range %d–%d; not touched", who, lo, hi)
		if g.Key.VMID >= lo && g.Key.VMID <= hi {
			msg = fmt.Sprintf("%s is tagged pbp and in the PBP range but on node %s, not %s; not touched (remove it by hand if it is a leftover)",
				who, g.Node, d.Cfg.Target.Node)
		}
		log.Warn(msg, "guest", g.Key.String(), "node", g.Node)
		out = append(out, msg)
	}
	return out
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

// finish notifies, logs the run's totals and records its outcome. Each guest
// result was logged as it became final.
func finish(d Deps, log *slog.Logger, s model.RunSummary, rec state.RunRecord) {
	sev := s.Severity()
	outcome := state.OutcomeOK
	switch {
	case s.RunError != "":
		outcome = state.OutcomeError
	case sev != "info":
		outcome = state.OutcomeProblems
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
			outcome = state.OutcomeError
		}
	}
	log.Info(fmt.Sprintf("run finished in %s · %s severity %s",
		report.Dur(s.Ended.Sub(s.Started)), report.SeverityMark(sev), sev), "outcome", outcome)
	log.Info(report.CountsLine(s))

	rec.Outcome, rec.Ended, rec.Counts = outcome, s.Ended, s.Counts()
	if err := d.Store.AppendRun(rec, d.Cfg.Logging.RetainRuns); err != nil {
		log.Error("record run outcome", "err", err)
	}
}

// execute discovers the backups to proof and proofs them. It returns the
// results in priority order and any run-level errors.
func execute(ctx context.Context, d Deps, opt Options, start time.Time, log *slog.Logger) ([]model.GuestResult, []string) {
	queue, old, errs := discover(ctx, d, opt, log)
	if queue == nil {
		return nil, errs
	}
	logDiscovery(d, queue, log)
	live, err := liveTags(ctx, d)
	if err != nil {
		log.Error("read guest tags for selection.exclude.tags", "err", err)
		return nil, append(errs, "read guest tags for selection.exclude.tags: "+err.Error())
	}
	p := &pool{d: d, log: log, inUse: map[int]bool{}, liveTags: live, stop: opt.Stop}
	results := p.run(ctx, queue, start)
	p.migrateNotes(ctx, old)
	return results, append(errs, p.errs...)
}

// discover lists every source storage and returns the newest backup per
// guest in proofing order, and every backup whose notes are in the old PBP
// format (migrateNotes). A storage that cannot be listed is reported and
// skipped; if none can be listed the queue is nil.
func discover(ctx context.Context, d Deps, opt Options, log *slog.Logger) ([]model.Backup, []model.Backup, []string) {
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
		return nil, nil, errs
	}
	rng := d.Cfg.Target.VMIDRange
	all, inRange := discovery.OutsideRange(all, rng[0], rng[1])
	if len(inRange) > 0 {
		keys := make([]string, len(inRange))
		for i, k := range inRange {
			keys[i] = k.String()
		}
		log.Warn("skipping backups of guests in the PBP VMID range; exclude the range from your backup jobs",
			"guests", strings.Join(keys, ", "), "vmid_range", fmt.Sprintf("%d-%d", rng[0], rng[1]))
	}
	newest := discovery.NewestPerGuest(all)
	old := oldNotes(all)
	if opt.OnlyVMID != 0 {
		only := func(bs []model.Backup) []model.Backup {
			var out []model.Backup
			for _, b := range bs {
				if b.Key.VMID == opt.OnlyVMID {
					out = append(out, b)
				}
			}
			return out
		}
		newest, old = only(newest), only(old)
	}
	lastProven := map[model.GuestKey]time.Time{}
	snap := d.Store.Snapshot()
	for _, b := range newest {
		if t, ok := snap.LastProven[b.Key.String()]; ok {
			lastProven[b.Key] = t
		}
	}
	return append([]model.Backup{}, discovery.Order(newest, lastProven)...), old, errs
}

// logDiscovery logs how many backups were found, on which storages, and how
// many of them are already proven.
func logDiscovery(d Deps, queue []model.Backup, log *slog.Logger) {
	var stores []string
	proven := 0
	for _, st := range d.Cfg.Sources.Storages {
		if slices.ContainsFunc(queue, func(b model.Backup) bool { return b.Storage == st }) {
			stores = append(stores, st)
		}
	}
	for _, b := range queue {
		if discovery.AlreadyProven(b, d.Cfg.Restore.Reproof) {
			proven++
		}
	}
	on := ""
	if len(stores) > 0 {
		on = " on " + strings.Join(stores, ", ")
	}
	log.Info(fmt.Sprintf("discovery: %d backups%s; %d already proven, %d to proof", len(queue), on, proven, len(queue)-proven))
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
	// liveTags are the live guests' tags, read only when exclusion by
	// tag is configured.
	liveTags map[model.GuestKey][]string
	// stop closes when no further proofs may start (Options.Stop).
	stop <-chan struct{}
}

// stopping reports whether admission has ended (Options.Stop closed).
func (p *pool) stopping() bool {
	select {
	case <-p.stop:
		return true
	default:
		return false
	}
}

// lease is what an admitted job holds until it ends, and the backup's
// original config admit read (passed on so the job does not read it again).
type lease struct {
	vmid       int
	space, ram int64
	orig       map[string]string
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
		glog := guestLogger(d, b.Key)
		if rule := d.Cfg.Selection.Exclude.ExcludedBy(string(b.Key.Kind), b.Key.VMID, p.liveTags[b.Key]); rule != "" {
			results[i] = p.exclude(glog, b, rule)
			continue
		}
		if discovery.AlreadyProven(b, d.Cfg.Restore.Reproof) {
			results[i] = p.skip(glog, b, reasonAlreadyProven)
			results[i].AlreadyProven = true
			p.logResult(results[i])
			continue
		}
		// Wait for a free worker; the gates below then see current figures.
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			results[i] = p.skip(glog, b, reasonCancelled)
			p.logResult(results[i])
			continue
		case <-p.stop:
			results[i] = p.skip(glog, b, reasonStopping)
			p.logResult(results[i])
			continue
		}
		if ctx.Err() != nil {
			<-slots
			results[i] = p.skip(glog, b, reasonCancelled)
			p.logResult(results[i])
			continue
		}
		if p.stopping() {
			<-slots
			results[i] = p.skip(glog, b, reasonStopping)
			p.logResult(results[i])
			continue
		}
		if limit := d.Cfg.Restore.MaxRunDuration; limit > 0 && !d.Now().Before(start.Add(limit)) {
			<-slots
			results[i] = p.skip(glog, b, reasonDeadline)
			p.logResult(results[i])
			continue
		}
		l, res := p.admitGuarded(ctx, glog, b)
		if res != nil {
			<-slots
			results[i] = *res
			p.logResult(results[i])
			continue
		}
		label := b.Key.Label()
		p.log.Log(ctx, report.LevelDetail, label+" started ("+label+".log)")
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			defer p.release(l)
			defer func() {
				if r := recover(); r != nil {
					glog.Error("panic in job", "panic", r, "stack", string(debug.Stack()))
					results[i] = model.GuestResult{Backup: b, TempVMID: l.vmid, Verdict: model.Error,
						Reason: oneLine(fmt.Sprintf("internal error: %v", r))}
					p.addErr(fmt.Sprintf("internal error: %v", r))
				}
				p.logResult(results[i])
			}()
			results[i] = proofOne(ctx, d, b, l.vmid, l.orig, glog)
		}()
	}
	wg.Wait()
	return results
}

// skip logs reason to the guest's log and returns b's SKIPPED result.
func (p *pool) skip(glog *slog.Logger, b model.Backup, reason string) model.GuestResult {
	glog.Info("skipped: " + reason)
	return model.GuestResult{Backup: b, Verdict: model.Skipped, Reason: reason}
}

// logResult writes a guest's final result to the run log.
func (p *pool) logResult(r model.GuestResult) {
	p.log.Info(r.Backup.Key.Label() + " " + string(r.Verdict) + " · " + r.Reason)
}

// exclude is the result for a guest excluded by config (issue #32): a skip
// that nothing in the guest's backup records and that does not raise the
// run's severity.
func (p *pool) exclude(glog *slog.Logger, b model.Backup, rule string) model.GuestResult {
	r := p.skip(glog, b, "excluded by config ("+rule+")")
	r.Excluded = true
	p.logResult(r)
	return r
}

// liveTags reads the tags of every guest when selection.exclude.tags is
// set, so a tag added in the Proxmox UI excludes the guest at the next
// run; otherwise it reads nothing.
func liveTags(ctx context.Context, d Deps) (map[model.GuestKey][]string, error) {
	if len(d.Cfg.Selection.Exclude.Tags) == 0 {
		return nil, nil
	}
	guests, err := d.API.Guests(ctx)
	if err != nil {
		return nil, err
	}
	tags := make(map[model.GuestKey][]string, len(guests))
	for _, g := range guests {
		tags[g.Key] = g.Tags
	}
	return tags, nil
}

func (p *pool) addErr(msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.errs = append(p.errs, msg)
}

// admitGuarded is admit with a panic turned into an ERROR result and a run
// error.
func (p *pool) admitGuarded(ctx context.Context, glog *slog.Logger, b model.Backup) (l lease, res *model.GuestResult) {
	defer func() {
		if r := recover(); r != nil {
			glog.Error("panic while admitting job", "panic", r, "stack", string(debug.Stack()))
			msg := fmt.Sprintf("internal error: %v", r)
			p.addErr(msg)
			res = &model.GuestResult{Backup: b, Verdict: model.Error, Reason: oneLine(msg)}
		}
	}()
	return p.admit(ctx, glog, b)
}

// admit runs the capacity gates for b (design section 6 step 4) and, if it
// passes, reserves its space, memory and a VMID. Otherwise it returns the
// SKIPPED or ERROR result for b. glog is b's guest log.
func (p *pool) admit(ctx context.Context, glog *slog.Logger, b model.Backup) (lease, *model.GuestResult) {
	d := p.d
	node, store := d.Cfg.Target.Node, d.Cfg.Restore.Storage
	fail := func(v model.Verdict, reason string) (lease, *model.GuestResult) {
		reason = oneLine(reason)
		if v == model.Error && ctx.Err() != nil {
			// The failure is most likely the cancellation itself.
			v, reason = model.Skipped, reasonCancelled
		}
		if v == model.Skipped {
			r := p.skip(glog, b, reason)
			return lease{}, &r
		}
		// Info, so it is not forwarded: the result line reports it.
		glog.Info("guest not proofed: " + reason)
		return lease{}, &model.GuestResult{Backup: b, Verdict: v, Reason: reason}
	}

	at := d.Now()
	glog.Info(fmt.Sprintf("backup %s (taken %s, %s old, %s)", b.VolID,
		b.Time.In(at.Location()).Format("2006-01-02 15:04"), report.Age(at.Sub(b.Time)), report.Bytes(b.Size)))
	raw, err := d.API.OriginalConfig(ctx, node, b.VolID)
	if err != nil {
		glog.Info("read the backup's config: " + oneLine(err.Error()))
		l, res := fail(model.Error, plainErr("cannot read the backup's config from PBS", err))
		// Unlike the node-wide gates below, this failure is the backup's
		// own, so its notes record it (annotate skips a SKIPPED result).
		// p.mu is not held yet. But when tags exclude guests and the guest
		// is gone, the unreadable config's own tags might have excluded
		// it, and an excluded guest's notes are left alone (code review
		// 1.8): report the ERROR without writing them.
		if _, live := p.liveTags[b.Key]; len(d.Cfg.Selection.Exclude.Tags) > 0 && !live {
			glog.Info("notes left alone: the guest is gone and its unreadable backup config might exclude it by tag")
			return l, res
		}
		annotate(d, glog, b, *res, b.Key.Label())
		return l, res
	}
	orig := sanitize.ParseConfigText(raw)
	// Exclusion by a tag in the backed-up config: the guest may be gone or
	// retagged since (the live tags were checked before admission).
	if rule := d.Cfg.Selection.Exclude.ExcludedBy(string(b.Key.Kind), b.Key.VMID, sanitize.SplitList(orig["tags"])); rule != "" {
		l, res := fail(model.Skipped, "excluded by config ("+rule+")")
		res.Excluded = true
		return l, res
	}
	// A template cannot be started, so it cannot be proven. The backed-up
	// config decides, not the live guest, which may be gone or converted.
	if orig["template"] == "1" {
		l, res := fail(model.Skipped, reasonTemplate)
		res.Template = true
		return l, res
	}
	// PVE drops raw lxc.* lines unless root@pam restores, so the copy would
	// not be the guest it stands for. Like an unreadable config, this is the
	// backup's own condition: its notes record it.
	if keys := sanitize.RawLXCKeys(orig); b.Key.Kind == model.KindLXC && len(keys) > 0 {
		l, res := fail(model.Error, "PBP cannot restore containers with raw lxc settings ("+strings.Join(keys, ", ")+"): only root@pam can ("+docsToken+")")
		annotate(d, glog, b, *res, noteName(b.Key, orig))
		return l, res
	}
	// PVE restores a privileged container only for a caller with Sys.Modify
	// on /, which PBP's role does not grant (issue #73). Say so plainly,
	// before anything is reserved or restored, rather than leave PVE's
	// refusal of the restore to explain it.
	if b.Key.Kind == model.KindLXC && orig["unprivileged"] != "1" {
		privs, err := d.API.Privileges(ctx, "/")
		switch {
		case err != nil:
			// The restore itself still reports a refusal.
			glog.Info("cannot read PBP's privileges on /, restoring the privileged container anyway: " + oneLine(err.Error()))
		case !privs["Sys.Modify"]:
			l, res := fail(model.Error, reasonPrivileged)
			annotate(d, glog, b, *res, noteName(b.Key, orig))
			return l, res
		}
	}
	needRAM := guestMemoryMiB(b.Key.Kind, orig) * (1 << 20) * 110 / 100

	p.mu.Lock()
	defer p.mu.Unlock()

	st, err := d.API.StorageStatus(ctx, node, store)
	if err != nil {
		glog.Info("read storage " + store + ": " + oneLine(err.Error()))
		return fail(model.Error, plainErr("cannot read storage "+store, err))
	}
	margin := st.Total * int64(d.Cfg.Restore.SpaceMarginPercent) / 100
	if freeSpace := st.Avail - p.spaceRes - margin; b.Size > freeSpace {
		glog.Info(fmt.Sprintf("capacity: need %s, %s free on %s (after %s reserved, %s margin)",
			report.Bytes(b.Size), report.Bytes(max(freeSpace, 0)), store, report.Bytes(p.spaceRes), report.Bytes(margin)))
		return fail(model.Skipped, "insufficient space on "+store)
	}

	mem, err := d.API.NodeMemory(ctx, node)
	if err != nil {
		glog.Info("read node memory: " + oneLine(err.Error()))
		return fail(model.Error, plainErr("cannot read the node's memory", err))
	}
	freeRAM := mem.Available - p.ramRes
	ramFigures := fmt.Sprintf("need %s RAM, %s free (after %s reserved)",
		report.Bytes(needRAM), report.Bytes(max(freeRAM, 0)), report.Bytes(p.ramRes))
	if needRAM > freeRAM {
		glog.Info("capacity: " + ramFigures)
		return fail(model.Skipped, reasonNoMemory)
	}
	glog.Info(fmt.Sprintf("capacity ok: need %s, %s free on %s (after %s reserved, %s margin); %s",
		report.Bytes(b.Size), report.Bytes(max(st.Avail-p.spaceRes-margin, 0)), store,
		report.Bytes(p.spaceRes), report.Bytes(margin), ramFigures))

	vmid, err := d.Target.AllocateVMID(ctx, p.inUse)
	if errors.Is(err, target.ErrNoFreeVMID) {
		return fail(model.Skipped, reasonNoVMID)
	}
	if err != nil {
		glog.Info("allocate a VMID: " + oneLine(err.Error()))
		return fail(model.Error, plainErr("cannot allocate a VMID for the test copy", err))
	}
	p.inUse[vmid] = true
	p.spaceRes += b.Size
	p.ramRes += needRAM
	return lease{vmid: vmid, space: b.Size, ram: needRAM, orig: orig}, nil
}

// release returns a finished job's space and RAM. Its VMID is freed only
// if the job left no InFlight record: one whose teardown failed (say, the
// unprotect) is the janitor's, and reusing the VMID in this run would
// overwrite that record (code review I2).
func (p *pool) release(l lease) {
	_, pending := p.d.Store.Snapshot().InFlight[l.vmid]
	p.mu.Lock()
	defer p.mu.Unlock()
	if !pending {
		delete(p.inUse, l.vmid)
	}
	p.spaceRes -= l.space
	p.ramRes -= l.ram
}

// guestMemoryMiB reads the "memory" key (MiB) from a parsed backed-up config,
// accepting PVE's "current=<n>[,...]" form, and falls back to the PVE default
// for the guest kind.
func guestMemoryMiB(kind model.GuestKind, orig map[string]string) int64 {
	v := orig["memory"]
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

// guestLogger is the log for one guest's steps: Deps.GuestLog, or the run log
// with the guest's key when there is none.
func guestLogger(d Deps, key model.GuestKey) *slog.Logger {
	if d.GuestLog != nil {
		return d.GuestLog(key)
	}
	return logger(d).With("guest", key.String())
}

func logger(d Deps) *slog.Logger {
	if d.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return d.Log
}
