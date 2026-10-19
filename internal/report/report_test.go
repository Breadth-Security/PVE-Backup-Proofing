package report

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
	"git.breadthanalytics.com/tdiana/pbv/internal/sshx"
)

func failedSummary() model.RunSummary {
	return model.RunSummary{
		ID:      "r1",
		Started: time.Date(2026, 10, 19, 2, 0, 0, 0, time.UTC),
		Ended:   time.Date(2026, 10, 19, 3, 2, 3, 400, time.UTC),
		Results: []model.GuestResult{{
			Backup:  model.Backup{Key: model.GuestKey{Kind: model.KindQEMU, VMID: 101}},
			Name:    "web",
			Verdict: model.Failed,
			Reason:  "failed: nginx.service",
		}},
	}
}

func TestLogGuest(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger(&bytes.Buffer{}, &buf)
	nc := model.NotCovered{Unit: "qemu/101", Piece: model.MissingPiece{Kind: "excluded-disk", Key: "scsi1"}}
	LogGuest(l, model.GuestResult{
		Verdict: model.Failed, Reason: "boom", FailedUnits: []string{"a.service"},
		RestoreDuration: 3 * time.Second, NotCovered: []model.NotCovered{nc},
		Checks: []model.CheckResult{{Name: "agent", OK: true}},
	})
	out := buf.String()
	if !strings.Contains(out, `"verdict":"FAILED"`) || !strings.Contains(out, nc.LogLine()) {
		t.Fatal(out)
	}
	for _, k := range []string{"guest", "reason", "restore_s", "boot_s", "failed_units", "ignored_units", "checks"} {
		if !strings.Contains(out, `"`+k+`"`) {
			t.Errorf("missing attr %s: %s", k, out)
		}
	}
}

func TestNewLoggerStdoutOnly(t *testing.T) {
	var out bytes.Buffer
	NewLogger(&out, nil).Info("hello")
	if !strings.Contains(out.String(), "hello") {
		t.Fatal(out.String())
	}
}

func TestPruneRunLogs(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 33; i++ {
		p := filepath.Join(dir, fmt.Sprintf("run-%02d.log", i))
		os.WriteFile(p, nil, 0o644)
		os.Chtimes(p, base.Add(time.Duration(i)*time.Minute), base.Add(time.Duration(i)*time.Minute))
	}
	os.WriteFile(filepath.Join(dir, "other.txt"), nil, 0o644)
	if err := PruneRunLogs(dir, 30); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 31 {
		t.Fatalf("got %d entries", len(ents))
	}
	for i := 0; i < 3; i++ {
		if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("run-%02d.log", i))); err == nil {
			t.Errorf("run-%02d should be pruned", i)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "other.txt")); err != nil {
		t.Error("other.txt removed")
	}
}

func TestOpenRunLog(t *testing.T) {
	f, err := OpenRunLog(t.TempDir(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if filepath.Base(f.Name()) != "run-abc.log" {
		t.Fatal(f.Name())
	}
}

func TestTemplateData(t *testing.T) {
	s := model.RunSummary{
		ID:      "r1",
		Started: time.Date(2026, 10, 19, 2, 0, 0, 0, time.UTC),
		Ended:   time.Date(2026, 10, 19, 3, 2, 3, 0, time.UTC),
		Results: []model.GuestResult{
			{Name: "a", Verdict: model.Booted, Reason: "no agent", Backup: model.Backup{Key: model.GuestKey{Kind: model.KindLXC, VMID: 5}},
				NotCovered: []model.NotCovered{{Unit: "lxc/5", Piece: model.MissingPiece{Kind: "network-storage", Path: "/mnt/x"}}}},
			{Name: "b", Verdict: model.Proven},
		},
	}
	d := TemplateData(s, "pve1")
	if d["node"] != "pve1" || d["run-id"] != "r1" || d["started"] != "2026-10-19T02:00:00Z" || d["elapsed"] != "1h2m3s" {
		t.Fatal(d)
	}
	if n := len(d["booted"].([]map[string]any)); n != 1 {
		t.Fatal(n)
	}
	if d["counts"].(map[string]int)["BOOTED"] != 1 {
		t.Fatal(d["counts"])
	}
	if len(d["not-covered"].([]string)) != 1 || len(d["guests"].([]map[string]any)) != 2 {
		t.Fatal(d)
	}
	if d["summary"] != "1 proven, 0 failed, 1 booted, 0 skipped, 0 error" {
		t.Fatal(d["summary"])
	}
	if d["run-error"] != "" {
		t.Fatal(d["run-error"])
	}
	if _, err := json.Marshal(d); err != nil {
		t.Fatal(err)
	}
}

func TestSend(t *testing.T) {
	var gotCmd string
	var gotIn []byte
	f := &sshx.Fake{Handler: func(cmd string, stdin []byte) (sshx.Result, error) {
		gotCmd, gotIn = cmd, stdin
		return sshx.Result{}, nil
	}}
	n := &PVENotifier{SSH: f, Node: "pve1"}
	if err := n.Send(context.Background(), failedSummary()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(gotCmd, "perl -e 'use PVE::Notify;") {
		t.Fatal(gotCmd)
	}
	var in struct {
		Severity string
		Fields   map[string]string
		Data     map[string]any
	}
	if err := json.Unmarshal(gotIn, &in); err != nil {
		t.Fatal(err)
	}
	if in.Severity != "error" || in.Fields["type"] != "pbp" || in.Fields["hostname"] != "pve1" || in.Data["node"] != "pve1" {
		t.Fatalf("%s", gotIn)
	}
}

func TestSendError(t *testing.T) {
	f := &sshx.Fake{Handler: func(string, []byte) (sshx.Result, error) {
		return sshx.Result{ExitCode: 255, Stderr: "  no such module\n"}, nil
	}}
	err := (&PVENotifier{SSH: f, Node: "n"}).Send(context.Background(), failedSummary())
	if err == nil || !strings.Contains(err.Error(), "no such module") {
		t.Fatal(err)
	}
	f.Handler = func(string, []byte) (sshx.Result, error) { return sshx.Result{}, fmt.Errorf("down") }
	if err := (&PVENotifier{SSH: f}).Send(context.Background(), failedSummary()); err == nil {
		t.Fatal("want transport error")
	}
}

func TestEnsureTemplatesWritesWhenMissing(t *testing.T) {
	writes := map[string]string{}
	var cmds []string
	f := &sshx.Fake{Handler: func(cmd string, stdin []byte) (sshx.Result, error) {
		cmds = append(cmds, cmd)
		if strings.Contains(cmd, "cat >") {
			writes[cmd] = string(stdin)
			return sshx.Result{}, nil
		}
		if strings.HasPrefix(cmd, "cat ") {
			return sshx.Result{ExitCode: 1}, nil
		}
		return sshx.Result{}, nil
	}}
	if err := (&PVENotifier{SSH: f, Node: "n"}).EnsureTemplates(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(writes) != 2 {
		t.Fatalf("writes: %v", cmds)
	}
	joined := strings.Join(cmds, "\n")
	for _, p := range []string{"/etc/pve/notification-templates/default/pbp-subject.txt.hbs", "/etc/pve/notification-templates/default/pbp-body.txt.hbs"} {
		if !strings.Contains(joined, p) {
			t.Errorf("missing %s", p)
		}
	}
	if !strings.Contains(joined, "mkdir -p") {
		t.Error("no mkdir")
	}
	for _, c := range writes {
		if !strings.Contains(c, "{{") {
			t.Error("template content not written")
		}
	}
}

func TestEnsureTemplatesSkipsWhenIdentical(t *testing.T) {
	f := &sshx.Fake{}
	f.Handler = func(cmd string, _ []byte) (sshx.Result, error) {
		if !strings.HasPrefix(cmd, "cat ") || strings.Contains(cmd, "cat >") {
			t.Errorf("unexpected write: %s", cmd)
		}
		name := "templates/pbp-body.txt.hbs"
		if strings.Contains(cmd, "pbp-subject") {
			name = "templates/pbp-subject.txt.hbs"
		}
		b, _ := templateFS.ReadFile(name)
		return sshx.Result{Stdout: string(b)}, nil
	}
	if err := (&PVENotifier{SSH: f}).EnsureTemplates(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 2 {
		t.Fatal(f.Calls)
	}
}

// notifyHelpers are the helpers PVE's proxmox-notify registers (plus the
// handlebars built-ins). A data key with one of these names is parsed as a
// helper call: on PVE 9.2, {{duration}} failed to render with "Helper/Decorator
// duration param at index 0 required but not found", so no notification was
// sent at all.
var notifyHelpers = []string{
	"duration", "timestamp", "human-bytes", "table", "object", "relative-percentage",
	"heading-1", "heading-2", "verbatim", "verbatim-monospaced", "url", "json",
	"if", "unless", "each", "with", "lookup", "raw", "log",
	"eq", "ne", "gt", "gte", "lt", "lte", "and", "or", "not", "len",
}

func TestTemplateDataKeysAreNotNotifyHelpers(t *testing.T) {
	d := TemplateData(failedSummary(), "pve1")
	keys := []string{}
	for k := range d {
		keys = append(keys, k)
	}
	for _, g := range d["guests"].([]map[string]any) {
		for k := range g {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		for _, h := range notifyHelpers {
			if k == h {
				t.Errorf("template data key %q is a proxmox-notify helper name", k)
			}
		}
	}
}

var reMustache = regexp.MustCompile(`{{\s*([#/]?)([\w-]+)(?:\s+([\w-]+))?\s*}}`)

// Every variable a template uses must be provided by TemplateData (top level,
// or per guest inside #each guests/booted), so nothing renders empty.
func TestTemplatesUseOnlyProvidedKeys(t *testing.T) {
	d := TemplateData(failedSummary(), "pve1")
	guest := d["guests"].([]map[string]any)[0]
	for _, name := range templateFiles {
		b, err := templateFS.ReadFile("templates/" + name)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range reMustache.FindAllStringSubmatch(string(b), -1) {
			sigil, word, arg := m[1], m[2], m[3]
			switch {
			case sigil == "/":
			case sigil == "#":
				if _, ok := d[arg]; !ok {
					t.Errorf("%s: block %s %s: no such key", name, word, arg)
				}
			case word == "this":
			default:
				_, top := d[word]
				_, inGuest := guest[word]
				if !top && !inGuest {
					t.Errorf("%s: {{%s}}: no such key", name, word)
				}
			}
		}
	}
}

// PVE::Notify reports per-target failures (including a template that does
// not render) as "ERROR:" lines on stderr and still exits 0; that run sent
// nothing, so Send must fail. INFO lines are not errors.
func TestSendFailsOnNotifyErrorLines(t *testing.T) {
	stderr := "ERROR: failed to render override template 'pbp-body.txt.hbs': could not render notification template\n" +
		"ERROR: could not notify via target `mail-to-root`: failed to render notification template\n"
	f := &sshx.Fake{Handler: func(string, []byte) (sshx.Result, error) {
		return sshx.Result{Stderr: stderr}, nil
	}}
	err := (&PVENotifier{SSH: f, Node: "n"}).Send(context.Background(), failedSummary())
	if err == nil || !strings.Contains(err.Error(), "could not notify via target `mail-to-root`") {
		t.Fatal(err)
	}
	f.Handler = func(string, []byte) (sshx.Result, error) {
		return sshx.Result{Stderr: "INFO: skipping disabled matcher 'x'\n"}, nil
	}
	if err := (&PVENotifier{SSH: f, Node: "n"}).Send(context.Background(), failedSummary()); err != nil {
		t.Fatal(err)
	}
}
