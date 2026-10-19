// Package run drives proofing runs: it takes selected backups through the
// protect, restore, sanitize, boot, probe and teardown lifecycle (design
// section 6) and records every step so a crash can be cleaned up.
package run

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"git.breadthanalytics.com/tdiana/pbv/internal/config"
	"git.breadthanalytics.com/tdiana/pbv/internal/model"
	"git.breadthanalytics.com/tdiana/pbv/internal/notes"
	"git.breadthanalytics.com/tdiana/pbv/internal/probe"
	"git.breadthanalytics.com/tdiana/pbv/internal/pve"
	"git.breadthanalytics.com/tdiana/pbv/internal/report"
	"git.breadthanalytics.com/tdiana/pbv/internal/sanitize"
	"git.breadthanalytics.com/tdiana/pbv/internal/state"
	"git.breadthanalytics.com/tdiana/pbv/internal/target"
)

// Cleanup timeouts. Each cleanup step runs on its own fresh context so that a
// shutdown (cancelled run context) still destroys the guest, restores the
// backup's protected flag and writes the verdict, and so one slow step cannot
// starve the next.
const (
	destroyTimeout   = 5 * time.Minute
	unprotectTimeout = 2 * time.Minute
	notesTimeout     = 2 * time.Minute
)

// Deps is what a run needs. Now supplies every timestamp.
type Deps struct {
	Cfg      config.Config
	API      pve.API
	Target   target.Target
	Store    *state.Store
	Log      *slog.Logger
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
// original config. A panic while proofing becomes an ERROR verdict; teardown
// still runs.
func proofOne(ctx context.Context, d Deps, b model.Backup, vmid int, orig map[string]string) (res model.GuestResult) {
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	j := &job{d: d, b: b, vmid: vmid, orig: orig, log: log.With("guest", b.Key.String(), "vmid", vmid, "volid", b.VolID)}
	res = model.GuestResult{Backup: b, TempVMID: vmid}

	defer func() {
		if p := recover(); p != nil {
			j.log.Error("panic while proofing", "panic", p, "stack", string(debug.Stack()))
			res.Verdict, res.Reason = model.Error, fmt.Sprintf("internal error: %v", p)
		} else if ctx.Err() != nil {
			res.Verdict, res.Reason = model.Error, "cancelled"
		}
		j.teardown(&res)
		if err := j.safely("notes", func() error { j.annotate(res); return nil }); err != nil {
			res.Verdict, res.Reason = model.Error, "internal error: "+err.Error()
		}
	}()
	j.prove(ctx, &res)
	return res
}

// prove runs §6 steps 5–8 and sets the verdict and reason on res.
func (j *job) prove(ctx context.Context, res *model.GuestResult) {
	d, b, kind := j.d, j.b, j.b.Key.Kind
	fail := func(v model.Verdict, format string, args ...any) {
		res.Verdict, res.Reason = v, fmt.Sprintf(format, args...)
	}

	orig := b.Protected
	err := d.Store.Update(func(st *state.State) error {
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
			fail(model.Error, "protect backup: %v", err)
			return
		}
	}

	if err := j.setStage(state.StageRestored); err != nil {
		fail(model.Error, "record job: %v", err)
		return
	}
	j.restoreTried = true
	t0 := d.Now()
	err = d.Target.Restore(ctx, b, j.vmid, j.orig)
	res.RestoreDuration = d.Now().Sub(t0)
	if err != nil {
		if errors.Is(err, target.ErrRestoreNotStarted) {
			// The node refused the request: no guest of PBP's exists, and
			// one at vmid now (e.g. created meanwhile) is not ours.
			// If even this write fails, the entry may stay "restored"; the
			// target then still accepts a guest at vmid only if it is
			// stopped and named as in this backup (or locked by a
			// restore), which a guest someone else made does not pass.
			j.restoreTried = false
			if serr := j.setStage(state.StageProtected); serr != nil {
				j.log.Error("record restore not started", "err", serr)
			}
		}
		// Otherwise a guest may exist, untagged; the InFlight entry
		// (stage restored) makes it PBP's, and teardown removes it.
		fail(target.ClassifyRestoreError(err), "restore failed: %v", err)
		return
	}

	missing, err := d.Target.Sanitize(ctx, kind, j.vmid, j.orig)
	if errors.Is(err, target.ErrUnsafe) {
		fail(model.Error, "unsafe config: %s", strings.TrimPrefix(err.Error(), target.ErrUnsafe.Error()+": "))
		return
	}
	if err != nil {
		fail(model.Error, "sanitize failed: %v", err)
		return
	}

	guest := j.originalIdentity()
	res.Name = guest.Name

	if err := j.setStage(state.StageStarted); err != nil {
		fail(model.Error, "record job: %v", err)
		return
	}
	if err := d.Target.Start(ctx, kind, j.vmid); err != nil {
		fail(model.Error, "start failed: %v", err)
		return
	}
	boot, err := d.Target.WaitBooted(ctx, kind, j.vmid, d.Cfg.Restore.BootTimeout)
	res.BootDuration = boot.Duration
	switch {
	case errors.Is(err, target.ErrBootFailed):
		fail(model.Failed, "did not boot: %v", err)
		return
	case err != nil:
		// A node, API or SSH fault while waiting: not the backup's fault.
		fail(model.Error, "boot check error: %v", err)
		return
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
	})
	res.Verdict, res.Reason = out.Verdict, out.Reason
	res.FailedUnits, res.IgnoredUnits = out.FailedUnits, out.IgnoredUnits
	res.NotCovered, res.Checks, res.BootLog = out.NotCovered, out.Checks, out.BootLog
}

// originalIdentity is the guest's name and tags from the config stored in
// the backup: the restored guest is renamed and re-tagged "pbp".
func (j *job) originalIdentity() probe.GuestInfo {
	g := probe.GuestInfo{VMID: j.b.Key.VMID, Name: j.orig["name"]}
	if g.Name == "" {
		g.Name = j.orig["hostname"]
	}
	g.Tags = sanitize.SplitList(j.orig["tags"])
	return g
}

// teardown destroys the guest and restores the backup's protected flag. The
// guest is PBP's by the InFlight record (tagged or not, e.g. after a failed
// restore), so it is removed with Target.DestroyRecorded. The
// InFlight entry is removed only when both succeed; otherwise the janitor
// finishes the job. A failed destroy turns PROVEN into ERROR (so the guest
// is proofed again next run) and otherwise only notes it in the reason, as
// does a failed unprotect.
func (j *job) teardown(res *model.GuestResult) {
	clean := true
	if j.restoreTried {
		err := j.safely("destroy", func() error {
			ctx, cancel := context.WithTimeout(context.Background(), destroyTimeout)
			defer cancel()
			return j.d.Target.DestroyRecorded(ctx, j.b.Key.Kind, j.vmid, j.b.VolID)
		})
		if err != nil {
			j.log.Error("destroy temporary guest failed; left for the janitor", "err", err)
			if res.Verdict == model.Proven {
				res.Verdict, res.Reason = model.Error, "cleanup failed: destroy (booted ok)"
			} else {
				res.Reason += "; cleanup failed: destroy"
			}
			clean = false
		} else if err := j.safely("record destroyed", func() error { return j.setStage(state.StageDestroyed) }); err != nil {
			j.log.Error("record destroyed stage", "err", err)
		}
	}
	if j.protectSet {
		err := j.safely("unprotect", func() error {
			ctx, cancel := context.WithTimeout(context.Background(), unprotectTimeout)
			defer cancel()
			return j.setProtected(ctx, false)
		})
		if err != nil {
			j.log.Error("restore backup protected flag failed; left for the janitor", "err", err)
			res.Reason += "; cleanup failed: unprotect"
			clean = false
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
		j.log.Error("remove finished job from state", "err", err)
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

// annotate writes the verdict line to the backup's notes and records PROVEN
// times. A failed notes write is logged; it never changes the verdict.
func (j *job) annotate(res model.GuestResult) { annotate(j.d, j.log, j.b, res) }

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
		text := notes.Merge(full, notes.Line(res.Verdict, at, res.Reason))
		if err := d.API.SetVolumeAttrs(ctx, node, b.Storage, b.VolID, &text, nil); err != nil {
			log.Error("write backup notes failed", "verdict", res.Verdict, "err", err)
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

func (j *job) setStage(stage string) error {
	return j.d.Store.Update(func(st *state.State) error {
		job := st.InFlight[j.vmid]
		job.Stage = stage
		st.InFlight[j.vmid] = job
		return nil
	})
}
