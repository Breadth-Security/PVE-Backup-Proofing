package node

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/report"
)

// InstallTemplates writes the embedded notification templates into dir
// (DefaultTemplateDir on a node), replacing outdated copies. It backs
// `pbp node install-templates`, which an admin runs as root; it is never
// reachable over SSH.
func InstallTemplates(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil { // #nosec G301 -- PVE's template dir is world-readable
		return fmt.Errorf("install templates: %w", err)
	}
	tmpl := report.Templates()
	names := make([]string, 0, len(tmpl))
	for n := range tmpl {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, tmpl[n], 0o644); err != nil { // #nosec G306 -- templates are not secret
			return fmt.Errorf("install templates: %w", err)
		}
	}
	return nil
}
