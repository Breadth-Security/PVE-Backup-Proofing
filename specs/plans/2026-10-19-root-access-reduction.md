# Root Access Reduction Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace PBP's root SSH to the Proxmox node with a restricted `pbp node` helper reached through an SSH forced command and one sudo rule, then remove root SSH entirely.

**Architecture:** A small wire protocol package (`internal/nodeproto`) is shared by both ends. The node side (`internal/node`, run as `pbp node`) parses `SSH_ORIGINAL_COMMAND`, enforces range, tag and bridge from its own `/etc/pbp/node.yaml`, and runs `pct`, `ip`, `ovs-vsctl` and `perl` behind a `System` interface. The daemon side gets a typed helper client (`internal/sshx` `Helper`) that replaces every shell command string PBP sends today.

**Tech Stack:** Go 1.26.9 (pinned), golang.org/x/crypto/ssh, gopkg.in/yaml.v3, existing Docker/nfpm packaging, Gitea Actions.

**Spec:** [specs/2026-10-19-root-access-reduction-design.md](../2026-10-19-root-access-reduction-design.md). It builds on [specs/2026-10-19-pbp-design.md](../2026-10-19-pbp-design.md). Read both before any task.

## Global Constraints

- **No root SSH path in product code.** No `root` default, no fallback, no shell-string commands to the node from `cmd/pbp` or `internal/*`. Only `test/integration` may hold an admin key (spec §8).
- **Helper subcommands are exactly:** `exec`, `bridge`, `notify`, `config`, `templates`, `version`, plus `install-templates`, which runs interactively only and is never reachable over SSH.
- **The helper never invokes a shell.** argv passes to `pct exec <vmid> -- <argv>` as a list.
- **Range, tag and bridge come from `/etc/pbp/node.yaml`.** Fields: `vmid_range: [lo, hi]`, `bridge: <name>`, `request_timeout` (default 15m). A missing or invalid file means refuse.
- **Refusal signalling:** reserved exit code `250` plus one stderr line starting `pbp-node: refused: `. Internal errors use `pbp-node: error: `, timeouts use `pbp-node: timeout`.
- **Deadline:** every daemon request carries `--deadline <unix-ms>`. The helper uses `min(deadline, now+request_timeout)` and kills the child's whole process group on expiry.
- **Versions:** daemon and helper must share major.minor, so `0.2.x` talks to `0.2.y`. A `dev` build is compatible only with `dev`, and the message says so.
- **SSH user:** `target.ssh.user` defaults to `pbp`, and config validation rejects `root` with a pointer to README "Node setup".
- **Release target `v0.2.0`.** This is a breaking change, and every document is updated (spec §9).
- **Dev tests run on the build host** (`ssh <build-host>`, Go 1.26.9 at `/usr/local/go`, use `bash -lc`). Live `pve-node` work needs owner approval at the time.

## Review Focus

1. **Injection through argv or VMID:** for example `exec 990001 -- sh -c 'id'; reboot`, `exec 990001;id`, `exec 9900010`, `exec -1`, a newline in argv, or a JSON request with extra fields. Expected: argv goes through verbatim as list elements, and any malformed VMID or request is refused with exit 250. Pinned in Task 2.
2. **A guest outside the range, untagged, or not an LXC:** a VM, a production CT at 101, or a CT in range without the `pbp` tag. Expected: refused (250) before `pct` runs, and the helper calls `pct exec` zero times. Pinned in Task 2.
3. **Deadline passing mid-command, or the client gone:** expected kill of the process group and the `pbp-node: timeout` marker. The daemon maps that to the same verdict a local command timeout gives today. Pinned in Tasks 2 and 4.
4. **Daemon and helper disagree:** different range, bridge, or minor version. Expected: `pbp check` fails naming both values. A run's preflight refuses too, and nothing is restored. Pinned in Task 6.
5. **A guest command that itself exits 250 or prints a `pbp-node:` line:** expected to be treated as a guest result, not a refusal. The marker must be the helper's own final stderr line, written only by the helper (Task 1's framing rule). Pinned in Tasks 1 and 4.

---

## File Structure

```
internal/nodeproto/proto.go          request type, encode/parse, exit code + markers, version compat
internal/node/config.go              /etc/pbp/node.yaml load+validate
internal/node/system.go              System interface (Pct, Bridge, Notify, Templates) + real impl
internal/node/helper.go              Serve(ctx, req, stdin, stdout, stderr) dispatch + validation
internal/node/install.go             install-templates (interactive root only)
internal/report/templates.go         exported Templates() (embedded files), shared by daemon and node
internal/sshx/helper.go              Helper client: typed calls over Runner, marker classification
internal/target/executors.go, isolation.go, boot.go   switch to Helper
internal/report/notify.go            Send via helper; EnsureTemplates -> CheckTemplates (hash compare)
internal/config/config.go            ssh.user default pbp, reject root
cmd/pbp/main.go                      `node` subcommand; check steps; wiring
test/nodee2e/                        end-to-end on the build host (sshd + forced command + sudo + stubs)
docs: README.md, config.example.yaml, docker-compose.yml, specs/*, test/integration/README.md, packaging README section
```

---

### Task 1: Wire protocol (`internal/nodeproto`)

**Files:** Create `internal/nodeproto/proto.go`, `internal/nodeproto/proto_test.go`

**Interfaces — Produces:**
- `const ExitRefused = 250`; marker prefixes `MarkRefused = "pbp-node: refused: "`, `MarkError = "pbp-node: error: "`, `MarkTimeout = "pbp-node: timeout"`.
- `type Request struct { Op string; VMID int; Argv []string; Deadline time.Time }`. Op is one of `exec bridge notify config templates version`.
- `func Encode(r Request) string`: the SSH command string. Format: `<op> --deadline <unix-ms> [<vmid> -- <argv as JSON array>]`. argv travels as one JSON array so no quoting rules apply. Example: `exec --deadline 1760900000000 990001 -- ["systemctl","is-system-running","--wait"]`.
- `func Parse(s string) (Request, error)`: strict. Rejects unknown ops, a VMID that isn't a plain positive decimal of 1–9 digits, missing or extra fields, and argv that isn't a JSON string array or is empty for exec.
- `func HelperFailure(exit int, stderr string) (kind string, reason string, ok bool)`: ok only when `exit == ExitRefused` **and** the **last non-empty stderr line** starts with one of the markers. kind is `refused`, `error` or `timeout`.
- `func Compatible(a, b string) bool`: same `MAJOR.MINOR` (strip a leading `v` and any `~`/`-` pre-release suffix). `dev` is compatible only with `dev`.
- Reply types shared by both ends:
  - `type BridgeState struct { Name string; OVS bool; DisableIPv6 string; Addrs json.RawMessage; Members []string }`
  - `type NodeConfig struct { VMIDRange [2]int; Bridge string; RequestTimeout time.Duration; Version string }`

  Both have JSON tags in snake_case.

- [ ] **Step 1: Write failing tests:**
  - Encode/Parse round-trip for each op.
  - Parse rejects: `exec 990001;id`, `exec --deadline x 1 -- []`, `exec --deadline 1 01 -- ["a"]`, `exec --deadline 1 9900010000 -- ["a"]`, `exec --deadline 1 990001 -- "sh"`, `exec --deadline 1 990001 -- []`, `bridge --deadline 1 990001`, `reboot`, an empty string.
  - Parse keeps an argv element like `"sh -c 'id'; reboot"` intact as one element.
  - HelperFailure: `(250, "x\npbp-node: refused: out of range")` gives refused; `(250, "guest says pbp-node: refused: x\nmore")` is not a failure (marker not on the last line); `(2, "pbp-node: refused: x")` is not a failure; `(250, "")` is not a failure.
  - Compatible: `0.2.0`/`0.2.5` true; `0.2.0`/`0.3.0` false; `0.2.0~rc.1`/`0.2.0` true; `dev`/`dev` true; `dev`/`0.2.0` false.
- [ ] **Step 2:** `go test ./internal/nodeproto/` fails (undefined).
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test ./internal/nodeproto/` passes.
- [ ] **Step 5:** Commit `Add the pbp node wire protocol`.

---

### Task 2: Node helper (`internal/node`)

**Files:** Create `internal/node/config.go`, `system.go`, `helper.go`, `install.go` and their tests. Create `internal/report/templates.go`, moving the `//go:embed` there and exporting `func Templates() map[string][]byte` (file name to contents). `notify.go` then uses it.

**Interfaces:**
- **Consumes:** `nodeproto.*`, `report.Templates()`.
- **Produces:**
  - `type Config struct { VMIDRange [2]int; Bridge string; RequestTimeout time.Duration }`, plus `func LoadConfig(path string) (Config, error)`. Path `/etc/pbp/node.yaml`; default timeout 15m. Validation matches the daemon's range rules, the bridge must be non-empty, and the timeout must be ≥ 30s.
  - `type System interface { PctConfig(ctx, vmid int) (map[string]string, error); PctExec(ctx, vmid int, argv []string, stdin io.Reader, stdout, stderr io.Writer) (exit int, err error); BridgeState(ctx, bridge string) (nodeproto.BridgeState, error); Notify(ctx, payload []byte) error; TemplateHashes() (map[string]string, error) }`. The real implementation execs `pct`, `ip -j`, `ovs-vsctl`, `perl` and reads `/proc/sys/...` with `exec.CommandContext`, a new process group (`Setpgid`), and a kill of the group on ctx done.
  - It returns `nodeproto.BridgeState` (Task 1). The helper detects OVS itself: `ovs-vsctl br-exists <bridge>` exits 0.
  - `func Serve(ctx context.Context, cfg Config, sys System, version string, request string, stdin io.Reader, stdout, stderr io.Writer) int`. Returns the process exit code. Only `Serve` writes markers, and always as the final stderr line.
  - `func InstallTemplates(dir string) error`, writing `report.Templates()` into `/etc/pve/notification-templates/default/` (mode 0644).
- **Rules in Serve:**
  - Parse with `nodeproto.Parse`; on failure, refuse.
  - Deadline = `min(req.Deadline, now+cfg.RequestTimeout)`. Past the deadline, write the timeout marker.
  - `exec`: in range, `PctConfig` succeeds (VMs fail it, so a VM is refused), and `tags` contains `pbp` (shared `sanitize.SplitList`). Only then call `PctExec`, passing stdout and stderr straight through and returning the guest's exit code. If the guest exits 250, the helper does **not** add a marker, so it stays a guest result.
  - `bridge`: JSON of `BridgeState` for `cfg.Bridge`.
  - `notify`: payload `{severity, data, fields}`. Severity must be one of info, notice, warning or error. Force `fields.type = "pbp"`, then call `Notify`.
  - `config`: JSON `{vmid_range, bridge, request_timeout, version}`.
  - `templates`: JSON of the sha256 of each installed template file. A missing file maps to `""`.
  - `version`: prints the version.

- [ ] **Step 1: Write failing tests** with a fake System that records calls:
  - Review Focus 1 and 2, as table tests asserting exit 250, the refused marker on the last line, and **zero** PctExec calls.
  - `exec` happy path passes argv verbatim (including `"sh -c 'id'; reboot"` as one element), plus stdin, stdout, stderr and the exit code.
  - Guest exit 250 with no helper marker.
  - Deadline in the past gives the timeout marker.
  - A fake PctExec that blocks until ctx is done gives the timeout marker within 1s of a 200ms deadline (Review Focus 3).
  - `notify` with severity `debug` is refused, and `fields.type:"other"` is forced to `pbp`.
  - LoadConfig rejects a missing file, a bad range, an empty bridge, and `request_timeout: 5s`.
  - Real-System tests use only the pure parts (BridgeState JSON assembly from canned command output).
- [ ] **Step 2:** The tests fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test -race ./internal/node/ ./internal/report/` passes.
- [ ] **Step 5:** Commit `Add the restricted pbp node helper`.

---

### Task 3: `pbp node` CLI wiring

**Files:** Modify `cmd/pbp/main.go` and `cmd/pbp/main_test.go`.

**Interfaces:**
- **Consumes:** `node.Serve`, `node.LoadConfig`, `node.InstallTemplates`.
- **Behaviour:**
  - `pbp node` reads `SSH_ORIGINAL_COMMAND`. If it is empty or unset, it refuses: exit 250 with the marker "no request (pbp node runs only as an SSH forced command)".
  - Otherwise it runs `Serve` with config `/etc/pbp/node.yaml`, overridable by the `PBP_NODE_CONFIG` environment variable for tests, and the real System.
  - `pbp node install-templates` runs only when `SSH_ORIGINAL_COMMAND` is unset and the effective UID is 0. Otherwise it refuses, exit 2, usage. On success it prints the paths it wrote.
  - Add `node` to the usage text.

- [ ] **Step 1: Write failing tests:**
  - Empty `SSH_ORIGINAL_COMMAND` gives exit 250 and the marker.
  - `install-templates` with `SSH_ORIGINAL_COMMAND` set is refused.
  - `install-templates` as non-root is refused.
  - With `PBP_NODE_CONFIG` pointing at a temp config and `SSH_ORIGINAL_COMMAND=version --deadline <future>`, it prints the version.
- [ ] **Step 2–4:** Fail, then implement, then pass.
- [ ] **Step 5:** Commit `Wire the pbp node subcommand`.

---

### Task 4: Daemon helper client (`internal/sshx`)

**Files:** Create `internal/sshx/helper.go` and `helper_test.go`. Modify `ssh.go`: remove `bounded()` and its tests once Tasks 5–6 no longer need it, and update the `Run` doc.

**Interfaces:**
- **Consumes:** `sshx.Runner`, `nodeproto`.
- **Produces:**
  - `type Helper struct { R Runner }`
  - `func (h Helper) Exec(ctx, vmid int, argv []string) (Result, error)`
  - `func (h Helper) Bridge(ctx) (nodeproto.BridgeState, error)`
  - `func (h Helper) Notify(ctx, payload []byte) error`
  - `func (h Helper) Config(ctx) (nodeproto.NodeConfig, error)`
  - `func (h Helper) Templates(ctx) (map[string]string, error)`
  - `func (h Helper) Version(ctx) (string, error)`
  - Errors: `var ErrRefused, ErrHelper, ErrTimeout`, wrapped with the reason from `HelperFailure`.
  - Deadline: from ctx; with no ctx deadline, use now+24h.
  - Mapping: an `Exec` timeout returns `context.DeadlineExceeded`-wrapping `ErrTimeout`, so the probe guard classifies it exactly as a local timeout today.

- [ ] **Step 1: Write failing tests** with `sshx.Fake`:
  - The command string sent equals `nodeproto.Encode` with the deadline from ctx.
  - Each failure kind maps to its error.
  - Review Focus 5: a guest exit 250 whose stderr marker isn't last gives a plain Result.
  - A Timeout error satisfies `errors.Is(err, context.DeadlineExceeded)`.
  - `Bridge` decodes JSON.
- [ ] **Step 2–4:** Fail, then implement, then pass.
- [ ] **Step 5:** Commit `Add the typed pbp node helper client`.

---

### Task 5: Target and report use the helper

**Files:** Modify `internal/target/executors.go`, `isolation.go`, `boot.go`, `existing.go` (the constructor takes a `sshx.Helper`), `internal/target/bridgetest/bridgetest.go` (it now produces helper `bridge` JSON answers), `internal/report/notify.go`, and the tests in those packages.

**Interfaces:**
- **Consumes:** `sshx.Helper` (Task 4).
- **Changes:**
  - `lxcExec.Exec` → `Helper.Exec(ctx, vmid, argv)`. A refused exec maps to `ErrOutOfRange` or `ErrNotOwned`; the reason text decides which.
  - `checkLiveIsolation` → `Helper.Bridge`, with the same verdict rules over `BridgeState`. OVS/Linux detection now comes from `BridgeState.OVS`; the API type is still checked as today.
  - `waitLXC` keeps its logic through the new executor.
  - `PVENotifier.Send` → `Helper.Notify`.
  - `EnsureTemplates` becomes `CheckTemplates(ctx) error`, comparing `Helper.Templates` with the sha256 of `report.Templates()`. A mismatch or missing file gives an error naming each file plus `run "pbp node install-templates" as root on the node`.
  - Update `report.Notifier` and every caller, including preflight.
- [ ] **Step 1:** Adapt existing tests to helper-level fakes. **Every existing safety assertion stays.** Add tests:
  - A refused exec gives `ErrOutOfRange`.
  - CheckTemplates fails on a hash mismatch with the install hint.
  - Isolation verdicts are identical for the existing bridgetest cases.
- [ ] **Step 2–4:** Fail, then implement, then pass. Run `go test -race ./...`.
- [ ] **Step 5:** Commit `Route node access through the pbp node helper`.

---

### Task 6: Config, `pbp check`, preflight

**Files:** Modify `internal/config/config.go` and its tests, `internal/run/preflight.go`, `cmd/pbp/main.go` and its tests.

**Interfaces / behaviour:**
- **Config:** `target.ssh.user` defaults to `pbp`. Validation rejects `root` with: `target.ssh.user: root is no longer supported; set up the pbp node helper (README "Node setup")`.
- **New `target.Target` method `CheckNode(ctx) error`** (ExistingNode implements it via Helper), run as the first preflight step. In order, each failure names the README section:
  1. `Version` is reachable and `Compatible` with the daemon version.
  2. `Config` matches `target.vmid_range` and `target.bridge`, naming both values on a mismatch.
  3. `request_timeout ≥ restore.probe_timeout`, otherwise a logged warning only (not a failure).
  4. Refusal probe: `Exec(ctx, vmidRange[1]+1, ["true"])` must return `ErrRefused` (out of range). Anything else is a failure: "helper did not refuse an out-of-range exec".
- **`pbp check`** prints each node step as a line (`node helper version: ok (0.2.0)`, and so on).
- [ ] **Step 1: Write failing tests:** a root user rejected; the default is `pbp`; CheckNode for each failing step (incompatible version, range mismatch, bridge mismatch, refusal probe not refused); a low request_timeout warns; preflight stops at a CheckNode failure before the janitor.
- [ ] **Step 2–4:** Fail, then implement, then pass.
- [ ] **Step 5:** Commit `Check the node helper in preflight and pbp check`.

---

### Task 7: End-to-end over real SSH on the build host (`test/nodee2e`)

**Files:** Create `test/nodee2e/Dockerfile` (debian:trixie with openssh-server, sudo, and the `pbp` user set up exactly per README "Node setup"); stub scripts `pct`, `ovs-vsctl` and `perl` that log their argv; `test/nodee2e/run.sh`; `test/nodee2e/nodee2e_test.go` (build tag `nodee2e`).

**Behaviour under test** (one container per run, the PBP binary built for linux/amd64 and copied in, key-based SSH from the test to the container):
- `Helper.Version`, `Config`, `Bridge`, `Templates` and `Exec` on an in-range, `pbp`-tagged stub CT all work. The stub `pct` answers `config` with `tags: pbp`.
- An out-of-range `Exec` is refused, and the stub `pct` log shows no `exec` call.
- With the **same key, as an attacker**:
  - `ssh pbp@c id`, `ssh pbp@c bash`, `ssh pbp@c 'exec --deadline … 990001 -- ["id"];id'` and `ssh -t pbp@c` are all refused or fail.
  - No `id` output ever appears.
  - `sudo -l` as `pbp` shows only `/usr/bin/pbp node`.
  - `pbp node install-templates` over SSH is refused.
- A deadline expiry kills a stub `pct exec` that sleeps for 60s within the deadline plus 5s (Review Focus 3).
- [ ] **Step 1:** Write `run.sh`. It rsyncs the repo to `<build-host>:/root/pbp`, builds the image there, and runs `go test -tags nodee2e ./test/nodee2e/ -v` with `bash -lc`.
- [ ] **Step 2:** Run it. Expect all PASS, and paste the output in the report.
- [ ] **Step 3:** Commit `Add the node helper end-to-end test over real SSH`.

---

### Task 8: Documentation, packaging, integration-test admin key

**Files:**
- `README.md`: rewrite the access/security, Setup and Troubleshooting sections; add a new "Node setup" section with the 7 steps from spec §5, including `usermod -s /bin/sh pbp`, the `authorized_keys` options line, sudoers with `visudo -cf`, the `UsePAM` note, and the `apt purge` warning. Add migration notes for 0.1 → 0.2.
- `config.example.yaml`: `user: pbp`.
- `docker-compose.yml`: comments.
- `specs/2026-10-19-pbp-design.md`: §3, §8.3 and §11 (root removed; link to the new design).
- `packaging/` README section: node install.
- `test/integration/integration_test.go` and its README: the fixtures use `PBP_IT_ADMIN_SSH_KEY` and `PBP_IT_ADMIN_KNOWN_HOSTS` through the test's own SSH client; PBP under test uses `target.ssh.user: pbp`. Document adding the admin key to `pve-node` before a live run and removing it afterwards.
- **No change to the `.deb` postinst** (nologin stays the default, per spec §5).
- [ ] **Step 1:** Grep check: `grep -rnE 'user: root|root SSH|as root' README.md config.example.yaml docker-compose.yml packaging specs/2026-10-19-pbp-design.md` shows only the intended mentions (the admin key in the test README, and the `install-templates` and `visudo` steps run as root).
- [ ] **Step 2:** `go vet -tags integration ./...` and `go test ./...` pass. The Docker build passes **on the build host**.
- [ ] **Step 3:** Commit `Document the pbp node setup and remove root SSH from the docs`.

---

### Task 9: Live verification on pve-node (owner approval required at the time)

- [ ] **Step 1:** Ask the owner to approve, then on `pve-node`:
  - set up the helper per README;
  - add the test-only admin key;
  - remove the old PBP root key from `/root/.ssh/authorized_keys`.
- [ ] **Step 2:** `pbp check` passes every node step.
- [ ] **Step 3:** Run the full integration suite (the same environment as before, plus the admin key variables). Expect 10/10, the range empty, and the backups annotated.
- [ ] **Step 4:** Ask the owner whether to remove the admin key now. Record the result in the ledger.
