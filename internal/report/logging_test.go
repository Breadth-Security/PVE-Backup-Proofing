package report

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
)

var edt = time.FixedZone("EDT", -4*3600)

// tickClock returns a clock that starts at start and advances one second per call.
func tickClock(start time.Time) func() time.Time {
	var mu sync.Mutex
	t := start.Add(-time.Second)
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t = t.Add(time.Second)
		return t
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func newRL(t *testing.T, root string) *RunLog {
	t.Helper()
	started := time.Date(2026, 10, 21, 2, 0, 0, 0, edt)
	rl, err := NewRunLog(root, started, "schedule", tickClock(started))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rl.Close() })
	return rl
}

func TestRunDirName(t *testing.T) {
	root := t.TempDir()
	rl := newRL(t, root)
	if !strings.HasSuffix(rl.Dir(), "2026-10-21_02-00-00_schedule") || !filepath.IsAbs(rl.Dir()) {
		t.Fatal(rl.Dir())
	}
	fi, err := os.Stat(rl.Dir())
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o750 {
		t.Fatalf("%v %v", fi, err)
	}
}

func TestOpenRunLogCollision(t *testing.T) {
	root := t.TempDir()
	a := newRL(t, root)
	b := newRL(t, root)
	c := newRL(t, root)
	if !strings.HasSuffix(b.Dir(), "_schedule-2") || !strings.HasSuffix(c.Dir(), "_schedule-3") {
		t.Fatal(b.Dir(), c.Dir())
	}
	a.Logger(&bytes.Buffer{}).Info("only in first")
	if strings.Contains(strings.Join(readLines(t, filepath.Join(b.Dir(), "run.log")), "\n"), "only in first") {
		t.Fatal("second run wrote into first run's folder")
	}
	if !strings.Contains(strings.Join(readLines(t, filepath.Join(a.Dir(), "run.log")), "\n"), "only in first") {
		t.Fatal("first run lost its record")
	}
}

func TestRunLogFormat(t *testing.T) {
	root := t.TempDir()
	start := time.Date(2026, 10, 21, 23, 59, 58, 0, edt)
	rl, err := NewRunLog(root, start, "schedule", tickClock(start))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rl.Close() }()
	var stdout bytes.Buffer
	l := rl.Logger(&stdout)
	l.Info("run started (schedule)")
	l.Info("second")
	l.Info("third")
	l.Warn("x", "err", "a b", "empty", "", "plain", "v", "eq", "a=b", "q", `a"b`)
	l.Error("boom")
	l.Info("line one\nline two")
	l.Log(context.Background(), LevelDetail, "VM102 started (VM102.log)")
	l.WithGroup("g").With("a", 1).Info("grouped", "k", "v")

	got := readLines(t, filepath.Join(rl.Dir(), "run.log"))
	want := []string{
		"2026-10-21 23:59:58 EDT run started (schedule)",
		"23:59:59 second",
		"2026-10-22 00:00:00 EDT third",
		`00:00:01 WARN x err="a b" empty="" plain=v eq="a=b" q="a\"b"`,
		"00:00:02 ERROR boom",
		"00:00:03 line one",
		"    line two",
		"00:00:04 VM102 started (VM102.log)",
		"00:00:05 grouped g.a=1 g.k=v",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	out := stdout.String()
	if strings.Contains(out, "VM102 started") || !strings.Contains(out, "run started (schedule)") || !strings.Contains(out, "grouped") {
		t.Fatalf("stdout: %s", out)
	}
	fi, err := os.Stat(filepath.Join(rl.Dir(), "run.log"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi, err)
	}
}

func TestRunLogGuestFiles(t *testing.T) {
	root := t.TempDir()
	rl := newRL(t, root)
	var stdout bytes.Buffer
	run := rl.Logger(&stdout)
	vm, ct := model.GuestKey{Kind: model.KindQEMU, VMID: 109}, model.GuestKey{Kind: model.KindLXC, VMID: 109}
	rl.Guest(vm).Info("a")
	rl.Guest(ct).Info("b")
	rl.Guest(vm).Error("destroy failed", "err", "x")
	run.Info("done")

	vmLines := strings.Join(readLines(t, filepath.Join(rl.Dir(), "VM109.log")), "\n")
	if !strings.Contains(vmLines, " a\n") || !strings.Contains(vmLines, "ERROR destroy failed err=x") || strings.Contains(vmLines, "VM109 destroy") {
		t.Fatal(vmLines)
	}
	if got := strings.Join(readLines(t, filepath.Join(rl.Dir(), "CT109.log")), "\n"); !strings.HasSuffix(got, " b") || strings.Contains(got, "destroy") {
		t.Fatal(got)
	}
	runLines := strings.Join(readLines(t, filepath.Join(rl.Dir(), "run.log")), "\n")
	if !strings.Contains(runLines, "VM109 destroy failed") {
		t.Fatal(runLines)
	}
	for _, ln := range strings.Split(runLines, "\n") {
		if strings.HasSuffix(ln, " a") || strings.HasSuffix(ln, " b") {
			t.Fatalf("guest record leaked into run.log: %q", ln)
		}
	}
	if !strings.Contains(stdout.String(), "VM109 destroy failed") || strings.Contains(stdout.String(), "msg=a") {
		t.Fatal(stdout.String())
	}
}

func TestRunLogGuestConcurrent(t *testing.T) {
	rl := newRL(t, t.TempDir())
	key := model.GuestKey{Kind: model.KindQEMU, VMID: 7}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				rl.Guest(key).Info("whole line of text")
			}
		}()
	}
	wg.Wait()
	lines := readLines(t, filepath.Join(rl.Dir(), "VM7.log"))
	if len(lines) != 400 {
		t.Fatal(len(lines))
	}
	for _, ln := range lines {
		if !strings.HasSuffix(ln, " whole line of text") {
			t.Fatalf("partial line %q", ln)
		}
	}
	ents, _ := os.ReadDir(rl.Dir())
	if len(ents) != 2 { // run.log and VM7.log
		t.Fatal(len(ents))
	}
}

func TestPruneRuns(t *testing.T) {
	root := t.TempDir()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	n := 0
	mk := func(name string, dir bool) string {
		p := filepath.Join(root, name)
		var err error
		if dir {
			err = os.Mkdir(p, 0o750)
		} else {
			err = os.WriteFile(p, []byte("x"), 0o600)
		}
		if err != nil {
			t.Fatal(err)
		}
		n++
		ts := base.Add(time.Duration(n) * time.Hour)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatal(err)
		}
		return p
	}
	keep := mk("2026-10-01_02-00-00_schedule", true)
	mk("run-a.log", false)
	mk("run-b.log", false)
	mk("2026-10-02_02-00-00_manual", true)
	mk("2026-10-03_02-00-00_schedule", true)
	mk("2026-10-04_02-00-00_schedule-2", true)
	mk("notes.txt", false)
	mk("lost+found", true)
	mk("2026-10-21_backup", true)
	mk("2026-10-21_02-00-00_daily", true)
	if err := PruneRuns(root, 3, keep); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(root)
	var got []string
	for _, e := range ents {
		got = append(got, e.Name())
	}
	want := []string{"2026-10-01_02-00-00_schedule", "2026-10-02_02-00-00_manual", "2026-10-03_02-00-00_schedule", "2026-10-04_02-00-00_schedule-2", "2026-10-21_02-00-00_daily", "2026-10-21_backup", "lost+found", "notes.txt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
	// retain < 1 behaves as 1: only the newest candidate (plus keep) remains.
	if err := PruneRuns(root, 0, keep); err != nil {
		t.Fatal(err)
	}
	ents, _ = os.ReadDir(root)
	got = nil
	for _, e := range ents {
		got = append(got, e.Name())
	}
	want = []string{"2026-10-01_02-00-00_schedule", "2026-10-04_02-00-00_schedule-2", "2026-10-21_02-00-00_daily", "2026-10-21_backup", "lost+found", "notes.txt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestFormat(t *testing.T) {
	for in, want := range map[time.Duration]string{
		1800 * time.Millisecond:                     "1.8s",
		44 * time.Second:                            "44s",
		4*time.Minute + 30*time.Second:              "4m30s",
		time.Hour + 33*time.Minute + 54*time.Second: "1h34m",
		2*time.Hour + 10*time.Second:                "2h0m",
	} {
		if got := Dur(in); got != want {
			t.Errorf("Dur(%v)=%q want %q", in, got, want)
		}
	}
	for in, want := range map[int64]string{
		32 << 30:   "32 GiB",
		4724464026: "4.4 GiB", // 4.4 GiB
		512 << 20:  "512 MiB",
		2048:       "2.0 KiB",
		100:        "100 B",
	} {
		if got := Bytes(in); got != want {
			t.Errorf("Bytes(%d)=%q want %q", in, got, want)
		}
	}
	for in, want := range map[time.Duration]string{
		20 * time.Hour:   "20h",
		45 * time.Minute: "45m",
		76 * time.Hour:   "3d 4h",
	} {
		if got := Age(in); got != want {
			t.Errorf("Age(%v)=%q want %q", in, got, want)
		}
	}
}

func TestNewRunLogRejectsUnknownTrigger(t *testing.T) {
	root := t.TempDir()
	started := time.Date(2026, 10, 21, 2, 0, 0, 0, time.UTC)
	if _, err := NewRunLog(root, started, "daily", tickClock(started)); err == nil {
		t.Fatal("want an error for trigger daily")
	}
	if ents, _ := os.ReadDir(root); len(ents) != 0 {
		t.Errorf("created %v", ents)
	}
}

// Guest-controlled text (a command's stderr, a Windows event) cannot put
// raw control characters into the log files: they could rewrite lines or
// send terminal escapes to whoever views the log (code review 5.12).
func TestRunLogEscapesControlCharacters(t *testing.T) {
	root := t.TempDir()
	start := time.Date(2026, 10, 21, 23, 59, 58, 0, edt)
	rl, err := NewRunLog(root, start, "schedule", tickClock(start))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rl.Close() }()
	l := rl.Logger(io.Discard)
	l.Info("check failed (exit 1: \x1b[2Jgone\rover\u0085x\x7f)\tok", "detail", "a\x1bb")
	got := readLines(t, filepath.Join(rl.Dir(), "run.log"))
	want := `2026-10-21 23:59:58 EDT check failed (exit 1: \x1b[2Jgone\x0dover\xc2\x85x\x7f)` + "\tok" + ` detail=a\x1bb`
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}
