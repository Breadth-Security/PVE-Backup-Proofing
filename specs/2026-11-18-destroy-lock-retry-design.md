# Destroy: wait out a short lock on a test copy (issue #144)

Short design, 2026-11-18.

## Why

In a scheduled run on the test node, a container's copy booted and passed its checks.
PBP stopped it, and 0.7 s later PVE refused to delete it with
`CT is locked (mounted)`. Something else on the node had locked the stopped
copy for about a second; `pct df` and `pct mount` both do this. The guest
ended `ERROR · booted, but PBP could not remove its test copy`, and the
copy stayed until the janitor's next run. A healthy backup was reported as
an error because of another tool's routine housekeeping.

## Goals

- When PVE refuses a step of tearing down a test copy only because the
  copy is locked, PBP waits a short, bounded time and tries the teardown
  again. A lock that clears in time leaves the verdict as it was.
- A lock that does not clear in time ends as it does today: the copy is
  left for the janitor, and the reason now names the lock.
- The janitor gets the same behaviour, because it uses the same `Destroy`.

## Non-goals

- Finding or stopping whatever takes the lock.
- Waiting for locks anywhere other than `Destroy` (restore, start, probe).

## Anti-goals

- PBP never clears, overrides or skips a lock (`skiplock`) it did not set.
- A retry never destroys a guest that is no longer PBP's. Each try does
  the full ownership check again.
- A lock refusal never takes the #108 fallback that destroys without the
  scan for unreferenced disks. That fallback is for a broken storage, and
  a lock would make it blame a healthy one.

## Approaches

- **A. Retry only the destroy request.** This is the smallest change, but
  it has to re-check ownership itself, and it does not help when the stop
  or the protection clear is the step refused.
- **B. Retry the whole `Destroy` (chosen by the owner).** Each try runs
  the existing sequence again: the range guard, the find, the kind and
  node check, the tag or leftover rule, the protection clear, the stop and
  the destroy. Ownership is checked again for free, and a lock at any step
  is covered. Every step is idempotent: a guest that is already gone
  returns nil, and a guest that is already stopped skips the stop.

## How PVE reports a lock (read from PVE 9.2 on the test node)

- `check_lock` dies with `CT is locked (<name>)` or `VM is locked (<name>)`.
- Destroy runs `check_lock` before its task starts, so the request is
  refused with HTTP 500. It runs it again inside the task after taking the
  config lock, so the task can also fail with the same message.
- Stop runs `check_lock` inside its task, so the task fails. A container's
  stop ignores a `mounted` lock.
- Config writes, such as clearing protection, refuse with HTTP 500.
- When PVE cannot take the config file's own lock in time, it says
  `can't lock file '…' - got timeout`. This is also short-lived and counts
  as a lock.

So a lock refusal is either a `*pve.APIError` with status 500, or a
`*pve.TaskError` whose exit status or log contains `is locked (` or
`can't lock file`. Nothing else counts.

## Design

1. **`internal/pve`:** add `Locked(err error) (lock string, ok bool)`
   next to `PermissionDenied`. It reports whether err is a lock refusal as
   defined above. `lock` is the name in the parentheses (`mounted`,
   `backup`, …), or `config file` for `can't lock file`. It unwraps with
   `errors.As`, so it also finds a lock inside joined or wrapped errors.
2. **`ExistingNode.destroy`:** a failed destroy task that `Locked`
   recognises is returned as is, without the #108 retry without the scan.
3. **`ExistingNode.Destroy`:** move today's body into `destroyOnce` and
   wrap it in a loop:
   - The loop stops on success, on any error that is not a lock refusal,
     on context cancellation, or once `lockWait` has passed since the
     first try.
   - Between tries it waits `lockPoll`.
   - `ErrNotOwned` is not a lock refusal, so it ends the loop at once.
   - `lockWait` (30 s) and `lockPoll` (2 s) are fields on `ExistingNode`,
     like the existing poll intervals, so tests can shorten them.
4. **Errors and logs:**
   - When the lock is still there at the end, `Destroy` returns an error
     that wraps the last refusal and says how long it waited, for example
     `CT <vmid> still locked (mounted) after 30s: …`.
   - When a retry succeeds, one line is logged at Info:
     `destroy waited 4s for a lock (mounted)`.
5. **The reason in `internal/run/job.go`:** when a destroy fails on a
   lock, the reason names it:
   - a PROVEN copy becomes `booted, but PBP could not remove its test copy
     (locked: mounted; retried at the next run)`;
   - another verdict gets the suffix `; test copy not removed (locked:
     mounted; retried at the next run)`.

   Any other destroy failure keeps today's wording. The reason must still
   fit the 160-rune notes line.
6. **The PVE fake (`pvefake`):** add a way to hold a lock on a guest for
   the next N calls of Destroy or Stop. Destroy refuses with
   `&pve.APIError{Status: 500, Message: "CT is locked (mounted)"}`. Stop
   starts a task that fails with `CT is locked (backup)`. Both match PVE
   above.
7. **Docs:**
   - a CHANGELOG entry under Unreleased;
   - the troubleshooting row for "could not remove its test copy" says PBP
     now waits up to 30 s for a lock, and what a lock that persists means
     (another tool such as a vzdump job holding the copy).

## Tests

All tests use the fake with shortened `lockWait` and `lockPoll`. Each
names the change that would make it fail.

- **Locked for the first two destroy requests, then free:** the copy is
  gone, `Destroy` returns nil, and a job keeps PROVEN. Today this ends
  ERROR.
- **Stop task fails on a lock once, then succeeds:** the copy is gone.
- **Locked for longer than `lockWait`:** the error wraps the refusal and
  names the lock. The job's reason contains `locked: mounted`.
- **A destroy task that fails on a lock:** it does not take the
  without-scan fallback. A destroy task that fails on a storage still
  does, which is the existing #108 test.
- **The `pbp` tag is removed between tries:** `Destroy` returns
  `ErrNotOwned` and nothing is destroyed.
- **The context is cancelled while waiting:** `Destroy` returns promptly
  with the context's error.
- **A refusal that is not a lock, such as a permission error:** it is
  not retried, and the fake records a single destroy call.
- **`pve.Locked` table:**
  - API 500 `CT is locked (mounted)` gives `mounted`.
  - VM `is locked (backup)` gives `backup`.
  - A task failure with `CT is locked (snapshot)` matches.
  - `can't lock file … got timeout` gives `config file`.
  - A wrapped or joined error matches.
  - A 403 does not match, and neither does a plain error with the same
    text.

## Left to the builder

- The exact shape of the loop, and the logger to use. `nodecheck.go`
  already logs from this package.
- Whether the job's teardown context leaves room for a 30 s wait. Check
  its deadline. If it is shorter, the wait must fit inside it, and that
  goes in the plan as a ruling.

## Changes after the branch review

The owner asked for every minor from the final review to be addressed:

- `pve.Locked` matches only PVE's wording, `CT|VM is locked (<name>)`,
  with a name of at most 24 lower-case letters or `-`. It names a lock file
  by what it guards (`config file`, `storage`, else `lock file`). It looks
  at every error in a join, not only the first of each type.
- A `create` lock is not waited for: only the restore that set it would
  clear it, so a leftover of a cut-short restore keeps it.
- Destroy logs the wait to the logger the caller puts in its context
  (`target.WithLog`): the guest's log for a job, the run log for the
  janitor.
- The fake checks a lock before the scan for unreferenced disks, as PVE
  does, and a locked stop checks the node and the guest first.
- More tests: a stop locked past the wait, a running container under
  `mounted`, the without-scan fallback refused on a lock, and a job and
  the janitor through the real target.
- The troubleshooting row separates the lock case from the rest and says
  what a lasting lock costs.

