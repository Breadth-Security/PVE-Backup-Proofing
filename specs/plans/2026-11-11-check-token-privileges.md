# Check the Token's Privileges Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `pbp check`, `pbp setup` and every run check the API token's effective privileges on each path PBP needs, so a missing ACL is named with its fix instead of silently hiding backups.

**Architecture:** A new dependency-free package `internal/privs` owns the per-path privilege table and the check. Preflight fails on missing *core* privileges (`/vms`, the node, the SDN zone, the restore storage). A source storage with missing privileges fails `pbp check`, but in a run it is only skipped, with a warning. A source storage that lists no backups also warns. `pbp setup` already reprints the node lines whenever the check fails, so it needs no change of its own.

**Tech Stack:** Go (module `git.breadthanalytics.com/tdiana/PVE-Backup-Proofing`), the existing `pve.API`, `pvefake` and `targetfake` test doubles.

**Spec:** issue #107 on the private tracker: "pbp check: verify the token's effective rights on every path PBP needs". The background is GitHub issue #5, where the token lacked ACLs on `/vms` and `/sdn/zones/localnetwork` while the user had them. The owner's decisions (2026-11-11) are in Global Constraints.

## Global Constraints

- **Per-path minimum privileges.** The check requires only what PBP uses on each path, as in the table in `docs/security.md` "The API token":
  - `/vms`: `VM.Allocate, VM.Audit, VM.Backup, VM.Config.HWType, VM.Config.Network, VM.Config.Options, VM.PowerMgmt, VM.GuestAgent.Audit, VM.GuestAgent.Unrestricted`
  - `/storage/<restore storage>` and each `/storage/<source storage>`: `Datastore.AllocateSpace, Datastore.Audit`
  - `/nodes/<target.node>`: `Sys.Audit`
  - `/sdn/zones/localnetwork`: `SDN.Use`
- **Missing core privilege:** `pbp check` fails, and a run fails at preflight. Core paths are `/vms`, the node, the SDN zone and the restore storage, including a restore storage that is also a source.
- **Missing source-storage privilege:** `pbp check` fails. A run skips that storage only, adds an environment error (the run ends as `warning`), and proofs the other storages.
- **A source storage that lists zero backups in a run:** an environment error, so the run ends as `warning`.
- **The token cannot read ACLs** (`GET /access/acl` returns `[]`, and `/access/permissions?userid=…` needs `Sys.Audit` on `/access`). So messages never claim to know whether the user or the token lacks the grant; they say both need it. Verified on a PVE 9.2 test node, 2026-11-11: `GET /access/permissions?path=…` called as a privilege-separated token returns the rights the user and token share.
- **Only the daemon changes.** No node helper or notification template change: `environment-errors` is already rendered by `internal/report/templates/pbp-body.txt.hbs`.
- **CHANGELOG:** add a `## Unreleased` section at the top. Refer to issues as plain `#107` and `#5`, with no links to the private tracker. The version bump is decided at release time, together with #108.

## Review Focus

1. **The restore storage is also in `sources.storages`.** One core need for that path, so a missing privilege fails preflight, not "skipped". No duplicate line. Test in Task 1 (`TestNeedsRestoreStorageAlsoSource`).
2. **A config `target.api.token_id` other than `pbp@pve!proof`** (manual setups). Messages use the configured ID, and its user part before `!`. Test in Task 1 (`TestDescribeUsesConfiguredToken`).
3. **The privileges call itself errors.** Preflight fails with `read the token's privileges on <path>: <err>`. In a run, a source storage whose privileges cannot be read is skipped with a warning; nothing panics or passes silently. Tests in Task 3 (`TestPreflightPrivilegesReadError`) and Task 4 (`TestRunSkipsSourceWhenPrivilegesUnreadable`).
4. **`pbp run --guest N`.** The zero-backups warning is based on the storage's whole listing, before the `OnlyVMID` filter. A storage with backups of other guests does not warn. Test in Task 4 (`TestRunGuestDoesNotWarnForOtherGuestsBackups`).
5. **A storage whose only backups are of guests inside PBP's VMID range** is not "zero backups" (count before `discovery.OutsideRange`). Test in Task 4 (`TestRunNoWarningWhenOnlyInRangeBackups`).

---

### Task 1: `internal/privs`: per-path needs and the check

**Files:**
- Create: `internal/privs/privs.go`
- Create: `internal/privs/privs_test.go`
- Modify: `internal/nodesetup/state.go` (`RolePrivs` becomes `= privs.RolePrivs`)
- Modify: `internal/nodesetup/plan_test.go` (add `TestACLPathsMatchPrivsNeeds`)

**Interfaces:**
- Produces:
  - `var RolePrivs []string`: the 13 PBPProofer privileges, same order as today's `nodesetup.RolePrivs`. `nodesetup` uses it from now on.
  - `const RoleName = "PBPProofer"`, `const SDNZonePath = "/sdn/zones/localnetwork"`.
  - `type Need struct { Path string; Privs []string; Source string }`. `Source` is the storage ID when the path is a source storage that is not the restore storage, else `""` (core).
  - `func Needs(node, restoreStorage string, sources []string) []Need`. Order: `/vms`, `/storage/<restore>`, each source storage not equal to the restore storage (repeats dropped, in config order), `/nodes/<node>`, the SDN zone. Each source storage's need is the same as the restore storage's.
  - `type Privileger interface { Privileges(ctx context.Context, path string) (map[string]bool, error) }` (`pve.API` satisfies it).
  - `type Missing struct { Need; Lacks []string }`. `Lacks` is in the need's order.
  - `func Check(ctx context.Context, api Privileger, needs []Need) ([]Missing, error)`. The error wraps the first failed read as `read the token's privileges on <path>: %w`.
  - `func (m Missing) Describe(tokenID string) string`. Exact copy, with `<user>` being `tokenID` before `!`:
    `token <tokenID> lacks <Lacks joined ", "> on <Path>; the user <user> and its privilege-separated token both need them: pveum acl modify <Path> --users <user> --tokens '<tokenID>' --roles PBPProofer`

- [ ] **Step 1: Write the failing tests** in `internal/privs/privs_test.go`, using a map-backed `Privileger` stub:
  - `TestNeedsOrderAndPrivs`: `Needs("pve1", "local-zfs", []string{"pbs", "pbs-sync"})` gives paths `[/vms /storage/local-zfs /storage/pbs /storage/pbs-sync /nodes/pve1 /sdn/zones/localnetwork]`. `Source` is `""`, `""`, `"pbs"`, `"pbs-sync"`, `""`, `""`. `/vms` has the 9 `VM.*` privileges from Global Constraints, the node has `[Sys.Audit]`, the zone has `[SDN.Use]`.
  - `TestNeedsRestoreStorageAlsoSource`: `Needs("pve1", "pbs", []string{"pbs", "pbs", "other"})` holds `/storage/pbs` once, with `Source == ""`, and `/storage/other` once, with `Source == "other"`.
  - `TestNeedsCoverRolePrivs`: the union of all `Needs(...)` privileges equals `RolePrivs` as a set.
  - `TestCheckReportsOnlyLacking`: the stub grants everything except `VM.Backup` on `/vms` and `Datastore.AllocateSpace` on `/storage/pbs`. `Check` returns exactly two `Missing`, with `Lacks` `[VM.Backup]` and `[Datastore.AllocateSpace]`, and the second has `Source "pbs"`.
  - `TestCheckReadError`: the stub errors on `/nodes/pve1`. The error contains `read the token's privileges on /nodes/pve1:`.
  - `TestDescribeUsesConfiguredToken`: `Missing{Need{Path: "/vms"}, []string{"VM.Backup", "VM.Audit"}}.Describe("pbp2@pve!night")` equals `token pbp2@pve!night lacks VM.Backup, VM.Audit on /vms; the user pbp2@pve and its privilege-separated token both need them: pveum acl modify /vms --users pbp2@pve --tokens 'pbp2@pve!night' --roles PBPProofer`.
- [ ] **Step 2: Run them and confirm they fail.** `go test ./internal/privs/` fails: the package does not exist.
- [ ] **Step 3: Implement `internal/privs/privs.go`** per the Interfaces block. The package imports only the standard library.
- [ ] **Step 4: Point `nodesetup` at it.** In `internal/nodesetup/state.go` set `var RolePrivs = privs.RolePrivs` (keep the doc comment), and `RoleName = privs.RoleName`. Add `TestACLPathsMatchPrivsNeeds` to `plan_test.go`: for node `pve1`, restore storage `local-zfs` and sources `[pbs]`, the paths of `privs.Needs` equal `ACLPaths("pve1", []string{"local-zfs", "pbs"})` as a set.
- [ ] **Step 5: Run the tests.** `go test ./internal/privs/ ./internal/nodesetup/` passes, including the existing `RolePrivs` assertion in `plan_test.go:569`.
- [ ] **Step 6: Commit.** `git add internal/privs internal/nodesetup` and commit: `privs: per-path privileges PBP's token needs, and the check (#107)`.

### Task 2: `pvefake` grants the role by default

**Files:**
- Modify: `internal/pve/pvefake/fake.go` (the `Privs` doc comment, `Privileges`)
- Test: `internal/pve/pvefake/fake_test.go` (create it if absent)

**Interfaces:**
- Consumes: `privs.RolePrivs` (Task 1).
- Produces: in `Fake.Privileges(path)`, a path **absent** from `f.Privs` (including a nil map) returns all of `privs.RolePrivs`. A path **present** returns exactly its list, so `f.Privs[p] = []string{}` means no privileges. Existing tests keep their meaning: none relies on a privilege inside the role on a path it did not list, and `Sys.Modify` / `VM.Config.Disk` are outside the role.

- [ ] **Step 1: Write the failing test.** `TestPrivilegesDefaultToRole`: a `New()` fake answers `/vms` with every `privs.RolePrivs` entry. After `f.Privs = map[string][]string{"/vms": {}}`, `/vms` gives an empty map and `/nodes/pve1` still gives the role.
- [ ] **Step 2: Run it.** `go test ./internal/pve/pvefake/ -run TestPrivilegesDefaultToRole` fails.
- [ ] **Step 3: Implement** the default in `Privileges` and update the `Privs` comment ("a path not listed has PBPProofer's privileges; list a path with an empty slice for none").
- [ ] **Step 4: Run the whole suite.** `go test ./...` passes. If a test changed meaning (it relied on an unlisted path having nothing), give it an explicit empty entry. Do not change what it asserts.
- [ ] **Step 5: Commit.** `pvefake: unlisted paths have PBPProofer's privileges (#107)`.

### Task 3: preflight fails on missing core privileges

**Files:**
- Create: `internal/run/privileges.go`
- Modify: `internal/run/preflight.go` (new step after `Target.CheckNode`, before `Target.Prepare`)
- Modify: `cmd/pbp/main.go:612-615` (both preflight line names gain `token privileges` after `node helper`)
- Test: `internal/run/ownership_test.go` (update `TestPreflightOrder`), new tests in `internal/run/privileges_test.go`

**Interfaces:**
- Consumes: `privs.Needs`, `privs.Check`, `Missing.Describe` (Task 1). `d.Cfg.Target.Node`, `d.Cfg.Restore.Storage`, `d.Cfg.Sources.Storages`, `d.Cfg.Target.API.TokenID`.
- Produces: `func checkCorePrivileges(ctx context.Context, d Deps) error`, wrapped by preflight as `token privileges: %w`. It passes only the core needs (`Source == ""`) to `privs.Check`, so it never reads a source storage's privileges. It errors on a read error, or on the `Missing` entries, each `Describe`d and joined with `"; "`.

- [ ] **Step 1: Write the failing tests** in `internal/run/privileges_test.go`, with the run fixture:
  - `TestPreflightFailsOnMissingCorePrivilege`: `Privs["/vms"]` = the role's `/vms` list minus `VM.Backup`. `Preflight` errors with text containing `token privileges: token pbp@pve!proof lacks VM.Backup on /vms` and `pveum acl modify /vms`. No `Prepare` call is recorded on the target.
  - `TestPreflightFailsOnRestoreStorageAlsoSource`: config with `restore.storage` equal to a source storage, and that path given `[]string{}`. Preflight errors, naming `/storage/<it>`.
  - `TestPreflightIgnoresSourceStorageMisses`: `Privs["/storage/pbs"] = []string{}`. Preflight passes.
  - `TestPreflightPrivilegesReadError`: `FailOn` errors on `Privileges` for `/sdn/zones/localnetwork`. Preflight errors, containing `read the token's privileges on /sdn/zones/localnetwork`.
- [ ] **Step 2: Run them.** `go test ./internal/run/ -run 'Preflight'` fails.
- [ ] **Step 3: Implement** `checkCorePrivileges` and call it in `preflight` between `CheckNode` and `Prepare`. Update the doc comment's order list.
- [ ] **Step 4: Update the order and the line names.** Update `TestPreflightOrder` to expect the core `Privileges` calls (`/vms`, the restore storage, the node, the zone) between the helper check and `Prepare`. Update `preflightName` and `preflightReadOnlyName` to `preflight (node helper, token privileges, node version, …)`.
- [ ] **Step 5: Run the tests.** `go test ./...` passes.
- [ ] **Step 6: Commit.** `run: preflight checks the token's core privileges (#107)`.

### Task 4: runs skip unreadable source storages and warn about empty ones

**Files:**
- Modify: `internal/run/runner.go` (`discover`, `execute`, the `Run` closure)
- Modify: `internal/run/privileges.go`
- Test: `internal/run/runner_test.go`

**Interfaces:**
- Consumes: Task 1 and Task 3 (`privs.Check` with the source `Need`s only).
- Produces:
  - `discover(...)` and `execute(...)` return an extra `warns []string`. The `Run` closure appends it to `envErrs`, as `strayTaggedErrors` does. `guarded`'s signature is unchanged.
  - `func sourcePrivilegeProblem(ctx context.Context, d Deps, storage string) string` returns `""` when the token has the storage's needs. Otherwise it returns the `Describe` text, or `cannot read the token's privileges on /storage/<sid>: <err>`.
  - **Warning copy (exact):**
    - Skipped storage: `storage <sid> not read: <problem>`
    - Empty storage: `storage <sid> lists no backups for the token: it is empty, or the token cannot see them (check: pveum user token permissions <user> <token name> --path /vms/<vmid of a guest backed up there>; see docs/troubleshooting.md)`
  - When no storage could be listed **and** at least one was skipped for privileges, `errs` gains `no source storage could be read`, so the run ends `error`, not a silent warning.

- [ ] **Step 1: Write the failing tests** in `runner_test.go`. Fixture storages are `pbs` and `pbs-sync`, restore storage `local-zfs`.
  - `TestRunSkipsSourceStorageWithoutPrivileges`: `Privs["/storage/pbs"] = []string{"Datastore.Audit"}`, with backups on both storages. `ListBackups` is never called for `pbs`. The guests on `pbs-sync` are proofed. `EnvironmentErrors` contains `storage pbs not read: token pbp@pve!proof lacks Datastore.AllocateSpace on /storage/pbs`. `Severity()` is `warning`.
  - `TestRunSkipsSourceWhenPrivilegesUnreadable`: `FailOn` errors on `Privileges` for `/storage/pbs` (preflight reads only core paths, so only discovery hits it). The warning starts `storage pbs not read: cannot read the token's privileges on /storage/pbs`, and `pbs-sync` is still proofed.
  - `TestRunWarnsOnSourceStorageWithNoBackups`: `pbs` has no backups, `pbs-sync` has some. `EnvironmentErrors` contains `storage pbs lists no backups for the token`.
  - `TestRunNoWarningWhenOnlyInRangeBackups`: `pbs` holds only a backup of a guest inside the VMID range. There is no "lists no backups" warning.
  - `TestRunGuestDoesNotWarnForOtherGuestsBackups`: `OnlyVMID` set to a guest whose backups are on `pbs-sync` only, while `pbs` has backups of other guests. There is no warning for either storage.
  - `TestRunAllSourcesUnreadable`: both source storages are given `[]string{}`. `RunError` contains `no source storage could be read`, and `Severity()` is `error`.
- [ ] **Step 2: Run them.** `go test ./internal/run/ -run 'TestRun(Skips|Warns|NoWarning|Guest|AllSources)'` fails.
- [ ] **Step 3: Implement.** In `discover`, before `ListBackups` for each source storage that is not the restore storage, call `sourcePrivilegeProblem`. On a problem, add the warning and `continue`. After a successful listing, warn when `len(bs) == 0`. Thread `warns` out through `execute` to `Run`.
- [ ] **Step 4: Run the tests.** `go test ./...` passes.
- [ ] **Step 5: Commit.** `run: skip a source storage the token cannot read, warn on one that lists nothing (#107)`.

### Task 5: `pbp check` checks every source storage; `pbp setup` repairs

**Files:**
- Modify: `internal/run/privileges.go` (`CheckSources`)
- Modify: `cmd/pbp/main.go` (`checkSteps`, a new line name constant)
- Test: `internal/run/privileges_test.go`, `cmd/pbp/main_test.go`, `cmd/pbp/setup_test.go`

**Interfaces:**
- Consumes: `sourcePrivilegeProblem` (Task 4), `d.API.StorageStatus`.
- Produces:
  - `func CheckSources(ctx context.Context, d Deps) error`. For each source storage that is not the restore storage (the restore storage is already covered by preflight), in config order: `sourcePrivilegeProblem`, then `StorageStatus(node, sid)`. Errors:
    - `storage <sid>: <err>`
    - `storage <sid> is not active and enabled`
    - `storage <sid> does not hold backup content (has %q)`, checking the comma-separated `Content` for `backup`

    They are joined with `"; "`, and the result is nil when everything passes.
  - In `cmd/pbp/main.go`: `const sourcesName = "source storages (token privileges, active, backup content)"`. `checkSteps` prints `c.line(sourcesName, run.CheckSources(ctx, d))` right after the preflight line, before the final `if !c.ok`, so both lines always print. `setupCheck` reuses `checkSteps`, so a failure makes `pbp setup` print "does not pass the check" and the node lines, whose `--storages` already lists every storage.

- [ ] **Step 1: Write the failing tests.**
  - `internal/run/privileges_test.go`:
    - `TestCheckSourcesOK`: all granted, and both storages `Active, Enabled` with content `backup`. Gives nil.
    - `TestCheckSourcesReportsEach`: `pbs` lacks `Datastore.AllocateSpace`, and `pbs-sync` has content `images`. The error contains both `lacks Datastore.AllocateSpace on /storage/pbs` and `storage pbs-sync does not hold backup content (has "images")`.
  - `cmd/pbp/main_test.go`:
    - Give `newEnv`'s fake a `Storage["pbs"]` status (`Active`, `Enabled`, content `backup`), so `TestCheck` still passes. Assert that it also prints `source storages (token privileges, active, backup content): ok`.
    - `TestCheckFailsOnSourceStoragePrivileges`: `e.api.Privs["/storage/pbs"] = []string{"Datastore.Audit"}`. Exit `exitFail`, and the output contains `source storages (token privileges, active, backup content): FAIL: token pbp@pve!proof lacks Datastore.AllocateSpace on /storage/pbs`.
  - `cmd/pbp/setup_test.go`:
    - `TestSetupReprintsNodeLinesWhenTokenLacksPrivileges`, modelled on `TestSetupAlreadyCompletedButCheckFails`: after a completed first run, set `e.api.Privs["/vms"] = []string{}`. The rerun's output lacks `msgAlreadyComplete`, contains `lacks` and `on /vms`, and contains `msgPasteReply`. Exit `exitFail`.
- [ ] **Step 2: Run them.** `go test ./internal/run/ ./cmd/pbp/ -run 'CheckSources|TestCheck|TestSetupReprints'` fails.
- [ ] **Step 3: Implement** `CheckSources` and the `checkSteps` line.
- [ ] **Step 4: Run the tests.** `go test ./...` and `go vet ./...` pass.
- [ ] **Step 5: Commit.** `pbp check: check every source storage; setup reprints the node lines when the token lacks rights (#107)`.

### Task 6: docs and CHANGELOG

**Files:**
- Modify: `docs/troubleshooting.md`, `docs/security.md`, `docs/setup.md`, `docs/manual-setup.md`, `docs/operating.md`, `CHANGELOG.md`

- [ ] **Step 1: `docs/troubleshooting.md`.** Add rows (the table where the preflight/check rows live; keep the existing `| Message | Cause and fix |` style):
  - `token pbp@pve!proof lacks … on <path>`: the token's effective rights on that path miss what PBP needs. The token is privilege-separated, so it holds only the rights both `pbp@pve` and the token have. Give the `pveum acl modify` line from the message, or run the node line `pbp setup` prints. Re-creating the token by hand drops its ACLs.
  - `storage <sid> not read: …`: the same, for one source storage. The run skips it.
  - `storage <sid> lists no backups for the token`: the storage is empty, or rights narrower than PBPProofer hide its backups. PVE leaves out what the caller may not see, without an error. Compare `pvesm list <sid>` with `pveum user token permissions pbp@pve proof --path /vms/<vmid>` and `--path /storage/<sid>`.
- [ ] **Step 2: `docs/security.md` "The API token".** One sentence: `pbp check`, `pbp setup` and every run check these privileges on each path, and name a missing one with its fix.
- [ ] **Step 3: Adding a storage later.** In `docs/setup.md` and `docs/operating.md`, add a short note wherever `sources.storages` is described: adding a storage later needs its ACL, so run `pbp setup` again (the check now fails and it prints the node line) and the node line, or the `pveum` line. In `docs/manual-setup.md`, next to the token commands: deleting and re-creating the token drops its ACLs, so run the `acl modify` loop again.
- [ ] **Step 4: `CHANGELOG.md`.** Add `## Unreleased` at the top, saying only the daemon changed, with bullets for:
  - `pbp check` / `pbp setup` / preflight checking the token's privileges per path (#107);
  - runs skipping an unreadable source storage with a warning;
  - the "lists no backups" warning, mentioning the silent filtering that hid backups in #5.
- [ ] **Step 5: Check.** Every doc anchor still resolves: `grep -rn '](.*#' docs README.md` and spot-check new or changed links. `go test ./...` passes.
- [ ] **Step 6: Commit.** `docs: token privilege checks, adding storages later (#107)`.

### Task 7: verify on the test node

Run it by hand, outside the test node's scheduled PBP run. Build on the build host, not the workstation. This changes nothing in the repo.

- [ ] **Step 1: Build.** Build a test `.deb` (a `~test` version) on the build host with `packaging/build.sh`, and install it on the test node (the daemon and helper share the package). Confirm `pbp version`, then run `runuser -u pbp -- pbp check`. Every line shows `ok`, including `source storages (…): ok`.
- [ ] **Step 2: Missing core rights.** `pveum acl delete /vms --tokens 'pbp@pve!proof' --roles PBPProofer`, then run `pbp check`. It fails with `lacks VM.Allocate, VM.Audit, VM.Backup, … on /vms` and the `pveum acl modify /vms …` line. `pbp setup` (as the `pbp` user, using the existing config) prints "does not pass the check" and the node lines. Restore the ACL with the printed `pveum` line, and confirm `pbp check` is ok again.
- [ ] **Step 3: Missing source rights.** Repeat Step 2 with `/storage/<a source storage>`. `pbp check` fails on the `source storages` line, and the preflight line stays ok. Restore the ACL and confirm it is ok again.
- [ ] **Step 4: Clean up.** `pveum acl list | grep pbp` matches the list from before the test. Leave the test build installed until the release replaces it (as with 0.9.0~test.3).
