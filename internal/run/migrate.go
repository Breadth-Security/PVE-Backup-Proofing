package run

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/notes"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/sanitize"
)

// oldNotes are the listed backups whose notes start with a PBP line in the
// format of 0.5 to 0.7, which pushed the guest's name out of the backup's
// comment (issue #76). The listing's notes are the first line on PBS.
func oldNotes(all []model.Backup) []model.Backup {
	var out []model.Backup
	for _, b := range all {
		if notes.OldFormat(b.Notes) {
			out = append(out, b)
		}
	}
	return out
}

// maxMigrations bounds the backups one run rewrites: each costs a config
// extraction from PBS, and the first run after upgrading may find hundreds.
// The rest wait for the next run.
const maxMigrations = 100

// errNoConfig is a backup whose config cannot be read while tags may
// exclude its guest: its notes are left alone.
var errNoConfig = errors.New("backup config unreadable")

// migrateNotes puts the guest's name back in front of the old-format PBP
// line of each backup in bs, every backup of a guest and not only the
// newest, so a search by name finds them again. It runs after the proofs,
// skips excluded guests (their notes are left alone), rewrites at most
// maxMigrations backups, and stops when the run is cancelled, stopping or
// past restore.max_run_duration. Failures are logged only: the next run
// retries.
func (p *pool) migrateNotes(ctx context.Context, bs []model.Backup, start time.Time) {
	d := p.d
	node := d.Cfg.Target.Node
	done, failed, left := 0, 0, 0
	for i, b := range bs {
		limit := d.Cfg.Restore.MaxRunDuration
		if ctx.Err() != nil || p.stopping() || done+failed >= maxMigrations ||
			(limit > 0 && !d.Now().Before(start.Add(limit))) {
			left = len(bs) - i
			break
		}
		if d.Cfg.Selection.Exclude.ExcludedBy(string(b.Key.Kind), b.Key.VMID, p.liveTags[b.Key]) != "" {
			continue
		}
		changed, err := migrateOne(ctx, d, node, b, p.log)
		switch {
		case errors.Is(err, errNoConfig):
			p.log.Info("notes of " + b.VolID + " left in the old format: its backup config cannot be read and might exclude the guest by tag")
		case err != nil:
			failed++
			p.log.Warn(fmt.Sprintf("notes of %s: cannot add the guest name to the PBP line: %s", b.VolID, oneLine(err.Error())))
		case changed:
			done++
		}
	}
	if done > 0 || failed > 0 || left > 0 {
		p.log.Info(fmt.Sprintf("notes: added the guest name in front of the PBP line on %d older backups (%d failed, %d left for the next run)", done, failed, left))
	}
}

// migrateOne rewrites b's notes if they still start with an old-format PBP
// line, naming the guest from the backed-up config.
func migrateOne(ctx context.Context, d Deps, node string, b model.Backup, log *slog.Logger) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, notesTimeout)
	defer cancel()
	full, err := d.API.VolumeNotes(ctx, node, b.Storage, b.VolID)
	if err != nil {
		return false, err
	}
	if !notes.OldFormat(full) {
		return false, nil // rewritten meanwhile, by this run's proof
	}
	var orig map[string]string
	switch raw, err := d.API.OriginalConfig(ctx, node, b.VolID); {
	case err == nil:
		orig = sanitize.ParseConfigText(raw)
	case len(d.Cfg.Selection.Exclude.Tags) > 0:
		// Its own tags might exclude it, as in admit.
		return false, errNoConfig
	default:
		// Named by its label (noteName with no config), as in admit.
		log.Debug("backup config unreadable, naming the guest by its label", "volid", b.VolID, "err", err)
	}
	if rule := d.Cfg.Selection.Exclude.ExcludedBy(string(b.Key.Kind), b.Key.VMID, sanitize.SplitList(orig["tags"])); rule != "" {
		return false, nil
	}
	text, changed := notes.Migrate(full, noteName(b.Key, orig))
	if !changed {
		return false, nil
	}
	if err := d.API.SetVolumeAttrs(ctx, node, b.Storage, b.VolID, &text, nil); err != nil {
		return false, err
	}
	log.Debug("notes rewritten", "volid", b.VolID)
	return true, nil
}
