package report

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
)

// NewLogger writes text records to stdout, for commands that are not runs.
func NewLogger(stdout io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(stdout, nil))
}

// LevelDetail marks records written to run.log but not to stdout.
const LevelDetail = slog.LevelDebug

// runDirRe matches the folder names NewRunLog creates; only these folders (and
// legacy run-*.log files) are pruning candidates.
var runDirRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2}_(schedule|manual)(-\d+)?$`)

// RunLog is one run's log folder: run.log plus one <Label>.log per guest.
type RunLog struct {
	dir string
	now func() time.Time
	run *fileSink

	mu     sync.Mutex
	runH   slog.Handler // run.log, plus stdout once Logger is called
	guests map[model.GuestKey]*slog.Logger
	sinks  []*fileSink
}

// NewRunLog creates the run's folder under root, named after started (in its
// own location) and trigger, with -2, -3, ... appended if the name is taken, and
// opens run.log in it. now supplies the time of every log line.
func NewRunLog(root string, started time.Time, trigger string, now func() time.Time) (*RunLog, error) {
	if trigger != "schedule" && trigger != "manual" {
		return nil, fmt.Errorf("report: unknown run trigger %q", trigger)
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("report: create log dir: %w", err)
	}
	base := started.Format("2006-01-02_15-04-05") + "_" + trigger
	dir := filepath.Join(root, base)
	for n := 2; ; n++ {
		err := os.Mkdir(dir, 0o750)
		if err == nil {
			break
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("report: create run folder: %w", err)
		}
		dir = filepath.Join(root, base+"-"+strconv.Itoa(n))
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("report: resolve run folder: %w", err)
	}
	r := &RunLog{dir: abs, now: now, guests: map[model.GuestKey]*slog.Logger{}}
	r.run = &fileSink{path: filepath.Join(abs, "run.log")}
	r.sinks = append(r.sinks, r.run)
	if err := r.run.open(); err != nil {
		return nil, err
	}
	r.runH = &lineHandler{sink: r.run, now: now, min: LevelDetail}
	return r, nil
}

// Dir is the absolute path of the run's folder.
func (r *RunLog) Dir() string { return r.dir }

// Logger writes every record to run.log and records at Info and above to
// stdout as slog text. Call it before Guest so guest warnings reach stdout.
func (r *RunLog) Logger(stdout io.Writer) *slog.Logger {
	text := slog.NewTextHandler(stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	h := slog.NewMultiHandler(&lineHandler{sink: r.run, now: r.now, min: LevelDetail}, text)
	r.mu.Lock()
	r.runH = h
	r.mu.Unlock()
	return slog.New(h)
}

// Guest returns the logger for one guest's file, opened on its first record.
// Records at Warn and above are also written to the run logger, prefixed with
// the guest's label. The same key always returns a logger for the same file.
func (r *RunLog) Guest(key model.GuestKey) *slog.Logger {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l, ok := r.guests[key]; ok {
		return l
	}
	s := &fileSink{path: filepath.Join(r.dir, key.Label()+".log")}
	r.sinks = append(r.sinks, s)
	l := slog.New(&guestHandler{
		file:  &lineHandler{sink: s, now: r.now, min: LevelDetail},
		run:   r.runH,
		label: key.Label(),
	})
	r.guests[key] = l
	return l
}

// Close closes every open file.
func (r *RunLog) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var first error
	for _, s := range r.sinks {
		if err := s.close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// PruneRuns keeps the newest retain run folders and legacy run-*.log files
// (by modtime, then name; retain below 1 counts as 1) and removes the rest,
// except keep, the current run's folder, which is never removed. Anything else
// in root is untouched.
func PruneRuns(root string, retain int, keep string) error {
	if retain < 1 {
		retain = 1
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("report: read log dir: %w", err)
	}
	type cand struct {
		name string
		mod  int64
	}
	var cands []cand
	for _, e := range ents {
		n := e.Name()
		isRun := e.IsDir() && runDirRe.MatchString(n)
		isLegacy := !e.IsDir() && strings.HasPrefix(n, "run-") && strings.HasSuffix(n, ".log")
		if !isRun && !isLegacy {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		cands = append(cands, cand{n, info.ModTime().UnixNano()})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].mod != cands[j].mod {
			return cands[i].mod > cands[j].mod
		}
		return cands[i].name > cands[j].name
	})
	if len(cands) <= retain {
		return nil
	}
	keepName := filepath.Base(keep)
	var firstErr error
	for _, c := range cands[retain:] {
		if c.name == keepName {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, c.name)); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("report: prune %s: %w", c.name, err)
		}
	}
	return firstErr
}

// fileSink is one log file shared by every handler derived from it. It opens
// the file on first use and writes each line whole under mu.
type fileSink struct {
	path string

	mu       sync.Mutex
	f        *os.File
	lastDate string
}

func (s *fileSink) open() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.openLocked()
}

func (s *fileSink) openLocked() error {
	if s.f != nil {
		return nil
	}
	// #nosec G304 -- path is inside the run folder PBP created; names come from the run time, trigger and guest label
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("report: open %s: %w", filepath.Base(s.path), err)
	}
	s.f = f
	return nil
}

func (s *fileSink) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// lineHandler writes records as readable text lines: the first line of a file,
// and the first after the date changes, start with the full date and zone, the
// others with the time. Only WARN and ERROR print a level. The time comes from
// now, not from the record.
type lineHandler struct {
	sink   *fileSink
	now    func() time.Time
	min    slog.Level
	attrs  []slog.Attr // already prefixed with their groups
	groups []string
}

func (h *lineHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.min }

func (h *lineHandler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(append([]slog.Attr(nil), h.attrs...), prefixAttrs(h.groups, as)...)
	return &c
}

func (h *lineHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.groups = append(append([]string(nil), h.groups...), name)
	return &c
}

func prefixAttrs(groups []string, as []slog.Attr) []slog.Attr {
	if len(groups) == 0 {
		return as
	}
	p := strings.Join(groups, ".") + "."
	out := make([]slog.Attr, len(as))
	for i, a := range as {
		a.Key = p + a.Key
		out[i] = a
	}
	return out
}

func (h *lineHandler) Handle(_ context.Context, rec slog.Record) error {
	var b strings.Builder
	switch {
	case rec.Level >= slog.LevelError:
		b.WriteString("ERROR ")
	case rec.Level >= slog.LevelWarn:
		b.WriteString("WARN ")
	}
	b.WriteString(strings.ReplaceAll(printable(rec.Message), "\n", "\n    "))
	for _, a := range h.attrs {
		writeAttr(&b, "", a)
	}
	group := strings.Join(h.groups, ".")
	rec.Attrs(func(a slog.Attr) bool {
		writeAttr(&b, group, a)
		return true
	})
	b.WriteByte('\n')

	s := h.sink
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.openLocked(); err != nil {
		return err
	}
	t := h.now()
	stamp := t.Format("15:04:05 ")
	if date := t.Format("2006-01-02"); date != s.lastDate {
		stamp = t.Format("2006-01-02 15:04:05 MST ")
		s.lastDate = date
	}
	_, err := s.f.WriteString(stamp + b.String())
	return err
}

func writeAttr(b *strings.Builder, prefix string, a slog.Attr) {
	v := a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	key := a.Key
	if prefix != "" && key != "" {
		key = prefix + "." + key
	}
	if v.Kind() == slog.KindGroup {
		for _, ga := range v.Group() {
			writeAttr(b, key, ga)
		}
		return
	}
	val := printable(v.String())
	if val == "" || strings.ContainsAny(val, " =\"") {
		val = strconv.Quote(val)
	}
	b.WriteString(" " + key + "=" + val)
}

// guestHandler writes to the guest's file and forwards Warn and above, with
// the guest label in front of the message, to the run logger.
type guestHandler struct {
	file  slog.Handler
	run   slog.Handler
	label string
}

func (g *guestHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return g.file.Enabled(ctx, l)
}

func (g *guestHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &guestHandler{file: g.file.WithAttrs(as), run: g.run.WithAttrs(as), label: g.label}
}

func (g *guestHandler) WithGroup(name string) slog.Handler {
	return &guestHandler{file: g.file.WithGroup(name), run: g.run.WithGroup(name), label: g.label}
}

func (g *guestHandler) Handle(ctx context.Context, rec slog.Record) error {
	err := g.file.Handle(ctx, rec)
	if rec.Level >= slog.LevelWarn && g.run.Enabled(ctx, rec.Level) {
		fwd := slog.NewRecord(rec.Time, rec.Level, g.label+" "+rec.Message, rec.PC)
		rec.Attrs(func(a slog.Attr) bool { fwd.AddAttrs(a); return true })
		if e := g.run.Handle(ctx, fwd); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// printable escapes the control characters in s as \xNN (the bytes of a C1
// character), keeping newlines and tabs. Messages carry guest-controlled
// text, such as a command's stderr; raw, it could rewrite log lines or send
// terminal escapes to whoever views the log (code review 5.12).
func printable(s string) string {
	if !strings.ContainsFunc(s, isControl) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if !isControl(r) {
			b.WriteRune(r)
			continue
		}
		var buf [utf8.UTFMax]byte
		for _, c := range buf[:utf8.EncodeRune(buf[:], r)] {
			fmt.Fprintf(&b, "\\x%02x", c)
		}
	}
	return b.String()
}

// isControl reports whether r is a C0 (other than newline and tab), DEL or
// C1 control character.
func isControl(r rune) bool {
	return (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r <= 0x9f)
}
