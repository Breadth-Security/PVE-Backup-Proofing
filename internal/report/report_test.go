package report

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
	NewLogger(&out).Info("hello")
	if !strings.Contains(out.String(), "hello") {
		t.Fatal(out.String())
	}
}

func sectionsSummary() model.RunSummary {
	edt := time.FixedZone("EDT", -4*3600)
	started := time.Date(2026, 10, 21, 2, 0, 0, 0, edt)
	bk := func(kind model.GuestKind, id int) model.Backup {
		return model.Backup{
			Key:  model.GuestKey{Kind: kind, VMID: id},
			Time: time.Date(2026, 10, 20, 6, 0, 0, 0, time.UTC), // 02:00 EDT
		}
	}
	return model.RunSummary{
		ID:      "r1",
		Trigger: "schedule",
		Started: started,
		Ended:   started.Add(time.Hour + 33*time.Minute + 54*time.Second),
		LogDir:  "/var/log/pbp/2026-10-21_02-00-00_schedule",
		Results: []model.GuestResult{
			{Backup: bk(model.KindQEMU, 102), Name: "web01", Verdict: model.Failed, Reason: "failed: a.service",
				NotCovered: []model.NotCovered{{Unit: "nfs-home.mount", Piece: model.MissingPiece{Kind: "network-storage", Path: "/nfs/home"}}}},
			{Backup: bk(model.KindLXC, 101), Name: "dns01", Verdict: model.Error, Reason: "restore failed"},
			{Backup: bk(model.KindQEMU, 117), Name: "win01", Verdict: model.Booted, Reason: "guest agent missing"},
			{Backup: bk(model.KindQEMU, 104), Verdict: model.Skipped, Reason: "insufficient node memory"},
			{Backup: bk(model.KindQEMU, 105), Verdict: model.Skipped, Reason: "insufficient node memory"},
			{Backup: bk(model.KindQEMU, 100), Verdict: model.Skipped, Reason: "already proven", AlreadyProven: true},
			{Backup: bk(model.KindQEMU, 108), Verdict: model.Proven},
			{Backup: bk(model.KindLXC, 109), Verdict: model.Proven},
		},
	}
}

func TestTemplateDataSections(t *testing.T) {
	d := TemplateData(sectionsSummary(), "pve1")
	want := map[string]any{
		"node":     "pve1",
		"run-id":   "r1",
		"headline": "PBP run on pve1, Wed 2026-10-21 02:00 EDT (scheduled), 1h34m",
		"logs":     "/var/log/pbp/2026-10-21_02-00-00_schedule/",
		"summary":  "1 failed, 1 error, 1 booted, 2 skipped · 2 proven",
		"mark":     "❌",
		"counts":   "✅ 2 proven · ❌ 1 failed · ⚠️ 1 error · ⚠️ 1 booted · ⏭️ 3 skipped",
		"proven":   "VM108, CT109",
		"failed":   []string{"VM102 web01 · backup Oct 20 02:00 · failed: a.service"},
		"errored":  []string{"CT101 dns01 · backup Oct 20 02:00 · restore failed"},
		"booted":   []string{"VM117 win01 · backup Oct 20 02:00 · guest agent missing"},
		"skipped":  []string{"insufficient node memory: VM104, VM105", "already proven: VM100"},
	}
	for k, v := range want {
		if fmt.Sprint(d[k]) != fmt.Sprint(v) {
			t.Errorf("%s = %#v, want %#v", k, d[k], v)
		}
	}
	if nc := d["not-covered"].([]string); len(nc) != 1 || !strings.HasPrefix(nc[0], "VM102: ") {
		t.Errorf("not-covered = %q", nc)
	}
	for _, k := range []string{"run-error", "environment-errors"} {
		if _, ok := d[k]; !ok {
			t.Errorf("missing key %s", k)
		}
	}
	if len(d) != len(DataKeys) {
		t.Errorf("%d keys: %v", len(d), d)
	}
	if _, err := json.Marshal(d); err != nil {
		t.Fatal(err)
	}
}

func TestTemplateDataNoProblemsNoLogs(t *testing.T) {
	s := sectionsSummary()
	s.LogDir = ""
	s.Results = s.Results[6:]
	d := TemplateData(s, "pve1")
	if d["logs"] != "" || d["summary"] != "all 2 proven" || d["proven"] != "VM108, CT109" {
		t.Fatal(d)
	}
	s.Results = nil
	if d := TemplateData(s, "pve1"); d["proven"] != "" {
		t.Fatalf("proven = %q", d["proven"])
	}
}

func TestSubjectSummary(t *testing.T) {
	res := func(v model.Verdict, mod func(*model.GuestResult)) model.GuestResult {
		r := model.GuestResult{Verdict: v}
		if mod != nil {
			mod(&r)
		}
		return r
	}
	proven := func(n int) []model.GuestResult {
		var out []model.GuestResult
		for i := 0; i < n; i++ {
			out = append(out, res(model.Proven, nil))
		}
		return out
	}
	already := res(model.Skipped, func(r *model.GuestResult) { r.AlreadyProven = true })
	tests := []struct {
		name string
		s    model.RunSummary
		want string
	}{
		{"all proven", model.RunSummary{Results: proven(13)}, "all 13 proven"},
		{"proven and already proven", model.RunSummary{Results: append(proven(2), already)}, "all 2 proven"},
		{"only already proven", model.RunSummary{Results: []model.GuestResult{already}}, "nothing to proof"},
		{"run error", model.RunSummary{RunError: "boom"}, "run error · 0 proven"},
		{"run error with failure", model.RunSummary{RunError: "boom", Results: append(proven(1), res(model.Failed, nil))}, "run error, 1 failed · 1 proven"},
		{"template skip", model.RunSummary{Results: []model.GuestResult{res(model.Skipped, func(r *model.GuestResult) { r.Template = true })}}, "nothing to proof"},
		{"excluded skip", model.RunSummary{Results: []model.GuestResult{res(model.Proven, nil), res(model.Skipped, func(r *model.GuestResult) { r.Excluded = true })}}, "all 1 proven"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := TemplateData(tc.s, "pve1")["summary"]; got != tc.want {
				t.Errorf("summary = %q, want %q", got, tc.want)
			}
		})
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
// helper call), or a variable TemplateData does not provide, which would
// render empty. Inside #each the sections are lists of strings, so only
// {{this}} and top-level keys are valid.
func templateProblems(body string, d map[string]any) []string {
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
			if _, ok := d[word]; !ok {
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
	for name, b := range Templates() {
		for _, p := range templateProblems(string(b), d) {
			t.Errorf("%s: %s", name, p)
		}
	}
}

func TestTemplateGuardCatchesBadTags(t *testing.T) {
	d := TemplateData(failedSummary(), "pve1")
	for body, want := range map[string]string{
		"{{{summary}}}":         "triple-stash",
		"{{table failed name}}": "more than one argument",
		"{{#each failed name}}": "more than one argument",
		"{{duration headline}}": "helper call with argument",
		"{{nope}}":              "no such key",
		"{{#each nope}}":        "no such key nope",
		"{{{ summary }}}":       "triple-stash",
		"{{this x}}":            "helper call with argument",
		"{{/each junk}}":        "closing tag with argument",
		"{{#each}}":             "block without argument",
	} {
		got := templateProblems(body, d)
		if len(got) != 1 || !strings.Contains(got[0], want) {
			t.Errorf("%q: problems %q, want one containing %q", body, got, want)
		}
	}
	if got := templateProblems("{{#each failed}}\n {{this}}\n{{/each}} {{summary}}", d); len(got) != 0 {
		t.Errorf("valid template flagged: %q", got)
	}
}

// The body leads with problems, each section guarded so it only shows when
// it has content, and the ordering is: run error, environment errors, FAILED,
// ERROR, BOOTED, SKIPPED, PROVEN, Not covered.
func TestBodySectionsInOrder(t *testing.T) {
	b, err := templateFS.ReadFile("templates/pbp-body.txt.hbs")
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	marks := []string{
		"{{#if run-error}}", "{{#if environment-errors}}",
		"{{#if failed}}", "FAILED: the backup has a problem",
		"{{#if errored}}", "ERROR: PBP could not finish; says nothing about the backup",
		"{{#if booted}}", "BOOTED: started, not looked inside",
		"{{#if skipped}}", "SKIPPED",
		"{{#if proven}}", "PROVEN",
		"{{#if not-covered}}", "Not covered by backup",
	}
	last := -1
	for _, m := range marks {
		i := strings.Index(body[last+1:], m)
		if i < 0 {
			t.Fatalf("body lacks %q after offset %d:\n%s", m, last, body)
		}
		last += 1 + i
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

// A run with very many problems, or a guest with a huge reason, still
// notifies: each line is clipped, and the lists are shortened until the
// payload fits the helper's 1 MiB limit (code review I14).
func TestSendFitsHelperLimit(t *testing.T) {
	var gotIn []byte
	n, _ := notifier("pve1", func(_ nodeproto.Request, stdin []byte) (sshx.Result, error) {
		gotIn = stdin
		return sshx.Result{}, nil
	})
	s := failedSummary()
	s.Results = nil
	for i := range 3000 {
		s.Results = append(s.Results, model.GuestResult{
			Backup:  model.Backup{Key: model.GuestKey{Kind: model.KindQEMU, VMID: 1000 + i}},
			Verdict: model.Failed,
			Reason:  "failed: " + strings.Repeat("x", 2000),
		})
	}
	if err := n.Send(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if len(gotIn) > 1<<20 {
		t.Fatalf("payload %d bytes, over the helper's 1 MiB", len(gotIn))
	}
	var in struct{ Data map[string]any }
	if err := json.Unmarshal(gotIn, &in); err != nil {
		t.Fatal(err)
	}
	failed, _ := in.Data["failed"].([]any)
	if len(failed) < 2 || len(failed) > 101 {
		t.Fatalf("failed list has %d lines", len(failed))
	}
	if first := failed[0].(string); len(first) > 600 || !strings.HasSuffix(first, "…") {
		t.Errorf("line not clipped: %d bytes", len(first))
	}
	if last := failed[len(failed)-1].(string); !strings.HasPrefix(last, "… and ") || !strings.HasSuffix(last, " more; see the run log") {
		t.Errorf("last line %q", last)
	}
}

// The node helper accepts only DataKeys in a notify payload: TemplateData
// must use exactly those (code review 2.4).
func TestTemplateDataKeysAreDataKeys(t *testing.T) {
	got := slices.Sorted(maps.Keys(TemplateData(failedSummary(), "pve1")))
	want := slices.Sorted(slices.Values(DataKeys))
	if !slices.Equal(got, want) {
		t.Errorf("TemplateData keys %q, DataKeys %q", got, want)
	}
}

// Even PBP's own text cannot push the notification over the helper's limit:
// a huge run error leaves the essentials (#48 review).
func TestSendFitsWithHugeRunError(t *testing.T) {
	var gotIn []byte
	n, _ := notifier("pve1", func(_ nodeproto.Request, stdin []byte) (sshx.Result, error) {
		gotIn = stdin
		return sshx.Result{}, nil
	})
	s := failedSummary()
	s.RunError = strings.Repeat("x", 2<<20)
	if err := n.Send(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if len(gotIn) > 1<<20 {
		t.Fatalf("payload %d bytes", len(gotIn))
	}
	var in struct{ Data map[string]any }
	if err := json.Unmarshal(gotIn, &in); err != nil {
		t.Fatal(err)
	}
	if in.Data["headline"] == "" || !strings.HasSuffix(in.Data["run-error"].(string), "…") {
		t.Errorf("essentials missing: %v", in.Data["headline"])
	}
}

func TestCountsLineAndMarks(t *testing.T) {
	if got := CountsLine(nil); got != "nothing proofed" {
		t.Errorf("empty = %q", got)
	}
	if got := CountsLine(map[model.Verdict]int{model.Proven: 13}); got != "✅ 13 proven" {
		t.Errorf("proven only = %q", got)
	}
	for sev, want := range map[string]string{"info": "✅", "warning": "⚠️", "error": "❌"} {
		if got := SeverityMark(sev); got != want {
			t.Errorf("SeverityMark(%s) = %q, want %q", sev, got, want)
		}
	}
}
