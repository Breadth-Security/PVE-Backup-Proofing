package report

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sshx"
)

const (
	templateDir = "/etc/pve/notification-templates/default"
	maxStderr   = 2048
	// exitMissing is the exit code of the template read when the file does
	// not exist (see readTemplateCmd).
	exitMissing = 3

	// NotifyScript sends one notification through PVE::Notify: a
	// {severity, data, fields} JSON payload on stdin, template name pbp.
	NotifyScript = `use PVE::Notify; use JSON; my $in = decode_json(do { local $/; <STDIN> }); PVE::Notify::notify($in->{severity}, 'pbp', $in->{data}, $in->{fields});`
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
	embedded := Templates()
	for _, name := range templateFiles {
		want, ok := embedded[name]
		if !ok {
			return fmt.Errorf("report: embedded template %s missing", name)
		}
		path := templateDir + "/" + name
		res, err := p.SSH.Run(ctx, readTemplateCmd(path), nil)
		if err != nil {
			return fmt.Errorf("report: read %s: %w", path, err)
		}
		if res.ExitCode == 0 && res.Stdout == string(want) {
			continue
		}
		if res.ExitCode != 0 && res.ExitCode != exitMissing {
			// Not merely missing: overwriting what cannot be read could
			// clobber a template this process cannot see.
			return fmt.Errorf("report: read %s: exit %d: %s", path, res.ExitCode, trimStderr(res.Stderr))
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

// readTemplateCmd reads path, exiting with exitMissing if it does not exist,
// so a missing file is told apart from an unreadable one by exit code alone.
func readTemplateCmd(path string) string {
	q := sshx.Quote(path)
	return "test -e " + q + " || exit " + fmt.Sprint(exitMissing) + "; cat " + q
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
	res, err := p.SSH.Run(ctx, "perl -e "+sshx.Quote(NotifyScript), payload)
	if err != nil {
		return fmt.Errorf("report: send notification: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("report: send notification: exit %d: %s", res.ExitCode, trimStderr(res.Stderr))
	}
	// PVE::Notify logs a target it could not notify (including a template
	// that failed to render) as an "ERROR:" line and still exits 0.
	var errs []string
	for _, line := range strings.Split(res.Stderr, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "ERROR:") {
			errs = append(errs, strings.TrimSpace(line))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("report: send notification: %s", trimStderr(strings.Join(errs, "; ")))
	}
	return nil
}

// trimStderr trims s and cuts it to at most maxStderr bytes, dropping any
// bytes that do not form valid UTF-8 (including a character cut in half).
func trimStderr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxStderr {
		s = strings.ToValidUTF8(s[:maxStderr], "")
	}
	return s
}
