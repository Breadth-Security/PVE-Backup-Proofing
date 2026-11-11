package run

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/privs"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/pve"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/sanitize"
)

// needs are the privileges PBP's token needs for d's config.
func needs(d Deps) []privs.Need {
	return privs.Needs(d.Cfg.Target.Node, d.Cfg.Restore.Storage, d.Cfg.Sources.Storages)
}

// describe joins what each missing need lacks, with its fix.
func describe(d Deps, missing []privs.Missing) string {
	parts := make([]string, len(missing))
	for i, m := range missing {
		parts[i] = m.Describe(d.Cfg.Target.API.TokenID)
	}
	return strings.Join(parts, "; ")
}

// checkCorePrivileges fails when the token lacks a privilege on a path no
// proof can do without: /vms, the restore storage, the node or the SDN zone
// (issue #107). Source storages are left to discovery, which skips one the
// token cannot read, and to pbp check.
func checkCorePrivileges(ctx context.Context, d Deps) error {
	var core []privs.Need
	for _, n := range needs(d) {
		if n.Source == "" {
			core = append(core, n)
		}
	}
	missing, err := privs.Check(ctx, d.API, core)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return errors.New(describe(d, missing))
	}
	return nil
}

// sourcePrivilegeProblem says why the token cannot read source storage st
// ("" if it can). The restore storage is a core path, checked by preflight.
func sourcePrivilegeProblem(ctx context.Context, d Deps, st string) string {
	for _, n := range needs(d) {
		if n.Source != st {
			continue
		}
		missing, err := privs.Check(ctx, d.API, []privs.Need{n})
		if err != nil {
			return "cannot " + err.Error() // "cannot read the token's privileges on …"
		}
		return describe(d, missing)
	}
	return ""
}

// emptyStorageWarning is the warning for a source storage that lists no
// backups to the token: PVE leaves out, without an error, every backup the
// caller may not see.
func emptyStorageWarning(d Deps, st string) string {
	user, token, _ := strings.Cut(d.Cfg.Target.API.TokenID, "!")
	return fmt.Sprintf("storage %s lists no backups for the token: it is empty, or the token cannot see them "+
		"(check: pveum user token permissions %s %s --path /vms/<vmid of a guest backed up there>; see docs/troubleshooting.md)",
		st, user, token)
}

// CheckSources is pbp check's look at each source storage: the token's
// privileges on it, and that it is active, enabled and holds backups. A
// restore storage that is also a source gets the backup check only; preflight
// covers its privileges (#115). Unlike a run, which skips a storage the token
// cannot read with a warning, it fails, so pbp setup prints the node lines
// that grant the missing ACLs. Its error is a *SourceProblems.
func CheckSources(ctx context.Context, d Deps) error {
	restore := d.Cfg.Restore.Storage
	var sp SourceProblems
	for _, n := range needs(d) {
		st := n.Source
		switch {
		case st != "":
			if p := sourcePrivilegeProblem(ctx, d, st); p != "" {
				// Its status would only add PVE's 403 for the same cause.
				sp.Access = append(sp.Access, p)
				continue
			}
		case n.Path == "/storage/"+restore && slices.Contains(d.Cfg.Sources.Storages, restore):
			// The restore storage's own (core) need: Needs drops it from
			// the sources, so it comes up here once and never above.
			st = restore
		default:
			continue
		}
		status, err := d.API.StorageStatus(ctx, d.Cfg.Target.Node, st)
		var ae *pve.APIError
		switch {
		case err != nil && errors.As(err, &ae) && ae.Status != http.StatusForbidden:
			// PVE's own answer, such as a storage that does not exist on
			// the node: the storage definition is at fault.
			sp.Storage = append(sp.Storage, fmt.Sprintf("storage %s: %v", st, err))
		case err != nil:
			// A 403 (rights the node line grants), or no answer at all (a
			// lost connection, a timeout): not a storage problem setup
			// could name, so the node lines stay.
			sp.Access = append(sp.Access, fmt.Sprintf("storage %s: %v", st, err))
		case !status.Active || !status.Enabled:
			sp.Storage = append(sp.Storage, fmt.Sprintf("storage %s is not active and enabled", st))
		case !slices.Contains(sanitize.SplitList(status.Content), "backup"):
			sp.Storage = append(sp.Storage, fmt.Sprintf("storage %s does not hold backup content (has %q)", st, status.Content))
		}
	}
	if len(sp.Access)+len(sp.Storage) > 0 {
		return &sp
	}
	return nil
}

// SourceProblems is what CheckSources found: Access is what the token lacks
// (the node line grants it), Storage what is wrong with a storage itself
// (inactive, missing, no backup content), which no node line fixes (#119).
type SourceProblems struct {
	Access, Storage []string
}

func (e *SourceProblems) Error() string {
	return strings.Join(append(append([]string{}, e.Access...), e.Storage...), "; ")
}
