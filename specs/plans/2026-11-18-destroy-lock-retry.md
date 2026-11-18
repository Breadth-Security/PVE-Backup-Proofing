# Destroy lock retry Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When PVE refuses a step of removing a test copy only because the copy is locked, `ExistingNode.Destroy` waits up to 30 s and tries the whole removal again. A lock that persists ends as today, but the reason names the lock (issue #144).

**Architecture:**
- **`internal/pve`:** a new `Locked(err)` recognises a lock refusal, whether it comes back as an API 500 or as a failed task.
- **`internal/target`:** `Destroy` becomes a loop around today's body, now called `destroyOnce`. The #108 fallback that destroys without the scan skips lock failures.
- **`internal/run`:** the job's teardown puts the lock's name in the reason.
- **`pvefake`:** learns to hold a lock, the way PVE 9.2 reports one.

**Tech Stack:** Go 1.26, `internal/pve/pvefake`, `internal/target/targetfake`, the run fixture.

**Spec:** `specs/2026-11-18-destroy-lock-retry-design.md`

## Global Constraints

- **Where tests run:** on the owner's development host, not the workstation. Sync the working tree there without `.git`, `.superpowers`, `.remember` and `dist`, and run `go test` in it. "Run:" lines below give only the `go test` part.
- **Before each commit:** `gofmt -l` is clean, and `go vet ./...` and `golangci-lint run` are clean on the development host.
- **Commits:** signed with the owner's SSH signing key through their ssh-agent. Each message ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- **No lab details in repo files:** no host names, guest names or IDs, storage names or IPs.
- **Waiting:** `lockWait` = 30 s and `lockPoll` = 2 s, as fields on `ExistingNode`. The job's `destroyTimeout` (5 min, `internal/run/job.go:38`) already leaves room, so no ruling is needed.
- **What counts as a lock refusal:** a `*pve.APIError` with status 500, or a `*pve.TaskError` whose exit status or log contains `is locked (` or `can't lock file`. Nothing else, and never a plain error with the same text.
- **Lock names:** the text inside the parentheses (`mounted`, `backup`, `snapshot`, …), or `config file` for `can't lock file`.
- **Reason wording, copied exactly:**
  - a PROVEN copy: `booted, but PBP could not remove its test copy (locked: <name>; retried at the next run)`;
  - any other verdict, as a suffix: `; test copy not removed (locked: <name>; retried at the next run)`.

  A failure that is not a lock keeps today's wording.
- **Never:** clear a lock, use `skiplock`, or take the without-scan fallback for a lock failure.

## Review Focus

1. **A lock on the protection clear** (`SetGuestConfig` refused with 500 `CT is locked (backup)`). The removal is retried like any other lock. Test in Task 2.
2. **A running copy whose stop task keeps failing on a lock past `lockWait`.** It ends with an error that `pve.Locked` still reads as that lock, through every wrap `Destroy` adds. Test in Task 2.
3. **The guest vanishes between tries** (someone else removed it). `Destroy` returns nil and issues no further destroy. Test in Task 2.
4. **A container stop under a `mounted` lock.** PVE lets it through, and so must the fake, so tests don't wait for a lock PVE ignores. Test in Task 2.
5. **A non-lock 500 whose message happens to say "locked"** (for example `got timeout`). It is not retried. Test in Task 1's table: `got timeout` and `resource is locked by another user` give false. The latter has no parenthesis and is not `can't lock file`.

---

### Task 1: `pve.Locked`

**Files:**
- Modify: `internal/pve/client.go` (next to `PermissionDenied`, about line 81)
- Test: `internal/pve/client_test.go`

**Interfaces:**
- Produces: `func Locked(err error) (lock string, ok bool)`

- [ ] **Step 1: Write the failing test `TestLocked`** as a table of `err → (lock, ok)`:

```go
{&APIError{Status: 500, Message: "CT is locked (mounted)"}, "mounted", true},
{&APIError{Status: 500, Message: "VM is locked (backup)\n"}, "backup", true},
{&TaskError{UPID: "u", ExitStatus: "CT is locked (snapshot)"}, "snapshot", true},
{&TaskError{UPID: "u", ExitStatus: "unexpected status", Log: []string{"TASK ERROR: VM is locked (clone)"}}, "clone", true},
{&APIError{Status: 500, Message: "can't lock file '/run/lock/lxc/pve-config-1.conf' - got timeout"}, "config file", true},
{fmt.Errorf("destroy 1: %w", &APIError{Status: 500, Message: "CT is locked (mounted)"}), "mounted", true},
{errors.Join(errors.New("x"), &APIError{Status: 500, Message: "CT is locked (disk)"}), "disk", true},
{&APIError{Status: 403, Message: "CT is locked (mounted)"}, "", false},
{&APIError{Status: 500, Message: "got timeout"}, "", false},
{&APIError{Status: 500, Message: "resource is locked by another user"}, "", false},
{errors.New("CT is locked (mounted)"), "", false},
{nil, "", false},
```

- [ ] **Step 2: Run it and watch it fail.** Run: `go test ./internal/pve -run TestLocked`. Expected: a build failure, `undefined: Locked`.
- [ ] **Step 3: Implement `Locked`.** Find the error with `errors.As`: first `*APIError` (only status 500), then `*TaskError` (exit status, then log lines). Match `is locked \(([^)]*)\)` for the name, else `can't lock file` for `config file`.
- [ ] **Step 4: Run it and watch it pass.** Run: `go test ./internal/pve`. Expected: PASS.
- [ ] **Step 5: Commit** with the message `pve: recognise PVE refusing a locked guest (#144)`.

### Task 2: the fake holds locks; `Destroy` waits them out

**Files:**
- Modify: `internal/pve/pvefake/fake.go` (`Stop`, `Destroy`, a new `Locks` field)
- Test: `internal/pve/pvefake/fake_test.go`
- Modify: `internal/target/existing.go` (`ExistingNode` fields, `NewExistingNode`, `Destroy`, `destroy`)
- Test: `internal/target/existing_test.go` (`newTarget` sets `n.lockWait, n.lockPoll = 50*time.Millisecond, time.Millisecond`)

**Interfaces:**
- Consumes: `pve.Locked` (Task 1).
- Produces:
  - in `pvefake`: `type Lock struct { Name string; Calls int; InTask bool }` and `Fake.Locks map[int]*Lock`, initialised in `New`;
  - in `target`: `ExistingNode.lockWait`, `ExistingNode.lockPoll` (`time.Duration`), and `func (n *ExistingNode) destroyOnce(ctx, kind, vmid, rec) error`. The signature of `Destroy` is unchanged.

**Fake behaviour (PVE 9.2), while `Locks[vmid]` has `Calls > 0`:**
- **`Destroy`:** each call uses up one. With `InTask` false it returns `&pve.APIError{Status: 500, Path: "/nodes/<node>/<kind>/<vmid>", Message: "<CT|VM> is locked (<Name>)"}`. With `InTask` true it returns a UPID whose `WaitTask` fails with a `*pve.TaskError` with that exit status. It is checked after `begin`, `f.at` and the existence check, and before the running and protection checks.
- **`Stop`:** each call uses one up and returns a UPID whose task fails with that message. The exception is an LXC guest with `Name == "mounted"`: PVE ignores that lock for a container stop, so the fake does too and uses nothing up.
- Use `CT` for LXC and `VM` for QEMU.

- [ ] **Step 1: Write the failing fake test `TestLocks`.**
  - A locked destroy with `Calls: 2` is refused twice with an error for which `pve.Locked` gives `mounted`, then succeeds.
  - With `InTask`, `WaitTask` on the returned UPID fails with that lock.
  - A locked QEMU stop's task fails with `VM is locked (backup)`.
  - An LXC stop under `mounted` succeeds and leaves `Calls` unchanged.
- [ ] **Step 2: Write the failing target test `TestDestroyWaitsForLock`.** Each subtest uses `healthyFake()` and an LXC guest at 990003 tagged `pbp`.
  - **"destroy refused twice, then gone":** `Locks[990003] = &pvefake.Lock{Name: "mounted", Calls: 2}`, guest stopped. `Destroy` returns nil, the guest is gone, and there are 3 `Destroy` calls, all `… true`.
  - **"stop task fails on a lock once":** guest running, `Lock{Name: "backup", Calls: 1}`. Returns nil, the guest is gone, and there are 2 `Stop` calls.
  - **"lock outlasts lockWait":** `Lock{Name: "mounted", Calls: 1000}`. The error is non-nil, `pve.Locked(err)` gives `("mounted", true)`, the message contains `still locked (mounted) after`, and the guest is still listed.
  - **"lock inside the destroy task takes no without-scan fallback":** `Lock{Name: "mounted", Calls: 1, InTask: true}`. Returns nil. The destroy calls are exactly `["Destroy pve1 lxc 990003 true", "Destroy pve1 lxc 990003 true"]`, with no `false`.
  - **"tag removed between tries":** `Lock{Name: "mounted", Calls: 1}`, and `FailOn` counts `GuestConfig` calls and deletes `f.Configs[990003]["tags"]` on the second one (the retry's read), returning nil. `errors.Is(err, ErrNotOwned)`, the guest is still listed, and there is exactly 1 `Destroy` call. Act on the retry's read, not the first `Destroy`: the fake checks existence before the lock, so changing the guest during that call would hide the lock refusal.
  - **"guest removed between tries":** `Lock{Name: "mounted", Calls: 1}`, and `FailOn` removes 990003 from `f.GuestList` on the second `Guests` call (the retry's find). Returns nil, with exactly 1 `Destroy` call.
  - **"protection clear refused on a lock":** the config has `protection: "1"`, and `FailOn` returns `&pve.APIError{Status: 500, Message: "CT is locked (backup)"}` on the first `SetGuestConfig` only. Returns nil and the guest is gone.
  - **"context cancelled while waiting":** set `n.lockPoll = time.Hour`, `n.lockWait = 2*time.Hour`, use a ctx with a 50 ms timeout, and `Lock{Calls: 1000}`. Returns within 1 s, and `errors.Is(err, context.DeadlineExceeded)`.
  - **"non-lock refusal not retried":** `f.Fail["Destroy"] = &pve.APIError{Status: 500, Message: "got timeout"}`. The error is non-nil, with exactly 1 `Destroy` call.
- [ ] **Step 3: Run them and watch them fail.** Run: `go test ./internal/pve/pvefake ./internal/target -run 'TestLocks|TestDestroyWaitsForLock'`. Expected: a build failure, `undefined: pvefake.Lock` and `n.lockWait`.
- [ ] **Step 4: Implement the fake's `Locks`** as described above.
- [ ] **Step 5: Implement the retry.**
  - Rename today's `Destroy` body to `destroyOnce`.
  - `Destroy` loops: call `destroyOnce`. Return its result unless `pve.Locked(err)` is true. Once `time.Since(start) >= n.lockWait`, return `fmt.Errorf("%s %d still locked (%s) after %s: %w", kind, vmid, lock, waited.Round(time.Second), err)`. Otherwise wait `n.lockPoll` on a `time.NewTimer` with a `select` on `ctx.Done()`, which returns `ctx.Err()` joined with the last error.
  - After a success that needed more than one try, log `slog.Info("destroy waited for a lock", "vmid", vmid, "lock", lock, "waited", waited.Round(time.Second))`.
  - In `destroy`, return `first` as is when `pve.Locked(first)` is true, before the `TaskFailure` fallback check.
  - In `NewExistingNode`, set `lockWait: 30 * time.Second, lockPoll: 2 * time.Second`.
  - Update `Destroy`'s doc comment: one sentence on the wait, and that it never clears a lock.
- [ ] **Step 6: Run the whole package set and watch it pass.** Run: `go test ./internal/pve/... ./internal/target/... ./internal/run/...`. Expected: PASS, including the existing #108 tests (`TestDestroyFallsBackWithoutUnreferencedScan`, `TestDestroyNoFallbackOnRequestError`, `TestDestroyFallbackAlsoFails`).
- [ ] **Step 7: Commit** with the message `target: wait out a short lock when removing a test copy (#144)`.

### Task 3: the reason names the lock; docs

**Files:**
- Modify: `internal/run/job.go` (teardown, about lines 502–508)
- Test: `internal/run/job_test.go` (`TestDestroyFailure`)
- Modify: `docs/troubleshooting.md` (the `destroy temporary guest failed; left for the janitor` row), `CHANGELOG.md` (a new `## Unreleased` section above `## v0.11.2`)

**Interfaces:**
- Consumes: `pve.Locked` (Task 1).

- [ ] **Step 1: Add the failing cases to `TestDestroyFailure`.** Add a `destroyErr error` field to the table, defaulting to today's `errors.New("destroy timed out")`. Use `locked := fmt.Errorf("lxc %d still locked (mounted) after 30s: %w", tmpVMID, &pve.APIError{Status: 500, Message: "CT is locked (mounted)"})`.
  - **"proven, locked":** verdict `model.Error`, reason `booted, but PBP could not remove its test copy (locked: mounted; retried at the next run)`.
  - **"failed kept, locked":** the "failed kept" setup gives the reason `failed: postgresql.service; test copy not removed (locked: mounted; retried at the next run)`.
  - The existing cases are unchanged.
- [ ] **Step 2: Run it and watch it fail.** Run: `go test ./internal/run -run TestDestroyFailure`. Expected: FAIL. The new cases still get the `(retried at the next run)` wording.
- [ ] **Step 3: Implement it.** In teardown's failure branch, `note := "retried at the next run"`, and when `pve.Locked(err)` is true, `note = "locked: " + lock + "; " + note`. Build both reasons from `note`, so the wording matches Global Constraints exactly. Keep `withSuffix` for the suffix.
- [ ] **Step 4: Run it and watch it pass.** Run: `go test ./internal/run`. Expected: PASS.
- [ ] **Step 5: Update the docs.**
  - **Troubleshooting row:** add that PBP first waits up to 30 s when PVE refuses because the copy is locked. Another tool may briefly lock a stopped container (`pct df`, `pct mount`, a backup job). A reason with `locked: <name>` means the lock outlasted that wait, and PBP never clears a lock itself.
  - **CHANGELOG:** under `## Unreleased`, add the entry `**A test copy that another tool briefly locks is still removed** (issue #144). …`, saying what used to happen (`ERROR · booted, but PBP could not remove its test copy`) and what happens now.
- [ ] **Step 6: Run the whole suite.** Run: `go test -race ./...`, plus `go vet ./...`, `golangci-lint run ./...` and `packaging/release-notes_test.sh`. Also run `packaging/release-notes.sh v0.11.3-rc.1`, which builds the notes from Unreleased: it must succeed, so the new entry has no bare `#N` and no relative link. Expected: all clean.
- [ ] **Step 7: Commit** with the message `run: name the lock when a test copy cannot be removed; docs (#144)`.
