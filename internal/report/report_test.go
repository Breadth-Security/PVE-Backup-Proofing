package report

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sshx"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sshx/helperfake"
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
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, base.Add(time.Duration(i)*time.Minute), base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "other.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
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
	defer func() { _ = f.Close() }()
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

// notifier returns a PVENotifier whose helper answers with fn.
func notifier(node string, fn func(req nodeproto.Request, stdin []byte) (sshx.Result, error)) (*PVENotifier, *helperfake.Fake) {
	f := &helperfake.Fake{Handler: fn}
	return &PVENotifier{Helper: f.Helper(), Node: node}, f
}

// Send hands the payload to the helper's notify request; no script travels
// over SSH (the helper owns NotifyScript).
func TestSend(t *testing.T) {
	var gotIn []byte
	n, f := notifier("pve1", func(_ nodeproto.Request, stdin []byte) (sshx.Result, error) {
		gotIn = stdin
		return sshx.Result{}, nil
	})
	if err := n.Send(context.Background(), failedSummary()); err != nil {
		t.Fatal(err)
	}
	if ops := f.Ops(); !slices.Equal(ops, []string{"notify"}) {
		t.Fatalf("helper requests = %q, want one notify", ops)
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
	n, _ := notifier("n", func(req nodeproto.Request, _ []byte) (sshx.Result, error) {
		return helperfake.Fail(req, "notify: perl exited 2: Can't locate PVE/Notify.pm"), nil
	})
	err := n.Send(context.Background(), failedSummary())
	if err == nil || !strings.Contains(err.Error(), "Can't locate PVE/Notify.pm") || !errors.Is(err, sshx.ErrHelper) {
		t.Fatal(err)
	}
	n, _ = notifier("n", func(nodeproto.Request, []byte) (sshx.Result, error) { return sshx.Result{}, fmt.Errorf("down") })
	if err := n.Send(context.Background(), failedSummary()); err == nil {
		t.Fatal("want transport error")
	}
}

// PVE::Notify reports per-target failures (including a template that does
// not render) as "ERROR:" lines and still exits 0; the helper turns them
// into a failure (internal/node TestRealNotify), so Send fails with them.
func TestSendFailsOnNotifyErrorLines(t *testing.T) {
	n, _ := notifier("n", func(req nodeproto.Request, _ []byte) (sshx.Result, error) {
		return helperfake.Fail(req, "notify: ERROR: could not notify via target `mail-to-root`: failed to render notification template"), nil
	})
	err := n.Send(context.Background(), failedSummary())
	if err == nil || !strings.Contains(err.Error(), "could not notify via target `mail-to-root`") {
		t.Fatal(err)
	}
}

// templateHashes is the helper's templates reply for the embedded set.
func templateHashes() map[string]string {
	out := map[string]string{}
	for name, b := range Templates() {
		sum := sha256.Sum256(b)
		out[name] = hex.EncodeToString(sum[:])
	}
	return out
}

func templatesReply(m map[string]string) func(nodeproto.Request, []byte) (sshx.Result, error) {
	return func(req nodeproto.Request, _ []byte) (sshx.Result, error) {
		if req.Op != "templates" {
			return sshx.Result{ExitCode: 127}, nil
		}
		return helperfake.JSON(m), nil
	}
}

const installHint = `run "pbp node install-templates" as root on the node`

// CheckTemplates only reads: it asks the helper for the installed hashes
// and compares them with the embedded templates.
func TestCheckTemplatesIdentical(t *testing.T) {
	n, f := notifier("n", templatesReply(templateHashes()))
	if err := n.CheckTemplates(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ops := f.Ops(); !slices.Equal(ops, []string{"templates"}) {
		t.Fatalf("helper requests = %q, want one templates request", ops)
	}
}

// A missing or different template fails with every bad file named and the
// install instruction; nothing is written.
func TestCheckTemplatesMismatch(t *testing.T) {
	tests := []struct {
		name  string
		edit  func(map[string]string)
		names []string
	}{
		{name: "hash mismatch", edit: func(m map[string]string) { m["pbp-body.txt.hbs"] = strings.Repeat("0", 64) },
			names: []string{"pbp-body.txt.hbs"}},
		{name: "missing file", edit: func(m map[string]string) { m["pbp-subject.txt.hbs"] = "" },
			names: []string{"pbp-subject.txt.hbs"}},
		{name: "not reported", edit: func(m map[string]string) { delete(m, "pbp-subject.txt.hbs") },
			names: []string{"pbp-subject.txt.hbs"}},
		{name: "both", edit: func(m map[string]string) { m["pbp-subject.txt.hbs"] = ""; m["pbp-body.txt.hbs"] = "abc" },
			names: []string{"pbp-subject.txt.hbs", "pbp-body.txt.hbs"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := templateHashes()
			tc.edit(m)
			n, f := notifier("n", templatesReply(m))
			err := n.CheckTemplates(context.Background())
			if err == nil || !strings.Contains(err.Error(), installHint) {
				t.Fatalf("err = %v, want the install hint", err)
			}
			for _, name := range tc.names {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("err %q does not name %s", err, name)
				}
			}
			if ops := f.Ops(); !slices.Equal(ops, []string{"templates"}) {
				t.Errorf("helper requests = %q, want only the templates read", ops)
			}
		})
	}
}

// A templates request that fails is an error naming the cause; it is never
// a cue to write anything.
func TestCheckTemplatesReadFailure(t *testing.T) {
	n, f := notifier("n", func(req nodeproto.Request, _ []byte) (sshx.Result, error) {
		return helperfake.Fail(req, "template pbp-body.txt.hbs: permission denied"), nil
	})
	err := n.CheckTemplates(context.Background())
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("err = %v", err)
	}
	if ops := f.Ops(); !slices.Equal(ops, []string{"templates"}) {
		t.Errorf("helper requests = %q", ops)
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

// reTag matches one template tag, including the {{{triple}}} form.
var reTag = regexp.MustCompile(`{{{?[^{}]*}?}}`)

// templateProblems lists what is wrong with a template: a triple-stash tag, a
// tag with more than one argument (it would be parsed as a multi-argument
// helper call), or a variable TemplateData does not provide (top level, or
// per guest inside #each guests/booted), which would render empty.
func templateProblems(body string, d, guest map[string]any) []string {
	var problems []string
	for _, tag := range reTag.FindAllString(body, -1) {
		if strings.HasPrefix(tag, "{{{") {
			problems = append(problems, tag+": triple-stash tag")
			continue
		}
		f := strings.Fields(strings.TrimSuffix(strings.TrimPrefix(tag, "{{"), "}}"))
		if len(f) == 0 {
			problems = append(problems, tag+": empty tag")
			continue
		}
		sigil, word, args := "", f[0], f[1:]
		if c := word[0]; c == '#' || c == '/' {
			sigil, word = word[:1], word[1:]
		}
		switch {
		case len(args) > 1:
			problems = append(problems, tag+": more than one argument")
		case sigil == "/" && len(args) > 0:
			problems = append(problems, tag+": closing tag with argument")
		case sigil == "/":
		case sigil == "#" && len(args) == 0:
			problems = append(problems, tag+": block without argument")
		case sigil == "#":
			if _, ok := d[args[0]]; !ok {
				problems = append(problems, tag+": no such key "+args[0])
			}
		case len(args) > 0:
			problems = append(problems, tag+": helper call with argument")
		case word == "this":
		default:
			_, top := d[word]
			_, inGuest := guest[word]
			if !top && !inGuest {
				problems = append(problems, tag+": no such key")
			}
		}
	}
	return problems
}

// Every template tag must be a plain provided variable or a block over a
// provided list.
func TestTemplatesUseOnlyProvidedKeys(t *testing.T) {
	d := TemplateData(failedSummary(), "pve1")
	guest := d["guests"].([]map[string]any)[0]
	for name, b := range Templates() {
		for _, p := range templateProblems(string(b), d, guest) {
			t.Errorf("%s: %s", name, p)
		}
	}
}

func TestTemplateGuardCatchesBadTags(t *testing.T) {
	d := TemplateData(failedSummary(), "pve1")
	guest := d["guests"].([]map[string]any)[0]
	for body, want := range map[string]string{
		"{{{reason}}}":          "triple-stash",
		"{{table guests name}}": "more than one argument",
		"{{#each guests name}}": "more than one argument",
		"{{duration started}}":  "helper call with argument",
		"{{nope}}":              "no such key",
		"{{#each nope}}":        "no such key nope",
		"{{{ reason }}}":        "triple-stash",
		"{{this x}}":            "helper call with argument",
		"{{/each junk}}":        "closing tag with argument",
		"{{#each}}":             "block without argument",
	} {
		got := templateProblems(body, d, guest)
		if len(got) != 1 || !strings.Contains(got[0], want) {
			t.Errorf("%q: problems %q, want one containing %q", body, got, want)
		}
	}
	if got := templateProblems("{{#each guests}}\n {{guest}} {{this}}\n{{/each}} {{summary}}", d, guest); len(got) != 0 {
		t.Errorf("valid template flagged: %q", got)
	}
}

// The BOOTED section names what BOOTED means (the agent may be present but
// unable to run commands) and shows each guest's reason.
func TestBodyBootedSection(t *testing.T) {
	b, err := templateFS.ReadFile("templates/pbp-body.txt.hbs")
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	const title = "Guests PBP could not look inside (booted only):"
	i := strings.Index(body, title)
	if i < 0 {
		t.Fatalf("body has no %q section", title)
	}
	section := body[i:]
	end := strings.Index(section, "{{/each}}")
	if !strings.HasPrefix(section[len(title):], "\n{{#each booted}}") || end < 0 || !strings.Contains(section[:end], "{{reason}}") {
		t.Errorf("booted section does not list each reason:\n%s", section)
	}
	if strings.Contains(body, "without a working guest agent") {
		t.Error("old booted title still present")
	}
}

func TestEnvironmentErrorsInTemplateData(t *testing.T) {
	s := failedSummary()
	d := TemplateData(s, "pve1")
	if got, ok := d["environment-errors"].([]string); !ok || len(got) != 0 {
		t.Fatalf("empty summary: %#v", d["environment-errors"])
	}
	s.EnvironmentErrors = []string{"VM 105 (web) is tagged pbp but outside the PBP range 990000–990049; not touched"}
	d = TemplateData(s, "pve1")
	if got := d["environment-errors"].([]string); len(got) != 1 || got[0] != s.EnvironmentErrors[0] {
		t.Fatalf("got %#v", got)
	}
	if _, err := json.Marshal(d); err != nil {
		t.Fatal(err)
	}
}

// The body shows an "Environment errors" section only when there are any.
func TestBodyEnvironmentErrorsSection(t *testing.T) {
	b, err := templateFS.ReadFile("templates/pbp-body.txt.hbs")
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	const want = "{{#if environment-errors}}\n\nEnvironment errors:\n{{#each environment-errors}}\n  {{this}}\n{{/each}}\n{{/if}}"
	if !strings.Contains(body, want) {
		t.Errorf("body lacks the guarded Environment errors section:\n%s", body)
	}
}
