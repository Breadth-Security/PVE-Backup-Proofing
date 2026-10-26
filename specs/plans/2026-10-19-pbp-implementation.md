# PBP v1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `pbp`, a Go daemon shipped as a Docker image, that restores the newest PBS backup of every guest into an isolated temporary guest on one PVE node, proves it boots and runs its services, records the verdict on the backup and notifies through the node.

**Architecture:** One binary with subcommands (`daemon`, `run`, `check`, `health`). Pure logic (selection, sanitizing, notes, probing rules, health) lives in small packages tested without a network. Side effects go through three interfaces (`pve.API`, `sshx.Runner`, `target.Target`) with in-memory fakes, so the run orchestrator is fully unit-testable. An opt-in integration suite runs against the real node `pve-node`.

**Tech Stack:** Go (go directive `1.26`), stdlib `net/http` + `log/slog`, `golang.org/x/crypto/ssh` (+ `knownhosts`), `gopkg.in/yaml.v3`, `github.com/robfig/cron/v3`. Docker multi-stage build to `gcr.io/distroless/static-debian12`.

**Spec:** [specs/2026-10-19-pbp-design.md](../2026-10-19-pbp-design.md). Section refs below (§n) point into it. Read it before any task.

## Global Constraints

- Product name **PBP / PVE Backup Proofing**; binary `pbp`; never "validation"/"PBV" in code, image, or docs.
- Go module path: `git.breadthanalytics.com/tdiana/PVE-Backup-Proofing` (originally `git.breadthanalytics.com/tdiana/pbv`; renamed to match the repository).
- Requires **PVE ≥ 9.0** (preflight refuses older), PBS ≥ 4.0.
- Restored guests: only inside `target.vmid_range`, tagged `pbp`; never start before sanitize succeeded (§11).
- Backup attributes PBP may write: `notes`, `protected` only; `protected` always restored to its original value.
- Verdict strings exactly: `PROVEN`, `FAILED`, `BOOTED`, `SKIPPED`, `ERROR`.
- Notes line format exactly: `PBP <VERDICT> <YYYY-MM-DD HH:MM> · <reason>` (middle dot U+00B7), first line of notes.
- Defaults: `workers: 1`, `space_margin_percent: 10`, `boot_timeout: 5m`, `reproof: false`, `notify.when: always`, `logging.retain_runs: 30`, `max_run_duration` unset.
- Secrets only via file paths in config; TLS: fingerprint or CA file, no skip-verify default; SSH: `known_hosts` mandatory.
- Notification sent with template name `pbp` and fields `type=pbp`, `hostname=<node>`.
- Config path: `/etc/pbp/config.yaml`, overridable by `--config` or env `PBP_CONFIG`. State dir `/var/lib/pbp`, logs `/var/log/pbp`.
- Integration tests touch **pve-node, which runs production guests**: they use only the configured VMID range and the isolated bridge; nothing else on the node is modified. Installing templates into `/etc/pve` on pve-node needs the owner's go-ahead first (ask before Task 19 step 4).

## Review Focus

1. **Encrypted backups** (all backups on pve-node are encrypted): restore must work with the node's key; a key/decrypt failure is `ERROR`, not `FAILED`. Pinned in Task 13 (error classification test) and Task 19.
2. **Production clones on the network:** the restored guest carries the original's IP/hostname; every NIC, including OVS VLAN-tagged ones (`bridge=vmbr0,tag=2`), must end on the isolated bridge with tag/trunks/rate removed. Pinned in Task 7.
3. **Existing notes:** vzdump writes the guest name as notes, sometimes multi-line; an old PBP line may sit anywhere. Merge must preserve user text and keep exactly one PBP line first. Pinned in Task 2.
4. **Crash mid-job:** container killed after restore or after protect. Next start must destroy the tagged guest and restore `protected`. Pinned in Task 15.
5. **Range trouble:** a foreign (untagged) guest inside the VMID range → refuse run; range smaller than `workers` or exhausted → jobs `SKIPPED` ("no free VMID"), never a VMID outside the range. Pinned in Tasks 13 and 15.

---

## File Structure

```
cmd/pbp/main.go                  subcommand dispatch
internal/model/model.go          shared types (Backup, GuestKey, Verdict, GuestResult, MissingPiece, RunSummary)
internal/config/                 config.go (types, Load, Validate), defaults.go (ignore lists, built-in profiles)
internal/notes/notes.go          PBP notes line compose/merge/parse
internal/discovery/select.go     newest-per-guest + ordering
internal/pve/                    client.go (HTTP, auth, tasks), backups.go, guests.go, node.go, api.go (API interface), pvefake/fake.go
internal/sshx/                   ssh.go (Runner + real client), quote.go
internal/sanitize/               sanitize.go (restored-config plan), original.go (missing pieces from original config), parse.go
internal/state/store.go          JSON state file
internal/probe/                  executor.go, linux.go, profiles.go, missing.go, windows.go, probe.go (entry)
internal/target/                 target.go (interface), existing.go (ExistingNodeTarget), boot.go, targetfake/fake.go
internal/run/                    job.go (one guest), runner.go (whole run), janitor.go, preflight.go
internal/report/                 logging.go, summary.go, notify.go, templates/*.hbs
internal/health/health.go
internal/daemon/daemon.go        scheduler + run lock
test/integration/                build tag `integration`
Dockerfile, docker-compose.yml, config.example.yaml, .gitignore, README.md
```

---

### Task 1: Repo cleanup, module scaffold, shared model

**Files:**
- Delete: `pbvRunner/` (entire tree, including committed `target/`), `pbv.code-workspace` only if it references pbvRunner (check; keep otherwise)
- Create: `.gitignore` (`/pbp`, `/dist/`, `*.test`, `coverage.out`), `go.mod`, `cmd/pbp/main.go`, `internal/model/model.go`, `internal/model/model_test.go`

**Interfaces:**
- Produces (`internal/model`):
  - `type GuestKind string` with `KindQEMU = "qemu"`, `KindLXC = "lxc"`
  - `type GuestKey struct{ Kind GuestKind; VMID int }` + `func (k GuestKey) String() string` → `"qemu/100"`
  - `type Backup struct{ Storage, VolID, Notes string; Key GuestKey; Time time.Time; Size int64; Protected, Encrypted bool }`
  - `type Verdict string` constants `Proven, Failed, Booted, Skipped, Error` (values per Global Constraints)
  - `type MissingPiece struct{ Kind, Key, Path, Detail string }` — Kind ∈ `excluded-disk|excluded-mountpoint|bind-mount|passthrough|network-storage`
  - `type CheckResult struct{ Name string; OK bool; Detail string }`
  - `type NotCovered struct{ Unit string; Piece MissingPiece }` + `func (n NotCovered) LogLine() string`
  - `type GuestResult struct{ Backup Backup; Name string; TempVMID int; Verdict Verdict; Reason string; FailedUnits, IgnoredUnits []string; NotCovered []NotCovered; Checks []CheckResult; AgentMissing bool; RestoreDuration, BootDuration time.Duration; BootLog string }`
  - `type RunSummary struct{ ID, Trigger string; Started, Ended time.Time; Results []GuestResult; RunError string }` + `func (s RunSummary) Counts() map[Verdict]int` + `func (s RunSummary) Severity() string`

- [ ] **Step 1: Write failing tests** in `internal/model/model_test.go`:

```go
func TestGuestKeyString(t *testing.T) {
	if got := (GuestKey{KindLXC, 101}).String(); got != "lxc/101" { t.Fatal(got) }
}
func TestNotCoveredLogLine(t *testing.T) {
	n := NotCovered{Unit: "postgresql.service", Piece: MissingPiece{Kind: "excluded-disk", Key: "scsi1", Path: "/srv/pgdata", Detail: "backup=0"}}
	want := "Found postgresql.service depends on /srv/pgdata (disk scsi1, backup=0): not backed up, skipped"
	if got := n.LogLine(); got != want { t.Fatalf("got %q", got) }
}
func TestSeverity(t *testing.T) {
	cases := []struct{ vs []Verdict; runErr, want string }{
		{[]Verdict{Proven, Proven}, "", "info"},
		{[]Verdict{Proven, Booted}, "", "warning"},
		{[]Verdict{Skipped}, "", "warning"},
		{[]Verdict{Proven, Error}, "", "warning"},
		{[]Verdict{Booted, Failed}, "", "error"},
		{nil, "ssh unreachable", "error"},
	}
	// build RunSummary from vs/runErr, assert Severity()==want
}
```
`LogLine` piece wording: `disk <Key>` for excluded-disk, `mount point <Key>` for excluded-mountpoint, `bind mount <Key>` for bind-mount, `device <Key>` for passthrough, `network storage` for network-storage; Detail appended after a comma when non-empty; Path falls back to Key when empty.

- [ ] **Step 2: Run** `go test ./internal/model/` — expect FAIL (undefined).
- [ ] **Step 3: Implement** `go.mod` (module path from Global Constraints, `go 1.26`), `internal/model/model.go`, and `cmd/pbp/main.go` that prints usage `pbp daemon|run [--guest VMID]|check|health [--config PATH]` and exits 2 for unknown subcommands.
- [ ] **Step 4: Run** `go test ./... && go build ./cmd/pbp` — expect PASS, binary builds.
- [ ] **Step 5: Commit** `git add -A && git commit -m "Replace Rust stub with Go module and shared model"`

---

### Task 2: Notes line compose / merge / parse (§8.2)

**Files:** Create `internal/notes/notes.go`, `internal/notes/notes_test.go`

**Interfaces:**
- Consumes: `model.GuestResult`, `model.Verdict`
- Produces:
  - `func Line(v model.Verdict, at time.Time, reason string) string`
  - `func Merge(existing, pbpLine string) string`
  - `func HasProven(notes string) bool` — true iff a PBP line with verdict PROVEN exists

- [ ] **Step 1: Write failing tests:**

```go
func TestLine(t *testing.T) {
	at := time.Date(2026, 10, 19, 2, 14, 0, 0, time.UTC)
	if got := Line(model.Proven, at, "agent ok, 3 profiles ok"); got != "PBP PROVEN 2026-10-19 02:14 · agent ok, 3 profiles ok" { t.Fatal(got) }
}
func TestMerge(t *testing.T) {
	l := "PBP FAILED 2026-10-19 02:31 · failed: postgresql.service"
	cases := map[string]struct{ in, want string }{
		"empty":           {"", l},
		"vzdump name":     {"web01.example.lan", l + "\nweb01.example.lan"},
		"multiline":       {"db01\nmonthly keep", l + "\ndb01\nmonthly keep"},
		"replace first":   {"PBP PROVEN 2026-10-18 02:10 · ok\ndb01", l + "\ndb01"},
		"replace middle":  {"db01\nPBP PROVEN 2026-10-18 02:10 · ok\nnote", l + "\ndb01\nnote"},
		"user PBP prefix": {"PBPX backup of x", l + "\nPBPX backup of x"},
	}
	// assert Merge(in, l) == want for each
}
func TestHasProven(t *testing.T) {
	// true: "db01\nPBP PROVEN 2026-10-18 02:10 · ok"; false: "PBP FAILED ...", "", "PBP PROVENANCE notes"
}
```
A PBP line is recognised by the regexp `^PBP (PROVEN|FAILED|BOOTED|SKIPPED|ERROR) \d{4}-\d{2}-\d{2} \d{2}:\d{2} · `. Trailing newlines of existing notes are trimmed.

- [ ] **Step 2: Run** `go test ./internal/notes/` — FAIL.
- [ ] **Step 3: Implement** the three functions. Times are formatted in the location of `at` (caller passes local time).
- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit** `"Add PBP notes line handling"`

---

### Task 3: Configuration load, defaults, validation (§9)

**Files:** Create `internal/config/config.go`, `internal/config/defaults.go`, `internal/config/config_test.go`, `config.example.yaml`

**Interfaces:**
- Produces:
  - `type Config struct{ Schedule string; Workers int; Target Target; Sources Sources; Selection map[string]any; Restore Restore; Checks Checks; Notify Notify; Logging Logging }`
  - `type Target struct{ Type, Node, Bridge string; API API; SSH SSH; VMIDRange [2]int \`yaml:"vmid_range"\` }`
  - `type API struct{ URL, TokenID, TokenSecretFile, TLSFingerprint, CAFile string }` (yaml snake_case)
  - `type SSH struct{ Host, User, KeyFile, KnownHostsFile string; Port int }` (Port default 22)
  - `type Sources struct{ Storages []string }`
  - `type Restore struct{ Storage string; SpaceMarginPercent int; BootTimeout time.Duration; Reproof bool; MaxRunDuration time.Duration }`
  - `type Checks struct{ IgnoreUnits, WindowsIgnoreServices []string; Profiles []Profile; Guests []GuestChecks }`
  - `type CheckSet struct{ Services []string; Ports []int; Commands []string }`
  - `type Profile struct{ Name, Unit string; CheckSet \`yaml:",inline"\` }`
  - `type GuestChecks struct{ Match GuestMatch; CheckSet \`yaml:",inline"\` }`; `type GuestMatch struct{ VMID int; Name, Tag string }`
  - `type Notify struct{ When string }`; `type Logging struct{ Dir string; RetainRuns int }`
  - `func Load(path string) (Config, error)` — read, apply defaults, validate
  - `func DefaultIgnoreUnits() []string`, `func DefaultWindowsIgnore() []string`, `func BuiltinProfiles() []Profile`
  - `func (c Checks) EffectiveProfiles() []Profile` — built-ins overridden by same-Name user profiles, then extra user profiles
  - `func ResolvePath(flag string) string` — flag > `$PBP_CONFIG` > `/etc/pbp/config.yaml`

Defaults (`defaults.go`):
- Ignore units: `systemd-networkd-wait-online.service`, `NetworkManager-wait-online.service`, `chronyd.service`, `chrony.service`, `systemd-timesyncd.service`, `ntp.service`, `ntpd.service`, `ntpsec.service`, `remote-fs.target`, `nfs-client.target`, `rpc-statd.service`, `cloud-init.service`, `cloud-init-network.service`, `cloud-final.service`, `cloud-config.service`. Each with a YAML comment in `config.example.yaml` explaining "fails because the proof network has no uplink".
- Windows ignore: `sppsvc`, `gupdate`, `edgeupdate`, `MapsBroker`, `RemoteRegistry`, `CDPSvc`, `tiledatamodelsvc`, `wuauserv`, `BITS`, `W32Time`. (Builder may extend after Task 19 testing; document each.)
- Built-in profiles (Name / Unit glob / checks):
  - `postgresql` / `postgresql*.service` / commands `pg_isready -q`
  - `mariadb` / `mariadb.service` / commands `mariadb-admin ping` ; `mysql` / `mysql*.service` / commands `mysqladmin ping`
  - `nginx` / `nginx.service` / ports any-of 80,443 (see Task 10 for any-of semantics: profile `ports` pass if **any** listed port listens)
  - `apache` / `apache2.service` ; `httpd` / `httpd.service` — ports 80,443
  - `docker` / `docker.service` / commands `docker info`
  - `redis` / `redis*.service` / commands `redis-cli ping`
  - `sshd` / `ssh.service` and `sshd` / `sshd.service` — ports 22 (name them `ssh`, `sshd`)
  - `cron` / `cron.service`, `crond` / `crond.service` — services (the unit itself) active

Validation errors (each a test case): missing `target.node`, `target.bridge`, `restore.storage`, empty `sources.storages`, `vmid_range` with lo<100 or hi<lo, `workers < 1`, `target.type` not `existing-node`, invalid cron in `schedule`, `notify.when` not in {always, problems}, neither `tls_fingerprint` nor `ca_file`, missing `known_hosts_file`.

- [ ] **Step 1: Write failing tests:** `TestLoadExampleConfig` (loads `../../config.example.yaml`, asserts `Workers==1`, `Restore.SpaceMarginPercent==10`, `Restore.BootTimeout==5*time.Minute`, `Notify.When=="always"`, `Logging.RetainRuns==30`, `Target.SSH.Port==22`, ignore list contains `systemd-networkd-wait-online.service`); `TestValidateErrors` (table over the list above, each error message names the offending key); `TestEffectiveProfiles` (user profile `Name: "nginx", Ports: [8443]` replaces built-in nginx; user profile `myapp` appended; total = len(builtins)+1); `TestResolvePath`.
- [ ] **Step 2: Run** `go test ./internal/config/` — FAIL.
- [ ] **Step 3: Implement** types, `Load`, defaults, validation (cron via `cron.ParseStandard`), and write `config.example.yaml` matching §9 with `pve-node`-agnostic placeholder values.
- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit** `"Add configuration loading, defaults and validation"`

---

### Task 4: Discovery — newest per guest and ordering (§6 step 2–3)

**Files:** Create `internal/discovery/select.go`, `internal/discovery/select_test.go`

**Interfaces:**
- Consumes: `model.Backup`, `model.GuestKey`, `notes.HasProven`
- Produces:
  - `func NewestPerGuest(all []model.Backup) []model.Backup` — across all storages; ties on Time → first storage in input order wins
  - `func Order(bs []model.Backup, lastProven map[model.GuestKey]time.Time) []model.Backup` — never-proven first (by VMID asc), then oldest lastProven first
  - `func AlreadyProven(b model.Backup, reproof bool) bool` — `!reproof && notes.HasProven(b.Notes)`

- [ ] **Step 1: Write failing tests:** two storages `pbs-offsite` and `pbs-onsite` both holding `lxc/101` at different times → one result, the newer; `qemu/100` and `lxc/100` are different guests (two results); `Order` with lastProven `{qemu/100: day-1, lxc/101: day-3}` and never-proven `lxc/109` → `[lxc/109, lxc/101, qemu/100]`; `AlreadyProven` true/false/reproof cases.
- [ ] **Step 2: Run** — FAIL. **Step 3: Implement.** **Step 4: Run** — PASS.
- [ ] **Step 5: Commit** `"Add backup discovery selection and ordering"`

---

### Task 5: PVE API client — core, backups, storage, node, network

**Files:** Create `internal/pve/client.go`, `internal/pve/backups.go`, `internal/pve/node.go`, `internal/pve/api.go`, `internal/pve/client_test.go`, `internal/pve/backups_test.go`, `internal/pve/testdata/*.json`

**Interfaces:**
- Consumes: `config.API`, `model.Backup`
- Produces (`api.go` declares `type API interface` containing every method below and in Task 6; `*Client` implements it):
  - `func New(cfg config.API) (*Client, error)` — reads token secret file; header `Authorization: PVEAPIToken=<TokenID>=<secret>`; TLS: if `TLSFingerprint` set, pin SHA-256 of leaf cert (colon hex, case-insensitive) via `VerifyPeerCertificate` with `InsecureSkipVerify` only for chain building; else `CAFile` pool
  - `type APIError struct{ Status int; Path, Message string }` (errors from non-2xx; body `errors`/`message` included)
  - `func (c *Client) Version(ctx) (string, error)` — `GET /version` → `version` (e.g. `"9.2.18"`)
  - `func (c *Client) ListBackups(ctx, node, storage string) ([]model.Backup, error)` — `GET /nodes/{node}/storage/{storage}/content?content=backup`; map `volid`, `vmid`, `subtype` (`qemu`|`lxc`), `ctime`→Time, `size`, `notes`, `protected` (absent=false), `encrypted` (non-empty=true)
  - `func (c *Client) SetVolumeAttrs(ctx, node, storage, volid string, notes *string, protected *bool) error` — `PUT /nodes/{node}/storage/{storage}/content/{volid}` (volid path-escaped)
  - `type StorageStatus struct{ Active, Enabled bool; Avail, Total int64; Content string }`; `func (c *Client) StorageStatus(ctx, node, storage string) (StorageStatus, error)`
  - `type NodeMemory struct{ Total, Available int64 }`; `func (c *Client) NodeMemory(ctx, node string) (NodeMemory, error)` — `GET /nodes/{node}/status` → `memory.total`, `memory.available`
  - `type NetIface struct{ Iface, Type, BridgePorts, OVSPorts, Address, CIDR, Address6, CIDR6 string }`; `func (c *Client) NetIface(ctx, node, iface string) (NetIface, error)`
  - `func (c *Client) WaitTask(ctx, node, upid string) error` — poll `GET /nodes/{node}/tasks/{upid}/status` every 2s until `status=="stopped"`; `exitstatus!="OK"` → `*TaskError{UPID, ExitStatus string; Log []string}` (last 20 lines from `/tasks/{upid}/log`)

- [ ] **Step 1: Write failing tests** with `httptest.NewTLSServer`: auth header exact; fingerprint pin accepts the test server cert and rejects a wrong fingerprint; `ListBackups` parses `testdata/content_pbs.json` (copy the two real entries shown in the plan's header research: `pbs-offsite:backup/ct/101/2026-07-26T06:00:03Z`, encrypted, notes `web01.example.lan`, size 43956736778, subtype lxc) into `model.Backup{Key: {lxc,101}, Encrypted: true, Protected: false, ...}`; `SetVolumeAttrs` sends PUT with form `notes=...&protected=1` to the escaped path; `WaitTask` returns `*TaskError` with log on `exitstatus: "command 'x' failed"`; non-2xx → `*APIError` with status.
- [ ] **Step 2: Run** `go test ./internal/pve/` — FAIL. **Step 3: Implement.** Request bodies are `application/x-www-form-urlencoded`; responses unwrap `{"data": ...}`.
- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit** `"Add PVE API client core and backup endpoints"`

---

### Task 6: PVE API client — guests, agent, original config; fake

**Files:** Create `internal/pve/guests.go`, `internal/pve/guests_test.go`, `internal/pve/pvefake/fake.go`, `internal/pve/pvefake/fake_test.go`

**Interfaces:**
- Produces (added to `API`):
  - `type GuestRef struct{ Key model.GuestKey; Node, Name, Status string; Tags []string }`; `func (c *Client) Guests(ctx) ([]GuestRef, error)` — `GET /cluster/resources?type=vm`, tags split on `;`
  - `func (c *Client) Restore(ctx, node string, kind model.GuestKind, vmid int, volid, storage string) (upid string, err error)` — qemu: `POST /nodes/{n}/qemu` `archive,vmid,storage,unique=1`; lxc: `POST /nodes/{n}/lxc` `ostemplate,vmid,storage,restore=1,unique=1`
  - `func (c *Client) GuestConfig(ctx, node string, kind model.GuestKind, vmid int) (map[string]string, error)` — values stringified (numbers via `%v`); the `lxc` raw array key preserved as `"lxc"` joined with `\n`
  - `func (c *Client) SetGuestConfig(ctx, node string, kind model.GuestKind, vmid int, set map[string]string, del []string) error` — qemu `POST .../config`, lxc `PUT .../config`; `delete` comma-joined
  - `func (c *Client) OriginalConfig(ctx, node, volid string) (string, error)` — `GET /nodes/{n}/vzdump/extractconfig?volume=`
  - `func (c *Client) Start/Shutdown/Stop(ctx, node string, kind model.GuestKind, vmid int) (upid string, err error)` — `status/start`, `status/shutdown` (`timeout=60`), `status/stop`
  - `func (c *Client) Destroy(ctx, node string, kind model.GuestKind, vmid int) (upid string, err error)` — `DELETE /nodes/{n}/{kind}/{vmid}?purge=1&destroy-unreferenced-disks=1`
  - `func (c *Client) GuestStatus(ctx, node string, kind model.GuestKind, vmid int) (string, error)` — `status/current` → `status`
  - `func (c *Client) AgentPing(ctx, node string, vmid int) error`
  - `type OSInfo struct{ ID, Name string }`; `func (c *Client) AgentOSInfo(ctx, node string, vmid int) (OSInfo, error)` — `agent/get-osinfo` → `result.id`, `result.name`
  - `func (c *Client) AgentExec(ctx, node string, vmid int, argv []string) (pid int, err error)` — `POST agent/exec` with repeated `command` params
  - `type AgentExecStatus struct{ Exited bool; ExitCode int; OutData, ErrData string }`; `func (c *Client) AgentExecStatus(ctx, node string, vmid, pid int) (AgentExecStatus, error)`
  - `pvefake.New() *Fake` implementing `pve.API` in memory: maps of guests/configs/backups/storage/memory/ifaces; `Restore` creates a guest with config copied from `Fake.BackupConfigs[volid]`; tasks complete instantly; error injection via `Fake.Fail map[string]error` keyed by method name (e.g. `"Restore"`), and `Fake.Calls []string` recording `"Method arg..."` for assertions.

- [ ] **Step 1: Write failing tests** (httptest): Restore request shape for both kinds; SetGuestConfig method differs by kind and sends `delete=hostpci0,usb0`; GuestConfig stringifies `{"cores":4,"memory":"16500"}`; AgentExec sends `command=sh&command=-c&command=echo hi`; Destroy query string exact. `pvefake`: compile-time `var _ pve.API = (*Fake)(nil)`; Restore then GuestConfig returns copied config; `Fail["Start"]` returned from Start.
- [ ] **Step 2: Run** — FAIL. **Step 3: Implement.** **Step 4: Run** — PASS.
- [ ] **Step 5: Commit** `"Add PVE guest, agent and restore endpoints with in-memory fake"`

---

### Task 7: Sanitizer and missing pieces (§6 step 7, §7.8)

**Files:** Create `internal/sanitize/parse.go`, `internal/sanitize/sanitize.go`, `internal/sanitize/original.go`, `internal/sanitize/sanitize_test.go`, `internal/sanitize/original_test.go`

**Interfaces:**
- Produces:
  - `func ParseConfigText(s string) map[string]string` — `key: value` lines; skip `#` comments and blank lines; stop at first `[` section line; `lxc.*` raw lines collected under key `"lxc"` joined by `\n`
  - `type Plan struct{ Set map[string]string; Delete []string; Removed []model.MissingPiece; Unsafe []string }`
  - `func Sanitize(kind model.GuestKind, restored map[string]string, bridge string) Plan`
  - `func MissingFromOriginal(kind model.GuestKind, original map[string]string) []model.MissingPiece`

Sanitize rules:
- Every `net\d+`: set `bridge=<bridge>`; remove `tag`, `trunks`, `rate`; keep other options in original order.
- `onboot` → Set `"0"`; `startup` → Delete; `tags` → Set `"pbp"`.
- qemu `hostpci\d+`, `usb\d+`, `parallel\d+`, `serial\d+` whose value ≠ `socket` → Delete + Removed{Kind:"passthrough", Key}.
- lxc `dev\d+` → Delete + Removed passthrough; lxc `mp\d+` whose source (first comma field) starts with `/` → Delete + Removed{Kind:"bind-mount", Key, Path: value of `mp=`}.
- lxc key `"lxc"` (raw lines) → Unsafe (`"raw lxc.* config"`); qemu `args` → Unsafe.
- Disk keys keep as-is.
- HA and replication need no handling: they are keyed by VMID and the restore uses a fresh VMID, so none can exist (§6 step 7 satisfied by construction; note this in a code comment).

MissingFromOriginal rules:
- qemu disk keys `(scsi|virtio|sata|ide)\d+` that are not `media=cdrom` and have `backup=0` → `{Kind:"excluded-disk", Key, Detail:"backup=0"}` (Path empty; probe resolves it).
- lxc `mp\d+` with storage source (not `/`) lacking `backup=1` → `{Kind:"excluded-mountpoint", Key, Path: mp=, Detail:"backup not enabled"}`.

- [ ] **Step 1: Write failing tests:**
  - Real pve-node VM 100 config (from research): `net0: virtio=BC:24:11:00:00:03,bridge=vmbr0,firewall=1,tag=2` → `virtio=BC:24:11:00:00:03,bridge=vmbr-pbp,firewall=1`; `onboot: 1` → `0`; `startup: order=5` deleted; `serial0: socket` kept; `tags` → `pbp`.
  - qemu `hostpci0: 0000:01:00.0,pcie=1`, `usb0: host=1234:5678`, `serial1: /dev/ttyUSB0` → all deleted, 3 Removed passthrough.
  - lxc `net0: name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:00:01,ip=10.0.0.5/24,gw=10.0.0.1,rate=50` → bridge swapped, rate dropped, ip kept.
  - lxc `mp0: /mnt/media,mp=/media` → deleted, Removed bind-mount Path `/media`; `mp1: local-zfs:subvol-114-disk-1,mp=/data,size=8G` kept.
  - lxc raw `lxc: lxc.cgroup2.devices.allow: c 195:* rwm` → `Unsafe` non-empty.
  - Every net key in output has `bridge=vmbr-pbp` — property test over a table of 8 NIC strings including OVS `tag=2,trunks=10;20`.
  - `ParseConfigText` on text with `#` description comments and a `[snap1]` section returns only main keys.
  - MissingFromOriginal: `scsi1: ...,backup=0,size=100G` → excluded-disk scsi1; `ide2: none,media=cdrom` ignored; lxc `mp0: local-zfs:subvol-1-disk-1,mp=/srv/pgdata` → excluded-mountpoint `/srv/pgdata`; with `backup=1` → none.
- [ ] **Step 2: Run** — FAIL. **Step 3: Implement** (option strings split on `,`, `key=value`; preserve order). **Step 4: Run** — PASS.
- [ ] **Step 5: Commit** `"Add restored-guest sanitizer and missing-piece detection"`

---

### Task 8: SSH runner

**Files:** Create `internal/sshx/ssh.go`, `internal/sshx/quote.go`, `internal/sshx/ssh_test.go`, `internal/sshx/quote_test.go`

**Interfaces:**
- Produces:
  - `type Result struct{ Stdout, Stderr string; ExitCode int }`
  - `type Runner interface{ Run(ctx context.Context, cmd string, stdin []byte) (Result, error) }` — non-zero exit is **not** an error (only transport failures are)
  - `func Dial(cfg config.SSH) (*Client, error)` — key auth from `KeyFile`, `knownhosts.New(KnownHostsFile)` callback (unknown/mismatch host → error); `*Client` implements Runner, one session per Run, ctx cancel closes the session; `Close() error`
  - `func Quote(args ...string) string` — POSIX single-quote each arg, join with spaces
  - `type Fake struct{ Handler func(cmd string, stdin []byte) (Result, error); Calls []string }` implementing Runner (for other packages' tests)

- [ ] **Step 1: Write failing tests:** `Quote("pct","exec","101","--","sh","-c","echo 'hi'")` == `'pct' 'exec' '101' '--' 'sh' '-c' 'echo '\''hi'\'''`; in-process SSH server test using `golang.org/x/crypto/ssh` server config on `127.0.0.1:0` that executes a fixed handler: exit code 3 returned in Result with nil error; host key not in known_hosts → Dial error mentioning `knownhosts`; ctx cancel mid-run returns ctx error.
- [ ] **Step 2: Run** — FAIL. **Step 3: Implement.** **Step 4: Run** — PASS.
- [ ] **Step 5: Commit** `"Add SSH runner with known_hosts enforcement"`

---

### Task 9: State store (§5 state, §11.6)

**Files:** Create `internal/state/store.go`, `internal/state/store_test.go`

**Interfaces:**
- Produces:
  - `type Job struct{ TempVMID int; Kind model.GuestKind; VolID, Storage string; ProtectedOrig *bool; Stage string }` — Stage ∈ `protected|restored|started|destroyed`
  - `type RunRecord struct{ ID, Trigger, Outcome string; Started, Ended time.Time; Counts map[model.Verdict]int }` — Outcome ∈ `running|ok|problems|error`
  - `type State struct{ InFlight map[int]Job; LastProven map[string]time.Time; Runs []RunRecord }` (LastProven keyed by `GuestKey.String()`)
  - `func Open(path string) (*Store, error)` — creates empty state if file missing
  - `func (s *Store) Update(fn func(*State) error) error` — mutex + write `path.tmp`, fsync, rename
  - `func (s *Store) Snapshot() State` (deep copy)
  - `func (s *Store) AppendRun(r RunRecord, retain int) error` — keep last `retain`; replaces an existing record with same ID

- [ ] **Step 1: Write failing tests:** round-trip through a reopen; Update error leaves file unchanged; AppendRun keeps the last 30 of 35; replacing a `running` record with its finished version; corrupt file → Open returns error (do not silently reset — crash-recovery data matters).
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `"Add crash-safe state store"`

---

### Task 10: Probe — executor contract, Linux base health, profiles, guest checks (§7.3, §7.6, §7.7)

**Files:** Create `internal/probe/executor.go`, `internal/probe/linux.go`, `internal/probe/profiles.go`, `internal/probe/probe.go`, `internal/probe/linux_test.go`, `internal/probe/profiles_test.go`, `internal/probe/probetest/scripted.go`

**Interfaces:**
- Consumes: `config.Checks`, `config.Profile`, `config.CheckSet`, `model.*`
- Produces:
  - `type ExecResult struct{ Stdout, Stderr string; ExitCode int }`
  - `type Executor interface{ Exec(ctx context.Context, argv []string) (ExecResult, error) }` — error only when the command could not be run at all
  - `type OSFamily string` — `linux|windows|unknown`
  - `type Input struct{ OS OSFamily; Checks config.Checks; Guest GuestInfo; Missing []model.MissingPiece }`; `type GuestInfo struct{ VMID int; Name string; Tags []string }`
  - `type Outcome struct{ Verdict model.Verdict; Reason string; FailedUnits, IgnoredUnits []string; NotCovered []model.NotCovered; Checks []model.CheckResult; BootLog string }`
  - `func Run(ctx context.Context, ex Executor, in Input) Outcome` — dispatches on OS
  - `var ErrExecBlocked = errors.New("agent cannot run commands")` — returned by executors (Task 13) when the guest agent refuses exec
  - `probetest.Scripted` (`internal/probe/probetest/scripted.go`): `type Scripted map[string]probe.ExecResult` keyed by `strings.Join(argv," ")` implementing Executor; unknown argv → ExitCode 127

Linux sequence (each via `sh -c`-free argv):
1. `systemctl is-system-running --wait` — exit 127 → OpenRC path; stdout `running` ok; `degraded` continue; `maintenance|emergency|starting|stopping|offline` → FAILED reason `system state: <s>`.
2. `findmnt -n -o OPTIONS /` — option list contains `ro` → FAILED `root filesystem read-only`.
3. `systemctl list-units --failed --plain --no-legend --no-pager` → first field per line = failed units; split into ignored (glob match against `IgnoreUnits` via `path.Match`) and real; real ones go to Task 11's tracer (Run calls `traceMissing`; until Task 11 lands, it returns all as untraced). Untraced real failures → FAILED `failed: a.service, b.service`.
4. `systemctl list-unit-files --state=enabled --no-legend --plain` → enabled units; for each EffectiveProfile whose `Unit` glob matches an enabled unit, run its CheckSet; for each GuestChecks whose Match hits (VMID equal, or Name equal, or Tag in Tags), run its CheckSet.
5. CheckSet semantics: `Services` → `systemctl is-active <u>` stdout `active`; `Ports` (profiles: any-of; guest checks: all-of) → `ss -ltnH` parse local port column; `Commands` → `sh -c <cmd>` exit 0. Each produces a CheckResult named `<profile|guest>:<kind>:<item>`. Any failure → FAILED `check failed: <name>`.
6. OpenRC path: `rc-status --crashed` lines (minus ignore) → failed units; exit 127 again → BOOTED `init answering, service state unknown`.
7. On FAILED, capture `journalctl -b --no-pager -n 200` into `BootLog` (ignore errors).
8. PROVEN reason: `<n> checks ok` plus `, <k> ignored` when any.

- [ ] **Step 1: Write failing tests** using Scripted: healthy (`running`, rw root, no failed, nginx enabled + port 443 listening) → PROVEN with check `nginx:ports:80,443` OK; `degraded` with `chronyd.service` failed only → PROVEN, IgnoredUnits=[chronyd.service]; failed `postgresql.service` → FAILED reason `failed: postgresql.service`; `emergency` → FAILED; `ro` root → FAILED; profile command `pg_isready -q` exit 2 → FAILED reason `check failed: postgresql:commands:pg_isready -q`; guest check matched by Tag `db`; OpenRC crashed `nginx` → FAILED; no systemctl and no rc-status → BOOTED; user override of nginx profile ports `[8443]` used instead of built-in.
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `"Add Linux probe with profiles and guest checks"`

---

### Task 11: Probe — tracing failures to pieces missing from the backup (§7.8)

**Files:** Create `internal/probe/missing.go`, `internal/probe/missing_test.go`

**Interfaces:**
- Consumes: `Input.Missing []model.MissingPiece`, Executor
- Produces: `func traceMissing(ctx context.Context, ex Executor, failed []string, missing []model.MissingPiece) (covered []model.NotCovered, untraced []string)` — wired into Task 10 step 3

Algorithm (conservative: anything not proven to trace is untraced):
1. For each failed unit: `systemctl show -p Type -p What -p Where -p RequiresMountsFor -p Requires -p BindsTo <unit>` (`key=value` lines).
2. A failed **mount** unit (`*.mount`) is covered when:
   - `Where` equals or is under a piece's `Path` (bind-mount / excluded-mountpoint), or
   - its `What` is a network source (`Type` in `nfs|nfs4|cifs|smb3`, or `What` matches `^[^/]+:/` or `^//`) → piece `{Kind:"network-storage"}`, or
   - `What` does not exist in the guest (`test -e <What>` exit ≠ 0; resolve `UUID=`/`LABEL=` via `/dev/disk/by-uuid/`, `/dev/disk/by-label/`) **and** `missing` contains at least one `excluded-disk` → that disk piece (if several, Key lists them joined by `/`; Path = `Where`).
3. A failed non-mount unit is covered when any `RequiresMountsFor` path is equal to/under a covered mount's `Where` or a piece `Path`, or any `Requires`/`BindsTo` unit is a covered mount unit.
4. Iterate step 3 until no change (dependency chains).

- [ ] **Step 1: Write failing tests** (Scripted): lxc bind mount piece Path `/media` + failed `media.mount` (Where=/media) + failed `jellyfin.service` with `RequiresMountsFor=/media/movies` → both covered, verdict PROVEN, NotCovered LogLine for jellyfin names `/media`; failed `srv-pgdata.mount` What=`/dev/disk/by-uuid/abc` absent + excluded-disk scsi1 → covered as disk scsi1; same mount but **no** excluded disks in Missing → untraced → FAILED; nfs mount `nas:/export` → covered network-storage; failed `postgresql.service` with no mount relation → untraced → FAILED; chain `a.mount`(covered) ← `b.service` Requires a.mount ← `c.service` BindsTo b.service → all covered.
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `"Trace unit failures to pieces missing from the backup"`

---

### Task 12: Probe — Windows (§7.4)

**Files:** Create `internal/probe/windows.go`, `internal/probe/windows_test.go`

**Interfaces:** `Run` dispatches `OSFamily windows` here.

- Command: argv `powershell.exe -NoProfile -NonInteractive -Command <script>` where script lists `Win32_Service` with `StartMode -eq 'Auto'`, `State -ne 'Running'`, `DelayedAutoStart -ne $true`, outputs `Name` one per line.
- Names matched case-insensitively against `WindowsIgnoreServices`; remaining → FAILED `stopped auto services: X, Y`; none → PROVEN `windows services ok`. Exec failure → BOOTED `agent cannot run commands`.

- [ ] **Step 1: Write failing tests:** output `sppsvc\r\nSpooler\r\n` → FAILED `stopped auto services: Spooler`, IgnoredUnits=[sppsvc]; empty → PROVEN; exit 127/err → BOOTED.
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `"Add Windows service probe"`

---

### Task 13: Target interface and ExistingNodeTarget (§5.1, §6 steps 1, 6–9, §7.2)

**Files:** Create `internal/target/target.go`, `internal/target/existing.go`, `internal/target/boot.go`, `internal/target/executors.go`, `internal/target/targetfake/fake.go`, `internal/target/existing_test.go`, `internal/target/boot_test.go`

**Interfaces:**
- Consumes: `pve.API`, `sshx.Runner`, `sanitize.*`, `probe.Executor`, `config.Config`
- Produces:
  - `var ErrUnsafe, ErrNoFreeVMID, ErrNotIsolated, ErrForeignInRange, ErrVersion = errors.New(...)`
  - `type BootInfo struct{ OS probe.OSFamily; AgentMissing, ExecBlocked bool; Duration time.Duration }`
  - `type Target interface {`
    `Prepare(ctx) error` — version ≥ 9 (`ErrVersion`), bridge isolated (`ErrNotIsolated`), restore storage active+enabled with `images` (qemu) and `rootdir` (lxc) content, no foreign guest in range (`ErrForeignInRange` naming the VMIDs)
    `Owned(ctx) ([]pve.GuestRef, error)` — in range and tagged `pbp`
    `AllocateVMID(ctx, inUse map[int]bool) (int, error)` — lowest free in range not in `inUse` and not existing; else `ErrNoFreeVMID`
    `Restore(ctx, b model.Backup, vmid int) error`
    `Sanitize(ctx, kind model.GuestKind, vmid int, volid string) ([]model.MissingPiece, error)` — applies `sanitize.Sanitize` (Unsafe → `ErrUnsafe` wrapping reasons), then re-reads config and **verifies** every `net\d+` has `bridge=<bridge>`; returns Removed + `MissingFromOriginal(ParseConfigText(OriginalConfig))`
    `Start(ctx, kind, vmid) error`
    `WaitBooted(ctx, kind model.GuestKind, vmid int, timeout time.Duration) (BootInfo, error)`
    `Executor(kind model.GuestKind, vmid int) probe.Executor`
    `Destroy(ctx, kind, vmid) error` — refuses VMIDs outside range or without `pbp` tag; shutdown → after 60s stop → Destroy → confirm absent from `Guests`
    `}`
  - `func NewExistingNode(cfg config.Config, api pve.API, ssh sshx.Runner) *ExistingNode`
  - `func ClassifyRestoreError(err error) model.Verdict` — task log mentioning `decrypt`, `encryption key`, `permission`, `no space`, `timeout`, connection errors → `ERROR`; anything else (e.g. archive read/corrupt chunk) → `FAILED`
  - Bridge isolation: `Type` ∈ {`bridge`, `OVSBridge`}; `BridgePorts` empty or `none`; `OVSPorts` empty; `Address`, `CIDR`, `Address6`, `CIDR6` empty.
  - LXC executor: `ssh.Run("pct exec <vmid> -- " + sshx.Quote(argv...))`; non-zero exit is a result, transport error is error.
  - VM executor: AgentExec + poll AgentExecStatus every 500ms until Exited (ctx-bounded); agent error containing `not allowed`/`disabled`/`blocked` → returned as `probe.ErrExecBlocked`.
  - WaitBooted qemu: wait `GuestStatus == running` (else error), config has agent enabled (`agent` value `1` or contains `enabled=1`)? If not → `AgentMissing`. Else poll AgentPing every 3s until timeout → none → `AgentMissing`. Then AgentOSInfo id `mswindows` → windows, else linux; exec `true`/`cmd /c exit 0` blocked → `ExecBlocked`.
  - WaitBooted lxc: `ssh.Run("pct status <vmid>")` contains `running`, then executor `true` succeeds within timeout; OS from config `ostype` (`unmanaged`→unknown, else linux).
  - `targetfake.Fake` implementing Target with scripted outcomes per VMID for the runner tests.

- [ ] **Step 1: Write failing tests** with `pvefake` + `sshx.Fake`:
  - Prepare: real pve-node `vmbr0` (OVSBridge with `ovs_ports: "enp66s0f0 ovsport_host ovsport_dev"`) → `ErrNotIsolated`; isolated Linux bridge and isolated OVS bridge pass; PVE `8.4.1` → `ErrVersion`; untagged guest 990003 in range `[990000,990049]` → `ErrForeignInRange` mentioning `990003`.
  - AllocateVMID: range `[990000,990001]` with 990000 existing and 990001 in `inUse` → `ErrNoFreeVMID`; never returns a value outside range (table).
  - Sanitize: restored config with `hostpci0` → SetGuestConfig called with delete containing `hostpci0`; fake that ignores the bridge change → verification error, Start never called by caller contract (assert returned error is not nil).
  - Destroy: VMID 100 → refused, no API call; in-range untagged → refused.
  - ClassifyRestoreError: `"decryption failed: missing key"` → ERROR; `"chunk 3f… corrupted"` → FAILED (Review Focus 1).
  - WaitBooted: VM with `agent: 0` → AgentMissing; agent ping succeeds, osinfo `mswindows` → windows; exec blocked → ExecBlocked; LXC `pct status` running + exec ok → linux.
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `"Add existing-node target with isolation and ownership safety"`

---

### Task 14: Job lifecycle — one guest (§6 steps 5–11)

**Files:** Create `internal/run/job.go`, `internal/run/job_test.go`

**Interfaces:**
- Consumes: `target.Target`, `pve.API` (SetVolumeAttrs), `state.Store`, `probe.Run`, `notes.*`, `config.Config`
- Produces: `func proofOne(ctx context.Context, d Deps, b model.Backup, vmid int) model.GuestResult` where `type Deps struct{ Cfg config.Config; API pve.API; Target target.Target; Store *state.Store; Log *slog.Logger; Now func() time.Time; Notifier report.Notifier }` (Notifier defined in Task 16; declare the interface `type Notifier interface{ EnsureTemplates(ctx context.Context) error; Send(ctx context.Context, s model.RunSummary) error }` in `internal/report/notifier.go` in this task so it compiles)

Order and state (every stage recorded in `state.InFlight[vmid]` **before** acting):
1. Record `Job{Stage:"protected", ProtectedOrig:&b.Protected}`; `SetVolumeAttrs(protected=true)` (skip call if already protected).
2. Restore (error → `ClassifyRestoreError`), Stage `restored`.
3. Sanitize (ErrUnsafe → ERROR `unsafe config: ...`, never start).
4. Start, Stage `started`; WaitBooted with `Restore.BootTimeout`.
5. Verdict: AgentMissing → BOOTED `guest agent missing` + `AgentMissing=true`; ExecBlocked → BOOTED `agent cannot run commands`; boot error → FAILED `did not boot: …`; else `probe.Run` with Missing from Sanitize.
6. Deferred, always: Destroy (failure → ERROR result appended reason `cleanup failed`, keep InFlight entry for janitor); restore `protected` to original if PBP changed it; delete InFlight entry only when both succeeded.
7. Write notes via `notes.Merge(b.Notes, notes.Line(verdict, Now(), reason))` — except verdict SKIPPED.
8. PROVEN → `state.LastProven[key] = Now()`.

- [ ] **Step 1: Write failing tests** (targetfake + pvefake): happy path call order `SetVolumeAttrs(protected) → Restore → Sanitize → Start → WaitBooted → Destroy → SetVolumeAttrs(protected=false) → SetVolumeAttrs(notes)`; backup already protected → no protected writes at all; ErrUnsafe → Start never called, Destroy called, verdict ERROR; restore decrypt error → ERROR and notes say ERROR; probe FAILED → notes line first, user notes preserved; Destroy failure → InFlight entry remains with Stage `started`; AgentMissing → BOOTED with `AgentMissing=true`.
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `"Add single-guest proofing lifecycle"`

---

### Task 15: Run orchestration, preflight, janitor, gates (§6 steps 1–4, 12)

**Files:** Create `internal/run/runner.go`, `internal/run/janitor.go`, `internal/run/preflight.go`, `internal/run/runner_test.go`, `internal/run/janitor_test.go`

**Interfaces:**
- Produces:
  - `type Options struct{ Trigger string; OnlyVMID int }` (Trigger `schedule|manual`)
  - `func Run(ctx context.Context, d Deps, opt Options) model.RunSummary`
  - `func Janitor(ctx context.Context, d Deps) error`
  - `func Preflight(ctx context.Context, d Deps) error` — `Target.Prepare`, `Notifier.EnsureTemplates`, `Janitor`

Run:
1. Append RunRecord `running`. Preflight error → summary `RunError`, Outcome `error`, notify, return.
2. Discovery: `ListBackups` per source storage → `NewestPerGuest` → filter `OnlyVMID` → `Order` with `LastProven`.
3. Already proven (`discovery.AlreadyProven`) → SKIPPED `already proven` (no notes write, counted).
4. Worker pool of `Workers`. Before each job: deadline passed (`MaxRunDuration` > 0) → SKIPPED `run deadline`; space gate: `b.Size` ≤ `Avail − reserved − Total*margin/100` else SKIPPED `insufficient space on <storage>`; reserve `b.Size` until job ends; RAM gate: guest memory from `OriginalConfig` (`memory` MiB, default 512 for lxc / 2048 qemu when absent) + 10% ≤ `NodeMemory.Available − reservedRAM` else SKIPPED `insufficient node memory`; `AllocateVMID` (ErrNoFreeVMID → SKIPPED `no free VMID`).
5. Collect results, compute Outcome (`ok` if severity info, `problems` otherwise, `error` on RunError), AppendRun with `Logging.RetainRuns`, send notification when `Notify.When=="always"` or severity ≠ info. Notification failure is logged and sets Outcome `error` (so health goes red) but does not alter verdicts.

Janitor: for each `state.InFlight` job: if guest exists and is tagged `pbp` and in range → Destroy; if `ProtectedOrig` non-nil and false → `SetVolumeAttrs(protected=false)`; remove entry. Then any `Owned` guest not in InFlight → Destroy (logged).

- [ ] **Step 1: Write failing tests:**
  - Preflight `ErrNotIsolated` → no Restore calls, RunError set, notification severity `error`.
  - Two storages, three guests, one already proven → 2 jobs, 1 SKIPPED `already proven`.
  - Space: avail 100G, total 1T, margin 10% → 100G−100G=0 → SKIPPED `insufficient space on local-zfs`.
  - `workers: 2`, range of 1 VMID → second concurrent job SKIPPED `no free VMID` (Review Focus 5); every Restore vmid within range.
  - Deadline elapsed after first job (fake clock) → remaining SKIPPED `run deadline`.
  - `notify.when: problems` with all PROVEN → no Send.
  - Janitor (Review Focus 4): state with InFlight 990000 Stage `started`, ProtectedOrig=false, guest tagged pbp exists → Destroy(990000) and protected cleared; untagged guest 990001 in InFlight → not destroyed, error logged; owned guest 990005 not in state → destroyed.
  - `OnlyVMID: 101` → exactly one job.
- [ ] **Step 2–4:** FAIL → implement → PASS (`go test -race ./internal/run/`).
- [ ] **Step 5: Commit** `"Add run orchestration, capacity gates and janitor"`

---

### Task 16: Reporting — logs, summary, PVE notification (§8)

**Files:** Create `internal/report/logging.go`, `internal/report/summary.go`, `internal/report/notify.go`, `internal/report/templates/pbp-subject.txt.hbs`, `internal/report/templates/pbp-body.txt.hbs`, `internal/report/*_test.go`

**Interfaces:**
- Produces:
  - `func NewLogger(stdout io.Writer, runFile io.Writer) *slog.Logger` — text to stdout, JSON to run file (`slog.NewMultiHandler`)
  - `func OpenRunLog(dir, runID string) (*os.File, error)` → `run-<runID>.log`; `func PruneRunLogs(dir string, retain int) error`
  - `func LogGuest(l *slog.Logger, r model.GuestResult)` — one record per guest with attrs `guest, verdict, reason, restore_s, boot_s, failed_units, ignored_units, checks`; plus one `Info` per NotCovered using `LogLine()`
  - `func TemplateData(s model.RunSummary, node string) map[string]any` — keys `node`, `run-id`, `started`, `duration`, `counts` (map verdict→n), `guests` (list of `{guest,name,verdict,reason}`), `booted` (list, FR4), `not-covered` (list of LogLines), `run-error`
  - `type PVENotifier struct{ SSH sshx.Runner; Node string }` implementing `Notifier`:
    - `EnsureTemplates`: for each embedded template, `cat` the remote file under `/etc/pve/notification-templates/default/`; if missing or differs, `mkdir -p` and write via `cat > path` with stdin
    - `Send`: `ssh.Run("perl -e " + sshx.Quote(script), json)` with script `use PVE::Notify; use JSON; my $in = decode_json(do { local $/; <STDIN> }); PVE::Notify::notify($in->{severity}, 'pbp', $in->{data}, $in->{fields});`, JSON `{severity: s.Severity(), data: TemplateData(...), fields: {type:"pbp", hostname: Node}}`; non-zero exit → error containing stderr
- Templates: subject `PBP {{node}}: {{counts.PROVEN}} proven, {{counts.FAILED}} failed` (use helpers only if stock templates use them; otherwise plain), body lists guests, BOOTED section "Guests without a working guest agent", "Not covered by backup" section, run error.

- [ ] **Step 1: Write failing tests:** LogGuest output (JSON handler into buffer) contains `"verdict":"FAILED"` and the NotCovered line; PruneRunLogs keeps newest 30 of 33; TemplateData for summary with one BOOTED guest has `booted` length 1 and `counts["BOOTED"]==1`; PVENotifier.Send with sshx.Fake: command starts with `perl -e 'use PVE::Notify;`, stdin JSON has `fields.type=="pbp"` and `severity=="error"` for a FAILED run; EnsureTemplates writes both files when `cat` exits 1 and writes nothing when contents match.
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `"Add logging, run summary and PVE notifications"`

---

### Task 17: Health evaluation, daemon scheduler, run lock, CLI wiring (§10)

**Files:** Create `internal/health/health.go`, `internal/health/health_test.go`, `internal/daemon/daemon.go`, `internal/daemon/lock.go`, `internal/daemon/daemon_test.go`; Modify `cmd/pbp/main.go`

**Interfaces:**
- Produces:
  - `func health.Evaluate(st state.State, now time.Time, sched cron.Schedule, maxRun time.Duration, startedAt time.Time) (ok bool, reason string)` — unhealthy when: last finished run Outcome `error` (`last run error`); a scheduled time `sched.Next(lastRunStart or startedAt)` + 30m < now with no run started since (`scheduled run missed`); a `running` record older than `maxRun` (or 24h if 0) (`run overran`). Healthy with no runs yet if no schedule was missed since `startedAt`.
  - `func daemon.AcquireLock(path string) (release func(), err error)` — `syscall.Flock` `LOCK_EX|LOCK_NB` on `/var/lib/pbp/run.lock`; busy → `ErrBusy`
  - `func daemon.Serve(ctx context.Context, cfg config.Config, runFn func(ctx context.Context, trigger string)) error` — cron loop; lock busy at tick → log `previous run still active, skipping`; SIGTERM cancels ctx, waits for in-flight run's teardown (ctx passed to run is a separate context cancelled only after a 10-minute grace)
  - CLI: `daemon` (writes `daemon.started` timestamp into state dir for health), `run [--guest N]` (lock busy → exit 3, message `a run is already in progress`), `check` (Preflight only, exit 0/1, prints each check), `health` (exit 0/1, prints reason)

- [ ] **Step 1: Write failing tests:** Evaluate table (error outcome; missed 02:00 at 03:00 with last run yesterday; running 25h with maxRun 0; fresh daemon started 10 minutes ago with no runs → ok); AcquireLock twice in same process via separate fds → second `ErrBusy`; Serve with a `@every 1s` schedule and a runFn that blocks 2.5s → exactly one concurrent run and a logged skip.
- [ ] **Step 2–4:** FAIL → implement → PASS. Wire `main.go` building real deps (`pve.New`, `sshx.Dial`, `target.NewExistingNode`, `report.PVENotifier`, `state.Open("/var/lib/pbp/state.json")`).
- [ ] **Step 5: Commit** `"Add daemon scheduler, run lock, health and CLI"`

---

### Task 18: Docker image, compose example, README (§10, FR7)

**Files:** Create `Dockerfile`, `docker-compose.yml`, `.dockerignore`; Modify `README.md`

- Dockerfile: `golang:1.26` builder, `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$VERSION" ./cmd/pbp`; runtime `gcr.io/distroless/static-debian12:nonroot` (uid 65532; README tells users to make mounted secrets/volumes readable by that uid); `ENTRYPOINT ["/pbp"]`, `CMD ["daemon"]`; `HEALTHCHECK --interval=5m --timeout=20s CMD ["/pbp","health"]`.
- Compose: service `pbp`, image `pbp:latest` + `build: .`, `restart: unless-stopped`, `TZ`, volumes `./config.yaml:/etc/pbp/config.yaml:ro`, `./known_hosts:/etc/pbp/known_hosts:ro`, `pbp-state:/var/lib/pbp`, `./logs:/var/log/pbp`; secrets `pve_token`, `pve_ssh_key` from files.
- README: rewrite to PBP naming throughout (keep the existing intro quote/voice), then sections: How it works (verdict table from §7.1), Requirements (PVE ≥ 9, PBS ≥ 4, root-equivalent SSH — stated plainly with why), Setup (API token + privileges: `Datastore.Allocate`/`Datastore.AllocateSpace` on restore storage, `VM.Allocate`, `VM.Config.*`, `VM.PowerMgmt`, `VM.Monitor`/`VM.GuestAgent.*`, `Sys.Audit`, `SDN.Use` on the bridge — builder confirms exact list in Task 19 and corrects here), creating the isolated bridge (Linux and OVS examples), VMID range, compose up, `pbp check`, manual run, Notifications (matcher on `type=pbp`), Ignore list (why each default exists), Profiles and guest checks, What "not covered by backup" means.

- [ ] **Step 1:** `docker build -t pbp:dev .` — succeeds.
- [ ] **Step 2:** `docker run --rm pbp:dev` with no config → exits non-zero with message naming `/etc/pbp/config.yaml`.
- [ ] **Step 3:** `docker compose config` — valid.
- [ ] **Step 4:** `grep -rniE 'validation|pbv' --include='*.go' --include='*.md' --include='*.yml' --include='Dockerfile' . | grep -v 'git.breadthanalytics.com/tdiana/pbv' | grep -v '^./specs/'` — no output.
- [ ] **Step 5: Commit** `"Add Docker image, compose example and README"`

---

### Task 19: Integration suite on pve-node (§12)

**Files:** Create `test/integration/integration_test.go` (build tag `integration`), `test/integration/README.md`

Environment: `PBP_IT_CONFIG` → a config file for pve-node (not committed); bridge `vmbr-pbp` created by the builder on pve-node (approved), range `[990000,990009]`, restore storage `local-zfs` (verify it is active on pve-node first — research showed it reported inactive; pick an active one via `pvesm status`).

- [ ] **Step 1:** Create isolated bridge on pve-node (Linux bridge, `bridge-ports none`, no address), `ifreload -a`; record in `test/integration/README.md`. Run `pbp check` — all green.
- [ ] **Step 2:** Test `TestProofSmallLXC` — proof one small running LXC's newest backup (pick e.g. `ct110` 110); assert verdict PROVEN or a FAILED whose reason names real units; assert no guest left in range; assert notes first line matches the PBP regexp and the previous user note (guest name) is on line 2; assert `protected` back to original.
- [ ] **Step 3:** Test `TestProofVMWithAgent` — a VM with `agent: 1` (e.g. `DebianBaseline` 103) → PROVEN/FAILED with agent used; and a VM with agent off → BOOTED `guest agent missing`.
- [ ] **Step 4: Ask the owner before this step** to install notification templates in `/etc/pve`. Then `TestNotificationDelivers` — `EnsureTemplates` + `Send` exit 0; confirm with the owner that the message arrived and rendered (verifies §8.3 override-dir assumption; if new types are not picked up, implement the `/usr/share/pve-manager/templates/default/` fallback and document reinstall-after-upgrade).
- [ ] **Step 5:** Test `TestBrokenBackupFails` — create a throwaway LXC in range, enable a unit that always fails (`ExecStart=/bin/false`), back it up to a PBS storage, proof it → FAILED naming that unit; then remove the throwaway guest and its backup.
- [ ] **Step 6:** Test `TestCrashRecovery` — start a run, kill it after Stage `started`, run `pbp run` again → janitor destroys leftover, protected restored.
- [ ] **Step 7:** Confirm token privilege list empirically (try with the minimal role) and correct README.
- [ ] **Step 8:** Run `go test -tags integration ./test/integration/ -v` — all PASS; `pvesh get /cluster/resources --type vm` shows nothing in `990000–990009`.
- [ ] **Step 9: Commit** `"Add integration suite against a real PVE node"`
