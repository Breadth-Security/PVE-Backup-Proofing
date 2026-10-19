// Command pbp is the Proxmox Backup Proofing daemon and CLI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/robfig/cron/v3"

	"git.breadthanalytics.com/tdiana/pbv/internal/config"
	"git.breadthanalytics.com/tdiana/pbv/internal/daemon"
	"git.breadthanalytics.com/tdiana/pbv/internal/fsutil"
	"git.breadthanalytics.com/tdiana/pbv/internal/health"
	"git.breadthanalytics.com/tdiana/pbv/internal/model"
	"git.breadthanalytics.com/tdiana/pbv/internal/pve"
	"git.breadthanalytics.com/tdiana/pbv/internal/report"
	"git.breadthanalytics.com/tdiana/pbv/internal/run"
	"git.breadthanalytics.com/tdiana/pbv/internal/sshx"
	"git.breadthanalytics.com/tdiana/pbv/internal/state"
	"git.breadthanalytics.com/tdiana/pbv/internal/target"
)

const usage = "usage: pbp daemon|run [--guest VMID]|check|health [--config PATH]\n       pbp version\n"

// version is set at build time: -ldflags "-X main.version=...".
var version = "dev"

// Exit codes.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
	exitBusy  = 3
)

const msgBusy = "a run is already in progress"

// A failed write to stdout or stderr cannot be reported anywhere else, so
// these console writers deliberately drop the error.
func fprint(w io.Writer, a ...any)                 { _, _ = fmt.Fprint(w, a...) }
func fprintf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
func fprintln(w io.Writer, a ...any)               { _, _ = fmt.Fprintln(w, a...) }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	code := newApp(os.Stdout, os.Stderr).main(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

// app is the CLI with its outside world injectable for tests.
type app struct {
	stdout, stderr io.Writer
	now            func() time.Time
	// connect builds the node-facing dependencies for one run or check.
	connect func(cfg config.Config) wiring
}

func newApp(stdout, stderr io.Writer) *app {
	return &app{stdout: stdout, stderr: stderr, now: time.Now, connect: connectReal}
}

// wiring is what talks to the node. apiErr and sshErr record setup failures;
// target and notifier are then stand-ins that fail, so a run still reaches
// its error path, attempts a notification and records outcome "error".
type wiring struct {
	api            pve.API
	target         target.Target
	notifier       report.Notifier
	apiErr, sshErr error
	close          func()
}

// setupErr joins the setup failures (nil if none).
func (w wiring) setupErr() error { return errors.Join(w.apiErr, w.sshErr) }

func connectReal(cfg config.Config) wiring {
	var api pve.API
	c, apiErr := pve.New(cfg.Target.API)
	if apiErr == nil {
		api = c
	}
	var ssh sshx.Runner
	cl, sshErr := sshx.Dial(cfg.Target.SSH)
	closeFn := func() {}
	if sshErr == nil {
		ssh, closeFn = cl, func() { _ = cl.Close() }
	}
	w := assemble(cfg, api, apiErr, ssh, sshErr)
	w.close = closeFn
	return w
}

// assemble builds the wiring from a possibly failed API client and SSH
// connection.
func assemble(cfg config.Config, api pve.API, apiErr error, ssh sshx.Runner, sshErr error) wiring {
	w := wiring{api: api, apiErr: apiErr, sshErr: sshErr, close: func() {}}
	if apiErr != nil {
		apiErr = fmt.Errorf("pve api: %w", apiErr)
		w.apiErr = apiErr
	}
	if sshErr != nil {
		sshErr = fmt.Errorf("ssh %s: %w", cfg.Target.SSH.Host, sshErr)
		w.sshErr = sshErr
		ssh = downRunner{sshErr}
	}
	w.target = target.NewExistingNode(cfg, api, ssh)
	if err := w.setupErr(); err != nil {
		w.target = unreachable{Target: w.target, err: err}
	}
	w.notifier = &report.PVENotifier{SSH: ssh, Node: cfg.Target.Node}
	return w
}

// downRunner fails every command with the error that stopped the SSH dial.
type downRunner struct{ err error }

func (r downRunner) Run(context.Context, string, []byte) (sshx.Result, error) {
	return sshx.Result{}, r.err
}

// unreachable fails preflight with the setup error, so nothing else runs.
type unreachable struct {
	target.Target
	err error
}

func (u unreachable) Prepare(context.Context) error { return u.err }

func (a *app) main(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fprint(a.stderr, usage)
		return exitUsage
	}
	cmd := args[0]
	fs := flag.NewFlagSet("pbp "+cmd, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	cfgFlag := fs.String("config", "", "config file (default $PBP_CONFIG or /etc/pbp/config.yaml)")
	var guest *int
	switch cmd {
	case "run":
		guest = fs.Int("guest", 0, "proof only this guest's newest backup")
	case "daemon", "check", "health", "version":
	default:
		fprintf(a.stderr, "pbp: unknown subcommand %q\n%s", cmd, usage)
		return exitUsage
	}
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fprintf(a.stderr, "pbp %s: unexpected arguments %q\n%s", cmd, fs.Args(), usage)
		return exitUsage
	}

	if cmd == "version" {
		fprintf(a.stdout, "pbp %s\n%s\n", version, runtime.Version())
		return exitOK
	}
	if guest != nil && !guestValid(fs, *guest) {
		fprintf(a.stderr, "pbp run: --guest must be a positive integer VMID\n%s", usage)
		return exitUsage
	}

	path := config.ResolvePath(*cfgFlag)
	cfg, err := config.Load(path)
	if err != nil {
		if cmd == "health" {
			fprintln(a.stdout, err)
		} else {
			fprintf(a.stderr, "pbp %s: %v\n", cmd, err)
		}
		return exitFail
	}
	switch cmd {
	case "daemon":
		return a.daemon(ctx, cfg)
	case "run":
		return a.runNow(ctx, cfg, *guest)
	case "check":
		return a.check(ctx, cfg, path)
	default:
		return a.health(cfg)
	}
}

// guestValid reports whether --guest was left unset or given a positive VMID.
func guestValid(fs *flag.FlagSet, guest int) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == "guest" })
	return !set || guest > 0
}

func statePath() string   { return filepath.Join(daemon.StateDir(), "state.json") }
func startedPath() string { return filepath.Join(daemon.StateDir(), "daemon.started") }

// lock takes the run lock for cmd, printing why not on failure.
func (a *app) lock(cmd string) (func(), int) {
	release, err := daemon.AcquireLock(daemon.LockPath())
	if errors.Is(err, daemon.ErrBusy) {
		fprintln(a.stderr, msgBusy)
		return nil, exitBusy
	}
	if err != nil {
		fprintf(a.stderr, "pbp %s: %v\n", cmd, err)
		return nil, exitFail
	}
	return release, exitOK
}

func (a *app) daemon(ctx context.Context, cfg config.Config) int {
	slog.SetDefault(report.NewLogger(a.stdout, nil))
	slog.Info("pbp daemon starting", "version", version, "go", runtime.Version())
	if err := writeStarted(startedPath(), a.now()); err != nil {
		slog.Error("record daemon start", "err", err)
	}
	err := daemon.Serve(ctx, cfg, func(ctx context.Context, trigger string) {
		a.runOnce(ctx, cfg, run.Options{Trigger: trigger})
	})
	if err != nil {
		slog.Error("daemon", "err", err)
		return exitFail
	}
	return exitOK
}

func (a *app) runNow(ctx context.Context, cfg config.Config, guest int) int {
	release, code := a.lock("run")
	if release == nil {
		return code
	}
	defer release()
	s := a.runOnce(ctx, cfg, run.Options{Trigger: "manual", OnlyVMID: guest})
	c := s.Counts()
	fprintf(a.stdout, "run %s: %s; proven %d, failed %d, booted %d, skipped %d, error %d\n",
		s.ID, s.Severity(), c[model.Proven], c[model.Failed], c[model.Booted], c[model.Skipped], c[model.Error])
	if s.RunError != "" {
		fprintf(a.stdout, "run error: %s\n", s.RunError)
	}
	if guest != 0 && s.RunError == "" && len(s.Results) == 0 {
		fprintf(a.stdout, "no backup found for guest %d\n", guest)
		return exitFail
	}
	if s.Severity() == "error" {
		return exitFail
	}
	return exitOK
}

// runOnce performs one run with real wiring. The caller holds the run lock.
// Logs go to stdout and to the run's own JSON log file.
func (a *app) runOnce(ctx context.Context, cfg config.Config, opt run.Options) model.RunSummary {
	opt.RunID = run.NewRunID(a.now(), opt.Trigger)
	f, logErr := report.OpenRunLog(cfg.Logging.Dir, opt.RunID)
	var log *slog.Logger
	if logErr != nil {
		log = report.NewLogger(a.stdout, nil)
		log.Error("open run log", "err", logErr)
	} else {
		defer func() { _ = f.Close() }()
		log = report.NewLogger(a.stdout, f)
	}
	defer func() {
		if logErr == nil {
			if err := report.PruneRunLogs(cfg.Logging.Dir, cfg.Logging.RetainRuns); err != nil {
				log.Error("prune run logs", "err", err)
			}
		}
	}()

	store, err := state.Open(statePath())
	if err != nil {
		log.Error("RUN NOT STARTED: cannot open state store; health will report unhealthy", "err", err)
		return model.RunSummary{ID: opt.RunID, Trigger: opt.Trigger, Started: a.now(), Ended: a.now(),
			RunError: "state: " + err.Error()}
	}
	w := a.connect(cfg)
	defer w.close()
	if w.sshErr != nil {
		log.Error("SSH TO THE NODE FAILED: the run fails and no notification can be sent; health will report unhealthy", "err", w.sshErr)
	}
	if w.apiErr != nil {
		log.Error("PVE API CLIENT FAILED: the run fails", "err", w.apiErr)
	}
	d := run.Deps{Cfg: cfg, API: w.api, Target: w.target, Store: store, Log: log, Now: a.now, Notifier: w.notifier}
	s := run.Run(ctx, d, opt)
	return s
}

func (a *app) check(ctx context.Context, cfg config.Config, path string) int {
	release, code := a.lock("check")
	if release == nil {
		return code
	}
	defer release()
	fprintf(a.stdout, "config %s: ok\n", path)
	ok := true
	line := func(name string, err error) {
		if err != nil {
			ok = false
			fprintf(a.stdout, "%s: FAIL: %v\n", name, err)
			return
		}
		fprintf(a.stdout, "%s: ok\n", name)
	}
	store, err := state.Open(statePath())
	line("state "+statePath(), err)
	w := a.connect(cfg)
	defer w.close()
	line("pve api "+cfg.Target.API.URL, w.apiErr)
	line("ssh "+cfg.Target.SSH.Host, w.sshErr)
	if !ok {
		return exitFail
	}
	d := run.Deps{Cfg: cfg, API: w.api, Target: w.target, Store: store,
		Log: report.NewLogger(a.stderr, nil), Now: a.now, Notifier: w.notifier}
	line("preflight (node version, isolated bridge, restore storage, notification templates, janitor, VMID range)",
		run.Preflight(ctx, d))
	if !ok {
		return exitFail
	}
	return exitOK
}

// health prints "ok" (exit 0) or why PBP is unhealthy (exit 1). It reads
// only the config and the state directory.
func (a *app) health(cfg config.Config) int {
	fail := func(reason string) int {
		fprintln(a.stdout, reason)
		return exitFail
	}
	sched, err := cron.ParseStandard(cfg.Schedule)
	if err != nil {
		return fail("schedule: " + err.Error())
	}
	st, err := readState(statePath())
	if err != nil {
		return fail(err.Error())
	}
	startedAt, err := readStarted(startedPath())
	if err != nil {
		return fail(err.Error())
	}
	r := cfg.Restore
	limits := health.RunLimits{MaxRun: r.MaxRunDuration, BootTimeout: r.BootTimeout, ProbeTimeout: r.ProbeTimeout}
	if ok, reason := health.Evaluate(st, a.now(), sched, limits, startedAt); !ok {
		return fail(reason)
	}
	fprintln(a.stdout, "ok")
	return exitOK
}

// writeStarted records the daemon start time atomically, so health never
// reads a half-written file.
func writeStarted(path string, t time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, []byte(t.Format(time.RFC3339)+"\n"), 0o600)
}

// readStarted reads the daemon start time; a missing file is the zero time,
// an unreadable or corrupt one is an error naming the file.
func readStarted(path string) (time.Time, error) {
	// #nosec G304 -- path is PBP's own state file under the state dir
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("daemon start time %s: %w", path, err)
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	if err != nil {
		return time.Time{}, fmt.Errorf("daemon start time %s is corrupt: %w", path, err)
	}
	return t, nil
}

// readState loads the state file without creating anything; a missing file
// is empty state.
func readState(path string) (state.State, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return state.State{}, nil
	}
	s, err := state.Open(path)
	if err != nil {
		return state.State{}, err
	}
	return s.Snapshot(), nil
}
