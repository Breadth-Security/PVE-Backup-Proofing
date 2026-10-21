package report

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

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

// maxPayload keeps the notification under the node helper's 1 MiB limit on
// a notify payload, with room to spare.
const maxPayload = 1000 << 10

// listKeys are the TemplateData lists shortened when the payload is too big.
var listKeys = []string{"environment-errors", "failed", "errored", "booted", "skipped", "not-covered"}

// shortened returns data with each list cut to keep lines plus a line saying
// how many more the run log has, and the proven list cut to 4 KiB; keep < 0
// returns data unchanged.
func shortened(data map[string]any, keep int) map[string]any {
	if keep < 0 {
		return data
	}
	out := make(map[string]any, len(data))
	for k, v := range data {
		out[k] = v
	}
	for _, k := range listKeys {
		l, _ := data[k].([]string)
		if len(l) > keep {
			out[k] = append(l[:keep:keep], fmt.Sprintf("… and %d more; see the run log", len(l)-keep))
		}
	}
	if p, _ := data["proven"].(string); len(p) > 4096 {
		if cut := strings.LastIndex(p[:4096], ", "); cut > 0 {
			out["proven"] = p[:cut] + ", … (see the run log)"
		} else {
			out["proven"] = "… (see the run log)"
		}
	}
	return out
}

// essentials keeps the headline, summary, log path and node of data, a run
// error cut to 4 KiB, and empty lists whose place says to read the run log.
func essentials(data map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"node", "run-id", "headline", "summary", "logs"} {
		out[k] = data[k]
	}
	re, _ := data["run-error"].(string)
	if len(re) > 4096 {
		cut := 4096
		for cut > 0 && !utf8.RuneStart(re[cut]) {
			cut--
		}
		re = re[:cut] + "…"
	}
	out["run-error"] = re
	for _, k := range listKeys {
		out[k] = []string{}
	}
	out["environment-errors"] = []string{"the details are too long for a notification; see the run log"}
	out["proven"] = ""
	return out
}

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
	data := TemplateData(s, p.Node)
	var payload []byte
	// Shorten the lists until the payload fits the helper's limit: a run
	// with very many problems still notifies, pointing to the run log.
	for _, keep := range []int{-1, 100, 20, 5, 0} {
		var err error
		payload, err = json.Marshal(map[string]any{
			"severity": s.Severity(),
			"data":     shortened(data, keep),
			"fields":   map[string]string{"type": "pbp", "hostname": p.Node},
		})
		if err != nil {
			return fmt.Errorf("report: encode notification: %w", err)
		}
		if len(payload) <= maxPayload {
			break
		}
	}
	if len(payload) > maxPayload {
		// Still too big (PBP's own lines: a huge run error or many
		// environment errors): send the essentials, which always fit, so
		// the run is never silent (#48 review).
		var err error
		payload, err = json.Marshal(map[string]any{
			"severity": s.Severity(),
			"data":     essentials(data),
			"fields":   map[string]string{"type": "pbp", "hostname": p.Node},
		})
		if err != nil {
			return fmt.Errorf("report: encode notification: %w", err)
		}
	}
	if err := p.Helper.Notify(ctx, payload); err != nil {
		return fmt.Errorf("report: send notification: %w", err)
	}
	return nil
}
