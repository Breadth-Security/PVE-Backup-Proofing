package main

import (
	"context"
	"io"
	"log/syslog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/node"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/report"
)

const nodeUsage = "usage: pbp node            (SSH forced command only; reads SSH_ORIGINAL_COMMAND)\n" +
	"       pbp node install-templates   (interactive, as root)\n"

const msgNoRequest = "pbp-node: refused: no request (pbp node runs only as an SSH forced command)"

// auditEntry is one helper request for the node's journal. It never holds
// argv or notify payloads, which may contain secrets.
type auditEntry struct {
	Outcome string // "ok", or a nodeproto.Kind* value
	Op      string
	VMID    int // 0 when the request has none
	Reason  string
	Client  string // from SSH_CONNECTION / SSH_CLIENT, may be empty
}

// auditLog records helper requests.
type auditLog interface{ Log(auditEntry) }

type noAudit struct{}

func (noAudit) Log(auditEntry) {}

// syslogAudit writes to the node's journal via syslog (facility AUTH, tag
// pbp-node).
type syslogAudit struct{ w *syslog.Writer }

// newSyslogAudit falls back to a no-op logger if syslog is unavailable.
func newSyslogAudit() auditLog {
	w, err := syslog.New(syslog.LOG_AUTH|syslog.LOG_INFO, "pbp-node")
	if err != nil {
		return noAudit{}
	}
	return syslogAudit{w}
}

func (s syslogAudit) Log(e auditEntry) {
	line := formatAudit(e)
	if e.Outcome == "ok" {
		_ = s.w.Info(line)
		return
	}
	_ = s.w.Warning(line)
}

func formatAudit(e auditEntry) string {
	var b strings.Builder
	b.WriteString("outcome=" + e.Outcome)
	if e.Op != "" {
		b.WriteString(" op=" + e.Op)
	}
	if e.VMID != 0 {
		b.WriteString(" vmid=" + strconv.Itoa(e.VMID))
	}
	if e.Client != "" {
		b.WriteString(" client=" + e.Client)
	}
	if e.Reason != "" {
		b.WriteString(" reason=" + strconv.Quote(e.Reason))
	}
	return b.String()
}

// clientAddr is the SSH client's address, if sshd told us.
func clientAddr() string {
	for _, k := range []string{"SSH_CONNECTION", "SSH_CLIENT"} {
		if f := strings.Fields(os.Getenv(k)); len(f) > 0 {
			return cleanField(f[0], 64)
		}
	}
	return ""
}

// cleanField drops non-printable characters and bounds the length.
func cleanField(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s)
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// tailWriter passes writes through and remembers the last complete line.
type tailWriter struct {
	w    io.Writer
	cur  []byte
	last string
}

const maxTailLine = 4096

func (t *tailWriter) Write(p []byte) (int, error) {
	for _, c := range p {
		if c == '\n' {
			t.last = string(t.cur)
			t.cur = t.cur[:0]
		} else if len(t.cur) < maxTailLine {
			t.cur = append(t.cur, c)
		}
	}
	return t.w.Write(p)
}

// nodeMain runs `pbp node` and `pbp node install-templates`.
func (a *app) nodeMain(ctx context.Context, args []string) int {
	request, sshSet := os.LookupEnv("SSH_ORIGINAL_COMMAND")
	if len(args) > 0 {
		if args[0] != "install-templates" || len(args) > 1 {
			fprintf(a.stderr, "pbp node: unexpected arguments %q\n%s", args, nodeUsage)
			return exitUsage
		}
		return a.installTemplates(sshSet)
	}
	if request == "" {
		fprintln(a.stderr, msgNoRequest)
		return nodeproto.ExitRefused
	}

	req, perr := nodeproto.Parse(request)
	entry := auditEntry{Outcome: "ok", Op: req.Op, VMID: req.VMID, Client: clientAddr()}

	cfg, err := node.LoadConfig(a.nodeConfigPath())
	if err != nil {
		reason := cleanField(strings.Join(strings.Fields(err.Error()), " "), 300)
		if perr == nil {
			fprintln(a.stderr, nodeproto.Marker(nodeproto.KindRefused, req.Nonce, reason))
		} else {
			fprintln(a.stderr, "pbp-node: refused: "+reason)
		}
		entry.Outcome, entry.Reason = nodeproto.KindRefused, reason
		a.audit.Log(entry)
		return nodeproto.ExitRefused
	}

	errw := &tailWriter{w: a.stderr}
	code := node.Serve(ctx, cfg, a.nodeSys, version, request, a.stdin, a.stdout, errw)

	if perr != nil {
		// The parse error can echo request text, so log a fixed reason.
		entry.Outcome, entry.Reason = nodeproto.KindRefused, "invalid request"
	} else if code == nodeproto.ExitRefused {
		if kind, reason, ok := nodeproto.HelperFailure(code, errw.last+"\n", req.Nonce); ok {
			entry.Outcome, entry.Reason = kind, cleanField(reason, 300)
		}
	}
	a.audit.Log(entry)
	return code
}

func (a *app) nodeConfigPath() string {
	if p := os.Getenv("PBP_NODE_CONFIG"); p != "" {
		return p
	}
	return node.DefaultConfigPath
}

// installTemplates installs the notification templates; interactive root only.
func (a *app) installTemplates(sshSet bool) int {
	if sshSet || a.euid() != 0 {
		fprintf(a.stderr, "pbp node install-templates: refused: run it interactively as root, not over SSH\n%s", nodeUsage)
		return exitUsage
	}
	if err := node.InstallTemplates(a.templateDir); err != nil {
		fprintf(a.stderr, "pbp node install-templates: %v\n", err)
		return exitFail
	}
	names := make([]string, 0)
	for n := range report.Templates() {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fprintln(a.stdout, filepath.Join(a.templateDir, n))
	}
	return exitOK
}
