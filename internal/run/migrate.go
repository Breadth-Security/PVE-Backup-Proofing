package run

import (
	"context"
	"fmt"
	"log/slog"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/notes"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sanitize"
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

// migrateNotes puts the guest's name back in front of the old-format PBP
// line of each backup in bs, every backup of a guest and not only the
// newest, so a search by name finds them again. It runs after the proofs,
// skips excluded guests (their notes are left alone) and stops when the run
// is cancelled or stopping. Failures are logged only: the next run retries.
func (p *pool) migrateNotes(ctx context.Context, bs []model.Backup) {
	d := p.d
	node := d.Cfg.Target.Node
	done, failed := 0, 0
	for _, b := range bs {
		if ctx.Err() != nil || p.stopping() {
			break
		}
		if d.Cfg.Selection.Exclude.ExcludedBy(string(b.Key.Kind), b.Key.VMID, p.liveTags[b.Key]) != "" {
			continue
		}
		changed, err := migrateOne(ctx, d, node, b, p.log)
		switch {
		case err != nil:
			failed++
			p.log.Warn(fmt.Sprintf("notes of %s: cannot add the guest name to the PBP line: %s", b.VolID, oneLine(err.Error())))
		case changed:
			done++
		}
	}
	if done > 0 || failed > 0 {
		p.log.Info(fmt.Sprintf("notes: added the guest name in front of the PBP line on %d older backups (%d failed, retried at the next run)", done, failed))
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
	raw, err := d.API.OriginalConfig(ctx, node, b.VolID)
	if err != nil {
		return false, err
	}
	orig := sanitize.ParseConfigText(raw)
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
