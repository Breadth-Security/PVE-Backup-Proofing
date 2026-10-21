# Run Logs and Notification Summary Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give every run a folder with a readable `run.log` and one log per guest (with each step of its proof), and restructure the notification so problems come first, with short reasons and the log folder's path.

**Architecture:** A new `report.RunLog` owns a run's folder and hands out `*slog.Logger`s backed by one plain-text handler: the run logger writes `run.log` and the service log (stdout); each guest logger writes `<Label>.log` and forwards warnings and errors to the run logger. The run package logs each step through a per-guest logger supplied in `run.Deps`. The notification data is pre-formatted in Go (`report.TemplateData`) so the handlebars templates on the node only iterate lists of strings.

**Tech Stack:** Go 1.26.10, `log/slog`, PVE notification templates (handlebars, rendered by proxmox-notify).

**Spec:** `specs/2026-10-21-log-notification-design.md` (agreed on issue #26). Read it first: its samples are the target output, and its "Decisions" section is binding.

## Global Constraints

- Go toolchain `go1.26.10`; no new module dependencies.
- Run tests, vet and lint on dev-scratch, not the workstation: `rsync -a --delete --exclude .git --exclude .superpowers --exclude .remember --exclude dist --exclude it ./ dev-scratch:/root/pbp/` then `ssh dev-scratch 'bash -lc "cd /root/pbp && go vet ./... && go test -race ./... && golangci-lint run ./..."'`. All must pass (lint: `0 issues.`) before each commit.
- Guest labels: `VM<vmid>` for `qemu`, `CT<vmid>` for `lxc`. `GuestKey.String()` (`qemu/102`) stays unchanged for state and notes.
- Run folder name: `<started in its own location, 2006-01-02_15-04-05>_<trigger>`, then `-2`, `-3`, … on collision.
- Files: folders `0750`, files `0600` (unchanged from today's run logs).
- Time in log lines comes from the injected clock (`now func() time.Time`), never from `slog.Record.Time`.
- Template data keys must not be proxmox-notify helper names (see `notifyHelpers` in `internal/report/report_test.go`; `duration` broke rendering on PVE 9.2).
- No reason string may contain a newline.
- Commit messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Never commit to `main`.

## Review Focus

1. Two runs that start in the same second (a manual run right after the scheduled one): the second gets `<name>-2`, and never writes into the first run's folder. Pinned in Task 1 (`TestOpenRunLogCollision`).
2. `logging.dir` cannot be written (wrong owner on a Docker bind mount): the run still runs and notifies, logging to stdout only, and the notification has no `Logs:` line. Pinned in Task 5 (`TestRunWiringLogDirUnwritable`).
3. A VM and a container with the same VMID (pve1 has VM109 and CT109): two separate files, `VM109.log` and `CT109.log`. Pinned in Task 1 (`TestRunLogGuestFiles`).
4. A failed PVE task whose error carries a multi-line task log: the reason is one line (the task's exit status) in `run.log`, the notification and the note, and the full tail is in the guest file only. Pinned in Task 3 (`TestRestoreTaskFailureLogged`).
5. Other files or folders in `logging.dir` (an operator's notes, `lost+found`): pruning never removes them, and legacy `run-*.log` files count toward `retain_runs`. Pinned in Task 1 (`TestPruneRuns`).

---

### Task 1: Run log folder and text handler

**Files:**
- Modify: `internal/report/logging.go` (add the new API beside the old one; `NewLogger(stdout, runFile)`, `OpenRunLog(dir, runID)`, `PruneRunLogs` and `LogGuest` stay until Tasks 3 and 5 remove them, so every commit builds)
- Create: `internal/report/format.go` (duration, size and age formatting)
- Modify: `internal/model/model.go` (`GuestKey.Label`)
- Test: `internal/report/logging_test.go` (new), `internal/model/model_test.go`

**Interfaces:**
- Produces:
  - `func (k model.GuestKey) Label() string` → `"VM102"` / `"CT101"`.
  - `const report.LevelDetail = slog.LevelDebug`: records written to `run.log` but not to stdout.
  - `func report.NewRunLog(root string, started time.Time, trigger string, now func() time.Time) (*RunLog, error)`.
  - `func (r *RunLog) Dir() string`: absolute path of the run's folder.
  - `func (r *RunLog) Logger(stdout io.Writer) *slog.Logger`: `run.log` (level ≥ `LevelDetail`) plus stdout (`slog.TextHandler`, level ≥ Info).
  - `func (r *RunLog) Guest(key model.GuestKey) *slog.Logger`: `<Label>.log`, opened on its first record; records ≥ Warn are also written to the run logger with the message prefixed `"<Label> "`. The same key always returns a logger writing the same file.
  - `func (r *RunLog) Close() error`: closes every open file.
  - `func report.PruneRuns(root string, retain int, keep string) error`.
  - `func report.Dur(d time.Duration) string`, `func report.Bytes(n int64) string`, `func report.Age(d time.Duration) string`.

- [ ] **Step 1: Write the failing tests** in `internal/report/logging_test.go` and `internal/model/model_test.go`:
  - `TestGuestKeyLabel`: `{qemu,102}` → `VM102`, `{lxc,101}` → `CT101`.
  - `TestRunDirName` (via `NewRunLog` in a `t.TempDir()`): `started = 2026-10-21 02:00:00` in `time.FixedZone("EDT", -4*3600)`, trigger `schedule` → `Dir()` ends in `2026-10-21_02-00-00_schedule`; the folder exists with mode `0750`.
  - `TestOpenRunLogCollision`: two `NewRunLog` calls with the same `started` and trigger → second `Dir()` ends in `_schedule-2`, third in `_schedule-3`; a record logged through the first never appears in the second's `run.log`.
  - `TestRunLogFormat`, with a clock that advances one second per call and starts at `2026-10-21 23:59:58` EDT:
    - `Logger(&stdout).Info("run started (schedule)")` → `run.log` line 1 is `2026-10-21 23:59:58 EDT run started (schedule)`.
    - The next Info line is `23:59:59 <msg>`. The one after that crosses midnight and carries the full date `2026-10-22 00:00:00 EDT <msg>`.
    - `Warn("x", "err", "a b")` → `<time> WARN x err="a b"`. `Error` prints `ERROR`. Attributes print as `key=value`, quoted with `strconv.Quote` when the value is empty or contains a space, `=` or `"`.
    - A message with newlines → continuation lines indented four spaces.
    - `Log(ctx, report.LevelDetail, "VM102 started (VM102.log)")` is in `run.log` but not in `stdout`; Info lines are in both.
    - Files are mode `0600`.
  - `TestRunLogGuestFiles`: `Guest({qemu,109}).Info("a")` and `Guest({lxc,109}).Info("b")` → `VM109.log` holds `a` and `CT109.log` holds `b`; `run.log` holds neither. `Guest({qemu,109}).Error("destroy failed", "err", "x")` → `VM109.log` has the line, and `run.log` and stdout have `VM109 destroy failed`. `Guest` called twice for one key, used from 8 goroutines under `-race` → one file, no interleaved partial lines.
  - `TestPruneRuns`: in `root`, create 4 run folders and 2 legacy files `run-a.log`, `run-b.log` with increasing mod times, plus `notes.txt`, a folder `lost+found` and a folder `2026-10-21_backup` (not a run name). `PruneRuns(root, 3, keep)`, where `keep` is the oldest run folder → the newest 3 entries among runs and legacy files remain, plus `keep`; `notes.txt`, `lost+found` and `2026-10-21_backup` are untouched. `retain` < 1 behaves as 1.
  - `TestFormat`:
    - `Dur`: `1800*time.Millisecond` → `1.8s`, `44s` → `44s`, `4m30s` → `4m30s`, `1h33m54s` → `1h34m`, `2h0m10s` → `2h0m`.
    - `Bytes`: `32<<30` → `32 GiB`, `4.4*(1<<30)` → `4.4 GiB`, `512<<20` → `512 MiB`.
    - `Age`: `20h` → `20h`, `45m` → `45m`, `76h` → `3d 4h`.
- [ ] **Step 2: Run them on dev-scratch** (`go test ./internal/report ./internal/model`). Expected: build failures (undefined `NewRunLog`, `Label`, `PruneRuns`, `Dur`).
- [ ] **Step 3: Implement.**
  - A run folder name matches `^\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2}_[a-z]+(-\d+)?$`; only those folders and `run-*.log` files are pruning candidates. Sort by mod time, then name, newest first; `os.RemoveAll` a folder.
  - Create the folder with `os.MkdirAll(root)` then `os.Mkdir(leaf)`, looping on `os.IsExist` for the `-N` suffix.
  - One `slog.Handler` type (`lineHandler`) per file, sharing a `*fileSink{mu, f, lastDate}`; `WithAttrs`/`WithGroup` return copies (group names prefix keys `g.k`).
  - `Bytes`: one decimal below 10 units, none from 10 up.
- [ ] **Step 4: Run** the full suite, vet and lint on dev-scratch (Global Constraints command). Expected: PASS, `0 issues.`
- [ ] **Step 5: Commit**: `git commit -m "Write each run's logs to its own folder as readable text"`.

### Task 2: Readable task failures, sanitize changes, OS name

**Files:**
- Modify: `internal/pve/client.go` (`TaskFailure`)
- Modify: `internal/target/target.go`, `internal/target/existing.go` (`Sanitized`), `internal/target/boot.go` (`BootInfo.OSName`), `internal/target/targetfake/*.go`
- Modify callers: `internal/run/job.go:142`, `internal/run/job_test.go:127`
- Test: `internal/pve/client_test.go`, `internal/target/existing_test.go`, `internal/target/boot_test.go`

**Interfaces:**
- Produces:
  - `func pve.TaskFailure(err error) (exit string, log []string, ok bool)`: if `errors.As(err, *TaskError)`, returns its `ExitStatus` (trimmed, newlines turned into spaces) and `Log`.
  - `type target.Sanitized struct { Removed []model.MissingPiece; Changes []string }`; `Target.Sanitize(ctx, kind, vmid) (Sanitized, error)`.
  - `BootInfo.OSName string`: the agent's OS `name` for a VM; the container's `ostype` for a CT; `""` when unknown.

- [ ] **Step 1: Write the failing tests:**
  - `TestTaskFailure`: `fmt.Errorf("restore x to 990000: %w", &pve.TaskError{UPID: "UPID:…", ExitStatus: "unable to restore CT 990000 - unable to parse\nvolume", Log: []string{"a", "b"}})` → `("unable to restore CT 990000 - unable to parse volume", ["a","b"], true)`; `errors.New("x")` → `ok == false`.
  - `TestSanitizeReportsChanges` (existing_test.go, using the file's fake API): live config `net0: virtio=AA:BB,bridge=vmbr0,tag=2`, `onboot: 1`, `usb0: host=1-1` (QEMU, bridge `pbpbr0`) → `Changes` equals, sorted by key:
    - `net0: virtio=AA:BB,bridge=vmbr0,tag=2 → virtio=AA:BB,bridge=pbpbr0`;
    - `onboot: 1 → 0`;
    - `usb0 removed (host=1-1)`.

    Keys whose value does not change are not listed. A key set that was unset shows `(unset)` as its old value.
  - `TestWaitBootedOSName` (boot_test.go): agent `OSInfo{Name: "Rocky Linux"}` → `OSName == "Rocky Linux"`; a container with `ostype: debian` → `"debian"`.
- [ ] **Step 2: Run** on dev-scratch (`go test ./internal/pve ./internal/target/...`). Expected: build failures.
- [ ] **Step 3: Implement.** `Changes` is computed in `ExistingNode.Sanitize` from the config it read and `plan.Set`/`plan.Delete`. The fake returns `Changes: nil`. Update `job.go` to use `.Removed` (step logging comes in Task 3).
- [ ] **Step 4: Run** the full suite, vet and lint on dev-scratch. Expected: PASS, `0 issues.`
- [ ] **Step 5: Commit**: `git commit -m "Report sanitize changes, the guest's OS name and a task's error line"`.

### Task 3: Log every step of a run and of each guest

**Files:**
- Modify: `internal/run/runner.go`, `internal/run/job.go`
- Modify: `internal/model/model.go` (`RunSummary.LogDir`)
- Modify: `internal/report/logging.go` (remove `LogGuest`, now unused)
- Test: `internal/run/runner_test.go`, `internal/run/job_test.go`

**Interfaces:**
- Consumes: Task 1's `RunLog`, `LevelDetail`, `Dur`, `Bytes`, `Age`, `Label`; Task 2's `TaskFailure`, `Sanitized.Changes`, `BootInfo.OSName`.
- Produces:
  - `run.Deps.GuestLog func(model.GuestKey) *slog.Logger`: nil means `Log.With("guest", key.String())`.
  - `run.Deps.Version string`.
  - `run.Options.LogDir string`, copied to `model.RunSummary.LogDir`.

The exact lines are the spec's samples. Messages are prose; values that are not prose stay as attributes only where the spec shows none. Steps and the logger they use:

| When | Logger | Message |
|------|--------|---------|
| run start | run | `run started (<trigger>) · PBP <Version> · node <node>` |
| preflight passed | run | `preflight ok (<Dur>)` |
| discovery done | run | `discovery: <n> backups on <storages, ", ">; <k> already proven, <m> to proof` |
| admit reads config | guest | `backup <volid> (taken <local 2006-01-02 15:04>, <Age> old, <Bytes>)` |
| gates passed | guest | `capacity ok: <Bytes size> of <Bytes avail> free on <store>; RAM <Bytes need> of <Bytes free> free` |
| job dispatched | run, `LevelDetail` | `<Label> started (<Label>.log)` |
| protect | guest | `backup marked protected (was not)` or `backup already protected` |
| restore | guest | `restore to <vmid> …`, then `restore done in <Dur>` or `restore failed after <Dur>: <exit or err>` and, if `TaskFailure` ok, `PVE task log (last <n> lines):\n<lines>` |
| sanitize | guest | `made safe: <Changes joined "; ">` (or `made safe: nothing to change`); if Removed: `not covered by the proof: <pieces joined ", ">` |
| start | guest | `started; waiting for the guest (up to <Dur boot_timeout>)`, then `running after <Dur> (<OSName>)` (no brackets when empty) or the boot failure reason |
| probe | guest | `failed units: …` and `ignored units: …` (when non-empty), `check <name> ok` / `check <name> failed (<detail>)`, each NotCovered `LogLine()`, and `boot log:\n<BootLog>` when non-empty |
| verdict | guest | `verdict <VERDICT> · <reason>` |
| teardown | guest | `teardown done in <Dur>: destroyed <vmid>, protected set back to false` (only the parts that ran) |
| notes written | guest | `notes: <the line written>` |
| skip (any reason) | guest | `skipped: <reason>` |
| each result | run | `<Label> <VERDICT> · <reason>` (Info), logged as each result is final, replacing `report.LogGuest` |
| run end | run | `run finished in <Dur> · <n> proven, <n> failed, <n> booted, <n> skipped, <n> error · severity <sev>` with attribute `outcome` |

Reasons:
- A failed restore or start becomes `restore failed: <exit>` / `start failed: <exit>` when `TaskFailure` is ok.
- Every reason that can carry an error string is passed through `oneLine(s string) string` in `job.go`, which turns runs of `\r`/`\n` into one space. This includes `boot check error`, `sanitize failed` and the `cleanup failed` suffixes.
- The existing `j.log.Error` calls (destroy, unprotect, panic, state) keep their level and now go to the guest logger, so Task 1's forwarding puts them in `run.log` and stdout.

- [ ] **Step 1: Write the failing tests** (a capturing logger: a `report.RunLog` in `t.TempDir()` with a fixed clock, read back from disk):
  - `TestRunLogsSteps`, using the run fixture with one PROVEN guest 101:
    - The fixture sets `Deps.Version = "test"` and `Deps.GuestLog = rl.Guest`. `run.log` contains, in order, `run started (manual) · PBP test · node pve1`, `preflight ok (`, `discovery: 1 backups on pbs; 0 already proven, 1 to proof`, `VM101 started (VM101.log)`, `VM101 PROVEN · `, `run finished in `.
    - `VM101.log` contains, in order, `backup pbs:backup/qemu/101/`, `capacity ok: `, `backup marked protected (was not)`, `restore to 990000 …`, `restore done in `, `made safe: `, `started; waiting for the guest (up to 5m0s)`, `running after `, `verdict PROVEN · `, `teardown done in `, `notes: PBP PROVEN `.
    - The stdout buffer has no `started (VM101.log)` line.
  - `TestRestoreTaskFailureLogged`: the target fake's Restore returns `fmt.Errorf("restore …: %w", &pve.TaskError{UPID: "UPID:x", ExitStatus: "unable to restore", Log: []string{"line one", "TASK ERROR: unable to restore"}})`. Then:
    - the result's reason is exactly `restore failed: unable to restore`;
    - the note line ends `· restore failed: unable to restore`;
    - `run.log` has `VM101 ERROR · restore failed: unable to restore` and no `UPID`;
    - `VM101.log` has `PVE task log (last 2 lines):` followed by `    line one` and `    TASK ERROR: unable to restore`.
  - `TestSkipLoggedToGuestFile`: the template guest from `TestRunSkipsTemplates` → `VM101.log` has `skipped: template (not bootable)` and `run.log` has `VM101 SKIPPED · template (not bootable)`.
  - `TestDestroyFailureForwarded`: Destroy fails → `run.log` has `VM101 destroy temporary guest failed; left for the janitor`.
  - `TestRunSummaryLogDir`: `Options{LogDir: "/x"}` → `s.LogDir == "/x"`.
  - Update `job_test.go`/`runner_test.go` callers of `Sanitize` and any test that asserted on `LogGuest` records.
- [ ] **Step 2: Run** on dev-scratch (`go test ./internal/run`). Expected: FAIL / build failures.
- [ ] **Step 3: Implement.**
  - `pool.run` creates the guest logger once per backup (via `Deps.GuestLog`) and passes it to `admit`, `skip` and `proofOne`.
  - The run logger's `With("trigger",…).With("run",…)` attributes are dropped from `Run`: the folder already names the run.
  - Durations are measured with `d.Now()`.
- [ ] **Step 4: Run** the full suite, vet and lint on dev-scratch. Expected: PASS, `0 issues.`
- [ ] **Step 5: Commit**: `git commit -m "Log each step of a guest's proof to its own file"`.

### Task 4: Notification: problems first, short subject, log path

**Files:**
- Modify: `internal/report/summary.go`, `internal/report/templates/pbp-subject.txt.hbs`, `internal/report/templates/pbp-body.txt.hbs`
- Test: `internal/report/report_test.go`

**Interfaces:**
- Consumes: `GuestKey.Label`, `RunSummary.LogDir`, `GuestResult.AlreadyProven`/`Template`.
- Produces: `report.TemplateData(s model.RunSummary, node string) map[string]any` with exactly these keys:
  - `node`, `run-id`;
  - `summary` (subject text);
  - `headline`;
  - `logs` (`LogDir + "/"`, or `""`);
  - `run-error`, `environment-errors` ([]string, unchanged);
  - `failed`, `errored`, `booted` ([]string of pre-formatted lines);
  - `skipped` ([]string, one line per reason);
  - `proven` (string, comma-joined labels);
  - `not-covered` ([]string).

Formats (times in `s.Started.Location()`):
- `headline`: `PBP run on <node>, <Started "Mon 2006-01-02 15:04 MST"> (<scheduled|manual>), <Dur(Ended−Started)>`. The trigger `schedule` reads `scheduled`; others print as they are.
- A guest line: `<Label>[ <Name>] · backup <Backup.Time "Jan 2 15:04"> · <Reason>`.
- A skipped line: `<reason>: <labels, ", ">`, in the order reasons first occur.
- A not-covered line: `<Label>: <NotCovered.LogLine()>`.
- `summary`:
  - The problem parts are the non-zero counts of `failed`, `error`, `booted` and the skips that are neither `AlreadyProven` nor `Template`, as `"<n> failed, <n> error, <n> booted, <n> skipped"`. `run error` comes first when `RunError != ""`.
  - With problems: `<problems> · <n> proven`.
  - Without problems: `all <n> proven` when `n > 0`, otherwise `nothing to proof`.

- [ ] **Step 1: Write the failing tests:**
  - `TestTemplateDataSections`: build a summary with:
    - `Started 2026-10-21 02:00 EDT`, `Ended` 1h33m54s later, trigger `schedule`, `LogDir /var/log/pbp/2026-10-21_02-00-00_schedule`;
    - results VM102 FAILED (name `web01`, backup 2026-10-20 02:00 EDT, reason `failed: a.service`), CT101 ERROR, VM117 BOOTED, VM104 and VM105 SKIPPED `insufficient node memory`, VM100 SKIPPED already proven, VM108 and CT109 PROVEN;
    - VM102 NotCovered with one piece.

    Assert:
    - `headline == "PBP run on pve1, Tue 2026-10-21 02:00 EDT (scheduled), 1h34m"`;
    - `logs == "/var/log/pbp/2026-10-21_02-00-00_schedule/"`;
    - `failed == ["VM102 web01 · backup Oct 20 02:00 · failed: a.service"]`;
    - `skipped == ["insufficient node memory: VM104, VM105", "already proven: VM100"]`;
    - `proven == "VM108, CT109"`;
    - `not-covered[0]` starts with `VM102: `;
    - `summary == "1 failed, 1 error, 1 booted, 2 skipped · 2 proven"`.
  - `TestSubjectSummary`, table:
    - only PROVEN ×13 → `all 13 proven`;
    - PROVEN ×2 plus SKIPPED already proven → `all 2 proven`;
    - only an already-proven skip → `nothing to proof`;
    - `RunError` set, no results → `run error · 0 proven`;
    - a template skip only → `nothing to proof`.
  - Update the template guards (`TestTemplatesUseOnlyProvidedKeys`, `TestTemplateGuardCatchesBadTags`, `TestTemplateDataKeysAreNotNotifyHelpers`) for list-of-string sections: the per-guest map is gone, so `templateProblems(body, d)` checks top-level keys and `this` only.
  - Replace `TestBodyBootedSection` with `TestBodySectionsInOrder`: the body contains `{{#if run-error}}`, `{{#if environment-errors}}`, `FAILED: the backup has a problem`, `ERROR: PBP could not finish; says nothing about the backup`, `BOOTED: started, not looked inside`, `SKIPPED`, `PROVEN`, `Not covered by backup` in that order, each section guarded by `{{#if <key>}}`.
  - Keep `TestBodyEnvironmentErrorsSection` unchanged.
- [ ] **Step 2: Run** on dev-scratch (`go test ./internal/report`). Expected: FAIL.
- [ ] **Step 3: Implement** `TemplateData` and the two templates.
  - Subject: `PBP {{node}}: {{summary}}`.
  - Body: `{{headline}}`, then `{{#if logs}}Logs: {{logs}}{{/if}}` on its own line, then the sections in the order the test pins. Each list section is a blank line, its title, then `{{#each <key>}}\n  {{this}}\n{{/each}}`. `proven` is one indented line.
- [ ] **Step 4: Run** the full suite, vet and lint on dev-scratch. Expected: PASS, `0 issues.`
- [ ] **Step 5: Commit**: `git commit -m "Lead the notification with problems and name the log folder"`.

### Task 5: Wire it in, and document it

**Files:**
- Modify: `cmd/pbp/main.go` (`runOnce`, `daemon`, `checkSteps` use the new `report` API)
- Modify: `internal/report/logging.go`: remove `OpenRunLog(dir, runID)` and `PruneRunLogs` and their tests in `internal/report/report_test.go`; `NewLogger` becomes `NewLogger(stdout io.Writer) *slog.Logger` (stdout `slog.TextHandler` only), for commands that are not runs
- Test: `cmd/pbp/main_test.go` (`TestRunWiring`, new `TestRunWiringLogDirUnwritable`)
- Docs: `docs/operating.md` (new `## Logs` section after "Everyday commands": the layout, a short `run.log` and guest-file sample from the spec, retention, the Docker path note; update the two "per-run files are in" comments), `docs/setup.md:193` (`sudo less logs/*/run.log`), `docs/install-deb.md` "Where things are" (per-run logs: one folder per run), `docs/troubleshooting.md` intro (the guest's own log file is where to look first), `README.md` line 38 (the BOOTED section's new title), `config.example.yaml` (`retain_runs` comment: `# runs (one folder each); default; at least 1`), `CHANGELOG.md` (`## Unreleased` entry, `Closes #26` in the commit).

- [ ] **Step 1: Write the failing tests:**
  - Rewrite `TestRunWiring`'s log assertions:
    - after `run --guest 101`, `logDir` holds exactly `run-c.log` (the newest legacy file; the test's `retain_runs` is 2, so the run's folder plus one legacy file remain) and one folder matching `^\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2}_manual$`;
    - its `run.log` contains `VM101 SKIPPED · already proven` and, after it, `run finished in `;
    - its `VM101.log` contains `skipped: already proven`;
    - no `qemu/102` or `VM102` file or line exists;
    - stdout still contains `run 20261019T020005.000Z-manual: info; …`.
  - `TestRunWiringLogDirUnwritable`: `logging.dir` is a regular file → exit code unchanged from a normal run, stdout contains `open run log`, and the notifier's sent summary has `LogDir == ""`.
- [ ] **Step 2: Run** on dev-scratch (`go test ./cmd/pbp`). Expected: FAIL.
- [ ] **Step 3: Implement** `runOnce`:
  - `rl, err := report.OpenRunLog(cfg.Logging.Dir, a.now(), opt.Trigger, a.now)`;
  - on success, `log := rl.Logger(a.stdout)`, `d.GuestLog = rl.Guest`, `opt.LogDir = rl.Dir()`, `defer rl.Close()`, then `report.PruneRuns(cfg.Logging.Dir, cfg.Logging.RetainRuns, rl.Dir())` after the run;
  - on failure, `report.NewLogger(a.stdout)` and log `open run log` at Error;
  - `d.Version = version`.

  Then write the docs listed above.
- [ ] **Step 4: Run** the full suite, vet and lint on dev-scratch (`go test -race ./...`, `golangci-lint run ./...`). Expected: all `ok`, `0 issues.`
- [ ] **Step 5: Commit**: `git commit -m "Write run logs to per-run folders and document them"` with `Closes #26` in the body.

### Acceptance (owner)

After the PR merges and a release is cut, the owner installs it on pve1. Then:
- `pbp node install-templates` (or the `pbp node setup` line) installs the new templates;
- the next scheduled run produces a folder under `/var/log/pbp/` and a notification shaped like the spec's sample.
