# PBP — Root Access Reduction: Design

Date: 2026-10-19
Status: approved for planning (owner, 2026-10-19)
Branch: `RootAccessReduction`
Target release: `v0.2.0` (breaking: node setup changes)
Builds on: [2026-10-19-pbp-design.md](2026-10-19-pbp-design.md) §3 (access), §8.3 (notifications), §11 (safety)

---

## 1. Intent

Today PBP logs in to the Proxmox node over SSH **as root**. Anyone who obtains PBP's SSH key
(from the proofing server, a backup of it, or the container's secrets) can do anything on a node
that also runs production guests. This change removes root SSH completely and replaces it with a
narrow, auditable helper: a stolen key can then only run commands inside PBP's own throwaway
guests, read the isolated bridge's state, and send PBP notifications.

Decided by the owner:
- **Root SSH is removed entirely** — no fallback, no option to keep it.
- **The owner sets up the node by hand, once** (PBP never changes node configuration itself).
- **All documentation is updated** to the new model.
- Packaging/delivery of the helper reuses the existing `pbp` binary and `.deb`.

## 2. Goals, non-goals, anti-goals

### Goals
- PBP's SSH identity on the node is an unprivileged user `pbp` whose key can only run the helper.
- The helper does exactly four things and nothing else (§4), validating every argument itself.
- `pbp check` verifies the node setup step by step and says exactly what is missing.
- README, design spec, config example and compose example describe only the new model.

### Non-goals
- Changing the PVE API token or its privileges (already least-privilege; unchanged).
- VM probing (already API-only via the guest agent; unchanged).
- Automating node setup (the owner does it by hand; PBP only checks it).
- Supporting multiple PBP instances against one node with different VMID ranges in one helper
  config (one range per node helper — see §5).

### Anti-goals (failure even if it "works")
- Any path where the SSH key can run an arbitrary command on the node, or `pct exec` into a guest
  outside the reserved range.
- The helper trusting the caller for its safety limits (range, bridge name, notification type).
- Keeping a hidden root-SSH code path "just in case".
- A setup so fiddly that operators fall back to root.

## 3. Constraints

- `pct exec` and `PVE::Notify` require root on the node; the helper therefore runs via `sudo`,
  but only the helper binary, only as the forced command.
- The helper is the same Go binary (`pbp node …`), so there is one artifact to build, version and
  ship. On the node it is installed either from the `.deb` (daemon left disabled — no
  `/etc/pbp/config.yaml` on the node, so the service doesn't start) or by copying `/usr/bin/pbp`.
- PVE 9 / Debian 13 nodes: `sudo` may need installing (`apt install sudo`).
- The isolation checks, `pct exec` and notification behaviour must stay byte-for-byte equivalent in
  outcome to today's (verdicts, safety refusals, timeouts).

## 4. The helper: `pbp node`

Invoked only through SSH's forced command; the client's requested command arrives in
`SSH_ORIGINAL_COMMAND` and is parsed by the helper (never by a shell).

Subcommands (the complete list):

| Request (SSH_ORIGINAL_COMMAND) | Does | Validation (helper-side) |
|---|---|---|
| `exec <vmid> -- <argv…>` | `pct exec <vmid> -- <argv…>`, stdin/stdout/stderr and exit code passed through | VMID inside the helper's configured range; guest exists, is an LXC, and carries the `pbp` tag; argv passed as an argv list (no shell) |
| `bridge` | prints the bridge's live state as one JSON document: `disable_ipv6`, addresses (`ip -j addr`), members (`ip -j link show master` or `ovs-vsctl list-ports`) | bridge name comes from the helper's config only (not the request) |
| `notify` | reads the JSON payload (severity, template data, fields) on stdin and calls `PVE::Notify::notify(severity, "pbp", data, fields)` | severity ∈ info/notice/warning/error; template name fixed to `pbp`; `fields.type` forced to `pbp` |
| `version` | prints the helper version | — |

Anything else is refused and logged to the node's journal.

**Refusals vs guest exit codes** *(my call)*: guest commands legitimately exit with any code, so
the helper never signals its own refusals or failures through an ordinary exit code alone. It uses
a reserved exit code (e.g. 250) **plus** a fixed stderr marker line (`pbp-node: refused: <reason>`
/ `pbp-node: error: <reason>`); the daemon treats only the combination as a helper refusal (mapped
to `ErrOutOfRange`/`ErrNotOwned`/transport ERROR as appropriate). `pbp check`'s refusal probe
asserts that combination.

**Timeouts** *(my call)*: every request carries the time the client still allows, as a
duration (`--timeout-ms <n>`, 1 to 10 digits, rounded up; 24h when the daemon's context has no
deadline), not as an absolute time, so clock skew between daemon and node cannot stretch or
shrink it. The helper starts the clock when it receives the request, applies the **shorter** of
that and `request_timeout` (a ceiling, default 15m), and kills the child's whole process group
on expiry, then reports a timeout distinctly (marker `pbp-node: timeout`). So nothing PBP starts
on the node outlives PBP's own deadline (give or take the request's transit time), and PBP's verdicts for timeouts stay as today. Note: `request_timeout` must be at
least `restore.probe_timeout`; `pbp check` warns if it is lower.
*(Background: a client-side cancel cannot reach a root process started through sudo under a
no-pty SSH session, so the deadline must travel with the request.)*

**Bridge type**: the helper detects OVS vs Linux bridge itself (it no longer gets the API's
interface type from the daemon).

### Helper configuration on the node
`/etc/pbp/node.yaml` (root-owned, 0644 — not secret):
```yaml
vmid_range: [990000, 990009]   # must equal the daemon's target.vmid_range
bridge: vmbr99                 # must equal the daemon's target.bridge
request_timeout: 15m
```
The helper refuses to run if the file is missing or invalid. `pbp check` compares these values with
the daemon's config (via the helper's `version`/`bridge` replies plus a new `config` read-only
request that prints the helper's range and bridge) and fails on any mismatch.
*(Builder note: add `config` as a fifth read-only subcommand returning range + bridge + version.)*

## 5. Node setup (manual, once) — documented in README

1. Install the helper: `apt install ./pbp_<ver>_<arch>.deb` (the daemon stays inactive without
   `/etc/pbp/config.yaml`), or copy `/usr/bin/pbp`.
2. Write `/etc/pbp/node.yaml` (§4).
3. Give the `pbp` user a shell so sshd can run the forced command: the `.deb` already creates
   `pbp` (home `/var/lib/pbp`, shell nologin), so run `usermod -s /bin/sh pbp`. Its home
   `/var/lib/pbp` (0700, owned by pbp) holds `.ssh/authorized_keys`. *(My call: the package keeps
   nologin by default, so daemon-only hosts don't get a login shell; only nodes are changed, by
   hand.)* Note: `apt purge pbp` on a node deletes the `pbp` user and therefore PBP's access —
   the README says so. Debian's default `UsePAM yes` lets this locked system account log in by key;
   the troubleshooting section mentions it.
4. `authorized_keys` for `pbp`, one line, forced command and no forwarding:
   `restrict,command="sudo /usr/bin/pbp node" <PBP public key>` (`restrict` disables every
   forwarding, the pty and `~/.ssh/rc`, including options a later OpenSSH adds). `/var/lib/pbp/.ssh`
   (0755) and `authorized_keys` (0644) are owned by root, so the `pbp` user cannot rewrite its own
   forced command; sshd's StrictModes accepts root-owned key files.
5. `/etc/sudoers.d/pbp` (mode 0440, checked with `visudo -cf`):
   `pbp ALL=(root) NOPASSWD: /usr/bin/pbp node` and `Defaults:pbp env_keep += "SSH_ORIGINAL_COMMAND"`
6. Install the notification templates once (previously done by PBP at runtime):
   `pbp node install-templates` run as root by the admin (writes the two files into
   `/etc/pve/notification-templates/default/`). It is not reachable over SSH: sudo only allows
   exactly `/usr/bin/pbp node`, and the helper also refuses a bare `pbp node` with no
   `SSH_ORIGINAL_COMMAND` (what an SSH client sending no command produces).
7. In PBP's config: `target.ssh.user: pbp` (now the default), same key and known_hosts as before.

## 6. Daemon-side changes

- `sshx` sends requests as `exec <vmid> -- <argv>` / `bridge` / `notify` / `config` / `version`
  instead of shell command strings. The `bounded()` timeout wrapper is no longer used for helper
  requests (the helper enforces timeouts); remove it if nothing else uses it.
- `target`: LXC executor → helper `exec`; isolation check → helper `bridge` (same verdict logic on
  the returned JSON).
- `report`: `PVENotifier.Send` → helper `notify`; `EnsureTemplates` → **check only** via a
  read-only helper request `templates` that returns the sha256 of the installed template files;
  the **daemon** compares them with its own embedded templates. A missing/outdated template fails
  preflight with the exact `pbp node install-templates` instruction. *(My call: a template change
  always comes with a minor-version bump, so daemon and node helper of the same major.minor carry
  identical templates; release notes say to re-run install-templates after a minor upgrade.)*
  *(Builder note: `templates` is a sixth read-only subcommand.)*
- `config`: `target.ssh.user` defaults to `pbp`; validation rejects `root` with a message pointing
  to the README's node setup.
- `pbp check`: new step-by-step node checks — helper reachable (`version`), versions compatible
  (same major.minor), range/bridge match (`config`), templates present (`templates`), and a
  harmless `exec` refusal probe (an out-of-range VMID must be refused) to prove the restriction is
  live. Each failing step names the README section to fix.

## 7. Safety (§11 of the main design, extended)

- Range, tag and bridge are enforced **on the node** by the helper from its own config; the daemon's
  checks stay as a second layer.
- The helper never invokes a shell; argv is passed through to `pct exec` as a list.
- A compromised PBP key can: run commands inside `pbp`-tagged LXCs in the reserved range, read the
  bridge's state, send PBP-typed notifications, read the helper's config. Nothing else.
- Spec §3's "PBP holds root-equivalent access" text is replaced by this.

## 8. Testing

- Unit: helper request parsing and validation (table tests: in/out of range, untagged guest, VM not
  LXC, injection attempts in argv and VMID, unknown subcommands, install-templates over SSH refused),
  with `pct`/`perl`/`ip` behind an interface faked in tests.
- Daemon: executor/isolation/notifier tests against a fake helper.
- Dev (on the build host): a Debian 13 container with `sshd`, the `pbp` user, forced command and
  sudo rule, and stub `pct`/`perl`/`ip` scripts — end-to-end over real SSH, including a stolen-key
  attempt to run `id` / `bash` / out-of-range exec (all refused).
- Live (on `pve-node`, with owner approval at the time): set up the helper per README, remove root's
  PBP key, re-run the integration suite.
- **Integration-suite fixtures** (decided by the owner): the suite's own setup steps (creating,
  backing up and destroying throwaway containers; the notification render test) still need root on
  the node. They use a **separate, test-only admin key**, configured only in the integration-test
  config (e.g. `PBP_IT_ADMIN_SSH_KEY` / its own known_hosts), never in PBP's config or product code.
  The owner authorises that key on `pve-node` only while live tests run. The test harness keeps its own
  SSH client for these fixtures; PBP itself (`pbp` daemon and `pbp node`) has no root path. The
  test README documents adding and removing the admin key.

## 9. Documentation (all updated, owner requirement)

README (setup, security notes, operating, troubleshooting), `config.example.yaml`
(`target.ssh.user: pbp`), `docker-compose.yml` comments, design spec §3/§8.3/§11, test/integration
README, and the `.deb` README section (node install path). Release notes for `v0.2.0` call out the
breaking change and the migration steps (set up the helper, switch `ssh.user`, remove root key).

## 10. Left to the builder

Request wire format details beyond §4, JSON field names, exact `pbp check` wording, how `pct`/
`perl`/`ip` are abstracted for tests, and where the helper logs.
