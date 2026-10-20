package report

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sshx"
)

// NotifyScript sends one notification through PVE::Notify: a
// {severity, data, fields} JSON payload on stdin, template name pbp. The
// node helper runs it; the daemon never sends it.
const NotifyScript = `use PVE::Notify; use JSON; my $in = decode_json(do { local $/; <STDIN> }); PVE::Notify::notify($in->{severity}, 'pbp', $in->{data}, $in->{fields});`

// InstallHint tells the operator how to fix missing or outdated templates.
const InstallHint = `run "pbp node install-templates" as root on the node`

// PVENotifier sends summaries through the node's own notification system,
// by way of the `pbp node` helper.
type PVENotifier struct {
	Helper sshx.Helper
	Node   string
}

var _ Notifier = (*PVENotifier)(nil)

// CheckTemplates compares the templates installed on the node (their sha256,
// as the helper reports them) with the embedded ones. It writes nothing: a
// missing or different template is an error naming each such file and how
// to install them.
func (p *PVENotifier) CheckTemplates(ctx context.Context) error {
	installed, err := p.Helper.Templates(ctx)
	if err != nil {
		return fmt.Errorf("report: read installed templates: %w", err)
	}
	embedded := Templates()
	names := make([]string, 0, len(embedded))
	for name := range embedded {
		names = append(names, name)
	}
	slices.Sort(names)
	var bad []string
	for _, name := range names {
		sum := sha256.Sum256(embedded[name])
		switch got := installed[name]; got {
		case hex.EncodeToString(sum[:]):
		case "":
			bad = append(bad, name+" is missing")
		default:
			bad = append(bad, name+" differs from this version's")
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("report: notification templates on the node: %s; %s", strings.Join(bad, ", "), InstallHint)
	}
	return nil
}

// Send delivers the run summary through the helper's notify request, which
// hands it to PVE::Notify::notify.
func (p *PVENotifier) Send(ctx context.Context, s model.RunSummary) error {
	payload, err := json.Marshal(map[string]any{
		"severity": s.Severity(),
		"data":     TemplateData(s, p.Node),
		"fields":   map[string]string{"type": "pbp", "hostname": p.Node},
	})
	if err != nil {
		return fmt.Errorf("report: encode notification: %w", err)
	}
	if err := p.Helper.Notify(ctx, payload); err != nil {
		return fmt.Errorf("report: send notification: %w", err)
	}
	return nil
}
