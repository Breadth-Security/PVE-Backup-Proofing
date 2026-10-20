# PBP — Proxmox Backup Proofing: Design

Date: 2026-10-19
Status: draft for approval
Traces to: [functionalSpecs.ms](functionalSpecs.ms) (FR1–FR7)

---

## 1. Intent

Proxmox Backup Server already verifies that backup snapshots are intact on disk.
That proves the bytes are good. It does not prove the backup will become a working
machine after a restore. PBP closes that gap: it **trusts** PBS's own verification
and **proves** recoverability by actually restoring each backup into an isolated,
throwaway guest, booting it, checking it is healthy, recording the verdict on the
backup itself, and destroying the guest.

Success looks like: every morning an operator can see, next to each backup in the
PVE/PBS UI and in a notification from their own PVE node, whether last night's
newest backup of every guest really boots and runs its services, and the operator
trusts that answer because PBP does not cry wolf.

The product name is **PBP (Proxmox Backup Proofing)**. "Validation"/"PBV" naming is
retired everywhere (README, binary, image, docs).

## 2. Goals, non-goals, anti-goals

### Goals (v1)
- Proof the **newest backup of every VM and LXC** on the configured PBS storage(s), on a schedule.
- Restore into an isolated temporary guest on a **designated existing PVE node**, boot it,
  run health checks, destroy it.
- Report per-guest verdicts in: the container log, the backup's PBS **notes**, and a run
  summary sent through the **PVE node's own notification system**.
- Ship as a Docker image with an example `docker-compose.yml` and YAML config.

### Non-goals (v1) — the config must leave room for both
- **Throwaway PVE hosts** (nested/ephemeral hypervisors). v1 targets an existing node; the
  code keeps a seam for a future "provisioned host" target (see §5.1).
- **Scoping which backups are proofed** (by guest, tag, pool, age…). v1 proofs newest-of-every-guest;
  the config has a reserved `selection:` section (see §9).
- Multiple target nodes per PBP instance (run multiple containers instead).
- A web UI.
- Application-data correctness beyond the configured checks (e.g. row counts, file diffs).
- Talking to PBS directly. Everything goes through the PVE node.

### Anti-goals: these make PBP a failure even if it "works"
- **Touching anything PBP did not create.** No production guest, VMID, bridge or disk is ever modified,
  except the notes/protected attributes of the backups being proofed.
- **Leaving debris.** Temporary guests or disks surviving a run (including after a crash).
- **Network leakage.** A restored guest reaching any real network (duplicate IPs, services
  announcing themselves, jobs running against production systems).
- **Crying wolf.** Good backups regularly marked FAILED because of the isolated environment.
  Operators learn to ignore the report, and then the tool is worthless.
- **Silent nights.** A run that fails to happen or fails to report must be noticeable; "no
  news" must never be mistaken for "good news".
- **Clobbering user notes** on backups.

## 3. Constraints

- **Language: Go.** (Decided: plumbing-heavy tool. API, SSH, polling, YAML. Single static binary.)
- **Distribution:** Docker image + compose example (FR7). Always-running container.
- **Versions:** PVE **9.0 or newer** and PBS **4.0 or newer** (the PBS line released alongside
  PVE 9) are required (decided). No fallbacks for 8.x. Preflight checks the node's PVE version
  and refuses older ones.
- **Access to the target node** requires both:
  - a **PVE API token** (reads, restores, config, annotations), and
  - **SSH as the unprivileged `pbp` user** to the node, restricted by an SSH forced command to
    the `pbp node` helper (one sudo rule), because:
    - `pct exec` has no API equivalent (FR6), and
    - arbitrary notifications can only be sent via `PVE::Notify` on the node itself
      (the notifications REST API only has a fixed "test" send).
  - There is no root SSH path. The helper only runs `pct exec` in tagged containers inside the
    reserved range, reads the isolated bridge's state, sends `pbp` notifications and prints its
    config. Revised from the original v1 text (root-equivalent access) by
    [2026-10-19-root-access-reduction-design.md](2026-10-19-root-access-reduction-design.md)
    (release 0.2.0).
- The source PBS storage(s) must be configured on the target node (PVE storage config is
  cluster-wide, so any cluster node's PBS storage works).
- The existing Rust stub (`pbvRunner/`, including its committed `target/` build output) is
  **disposable** and is removed.

## 4. Requirement traceability

| FR | Requirement (abridged) | Where satisfied |
|----|------------------------|-----------------|
| FR1 | Pull from a PBS storage on a Proxmox host | §6 step 2 (discovery via PVE storage API) |
| FR2 | Restore VM/LXC to a temporary host, verify start | §6 steps 4–7, §7 |
| FR3 | Temporary host created programmatically, isolated, destroyed | Reinterpreted: **temporary guest** on a designated node, on an operator-created isolated bridge that PBP verifies; guest destroyed after use (§6, §8). Throwaway hosts deferred (§5.1). |
| FR4 | VMs without guest agent noted and reported | §7.1 verdict `BOOTED` + log/notes/notification |
| FR5 | VMs with agent: use agent status to verify startup | §7.1 |
| FR6 | LXCs: verify via logs, `pct status`, `pct exec` | §7.1 |
| FR7 | Docker, compose-enabled, example compose file | §10 |

## 5. Architecture

Single Go binary `pbp`, running as a long-lived daemon inside the container.

Components:

- **Scheduler:** cron expression from config; starts a run. Enforces **one run at a time**
  (a run lock); a scheduled run that comes due while one is active is skipped and logged.
- **Target:** abstraction over "where proof guests live" (§5.1).
- **Discovery:** lists backups on the source storage(s) and selects newest per guest.
- **Planner / capacity gate:** orders work, applies space checks, hands jobs to workers.
- **Worker pool:** N concurrent workers (configurable, default 1). Each runs the per-backup
  lifecycle (§6).
- **Prober:** runs health checks inside a guest via the guest agent (VMs) or `pct exec` (LXCs).
- **Reporters:** log, backup notes, node notification.
- **Janitor:** removes leftover PBP guests and restores leftover protected-flag state; runs at
  startup and at the start of every run.
- **State store:** small file in a mounted volume (`/var/lib/pbp`) recording in-flight jobs
  (which VMIDs were created, which backups PBP set `protected` on and their original value),
  and run history. Needed for crash recovery.

### 5.1 Target seam
`Target` interface: prepare (verify node, bridge, storage), allocate VMID, restore, start/stop,
destroy, exec, release. v1 implements `ExistingNodeTarget`. A future `ProvisionedHostTarget`
would create/destroy a PVE host around the run. Config has `target.type: existing-node` to
leave room for it.

## 6. Run lifecycle

1. **Preflight** (abort run with ERROR notification if any fails):
   - API token works; SSH works and host key matches `known_hosts`.
   - Isolated bridge exists on the node, is a bridge, has **no physical ports/bond members
     and no IP address**. If not isolated, refuse to run.
   - Restore storage exists, is enabled, supports VM images/container rootfs.
   - VMID range contains **no guest PBP does not own** (no `pbp` tag). If one exists, refuse
     to run and report it.
   - Notification template present on node (install/update if missing, §8.3).
   - Janitor pass.
2. **Discovery:** for each configured source storage, list backup content via the PVE API;
   group by guest (type + VMID); pick the newest snapshot per guest **across all configured
   storages** (a guest backed up to two storages, e.g. main + sync, is proofed once, using its
   newest snapshot). Guests are identified by type + VMID; one PBP instance assumes its source
   storages belong to one cluster (use PBS namespaces/separate storages to keep clusters apart).
   *(builder note: my call)*
   **Ordering** *(my call)*: guests whose last PROVEN verdict is oldest (or who were never
   proven) go first, so if a run is cut short the most-overdue guests were covered.
3. **Already-proven skip** *(builder note: my call)*: if the snapshot's notes already carry a
   `PBP PROVEN` line for this exact snapshot, skip it (logged as already proven). Config
   `reproof: false` default; `true` forces re-proofing.
4. **Capacity gate:** before each restore, compare the backup's size against the restore
   storage's free space **minus** space reserved by in-flight restores **minus** a safety
   margin (`restore.space_margin_percent`, default 10). If it does not fit → `SKIPPED`.
   (Builder: backup size reported by PVE is the logical/uncompressed guest size for PBS
   content; confirm on a real node and use the more conservative figure.)
   **RAM gate** *(my call)*: before starting a restored guest, check the node's free memory
   covers the guest's configured memory plus a margin; if not → `SKIPPED` (reason: node memory).
   The owner said load on the node is otherwise acceptable, so no I/O throttling in v1.
   **Run deadline** *(my call)*: optional `max_run_duration` (default unset). When reached, no
   new jobs start; in-flight jobs finish; unstarted guests are `SKIPPED` (reason: run deadline)
   and come first next run via the ordering above.
5. **Protect:** record the backup's current `protected` value, set `protected=1` so pruning
   cannot remove it mid-proof.
6. **Restore** to the next free VMID in the reserved range on the restore storage, with unique
   MACs, not started. Name `pbp-<origvmid>-<YYYYMMDDHHMM>`, tag `pbp`.
7. **Sanitize** the restored config before first start:
   - Every network device → the isolated bridge (keep model; drop VLAN tags/rate limits).
   - `onboot=0`; ensure not in HA; no replication jobs.
   - Remove PCI/USB passthrough and other host-hardware mappings (hostpci, usb, serial-to-host,
     LXC device passthrough, bind mounts to host paths).
   - Anything that cannot be sanitized → `ERROR` (do not start an unsafe guest).
8. **Start** and run checks (§7) within the boot timeout.
9. **Teardown:** stop (hard stop after grace), destroy with purge of all disks. Verify gone.
10. **Unprotect:** restore the backup's original `protected` value.
11. **Annotate** the backup's notes (§8.2).
12. After all jobs: write run summary to log, send notification (§8.3), record history.

Steps 9–10 run regardless of check outcome (defer-style), and the state store makes them
recoverable by the janitor after a crash.

Builder note: some operations may be refused to an API token (e.g. removing `hostpci`, restoring
privileged containers, bind mounts are root-only in PVE). Prefer the API; fall back to the
equivalent CLI (`qmrestore`, `pct restore`, `qm set`, `pct set`) over SSH where the API refuses.
(Superseded in 0.2.0: the `pbp node` helper offers no such commands, so the API must suffice.)
Verify each on a real node.

## 7. Proofing checks

### 7.1 Verdicts

| Verdict | Meaning | Says something about the backup? |
|---------|---------|-----|
| `PROVEN` | Restored, booted, all checks passed | Yes: good |
| `FAILED` | Restore failed, boot failed, or any check failed | Yes: bad |
| `BOOTED` | Guest reached running but PBP could not look inside (VM without guest agent, or agent without exec rights) *(builder note: my call; satisfies FR4)* | Partially |
| `SKIPPED` | Not attempted (insufficient space, already proven) | No |
| `ERROR` | PBP or node problem (SSH/API failure, node out of RAM, unsafe config) | No |

Only `FAILED` indicts the backup. `SKIPPED`/`ERROR` mean "not proven yet" and are reported as such.

### 7.2 Boot detection
- **VM:** wait for `status=running`, then poll guest agent ping until it answers or timeout.
  No agent configured / never answers but VM running → `BOOTED` with "guest agent missing" (FR4).
  Agent answers → proceed to inside checks via agent exec (FR5). Agent answers but exec is
  blocked (common on RHEL-family default agent policy) → `BOOTED` with reason.
- **LXC:** `pct status` = running, then `pct exec` into it (FR6). Capture recent journal/boot log
  (`journalctl -b` or `/var/log` equivalent) into the PBP log on failure.
- Boot timeout configurable, default **5 minutes** per guest. Use `systemctl is-system-running --wait`
  bounded by the timeout where available.
- The `--wait` is bounded by the probe's remaining time (`restore.probe_timeout`, keeping back a
  quarter of it, at most 30s, for the checks that follow), **not** by the per-command
  `checks.command_timeout`: on the isolated bridge, DHCP and wait-online units can keep a healthy
  guest "starting" for a minute or two. If boot still hasn't settled, PBP asks once more without
  `--wait`: `starting`/`initializing` → ERROR `guest still starting after <time>` (a hung boot step
  doesn't by itself show the backup is bad); `running`/`degraded` continue as normal; any other
  state → FAILED as below.

### 7.3 Base health (every Linux guest)
- `systemctl is-system-running`: `running` ok; `degraded` → inspect failed units;
  `maintenance`/`emergency`/no answer → FAILED.
- `systemctl --failed`: **any failed unit not on the ignore list → FAILED** (decided).
  Failed units are listed in the log and summary.
- Root filesystem mounted read-write; read-only root → FAILED.
- Non-systemd (e.g. Alpine/OpenRC): `rc-status`, any crashed/stopped-but-enabled service not on
  the ignore list → FAILED. Unknown init: report "init answering, service state unknown" as `BOOTED`.
  Before `rc-status --crashed`, wait (bounded like the systemd `--wait`) until OpenRC has finished
  booting: runlevel past sysinit/boot, the runlevel-changing `openrc` process exited, and no
  service starting/stopping. Default-runlevel services, where crashes show up, start only after the
  boot runlevel, which on the isolated bridge waits on networking (~15s on Alpine, seen on pve1).
  Never settled in time → ERROR `guest still starting after <time>`.
- OS detection: guest agent `get-osinfo` for VMs; container `ostype` for LXCs.

### 7.4 Windows VMs
- Via agent exec (PowerShell): automatic-start services that are not running → FAILED, minus a
  Windows ignore list. Delayed-start and trigger-start services need care to avoid crying wolf;
  builder curates the default Windows ignore list and documents it.

### 7.5 Ignore list (decided)
- Default config ships with network-dependent units that fail on an isolated bridge, e.g.
  `systemd-networkd-wait-online.service`, `NetworkManager-wait-online.service`,
  `chronyd.service`, `systemd-timesyncd.service`, `ntp*.service`, remote mounts
  (`*.mount` of type nfs/cifs, `remote-fs.target` dependents), `cloud-init*` network stages.
  Builder finalizes the list by testing common distros.
- Ignored failures still appear in the log (as "ignored"), never silently dropped.
- Documented in the README with the reason (isolated network).

### 7.6 Service profiles (decided)
- PBP reads enabled units inside the restored guest (`systemctl list-unit-files --state=enabled`),
  which reflects the original host.
- For every enabled unit matching a **profile**, run the profile's checks.
- **Built-in profiles** (starter set, builder may extend): postgresql (`pg_isready`),
  mariadb/mysql (`mysqladmin ping`), nginx / apache2 / httpd (80/443 listening),
  docker (`docker info`), redis (`redis-cli ping`), sshd (22 listening), cron (active).
- **User profiles** in YAML, same shape, keyed by unit glob; apply to every guest that has the
  unit enabled; a user profile with the same name overrides a built-in.
- Check kinds: `services` (must be active), `ports` (TCP listening inside guest, via `ss -ltn`),
  `commands` (must exit 0, with timeout).
- Profile check failure → FAILED.

### 7.7 Per-guest checks
Optional, matched by VMID, name, or PVE tag; same check kinds. Applied in addition to profiles.

### 7.8 Dependencies missing from the backup (decided)
A restore can legitimately lack things the original guest used: disks marked `backup=0`, LXC
mount points excluded from backup, bind mounts and passthrough devices removed by sanitization
(§6.7), and network storage (NFS/CIFS/iSCSI) unreachable on the isolated bridge.

- PBP records what was missing or removed while restoring and sanitizing (from the original
  guest config in the backup vs the restored, sanitized config).
- When analysing failed units, PBP traces each failure to those missing pieces (e.g. a `.mount`
  unit for an excluded disk's fstab entry, a service whose `RequiresMountsFor=`/`After=`/
  `BindsTo=` chain leads to such a mount, or a device unit for removed passthrough).
- Such failures **do not count toward FAILED**. They are logged explicitly, e.g.
  `Found postgresql.service depends on /srv/pgdata (disk scsi1, backup=0): not backed up, skipped`.
- They are also listed in the run summary under "not covered by backup", so operators can see
  when a backup is missing something a service needs. This is a feature, not just noise control.
- Failures that cannot be traced to a missing piece still count as FAILED.
- The builder decides the tracing method (systemd dependency queries, fstab parsing, mount
  source matching); it must be conservative: when in doubt, it is a real failure.

## 8. Reporting

### 8.1 Log
- Structured logging (Go `log/slog`) to stdout (for `docker logs`) and to a per-run file in a
  mounted volume (`/var/log/pbp/run-<timestamp>.log`), retention configurable.
- Per guest: verdict, reason, restore duration, boot duration, failed/ignored units, check results.
- `BOOTED` (no agent) guests are explicitly listed in the run summary (FR4).

### 8.2 Backup notes
- Written via `PUT /nodes/{node}/storage/{storage}/content/{volid}` (`notes` field).
- PBP owns exactly **one line**, placed **first** so it shows as the backup's comment in the UI:
  `PBP <VERDICT> <YYYY-MM-DD HH:MM> · <short reason>`
  e.g. `PBP PROVEN 2026-10-19 02:14 · agent ok, 3 profiles ok`
  e.g. `PBP FAILED 2026-10-19 02:31 · failed: postgresql.service`
- Existing user notes are **preserved** below it; a previous PBP line is replaced, never stacked.
  *(builder note: my call on line placement)*
- `SKIPPED` because already proven does not rewrite notes.

### 8.3 Notification via the PVE node
- One summary per run, sent on the target node via the `pbp node` helper (over SSH) calling
  `PVE::Notify::notify(severity, "pbp", data, fields)`, so it uses whatever targets/matchers the operator already has.
- The summary includes a "not covered by backup" section (§7.8) and, when non-empty, an
  "Environment errors" section (pbp-tagged guests outside the VMID range, §11; a failed check
  for them).
- Severity: `info` all PROVEN; `warning` any BOOTED/SKIPPED/ERROR or any environment error; `error` any FAILED or
  run-level failure.
- Metadata fields include `type=pbp` and `hostname` so operators can route via matchers.
- Template files `pbp-subject.txt.hbs`, `pbp-body.txt.hbs` (and optionally `.html.hbs`) installed
  by the admin with `pbp node install-templates` (as root on the node) into
  `/etc/pve/notification-templates/default/`; preflight only checks them (changed in 0.2.0, since
  PBP no longer has root).
  **Must verify on a real node** that a brand-new type is picked up from the override dir; if
  not, fall back to `/usr/share/pve-manager/templates/default/` and document that it may need
  reinstalling after upgrades.
- Config `notify.when: always | problems`, default **always**, so a missing nightly message is
  itself a signal (anti-goal: silent nights).

## 9. Configuration

YAML, mounted at `/etc/pbp/config.yaml`. Secrets referenced by file path (Docker secrets or
mounted files), never inline. Illustrative shape (builder finalizes names):

```yaml
schedule: "0 2 * * *"          # cron, container TZ
workers: 1

target:
  type: existing-node          # future: provisioned-host
  node: pve1
  api:
    url: https://pve1.example.lan:8006
    token_id: pbp@pve!proof
    token_secret_file: /run/secrets/pve_token
    tls_fingerprint: "AB:CD:..."   # or ca_file; never skip-verify by default
  ssh:
    host: pve1.example.lan
    user: pbp                  # restricted helper user; root is rejected (see root-access-reduction design)
    key_file: /run/secrets/pve_ssh_key
    known_hosts_file: /etc/pbp/known_hosts
  bridge: vmbr-pbp             # operator-created, no ports, no IP; PBP verifies
  vmid_range: [990000, 990049]

sources:
  storages: [pbs-main]         # PBS-type storages visible on the node

selection: {}                  # reserved for future scoping; v1 = newest per guest

restore:
  storage: local-zfs
  space_margin_percent: 10
  boot_timeout: 5m
  reproof: false

checks:
  ignore_units:                # defaulted; see README for why
    - systemd-networkd-wait-online.service
    - NetworkManager-wait-online.service
    - chronyd.service
    - systemd-timesyncd.service
  windows_ignore_services: [ ... ]
  profiles:
    - name: myapp
      unit: "myapp*.service"
      ports: [8080]
      commands: ["curl -sf http://127.0.0.1:8080/health"]
  guests:
    - match: { vmid: 105 }
      services: [postgresql]

notify:
  when: always

logging:
  dir: /var/log/pbp
  retain_runs: 30
```

## 10. Distribution and operation

- Multi-stage Docker build → static Go binary on a minimal base (distroless/static or alpine).
  Images for amd64 (arm64 nice-to-have).
- Example `docker-compose.yml` with: config mount, secrets, volumes for `/var/lib/pbp` (state)
  and `/var/log/pbp` (logs), `restart: unless-stopped`, TZ.
- **Docker HEALTHCHECK** (decided): the container reports unhealthy when the last run ended in a
  run-level ERROR (e.g. node or SSH unreachable, so no notification could be sent), when a
  scheduled run did not start, or when the current run has overrun. This is the backstop for
  "silent nights" when the PVE node itself cannot notify. Implemented as `pbp health` reading
  the state store. An external heartbeat URL is **not** in v1.
- CLI subcommands of the same binary:
  - `pbp daemon`: default entrypoint, scheduled runs.
  - `pbp run [--guest <vmid>]`: one-off run now (via `docker compose exec pbp pbp run`);
    respects the single-run lock.
  - `pbp check`: preflight only; validates config and access without restoring anything.
- README: setup (API token + required privileges, SSH key, creating the isolated bridge, VMID
  range, storage), the ignore list rationale, verdict meanings, notification routing.

## 11. Safety rules (non-negotiable)

1. Never create, modify, start, stop or destroy a guest outside the reserved VMID range.
2. Destroy a guest in the range only if it carries the `pbp` tag, or — for a restore that
   failed before it could be tagged — PBP's state records the VMID as an in-flight restore,
   the guest is stopped, and its name/hostname matches the backup's original config (or it is
   still locked by the restore).
3. Never start a restored guest until every NIC is on the verified isolated bridge and host
   passthrough is removed.
4. Refuse to run if the bridge is not isolated.
5. Only modify backup attributes `notes` and `protected`, and always restore `protected` to its
   original value.
6. All teardown is crash-recoverable via the state store + janitor.
7. A guest tagged `pbp` outside the reserved range is never touched; it is reported as an
   environment error in the run summary.
8. No root path: PBP reaches the node only through the `pbp node` helper, which itself refuses any
   guest outside its configured range, any untagged guest and any non-container, whatever the
   daemon asks (root-access-reduction design, §3 and §7).

## 12. Testing

- Unit tests with interfaces around the PVE API client and SSH runner, using fakes
  (`httptest` for the API).
- Integration test suite (build-tagged, opt-in) against a real PVE node with a small LXC and
  VM backup set, including a deliberately broken backup to prove FAILED is detected.
- Build/run environment: `dev-scratch.cic.unifiedlab.lan` (SSH key access to be granted).
- Integration node: a **standalone** PVE 9 node lent by the owner. Creating the isolated bridge
  there is approved, and so is restoring deliberately broken backups for FAILED-path tests.
  HA/replication sanitization cannot be integration-tested on a standalone node and is
  covered by unit tests. Installing notification templates into `/etc/pve` on it still needs
  the owner's go-ahead when the time comes.
- Items flagged "verify on a real node" above must each have an integration test or a recorded
  manual check before v1 is called done.

## 13. Left to the builder

- Go module layout, libraries (suggested: `golang.org/x/crypto/ssh`, `log/slog`, a cron lib,
  a YAML lib; a thin in-house PVE client rather than a large SDK).
- API vs CLI-over-SSH per operation, subject to §6 builder note.
- Exact config key names, default ignore lists, built-in profile commands.
- Notification template wording.
- Repo cleanup: remove `pbvRunner/`, add `.gitignore`, update README naming to PBP.
