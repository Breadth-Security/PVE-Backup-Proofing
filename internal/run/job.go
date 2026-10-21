// Package run drives proofing runs: it takes selected backups through the
// protect, restore, sanitize, boot, probe and teardown lifecycle (design
// section 6) and records every step so a crash can be cleaned up.
package run

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/notes"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/probe"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/pve"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/report"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sanitize"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/state"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/target"
)

// reasonPBSRefused explains a protect that PBS refused: the PVE storage
// connects to PBS as a user that may not change the backup.
const reasonPBSRefused = "PBS refused: the storage's PBS user needs Datastore.Backup as the backup's owner, or Datastore.Modify (docs/dedicated-node.md)"

// Cleanup timeouts. Each cleanup step runs on its own fresh context so that a
// shutdown (cancelled run context) still destroys the guest, restores the
// backup's protected flag and writes the verdict, and so one slow step cannot
// starve the next.
const (
	destroyTimeout   = 5 * time.Minute
	unprotectTimeout = 2 * time.Minute
	notesTimeout     = 2 * time.Minute
)

// Deps is what a run needs. Now supplies every timestamp. GuestLog returns
// the log for one guest's steps; nil means Log with the guest's key. Version
// is PBP's version, named in the run log.
type Deps struct {
	Cfg      config.Config
	API      pve.API
	Target   target.Target
	Store    *state.Store
	Log      *slog.Logger
	GuestLog func(model.GuestKey) *slog.Logger
	Version  string
	Now      func() time.Time
	Notifier report.Notifier
}

// job tracks what proofOne has done so teardown undoes exactly that.
type job struct {
	d    Deps
	b    model.Backup
	vmid int
	// orig is the backup's original config, read once at admission.
	orig map[string]string
	log  *slog.Logger
	// protectSet: PBP asked to set protected (the original was false).
	protectSet bool
	// restoreTried: a restore was started, so a guest PBP owns (by the
	// state record, tagged or not) may exist at vmid.
	restoreTried bool
}

// proofOne proofs backup b in the temporary guest vmid and always tears it
// down again, then writes the verdict to the backup's notes. orig is b's
// original config; each step is logged to log, the guest's log. A panic while
// proofing becomes an ERROR verdict; teardown still runs.
func proofOne(ctx context.Context, d Deps, b model.Backup, vmid int, orig map[string]string, log *slog.Logger) (res model.GuestResult) {
	j := &job{d: d, b: b, vmid: vmid, orig: orig, log: log}
	res = model.GuestResult{Backup: b, TempVMID: vmid}

	defer func() {
		if p := recover(); p != nil {
			j.log.Error("panic while proofing", "panic", p, "stack", string(debug.Stack()))
			res.Verdict, res.Reason = model.Error, oneLine(fmt.Sprintf("internal error: %v", p))
		} else if ctx.Err() != nil {
			res.Verdict, res.Reason = model.Error, "cancelled"
		}
		// One line before it is logged, so that "verdict now" below means
		// teardown or the notes changed something (issue #42).
		res.Reason = oneLine(res.Reason)
		j.log.Info(fmt.Sprintf("verdict %s · %s", res.Verdict, res.Reason))
		logged := string(res.Verdict) + " · " + res.Reason
		j.teardown(&res)
		res.Reason = oneLine(res.Reason)
		if err := j.safely("notes", func() error { annotate(j.d, j.log, j.b, res); return nil }); err != nil {
			res.Verdict, res.Reason = model.Error, oneLine("internal error: "+err.Error())
		}
		if now := string(res.Verdict) + " · " + res.Reason; now != logged {
			j.log.Info(fmt.Sprintf("verdict now %s", now))
		}
	}()
	j.prove(ctx, &res)
	return res
}

// prove runs §6 steps 5–8 and sets the verdict and reason on res.
func (j *job) prove(ctx context.Context, res *model.GuestResult) {
	d, b, kind := j.d, j.b, j.b.Key.Kind
	fail := func(v model.Verdict, format string, args ...any) {
		res.Verdict, res.Reason = v, oneLine(fmt.Sprintf(format, args...))
	}

	// Read the flag now, not from the run's listing, which may be hours
	// old: undoing protection an admin added since would expose the backup
	// to pruning (code review I1).
	orig, err := d.API.VolumeProtected(ctx, d.Cfg.Target.Node, b.Storage, b.VolID)
	if err != nil {
		fail(model.Error, "read backup protected flag: %v", err)
		return
	}
	err = d.Store.Update(func(st *state.State) error {
		st.InFlight[j.vmid] = state.Job{
			TempVMID: j.vmid, Kind: kind, VolID: b.VolID, Storage: b.Storage,
			ProtectedOrig: &orig, Stage: state.StageProtected,
		}
		return nil
	})
	if err != nil {
		fail(model.Error, "record job: %v", err)
		return
	}
	if !orig {
		j.protectSet = true
		if err := j.setProtected(ctx, true); err != nil {
			if pve.PermissionDenied(err) {
				// PBS refused: nothing changed, so nothing to undo and
				// nothing for the janitor (issue #30). Say so in the
				// record at once: were PBP to stop before teardown
				// drops it, the janitor would try to unprotect, PBS
				// would refuse again, and every later run would stop
				// at preflight (code review 1.6a).
				j.protectSet = false
				if uerr := d.Store.Update(func(st *state.State) error {
					if job, ok := st.InFlight[j.vmid]; ok {
						job.ProtectedOrig = nil
						st.InFlight[j.vmid] = job
					}
					return nil
				}); uerr != nil {
					j.log.Warn("record that protect was refused: " + oneLine(uerr.Error()))
				}
				j.log.Info("PBS refused to protect the backup: " + oneLine(err.Error()))
				fail(model.Error, "protect backup: %s", reasonPBSRefused)
				return
			}
			fail(model.Error, "protect backup: %v", err)
			return
		}
		j.log.Info("backup marked protected (was not)")
	} else {
		j.log.Info("backup already protected")
	}

	if err := j.setStage(state.StageRestored); err != nil {
		fail(model.Error, "record job: %v", err)
		return
	}
	j.restoreTried = true
	j.log.Info(fmt.Sprintf("restore to %d …", j.vmid))
	t0 := d.Now()
	err = d.Target.Restore(ctx, b, j.vmid, j.orig["tags"])
	res.RestoreDuration = d.Now().Sub(t0)
	if err != nil {
		detail := j.stepFailed("restore", res.RestoreDuration, err)
		if errors.Is(err, target.ErrRestoreNotStarted) {
			// No guest of PBP's exists. Should this write fail, the stale
			// "restored" record still fails safe: Destroy's identity check
			// (§11.2) refuses a guest someone else made.
			j.restoreTried = false
			if serr := j.setStage(state.StageProtected); serr != nil {
				j.log.Error("record restore not started", "err", serr)
			}
		}
		// Otherwise a guest may exist, untagged; the InFlight entry
		// (stage restored) makes it PBP's, and teardown removes it.
		fail(target.ClassifyRestoreError(err), "restore failed: %s", detail)
		return
	}
	j.log.Info("restore done in " + report.Dur(res.RestoreDuration))

	sanitized, err := d.Target.Sanitize(ctx, kind, j.vmid)
	removed := sanitized.Removed
	if errors.Is(err, target.ErrUnsafe) {
		fail(model.Error, "unsafe config: %s", strings.TrimPrefix(err.Error(), target.ErrUnsafe.Error()+": "))
		return
	}
	if err != nil {
		fail(model.Error, "sanitize failed: %v", err)
		return
	}
	j.logSanitized(sanitized)

	missing := j.missingPieces(removed)
	guest := j.originalIdentity()
	res.Name = guest.Name

	if err := j.setStage(state.StageStarted); err != nil {
		fail(model.Error, "record job: %v", err)
		return
	}
	t0 = d.Now()
	if err := d.Target.Start(ctx, kind, j.vmid); err != nil {
		fail(model.Error, "start failed: %s", j.stepFailed("start", d.Now().Sub(t0), err))
		return
	}
	j.log.Info("started; waiting for the guest (up to " + report.Dur(d.Cfg.Restore.BootTimeout) + ")")
	t0 = d.Now()
	boot, err := d.Target.WaitBooted(ctx, kind, j.vmid, d.Cfg.Restore.BootTimeout)
	res.BootDuration = boot.Duration
	switch {
	case errors.Is(err, target.ErrBootFailed):
		fail(model.Failed, "did not boot: %v", err)
		j.log.Info(res.Reason)
		return
	case err != nil:
		// A node, API or SSH fault while waiting: not the backup's fault.
		fail(model.Error, "boot check error: %v", err)
		j.log.Info(res.Reason)
		return
	}
	running := "running after " + report.Dur(d.Now().Sub(t0))
	if boot.OSName != "" {
		running += " (" + boot.OSName + ")"
	}
	j.log.Info(running)
	switch {
	case boot.AgentMissing:
		res.AgentMissing = true
		fail(model.Booted, "guest agent missing")
		return
	case boot.ExecBlocked:
		fail(model.Booted, "agent cannot run commands")
		return
	}

	// restore.probe_timeout bounds everything run inside the guest; a probe
	// cut short by it is ERROR, never FAILED.
	pctx := ctx
	if t := d.Cfg.Restore.ProbeTimeout; t > 0 {
		var cancel context.CancelFunc
		pctx, cancel = context.WithTimeout(ctx, t)
		defer cancel()
	}
	out := probe.Run(pctx, d.Target.Executor(kind, j.vmid), probe.Input{
		OS: boot.OS, OSType: boot.OSType, Checks: d.Cfg.Checks, Guest: guest, Missing: missing,
		NoNesting: kind == model.KindLXC && !sanitize.Nesting(j.orig),
	})
	res.Verdict, res.Reason = out.Verdict, out.Reason
	res.FailedUnits, res.IgnoredUnits = out.FailedUnits, out.IgnoredUnits
	res.NotCovered, res.Checks, res.BootLog = out.NotCovered, out.Checks, out.BootLog
	j.logProbe(*res, boot.OS)
}

// stepFailed logs that step failed after took and, for a failed PVE task,
// the tail of its task log. It returns what the reason names: the task's
// exit status, or else err, on one line.
func (j *job) stepFailed(step string, took time.Duration, err error) string {
	exit, tail, ok := pve.TaskFailure(err)
	detail := oneLine(err.Error())
	if ok && strings.TrimSpace(exit) != "" {
		detail = strings.TrimSpace(exit)
	}
	j.log.Info(fmt.Sprintf("%s failed after %s: %s", step, report.Dur(took), detail))
	if ok && len(tail) > 0 {
		j.log.Info(fmt.Sprintf("PVE task log (last %d lines):\n%s", len(tail), strings.Join(tail, "\n")))
	}
	return detail
}

// logSanitized logs the config edits that made the guest safe and what the
// proof can no longer cover.
func (j *job) logSanitized(s target.Sanitized) {
	if len(s.Changes) == 0 {
		j.log.Info("made safe: nothing to change")
	} else {
		j.log.Info("made safe: " + strings.Join(s.Changes, "; "))
	}
	if len(s.Removed) == 0 {
		return
	}
	pieces := make([]string, len(s.Removed))
	for i, p := range s.Removed {
		pieces[i] = cmp.Or(p.Key, p.Path) + " (" + strings.TrimSpace(p.Kind+" "+p.Path) + ")"
	}
	j.log.Info("not covered by the proof: " + strings.Join(pieces, ", "))
}

// logProbe logs what the probe found inside the guest. Its extra log is
// the boot journal for Linux, and why services stopped for Windows.
func (j *job) logProbe(r model.GuestResult, os probe.OSFamily) {
	if len(r.FailedUnits) > 0 {
		j.log.Info("failed units: " + strings.Join(r.FailedUnits, ", "))
	}
	if len(r.IgnoredUnits) > 0 {
		j.log.Info("ignored units: " + strings.Join(r.IgnoredUnits, ", "))
	}
	for _, c := range r.Checks {
		if c.OK {
			j.log.Info("check " + c.Name + " ok")
		} else {
			j.log.Info("check " + c.Name + " failed (" + c.Detail + ")")
		}
	}
	for _, n := range r.NotCovered {
		j.log.Info(n.LogLine())
	}
	if r.BootLog != "" {
		heading := "boot log:"
		if os == probe.Windows {
			heading = "service details:"
		}
		j.log.Info(heading + "\n" + r.BootLog)
	}
}

// missingPieces is what the proof cannot cover: the pieces Sanitize removed,
// then those the backup excludes, judged from its original config.
func (j *job) missingPieces(removed []model.MissingPiece) []model.MissingPiece {
	return append(slices.Clone(removed), sanitize.MissingFromOriginal(j.b.Key.Kind, j.orig)...)
}

// originalIdentity is the guest's name and tags from the config stored in
// the backup: the restored guest is renamed and re-tagged "pbp".
func (j *job) originalIdentity() probe.GuestInfo {
	return probe.GuestInfo{
		VMID: j.b.Key.VMID,
		Name: sanitize.GuestName(j.b.Key.Kind, j.orig),
		Tags: sanitize.SplitList(j.orig["tags"]),
	}
}

// teardown destroys the guest and restores the backup's protected flag. The
// guest is PBP's by the InFlight record (tagged or not, e.g. after a failed
// restore), so Target.Destroy gets that record. The
// InFlight entry is removed only when both succeed; otherwise the janitor
// finishes the job. A failed destroy turns PROVEN into ERROR (so the guest
// is proofed again next run) and otherwise only notes it in the reason, as
// does a failed unprotect.
func (j *job) teardown(res *model.GuestResult) {
	clean := true
	t0 := j.d.Now()
	var done []string
	defer func() {
		if len(done) > 0 {
			j.log.Info(fmt.Sprintf("teardown done in %s: %s", report.Dur(j.d.Now().Sub(t0)), strings.Join(done, ", ")))
		}
	}()
	if j.restoreTried {
		err := j.safely("destroy", func() error {
			ctx, cancel := context.WithTimeout(context.Background(), destroyTimeout)
			defer cancel()
			kind := j.b.Key.Kind
			rec := target.Recorded{VolID: j.b.VolID, Name: sanitize.GuestName(kind, j.orig)}
			return j.d.Target.Destroy(ctx, kind, j.vmid, rec)
		})
		if err != nil {
			j.log.Error("destroy temporary guest failed; left for the janitor", "vmid", j.vmid, "err", err)
			if res.Verdict == model.Proven {
				res.Verdict, res.Reason = model.Error, "cleanup failed: destroy (booted ok)"
			} else {
				res.Reason += "; cleanup failed: destroy"
			}
			clean = false
		} else {
			done = append(done, fmt.Sprintf("destroyed %d", j.vmid))
			if err := j.safely("record destroyed", func() error { return j.setStage(state.StageDestroyed) }); err != nil {
				j.log.Error("record destroyed stage", "vmid", j.vmid, "err", err)
			}
		}
	}
	if j.protectSet {
		err := j.safely("unprotect", func() error {
			ctx, cancel := context.WithTimeout(context.Background(), unprotectTimeout)
			defer cancel()
			return j.setProtected(ctx, false)
		})
		if err != nil {
			j.log.Error("restore backup protected flag failed; left for the janitor", "vmid", j.vmid, "err", err)
			res.Reason += "; cleanup failed: unprotect"
			clean = false
		} else {
			done = append(done, "protected set back to false")
		}
	}
	if !clean {
		return
	}
	err := j.safely("remove job", func() error {
		return j.d.Store.Update(func(st *state.State) error {
			delete(st.InFlight, j.vmid)
			return nil
		})
	})
	if err != nil {
		j.log.Error("remove finished job from state", "vmid", j.vmid, "err", err)
	}
}

// panicError is a recovered panic value turned into an error.
type panicError struct{ v any }

func (e panicError) Error() string { return fmt.Sprint(e.v) }

// safely runs one cleanup step so that a panic in it is logged with its
// stack and returned as an error, letting the remaining steps still run.
func (j *job) safely(step string, fn func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			j.log.Error("panic during "+step, "panic", p, "stack", string(debug.Stack()))
			err = panicError{p}
		}
	}()
	return fn()
}

// annotate writes res's verdict line into b's notes and, for PROVEN, records
// the time in LastProven. SKIPPED is never written. Failures are logged only.
func annotate(d Deps, log *slog.Logger, b model.Backup, res model.GuestResult) {
	if res.Verdict == model.Skipped {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), notesTimeout)
	defer cancel()
	at := d.Now()
	// The listed notes are only the first line on PBS; merge into the full
	// notes, and write nothing if they cannot be read.
	node := d.Cfg.Target.Node
	if full, err := d.API.VolumeNotes(ctx, node, b.Storage, b.VolID); err != nil {
		log.Error("read backup notes failed; notes not written", "verdict", res.Verdict, "err", err)
	} else {
		line := notes.Line(res.Verdict, at, res.Reason)
		text := notes.Merge(full, line)
		if err := d.API.SetVolumeAttrs(ctx, node, b.Storage, b.VolID, &text, nil); err != nil {
			log.Error("write backup notes failed", "verdict", res.Verdict, "err", err)
		} else {
			log.Info("notes: " + line)
		}
	}
	if res.Verdict != model.Proven {
		return
	}
	err := d.Store.Update(func(st *state.State) error {
		st.LastProven[b.Key.String()] = at
		return nil
	})
	if err != nil {
		log.Error("record last proven", "err", err)
	}
}

func (j *job) setProtected(ctx context.Context, v bool) error {
	return j.d.API.SetVolumeAttrs(ctx, j.d.Cfg.Target.Node, j.b.Storage, j.b.VolID, nil, &v)
}

func (j *job) setStage(stage string) error { return j.d.Store.SetStage(j.vmid, stage, true) }

// lineBreaks are the runs of line breaks oneLine replaces.
var lineBreaks = regexp.MustCompile(`[\r\n]+`)

// oneLine turns each run of line breaks in s into one space: a reason is
// always one line.
func oneLine(s string) string { return lineBreaks.ReplaceAllString(s, " ") }
