package run

import (
	"context"
	"errors"
	"strings"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/privs"
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
