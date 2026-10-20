# Guided Setup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `pbp setup` (PBP host) and `pbp node setup` / `pbp node uninstall` (PVE node) so an admin goes from nothing to a passing `pbp check` by pasting between two terminals, with the node ending up exactly as README "Node setup" describes.

**Architecture:** A new `internal/nodesetup` package owns the node's desired state (the single source of the sudoers text, key line, `node.yaml`, role privileges), a planner that compares it with the disk, an applier, a self-test and uninstall. A new `internal/hostsetup` package owns the host side: questions, config writer, key generation, the printed node lines and applying the node's reply. The two meet in `internal/nodeproto` (the node line and the reply block). Node-side checks reuse the daemon's own isolation and range rules, extracted into pure functions, and read PVE through `pvesh` with the existing `pve.Client` types.

**Tech Stack:** Go 1.26.9; `gopkg.in/yaml.v3` (Node API for comment-preserving edits); `golang.org/x/crypto/ssh` (key parse/generate); `golang.org/x/sys/unix` (terminal detection and echo off; promote from indirect). No new modules.

**Spec:** `specs/2026-10-20-setup-subcommand-design.md`

## Global Constraints

- Exact message on a run that finds everything correct: `setup has already been completed` (node and host).
- Node end state identical to README "Node setup" steps 1–6: `/etc/pbp` `0750 root:pbp`; `/etc/pbp/node.yaml` `0640 root:pbp` with `vmid_range`, `bridge`, `request_timeout: 15m`; `pbp` shell `/bin/sh`, home `/var/lib/pbp` `0700 pbp`; `/var/lib/pbp/.ssh` `0755 root:root`; `authorized_keys` one line `restrict,command="sudo /usr/bin/pbp node" <key>` `0644 root:root`; `/etc/sudoers.d/pbp` `0440 root:root` with exactly:
  ```
  Defaults:pbp env_reset
  Defaults:pbp env_keep = "SSH_ORIGINAL_COMMAND SSH_CONNECTION"
  pbp ALL=(root) NOPASSWD: /usr/bin/pbp node
  ```
- Token: role `PBPProofer` with privileges `VM.Allocate,VM.Audit,VM.Backup,VM.Config.HWType,VM.Config.Network,VM.Config.Options,VM.PowerMgmt,VM.GuestAgent.Audit,VM.GuestAgent.Unrestricted,Datastore.AllocateSpace,Datastore.Audit,Sys.Audit,SDN.Use`; user `pbp@pve`; token `pbp@pve!proof` `--privsep 1`; ACLs for user and token on `/vms`, `/storage/<each>`, `/nodes/<node>`, `/sdn/zones/localnetwork`.
- Node setup/uninstall run only as root, interactively: refused when `SSH_ORIGINAL_COMMAND` or `SUDO_USER` is set or stdin is not a terminal. `--yes` skips the prompt, never the gate.
- Setup never creates the bridge, never reloads networking, never touches root's `authorized_keys`, `/etc/ssh/sshd_config`, or any guest.
- A file that differs is never overwritten without `--replace`; with it, the old file is copied to `<file>.pbp-backup-<YYYYMMDDTHHMMSS>` first.
- Writes are temp-file-in-same-dir + chmod/chown + rename, except the templates on `/etc/pve` (written as `node.InstallTemplates` does; no mode/owner checks).
- Apply order: `/etc/pbp` + `node.yaml` → shell → sudoers → templates → role/user/token/ACLs → `authorized_keys` last. Uninstall order: sudoers and `authorized_keys` first.
- Node commands by absolute path with the helper's minimal env (`node.cmdEnv`): `/usr/bin/pvesh`, `/usr/sbin/pveum`, `/usr/sbin/visudo`, `/usr/sbin/usermod`, `/usr/sbin/sshd`, `/usr/bin/sudo`, `/usr/bin/env`.
- No secret in argv, logs, the audit line, or echoed to the host terminal. The token secret appears only inside the reply block on the node's screen and in the host's token file.
- Releases are public: `https://git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/releases/download/v<tag>/…`; tag = version with `~` → `-`.
- Release version 0.3.0 (`VERSION`), CHANGELOG `## v0.3.0`.
- Tests that need Docker, root, PVE or SSH run on dev-scratch (`ssh dev-scratch`, Go via `bash -lc`), never the workstation. pve1: only VMIDs 990000–990009; PBP's own setup there may be removed and changed; root's `authorized_keys`, sshd config, networking and other guests are off-limits.

## Review Focus

1. A pubkey or reply pasted with a trailing newline, CRLF line endings, terminal indentation, or surrounding shell prompt text must still be accepted; a pubkey holding two keys must be refused. (Tasks 2, 3)
2. An `/etc/sudoers.d/pbp` or `authorized_keys` hand-written from the 0.2 README with different whitespace or a trailing comment must count as already correct, not "differs". (Task 3)
3. A node setup interrupted after sudoers but before `authorized_keys` must leave no authorized key, and a rerun must complete. (Task 5)
4. A host directory from a complete 0.2 manual install (valid `config.yaml`, secrets, `known_hosts`) must get no questions and a node line without `--create-token`. (Task 8)
5. Docker created `config.yaml` as a directory (a bind mount run before the file existed): setup must stop with a message saying to remove the directory, not crash or write inside it. (Task 8)

---

### Task 1: Extract the isolation and range rules; read PVE through pvesh

**Files:**
- Create: `internal/target/rules.go`, `internal/target/rules_test.go`, `internal/pve/local.go`, `internal/pve/local_test.go`
- Modify: `internal/target/existing.go` (`notIsolated`, `CheckRangeClear`), `internal/target/isolation.go` (`checkLiveIsolation`, `proofPort`), `internal/pve/client.go` (`do`)

**Interfaces:**
- Produces:
  - `func NotIsolated(i pve.NetIface) string` — the current `notIsolated`, exported, unchanged.
  - `func LiveIsolationProblem(st nodeproto.BridgeState, bridge string, apiOVS bool, lo, hi int) (problem string, unverifiable bool)` — the body of `checkLiveIsolation` after the helper call, as a pure function; `unverifiable` marks the "cannot be verified" cases.
  - `func ForeignInRange(guests []pve.GuestRef, lo, hi int) []string` — keys of untagged guests in range.
  - `func NewLocal(pvesh string, run func(ctx context.Context, name string, args []string) (stdout []byte, err error)) *Client` — a `Client` whose GETs run `<pvesh> get <path> [--<k> <v>…] --output-format json`. Non-GET returns an error.

- [ ] **Step 1: Tests.** `TestForeignInRange` (tagged in range ignored, untagged in range listed, untagged out of range ignored); `TestLiveIsolationProblem` table covering each existing message case (IPv6 enabled, address present, foreign member, member `tap990003i0` allowed, `tap105i0` refused, wrong bridge name and OVS mismatch → `unverifiable`); `TestLocalClientGuests` / `TestLocalClientNetIface` / `TestLocalClientVersion` with a fake `run` returning canned pvesh JSON and asserting the argv (`get /cluster/resources --type vm --output-format json`).
- [ ] **Step 2: Run** `go test ./internal/target/ ./internal/pve/` — FAIL (undefined).
- [ ] **Step 3: Implement.** Move logic, make `ExistingNode` call the pure functions so its behaviour is unchanged. Give `Client` a request seam used by `do` so `NewLocal` reuses every typed method's decoding.
- [ ] **Step 4: Run** `go test -race ./...` — PASS, including the untouched `existing_test.go`.
- [ ] **Step 5: Commit** "Extract isolation and range rules; read PVE locally through pvesh".

### Task 2: Node line and reply block in nodeproto

**Files:**
- Create: `internal/nodeproto/setup.go`, `internal/nodeproto/setup_test.go`

**Interfaces:**
- Produces:
  ```go
  type SetupArgs struct {
      VMIDRange [2]int; Bridge, PubKey, DaemonVersion string
      Storages []string; CreateToken bool
  }
  func (a SetupArgs) Line() string // "pbp node setup --vmid-range 990000-990049 --bridge vmbr99 --pubkey '<key>' --storages a,b --daemon-version 0.3.0 [--create-token]", POSIX single-quoted where needed
  type SetupReply struct {
      Format int `json:"format"` // 1
      Node, HelperVersion string; VMIDRange [2]int; Bridge, PubKey string
      HostKey string        // "ssh-ed25519 AAAA…" (no comment)
      APIFingerprint string // "AB:CD:…" SHA-256 of the DER cert, uppercase, colon-separated
      TokenID string; TokenSecret string `json:"token_secret,omitempty"`
  }
  const ReplyBegin = "-----BEGIN PBP NODE REPLY-----"
  const ReplyEnd   = "-----END PBP NODE REPLY-----"
  func EncodeReply(r SetupReply) (string, error) // markers + base64(JSON) wrapped at 64 columns
  func ReadReply(r io.Reader) (SetupReply, error) // ignores text before ReplyBegin; stops at ReplyEnd; max 64 KiB; strict JSON (DisallowUnknownFields); Format must be 1
  func ParsePubKey(s string) (string, error) // trims; exactly one ssh-ed25519 key; returns "ssh-ed25519 AAAA… [comment]" normalized
  ```

- [ ] **Step 1: Tests.** `TestSetupLineRoundTrip` (split the line with a POSIX shell-word splitter in the test, assert flags/values; a pubkey comment with `'` is quoted correctly); `TestReplyRoundTrip`; `TestReadReplyTolerant` — CRLF endings, leading spaces on each line, prompt text before and after the block (Review Focus 1); `TestReadReplyRejects` — missing end marker, bad base64, unknown field, format 2, oversize; `TestParsePubKey` — trailing `\n` and `\r\n` accepted, RSA refused, two keys refused, garbage refused.
- [ ] **Step 2: Run** `go test ./internal/nodeproto/` — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit** "Add the setup line and reply block to nodeproto".

### Task 3: Node desired state and planner

**Files:**
- Create: `internal/nodesetup/state.go`, `internal/nodesetup/env.go`, `internal/nodesetup/plan.go`, `internal/nodesetup/plan_test.go`, `internal/nodesetup/docs_test.go`

**Interfaces:**
- Consumes: `nodeproto.SetupArgs` (Task 2), `node.LoadConfig` semantics for `node.yaml`.
- Produces:
  ```go
  const SudoersText = "Defaults:pbp env_reset\nDefaults:pbp env_keep = \"SSH_ORIGINAL_COMMAND SSH_CONNECTION\"\npbp ALL=(root) NOPASSWD: /usr/bin/pbp node\n"
  const RoleName = "PBPProofer"; const UserID = "pbp@pve"; const TokenName = "proof"; const TokenID = "pbp@pve!proof"
  var RolePrivs = []string{ /* the 13 privileges, in the Global Constraints order */ }
  func AuthorizedKeysLine(pubKey string) string
  func NodeYAML(lo, hi int, bridge string) []byte // vmid_range, bridge, request_timeout: 15m
  func ACLPaths(node string, storages []string) []string

  // Env is every outside effect, so tests run unprivileged on a temp root.
  type Env struct {
      Root   string // "/" in production; every path is joined under it
      Run    func(ctx context.Context, name string, args []string, stdin []byte) (stdout, stderr string, exit int, err error)
      Chown  func(path string, uid, gid int) error
      Lookup func(user string) (uid, gid int, home, shell string, err error)
      Now    func() time.Time
  }
  type Status int // Correct, Create, Differs
  type Item struct { Name, Path string; Status Status; Diff string }
  type Plan struct { Items []Item }
  func (p Plan) Complete() bool // every item Correct
  func BuildPlan(ctx context.Context, env Env, a nodeproto.SetupArgs, node string) (Plan, error)
  ```
- Comparison rules (spec §5.3): `node.yaml` by parsed `vmid_range` and `bridge` (a different `request_timeout` is Correct); `authorized_keys` exactly one non-empty non-`#` line equal to the desired line after trimming; sudoers line-by-line after trimming and dropping blank/`#` lines; templates by hash as `report`/`CheckTemplates` does, no mode/owner; role by privilege set (extra or missing → Differs); user, token and each ACL present → Correct else Create; everything else mode, owner and content exactly. Role/token/ACL items appear only when `a.CreateToken` or `a.Storages` is non-empty; the token item appears only with `CreateToken`.

- [ ] **Step 1: Tests** (fake `Env` on `t.TempDir()`, fake `Run` answering `pveum … --output-format json`): `TestPlanFreshNode` (all Create, in apply order); `TestPlanComplete` (a temp root laid out exactly per Global Constraints → `Complete()`); `TestPlanToleratesHandWritten` — sudoers with extra spaces and a `# comment`, `authorized_keys` with trailing newline and a comment line, `node.yaml` with comments and `request_timeout: 20m` → all Correct (Review Focus 2); `TestPlanDiffers` — sudoers with a widened `env_keep`, a second key line, wrong mode `0640` on sudoers, role with an extra privilege → Differs with a non-empty `Diff`; `TestDocsMatchState` — README.md contains `SudoersText` verbatim, the `restrict,command="sudo /usr/bin/pbp node"` prefix, and `strings.Join(RolePrivs, ",")`.
- [ ] **Step 2: Run** `go test ./internal/nodesetup/` — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit** "Add the node setup desired state and planner".

### Task 4: Node pre-checks

**Files:**
- Create: `internal/nodesetup/checks.go`, `internal/nodesetup/checks_test.go`

**Interfaces:**
- Consumes: Task 1 (`pve.NewLocal`, `target.NotIsolated`, `target.LiveIsolationProblem`, `target.ForeignInRange`), `node.System.BridgeState`, `nodeproto.Compatible`, `nodeproto.ParsePubKey`.
- Produces: `func Check(ctx context.Context, env Env, api *pve.Client, sys node.System, a nodeproto.SetupArgs, helperVersion string) (node string, err error)` — returns the PVE node name (short hostname, confirmed present in `/nodes`); the error text is one plain sentence plus what to do.

Order and messages: PVE major ≥ 9; `Compatible(helperVersion, a.DaemonVersion)` ("this node has pbp X, the PBP host runs Y: install pbp Y on the node first"); pubkey valid; bridge via `api.NetIface` — on a 404/absent bridge print the stanza below and the UI path "Datacenter → <node> → System → Network → Create → Linux Bridge (no ports, no IPv4/IPv6)", then stop; `NotIsolated` and `LiveIsolationProblem` (unverifiable is also a stop); `ForeignInRange` over `api.Guests`; `sshd -T` contains `usepam yes`; with storages, each `api.StorageStatus` succeeds.

```
auto vmbr99
iface vmbr99 inet manual
        bridge-ports none
        bridge-stp off
        bridge-fd 0
        post-up echo 1 > /proc/sys/net/ipv6/conf/$IFACE/disable_ipv6
```
(The bridge name is substituted; match the README "Create the isolated bridge" stanza exactly — read it from README step 2 and add it to `TestDocsMatchState`.)

- [ ] **Step 1: Tests** with fakes: each failure in order produces its message and stops before the next probe; a range holding `pbp`-tagged guests and a bridge with `tap990001i0` passes (spec: rerun on a node in use); missing bridge prints the stanza with the configured name.
- [ ] **Step 2–4:** Run FAIL → implement → `go test -race ./internal/nodesetup/` PASS.
- [ ] **Step 5: Commit** "Check the node before setup".

### Task 5: Apply, backups, token

**Files:**
- Create: `internal/nodesetup/apply.go`, `internal/nodesetup/apply_test.go`

**Interfaces:**
- Consumes: Task 3 `Plan`, `Env`; `node.InstallTemplates(dir)`.
- Produces: `type ApplyOpts struct{ Replace, RotateToken bool }`; `func Apply(ctx context.Context, env Env, p Plan, a nodeproto.SetupArgs, node string, o ApplyOpts) (tokenSecret string, err error)`.

Rules: refuse (error naming the items) if any item is Differs and `!o.Replace`; backup before replacing; sudoers temp file `visudo -cf`, rename, `visudo -c`, restore backup (or remove) on failure; `usermod -s /bin/sh pbp`; token: create if absent (`pveum user token add pbp@pve proof --privsep 1 --output-format json`, secret from `value`), with `RotateToken` remove then add; role add/modify (`--privs` full list, replacing), user add if absent, `pveum acl modify <path> --users pbp@pve --tokens pbp@pve!proof --roles PBPProofer` per missing path; `authorized_keys` last. The secret never appears in an error or in `Run` argv.

- [ ] **Step 1: Tests:** `TestApplyFresh` (temp root ends `Complete()` per a second `BuildPlan`; `Run` saw the commands in the Global Constraints order; `authorized_keys` written last by recording write order); `TestApplyRefusesDiffers`; `TestApplyReplaceBacksUp` (backup file exists with the old content, name matches `.pbp-backup-\d{8}T\d{6}$`); `TestApplyVisudoFailureRestores`; `TestApplyInterrupted` — fake `Run` fails at the token step → `authorized_keys` absent, rerun with a working `Run` completes (Review Focus 3); `TestApplyExistingTokenNoSecret`; `TestApplyRotate`.
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5: Commit** "Apply node setup with backups and the API token".

### Task 6: Self-test, reply, uninstall

**Files:**
- Create: `internal/nodesetup/selftest.go`, `internal/nodesetup/reply.go`, `internal/nodesetup/uninstall.go`, tests for each

**Interfaces:**
- Produces:
  - `func SelfTest(ctx context.Context, env Env, a nodeproto.SetupArgs) error` — runs `/usr/bin/sudo -u pbp /usr/bin/env SSH_ORIGINAL_COMMAND=<req> /usr/bin/sudo -n /usr/bin/pbp node` for a `version` request (must exit 0 and print a version) and an `exec` at `a.VMIDRange[1]+1` with argv `["true"]` (must exit 250 with refusal code `out-of-range` via `nodeproto.HelperFailure`); then `BuildPlan` must be `Complete()`.
  - `func BuildReply(env Env, a nodeproto.SetupArgs, node, helperVersion, tokenSecret string) (nodeproto.SetupReply, error)` — host key from `/etc/ssh/ssh_host_ed25519_key.pub`, fingerprint from `/etc/pve/local/pveproxy-ssl.pem` else `/etc/pve/local/pve-ssl.pem`.
  - `func BuildUninstallPlan(ctx context.Context, env Env, removeToken bool) (Plan, error)`; `func Uninstall(ctx context.Context, env Env, p Plan, removeToken bool) error` — order: sudoers (then `visudo -c`), `authorized_keys` (+ `.ssh` if empty), `node.yaml`, shell → `/usr/sbin/nologin`; with `removeToken`: token, ACLs, user, role. A file whose content is not what setup writes is backed up first. Templates and the package untouched. Nothing to remove → plan empty.
- [ ] **Step 1: Tests:** self-test passes/fails on canned `Run` output, failure names which probe; reply reads a fixture cert and host key and computes the expected fingerprint (`openssl x509 -fingerprint -sha256` value hard-coded in the test); uninstall order recorded; a hand-edited sudoers is backed up before removal; second uninstall plan is empty.
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5: Commit** "Self-test, reply block and uninstall for node setup".

### Task 7: `pbp node setup` and `pbp node uninstall` commands

**Files:**
- Create: `cmd/pbp/nodesetup.go`, `cmd/pbp/nodesetup_test.go`, `internal/nodesetup/term.go` (terminal check and yes/no prompt via `golang.org/x/sys/unix`)
- Modify: `cmd/pbp/node.go` (`nodeMain` dispatch, usage), `cmd/pbp/main.go` (`app` seams: `isTerminal func() bool`, `nodeSetupEnv nodesetup.Env`), `go.mod` (x/sys direct)

**Interfaces:**
- Consumes: Tasks 2–6.
- Flags: `setup`: `--vmid-range lo-hi`, `--bridge`, `--pubkey`, `--storages a,b`, `--daemon-version`, `--create-token`, `--rotate-token` (implies create), `--dry-run`, `--yes`, `--replace`. `uninstall`: `--remove-token`, `--dry-run`, `--yes`.
- Flow (setup): gate → `Check` → `BuildPlan` → if `Complete()` and no `--rotate-token`: print `setup has already been completed`, print the reply (no secret), exit 0 → print plan (each item with Correct/Create/Differs and diffs) → `--dry-run` exits 0 → prompt `Apply these changes? [y/N]` unless `--yes` → `Apply` → `SelfTest` → print "Copy this block now; the token secret is not shown again." (only when a secret is present) and the reply. Exit codes: existing `exitOK`/`exitFail`/`exitUsage`; a refused gate is `exitUsage` with the same wording style as `install-templates`.
- [ ] **Step 1: Tests:** gate refusals (SSH_ORIGINAL_COMMAND set, SUDO_USER set, non-root, non-terminal — each, also with `--yes`); `--dry-run` writes nothing; already-complete prints exactly `setup has already been completed`; `N` at the prompt writes nothing; full flow on a fake `Env` prints a reply that `nodeproto.ReadReply` decodes.
- [ ] **Step 2–4:** FAIL → implement → `go test -race ./cmd/pbp/` PASS.
- [ ] **Step 5: Commit** "Add pbp node setup and pbp node uninstall".

### Task 8: Host setup — questions, config, key, node lines

**Files:**
- Create: `internal/hostsetup/layout.go`, `internal/hostsetup/questions.go`, `internal/hostsetup/configfile.go`, `internal/hostsetup/keys.go`, `internal/hostsetup/nodelines.go`, tests for each; `examples.go` at the repo root (`package pbp`, `//go:embed config.example.yaml` → `var ExampleConfig []byte`)
- Modify: `internal/config/config.go` (pending-setup message)

**Interfaces:**
- Produces:
  ```go
  type Layout struct { Dir string; Deb bool } // paths per spec §4.1
  func (l Layout) Config() string; ConfigPathInConfig(kind string) string // and accessors for key, pub, token, known_hosts, logs, .env
  type Answers struct { Address string; VMIDRange [2]int; Bridge, RestoreStorage string; PBSStorages []string; Schedule, TZ string }
  func Ask(in io.Reader, out io.Writer, defaultTZ string) (Answers, error) // re-asks on invalid input with the config rule's message
  func WriteConfig(l Layout, a Answers) error // from pbp.ExampleConfig via yaml.v3 Node edits; comments kept; checks.profiles and checks.guests removed (head comment points to config.example.yaml); target.node and target.api.tls_fingerprint set to ""; owner/mode per layout
  func LoadState(l Layout) (cfg config.Config, pending bool, err error) // pending = node and fingerprint both empty and everything else valid
  func EnsureKey(l Layout) (pub string, created bool, err error) // ed25519, comment "pbp", OpenSSH private-key format; never replaces
  func NodeLines(cfg config.Config, pub, version string, haveToken bool) string // install line(s) + SetupArgs.Line()
  ```
- `config` validation: an empty `target.node` reports `target.node: required (run pbp setup to finish)`; same hint on a missing fingerprint/ca_file.
- Install line: for a release version `v` (no `dev`), `a=$(dpkg --print-architecture)`, download `pbp_<v>_${a}.deb` and `SHA256SUMS` from the release URL, `sha256sum -c --ignore-missing SHA256SUMS`, `apt install ./pbp_<v>_${a}.deb`. For `dev`: the note "install a matching dev build on the node".
- [ ] **Step 1: Tests:** `TestAskDefaults` (Enter everywhere except address and PBS storages → defaults from spec §4.2); `TestAskReasks` on a bad range and bad bridge; `TestWriteConfigLoads` — written file plus placeholders filled passes `config.Load`, keeps a known example comment, has no `profiles`/`guests` entries, Docker paths `/run/secrets/…` vs deb paths `/etc/pbp/secrets/…`; `TestLoadStateManualInstall` — a complete valid 0.2-style dir: not pending, no questions, `NodeLines` has no `--create-token` (Review Focus 4); `TestConfigIsDirectory` — `config.yaml` is a directory → error "config.yaml is a directory (Docker creates one when the file is missing); remove it and run pbp setup again" (Review Focus 5); `TestEnsureKeyKeeps`; `TestNodeLinesRelease` and `TestNodeLinesDev`; `TestEmbeddedExampleMatches` (bytes equal the root file).
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5: Commit** "Host setup: questions, config, key and node lines".

### Task 9: Host setup — reply, finish, `pbp setup` command

**Files:**
- Create: `internal/hostsetup/finish.go`, `internal/hostsetup/finish_test.go`, `cmd/pbp/setup.go`, `cmd/pbp/setup_test.go`
- Modify: `cmd/pbp/main.go` (dispatch `setup`, usage)

**Interfaces:**
- Consumes: Tasks 2, 8; `a.check` logic in `cmd/pbp`.
- Produces: `func Finish(l Layout, cfg config.Config, r nodeproto.SetupReply) (needRotate bool, err error)` — refuses a reply whose range, bridge or pubkey differ from the config; writes `known_hosts` (`<host> ssh-ed25519 …`, `[host]:port` when port ≠ 22), sets `target.node` and `target.api.tls_fingerprint` in place (yaml.v3 Node edit of those two values only), writes the token file when the reply has a secret, returns `needRotate` when the reply has none and no token file exists. Docker: `.env` with `TZ=<zone>`.
- Command: `pbp setup [--dir DIR] [--deb]` (default `--dir .`, Docker layout). Flow: `LoadState` (or `Ask` + `WriteConfig`) → `EnsureKey` → if not pending and token file present and the check passes: print `setup has already been completed`, exit 0 → print `NodeLines` → prompt "Paste the reply printed by pbp node setup, then press Enter:" and read with echo off when stdin is a terminal → `Finish` → `needRotate`: print why and the node line again with `--rotate-token`, exit `exitFail` → run the check on a copy of the config whose `key_file`, `token_secret_file`, `known_hosts_file` point at the layout's on-disk paths, print its result, exit with its code.
- [ ] **Step 1: Tests:** `TestFinishWritesFiles` (contents, modes, the two config values changed and nothing else — compare re-parsed YAML); `TestFinishRefusesMismatch`; `TestFinishNeedRotate`; `TestSetupCommandRoundTrip` with a fake check and canned stdin: answers → node line printed → reply pasted → files present; `TestSetupAlreadyCompleted` prints exactly the message; echo is restored after the prompt even on error (fake terminal seam).
- [ ] **Step 2–4:** FAIL → implement → `go test -race ./...` PASS.
- [ ] **Step 5: Commit** "Add pbp setup".

### Task 10: End-to-end on dev-scratch

**Files:**
- Modify: `test/nodee2e/Dockerfile` (install the `.deb` and fixtures only; no README replay), `test/nodee2e/nodee2e_test.go`, `test/nodee2e/run.sh` if needed
- Create: `test/nodee2e/node/stubs/pvesh`, `test/nodee2e/node/stubs/pveum` (state kept in files under `/var/lib/pbp-e2e`), fixture `/etc/pve/local/pve-ssl.pem` and `/etc/pve/nodes/…` as needed; delete `test/nodee2e/node/sudoers` and `node/node.yaml` if no longer used

**Interfaces:**
- Consumes: everything above; the container starts with the node unconfigured.

- [ ] **Step 1:** In `TestMain`, after the container starts: run the host side in-process (`hostsetup` with a temp dir, scripted answers), then `docker exec -t <c> pbp node setup <line> --yes`, feed the printed reply back to `Finish`, and use the resulting key and `known_hosts` for the existing tests.
- [ ] **Step 2:** The ten existing tests pass unchanged.
- [ ] **Step 3: New tests:** `TestSetupRerunIsComplete` (output contains `setup has already been completed`; mtimes of every §5.3 file unchanged); `TestSetupDiffersStops` (rewrite sudoers with an extra line → setup exits non-zero naming it, file unchanged; with `--replace` a backup exists and the file is restored); `TestSetupRefusedOverSSHAndSudo` (via the PBP key: `pbp node setup` refused; via `docker exec` without `-t`: refused as non-terminal); `TestUninstallRemovesAccess` (last test: uninstall `--yes`, then every attacker case and a valid `version` request get nothing).
- [ ] **Step 4: Run** `test/nodee2e/run.sh` on dev-scratch — all PASS. Also `go vet -tags nodee2e ./...` and golangci-lint for the `nodee2e` tag set.
- [ ] **Step 5: Commit** "Drive the node e2e through pbp setup".

### Task 11: Docs, version, compose

**Files:**
- Modify: `README.md`, `CHANGELOG.md`, `VERSION` (`0.3.0`), `docker-compose.yml` (`TZ: ${TZ:-Europe/London}`), `.gitignore` (add `/.env`)

- [ ] **Step 1:** README "Setup" leads with the guided path: build/install, the exact Docker one-shot command
  `docker compose build && docker run --rm -it --user 0 -e TZ="$(timedatectl show -p Timezone --value)" -v "$PWD:/setup" --entrypoint /pbp pbp:latest setup --dir /setup`
  (adjust only if Task 9 proved it wrong), `sudo pbp setup --deb` for `.deb` hosts, the paste round trip, `docker compose up -d`. Keep VMID-range, bridge and privileges explanations. The current manual steps move under "Manual setup (what `pbp setup` does)" unchanged in substance; `TestDocsMatchState` keeps them honest. Document `pbp node uninstall`, `--rotate-token`, `--replace` backups.
- [ ] **Step 2:** CHANGELOG `## v0.3.0` (guided setup; upgrade both sides together).
- [ ] **Step 3: Run** `go test ./...` (docs test), gofmt, `go vet` (all three tag sets), staticcheck, golangci-lint (all three tag sets), gosec, Trivy and gitleaks on dev-scratch — clean.
- [ ] **Step 4: Commit** "Document guided setup; v0.3.0".

### Task 12: Acceptance on pve1 (controller-run)

- [ ] Build `0.3.0~rc.1` `.deb` on dev-scratch; install on pve1 (upgrades the 0.2 helper).
- [ ] `pbp node setup … --dry-run` then for real against the existing manual setup → `setup has already been completed`, no file changed (compare `stat` and `sha256sum` before/after for every §5.3 path).
- [ ] `pbp node uninstall --yes`; the PBP key gets nothing (`id`, `version` request).
- [ ] Fresh host dir on dev-scratch: `pbp setup` with pve1's values (restore storage `pool_qemu`, PBS storage `PBS-qemu`, range 990000–990009, vmbr99), paste on pve1 with `--rotate-token` (the token exists), paste the reply back → check passes.
- [ ] Live integration suite with the new files: all PASS; range 990000–990009 empty afterwards.
- [ ] Root's `authorized_keys` on pve1 unchanged (line count and hash before/after).
