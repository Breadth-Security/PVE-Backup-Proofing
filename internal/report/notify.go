package report

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"strings"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
	"git.breadthanalytics.com/tdiana/pbv/internal/sshx"
)

//go:embed templates/*.hbs
var templateFS embed.FS

const (
	templateDir = "/etc/pve/notification-templates/default"
	maxStderr   = 2048

	notifyScript = `use PVE::Notify; use JSON; my $in = decode_json(do { local $/; <STDIN> }); PVE::Notify::notify($in->{severity}, 'pbp', $in->{data}, $in->{fields});`
)

var templateFiles = []string{"pbp-subject.txt.hbs", "pbp-body.txt.hbs"}

// PVENotifier sends summaries through the node's own notification system.
type PVENotifier struct {
	SSH  sshx.Runner
	Node string
}

var _ Notifier = (*PVENotifier)(nil)

// EnsureTemplates installs the embedded templates on the node when missing
// or different.
func (p *PVENotifier) EnsureTemplates(ctx context.Context) error {
	for _, name := range templateFiles {
		want, err := templateFS.ReadFile("templates/" + name)
		if err != nil {
			return fmt.Errorf("report: embedded template %s: %w", name, err)
		}
		path := templateDir + "/" + name
		res, err := p.SSH.Run(ctx, "cat "+sshx.Quote(path), nil)
		if err != nil {
			return fmt.Errorf("report: read %s: %w", path, err)
		}
		if res.ExitCode == 0 && res.Stdout == string(want) {
			continue
		}
		cmd := "mkdir -p " + sshx.Quote(templateDir) + " && cat > " + sshx.Quote(path)
		res, err = p.SSH.Run(ctx, cmd, want)
		if err != nil {
			return fmt.Errorf("report: write %s: %w", path, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("report: write %s: exit %d: %s", path, res.ExitCode, trimStderr(res.Stderr))
		}
	}
	return nil
}

// Send delivers the run summary via PVE::Notify::notify.
func (p *PVENotifier) Send(ctx context.Context, s model.RunSummary) error {
	payload, err := json.Marshal(map[string]any{
		"severity": s.Severity(),
		"data":     TemplateData(s, p.Node),
		"fields":   map[string]string{"type": "pbp", "hostname": p.Node},
	})
	if err != nil {
		return fmt.Errorf("report: encode notification: %w", err)
	}
	res, err := p.SSH.Run(ctx, "perl -e "+sshx.Quote(notifyScript), payload)
	if err != nil {
		return fmt.Errorf("report: send notification: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("report: send notification: exit %d: %s", res.ExitCode, trimStderr(res.Stderr))
	}
	return nil
}

func trimStderr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxStderr {
		s = s[:maxStderr]
	}
	return s
}
