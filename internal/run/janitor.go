package run

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"

	"git.breadthanalytics.com/tdiana/pbv/internal/state"
)

// Janitor finishes the teardown of jobs a crashed run left in the state
// store, then destroys any other guest PBP owns (in the VMID range and tagged
// "pbp"). For a job whose restore may have started, an untagged guest at its
// VMID is removed too if the target accepts it as that restore's leftover
// (stopped, named as in the backup or still locked by the restore; design
// section 11.2); range, kind and node are enforced as always. A guest the target refuses is left alone and its entry
// kept. An entry is removed only once every step for it succeeded. The
// returned error joins every failure.
func Janitor(ctx context.Context, d Deps) error {
	log := logger(d)
	var errs []error

	inFlight := d.Store.Snapshot().InFlight
	vmids := make([]int, 0, len(inFlight))
	for v := range inFlight {
		vmids = append(vmids, v)
	}
	slices.Sort(vmids)
	for _, v := range vmids {
		if err := cleanupJob(ctx, d, inFlight[v]); err != nil {
			log.Error("janitor: crashed job not cleaned up; entry kept", "vmid", v, "volid", inFlight[v].VolID, "err", err)
			errs = append(errs, fmt.Errorf("job %d: %w", v, err))
		}
	}

	owned, err := d.Target.Owned(ctx)
	if err != nil {
		log.Error("janitor: list owned guests", "err", err)
		return errors.Join(append(errs, fmt.Errorf("list owned guests: %w", err))...)
	}
	kept := d.Store.Snapshot().InFlight
	for _, g := range owned {
		if _, ok := kept[g.Key.VMID]; ok {
			continue // its job's cleanup failed above and was reported
		}
		log.Warn("janitor: destroying leftover guest", "guest", g.Key.String(), "name", g.Name)
		if err := d.Target.Destroy(ctx, g.Key.Kind, g.Key.VMID); err != nil {
			log.Error("janitor: destroy leftover guest", "guest", g.Key.String(), "err", err)
			errs = append(errs, fmt.Errorf("leftover %s: %w", g.Key, err))
		}
	}
	return errors.Join(errs...)
}

// cleanupJob undoes what the recorded stage says may have happened: destroy
// the guest unless already destroyed (and record that at once, so a failure
// below never leaves an empty VMID recorded as an in-flight restore), restore
// the backup's protected flag, then drop the entry.
func cleanupJob(ctx context.Context, d Deps, j state.Job) error {
	log := logger(d).With("vmid", j.TempVMID, "volid", j.VolID, "stage", j.Stage)
	var errs []error
	if j.Stage != state.StageDestroyed {
		log.Warn("janitor: destroying guest of crashed job")
		var err error
		if j.MayHaveGuest() {
			err = d.Target.DestroyRecorded(ctx, j.Kind, j.TempVMID, j.VolID)
		} else {
			err = d.Target.Destroy(ctx, j.Kind, j.TempVMID)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("destroy %s/%d: %w", j.Kind, j.TempVMID, err))
		} else if err := d.Store.SetStage(j.TempVMID, state.StageDestroyed, false); err != nil {
			errs = append(errs, fmt.Errorf("record %d destroyed: %w", j.TempVMID, err))
		}
	}
	if j.ProtectedOrig != nil && !*j.ProtectedOrig {
		off := false
		log.Info("janitor: restoring backup protected flag")
		if err := d.API.SetVolumeAttrs(ctx, d.Cfg.Target.Node, j.Storage, j.VolID, nil, &off); err != nil {
			if volumeGone(err, j.VolID) {
				log.Warn("janitor: backup no longer exists; nothing to unprotect", "err", err)
			} else {
				errs = append(errs, fmt.Errorf("unprotect %s: %w", j.VolID, err))
			}
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return d.Store.Update(func(st *state.State) error {
		delete(st.InFlight, j.TempVMID)
		return nil
	})
}

// volumeGone reports whether err says this backup volume no longer exists
// (pruned or deleted since the crash), so there is no flag left to restore.
// The message must name the volume (its volid, or "volume ... does not
// exist", "no such volume", "backup ... not found"): a bare 404, a missing
// storage or a transport error ("no such host") never counts, since the
// volume may still be there and protected.
func volumeGone(err error, volid string) bool {
	var ne net.Error
	if errors.As(err, &ne) {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "no such host") || storageMissing.MatchString(msg) {
		return false
	}
	if volumeMissing.MatchString(msg) {
		return true
	}
	if !strings.Contains(msg, strings.ToLower(volid)) {
		return false
	}
	for _, s := range []string{"does not exist", "not found", "no such"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

var (
	storageMissing = regexp.MustCompile(`storage '[^']*' does not exist`)
	volumeMissing  = regexp.MustCompile(`volume .* does not exist|no such volume|backup .* not found`)
)
