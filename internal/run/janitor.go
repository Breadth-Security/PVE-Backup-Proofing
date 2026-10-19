package run

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"git.breadthanalytics.com/tdiana/pbv/internal/state"
)

// Janitor finishes the teardown of jobs a crashed run left in the state
// store, then destroys any other guest PBP owns (in the VMID range and tagged
// "pbp"). Target.Destroy enforces range and tag, so a guest it refuses is
// left alone and its entry kept. An entry is removed only once every step for
// it succeeded. The returned error joins every failure.
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
// the guest unless already destroyed, restore the backup's protected flag,
// then drop the entry.
func cleanupJob(ctx context.Context, d Deps, j state.Job) error {
	log := logger(d).With("vmid", j.TempVMID, "volid", j.VolID, "stage", j.Stage)
	var errs []error
	if j.Stage != stageDestroyed {
		log.Warn("janitor: destroying guest of crashed job")
		if err := d.Target.Destroy(ctx, j.Kind, j.TempVMID); err != nil {
			errs = append(errs, fmt.Errorf("destroy %s/%d: %w", j.Kind, j.TempVMID, err))
		}
	}
	if j.ProtectedOrig != nil && !*j.ProtectedOrig {
		off := false
		log.Info("janitor: restoring backup protected flag")
		if err := d.API.SetVolumeAttrs(ctx, d.Cfg.Target.Node, j.Storage, j.VolID, nil, &off); err != nil {
			errs = append(errs, fmt.Errorf("unprotect %s: %w", j.VolID, err))
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
