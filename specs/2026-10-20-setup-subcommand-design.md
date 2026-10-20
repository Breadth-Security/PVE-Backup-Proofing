# Guided setup: `pbp setup` and `pbp node setup` — design

Status: draft for owner review. Branch: `SetupSubcommandAddition`.

## 1. Why

Setting PBP up today is about a dozen root commands on the PVE node (README
"Node setup", steps 1–6) plus hand-written files on the PBP host (README
steps 3, 4 and 6: `config.yaml`, the SSH key, `ssh-keyscan`, the token secret).
The owner's worry is typos, and specifically three kinds of damage an
administrator can do by accident:

1. **A typo that silently weakens the restriction** (a wrong `env_keep`, a
   missing `restrict`, a key file the `pbp` user can rewrite).
2. **A broken file that locks them out** of sudo or SSH.
3. **Something that touches the node's networking or other guests.**

The guided setup removes the typing and the judgement calls. Every value is
generated or carried over by copy and paste between two terminals; nothing is
retyped.

## 2. The picture

The administrator has two terminals side by side: the PBP host, and a root
shell on the PVE node. The PBP host is set up first (Docker checkout or
`.deb`). The flow is a round trip:

1. **PBP host:** `pbp setup` asks a few questions (or reads an existing
   `config.yaml`), writes `config.yaml`, generates PBP's SSH key, and prints
   the lines to paste on the node. It then waits: "paste the node's reply here".
2. **Node:** the admin installs the `.deb` and pastes
   `pbp node setup --vmid-range … --bridge … --pubkey '…' [--create-token …]`.
   It checks, shows a plan, asks, applies, proves the result, and prints a
   reply block.
3. **PBP host:** the admin pastes the reply block into the waiting
   `pbp setup`. It writes `known_hosts`, the API TLS fingerprint and the token
   secret, then runs `pbp check`.

## 3. Goals, non-goals, anti-goals

Goals
- One guided path from nothing to a passing `pbp check`, for both the Docker
  and the `.deb` host layouts.
- The node ends up **byte-for-byte** as README "Node setup" describes today
  (same files, modes, owners, contents). The guided setup automates the
  documented design; it does not change it.
- Safe to run twice. A second run that finds everything correct changes
  nothing and prints exactly: **`setup has already been completed`** (owner's
  wording). *(My call: the host-side `pbp setup` uses the same message when
  its own side is complete and `pbp check` passes.)*
- A clean undo on the node: `pbp node uninstall`.

Non-goals
- Creating the isolated bridge (see §6.2).
- Discovering storage names (no API token exists yet when the questions are
  asked; `pbp check` validates them at the end).
- The optional `maxlogins` cap stays a documented manual extra. *(My call: a
  cap set too low shows up as wrongly failed checks.)*
- Multi-node or provisioned-host targets.

Anti-goals (a failure even if it "works")
- Reloading or editing the node's network configuration.
- Touching root's `authorized_keys`, `/etc/ssh/sshd_config`, any guest, or
  anything outside the files listed in §5.3.
- Overwriting a file it did not write without showing the difference and
  being told to.
- A secret in shell history, in a command line, or in a log.
- Setup reachable over SSH or through the sudo rule.

## 4. Host side: `pbp setup`

### 4.1 Where it writes

| | Docker (default) | `.deb` host (`--deb`) |
|---|---|---|
| config | `./config.yaml` | `/etc/pbp/config.yaml` |
| SSH key | `./secrets/pve_ssh_key` (+ `.pub`) | `/etc/pbp/secrets/pve_ssh_key` (+ `.pub`) |
| token secret | `./secrets/pve_token` | `/etc/pbp/secrets/pve_token` |
| known_hosts | `./known_hosts` | `/etc/pbp/known_hosts` |
| logs dir | `./logs` | (package-owned `/var/log/pbp`) |
| ownership | uid/gid 65532, secrets `0400`, others `0444`, `logs/` owned by 65532 | `root:pbp`, `0640`; `/etc/pbp/secrets` `0750 root:pbp` |
| paths in config | `/run/secrets/…`, `/etc/pbp/known_hosts` | `/etc/pbp/secrets/…`, `/etc/pbp/known_hosts` |

These are the layouts README step 6 and "Install from the .deb" already
require; setup produces exactly them.

How it runs under Docker is left to the builder, with these constraints: the
`docker-compose.yml` bind-mounts `config.yaml` read-only and Docker creates a
*directory* when a bind-mount source file is missing, so setup must not be run
through the normal service before `config.yaml` exists. A one-shot
`docker run --rm -it -v "$PWD:/setup" … pbp:latest setup --dir /setup` (or a
compose `profiles: [setup]` service) is the expected shape. The README gives
one exact command. On `.deb` hosts it is `sudo pbp setup --deb`.

### 4.2 Questions

Asked only when `config.yaml` does not exist; each has a default shown in
brackets that Enter accepts. Answers are validated as they are typed, with
the same rules `config` enforces, and re-asked on error.

- node SSH/API address (no default) — fills `target.ssh.host` and
  `target.api.url` (`https://<addr>:8006`)
- VMID range [990000–990049]
- bridge [vmbr99]
- restore storage [local-zfs]
- PBS storage(s), comma-separated (no default)
- schedule ["0 2 * * *"]; for Docker also the time zone (default: the
  host's zone, passed into the one-shot container by the README's command,
  e.g. from `timedatectl show -p Timezone --value`) — *my call*

For Docker, the time zone goes in a git-ignored `.env` next to
`docker-compose.yml`, and the compose file changes `TZ: Europe/London` to
`TZ: ${TZ:-Europe/London}`, so setup never edits a tracked file. *(My call.)*

Everything else comes from `config.example.yaml`'s defaults, with its
comments kept, so the written file reads like the example. `target.node` and
`target.api.tls_fingerprint` are filled from the node's reply (§5.5); until
then they hold placeholders that make `pbp check` fail with "finish `pbp
setup`".

If `config.yaml` exists, setup asks nothing: it reads it, fails on an invalid
config with the normal validation message, and uses its values.

### 4.3 What it prints for the node

1. The install line for the `.deb` of **this daemon's exact version** from the
   release page, with checksum verification and the node's architecture taken
   from `dpkg --print-architecture` on the node. A `dev` build prints a note
   instead ("install a matching dev build on the node") — no URL.
2. The `pbp node setup` line (§5.1) with the range, bridge, public key and
   `--storages <restore>,<pbs…>` filled in, plus `--create-token` when no
   token secret file exists yet. `--storages` is always passed, so a storage
   added to `config.yaml` later gets its ACL on the next setup run. *(My
   call.)*

The release page is public: the download needs no login (checked against
`v0.2.0-rc.1`).

If the reply carries no secret but the host has no token file (the token
already exists on the node: a reinstalled host, a lost secret), `pbp setup`
does not guess. It says the existing token's secret cannot be read back and
prints the node line again with `--rotate-token`, warning that any other
daemon using that token loses access. *(My call: rotation stays an explicit
admin act.)*

The SSH key pair is generated only if it does not exist (ed25519, comment
`pbp`). An existing key is reused, never replaced.

### 4.4 The reply and finish

Setup then prompts "Paste the reply printed by `pbp node setup`, then press
Enter" and reads until the reply's end marker **without echoing**. Ctrl-C
leaves everything written so far in place; re-running `pbp setup` skips the
questions, re-prints the node lines and waits again.

From the reply it writes `known_hosts`, sets `target.node` and
`target.api.tls_fingerprint` in `config.yaml` (an in-place edit of those two
values only), writes the token secret file if the reply carries one, and runs
the equivalent of `pbp check`, printing its result.

*Amended during the build:* setup's final check is `pbp check` without the
janitor. The janitor needs the daemon's state store, which setup must not
open, and with an empty store and no run lock it would destroy every
PBP-owned guest in the range, including one a running daemon is proving
during a re-pair. Setup runs the read-only preflight (node helper, node
version, isolated bridge, restore storage, notification templates, VMID
range); `pbp check` is unchanged. A reply whose range,
bridge or public key do not match this host's config is refused.

## 5. Node side: `pbp node setup`

### 5.1 Command line

```
pbp node setup --vmid-range 990000-990049 --bridge vmbr99 --pubkey 'ssh-ed25519 AAAA… pbp'
               [--create-token --storages local-zfs,pbs-main]
               [--dry-run] [--yes] [--replace] [--rotate-token]
```

Runs only as root, interactively: refused when `SSH_ORIGINAL_COMMAND` is set
(even empty), when `SUDO_USER` is `pbp` (started through the `pbp` account's
sudo rule), when not root, or when stdin is not a terminal. `--yes` skips the
confirmation prompt but not the terminal gate. *(My call.)*

*Amended during the build (ruling R3):* the gate refuses only
`SUDO_USER=pbp`, not any `SUDO_USER`. An admin who becomes root with
`sudo -i` or `sudo pbp node setup` carries their own `SUDO_USER`, and refusing
that would only push them to `su`; the attack the gate exists for is the
`pbp` account reaching setup through its own sudo rule, which that rule does
not allow anyway (it permits only `/usr/bin/pbp node` with no arguments).

### 5.2 Phase 1 — checks (read-only)

Any failure stops before the plan, with a plain sentence saying what is wrong
and what to do:
- root, interactive terminal (above)
- PVE 9 or later; `/etc/pve` present
- this helper's version: printed; the daemon's version is carried in the
  pasted line (*my call: add `--daemon-version`, filled by `pbp setup`*) and
  must be compatible (`nodeproto.Compatible`)
- the public key parses and is `ssh-ed25519`
- the bridge exists and is isolated, by exactly the rule `Prepare` applies
  (so the interfaces of PBP's own running guests are allowed and setup can
  be rerun on a node in use). If it does not exist, print the exact
  `/etc/network/interfaces` stanza and the PVE web UI path (Node → System →
  Network → Create → Linux Bridge, no ports, no IP) and stop. Setup never
  creates it.
- the VMID range holds no guest that PBP does not own: guests inside it must
  carry the `pbp` tag, the rule `CheckRangeClear` applies (*my call*: a rerun
  on a node mid-proof must not fail)
- `UsePAM yes` in effective sshd config (`sshd -T`)
- with `--create-token`: each named storage exists

### 5.3 Phase 2 — plan

Compare the desired state with what is on disk and list every action:

| item | desired state (identical to README "Node setup") |
|---|---|
| `/etc/pbp` | dir `0750 root:pbp` |
| `/etc/pbp/node.yaml` | `vmid_range`, `bridge`, `request_timeout: 15m`; `0640 root:pbp` |
| `pbp` login shell | `/bin/sh`; home `/var/lib/pbp` stays `0700 pbp` |
| `/var/lib/pbp/.ssh` | dir `0755 root:root` |
| `/var/lib/pbp/.ssh/authorized_keys` | one line `restrict,command="sudo /usr/bin/pbp node" <key>`; `0644 root:root` |
| `/etc/sudoers.d/pbp` | the three lines from README step 5; `0440 root:root` |
| notification templates | as `install-templates` writes them |
| token (with `--create-token`) | role `PBPProofer` with exactly README's privilege list; user `pbp@pve`; token `pbp@pve!proof` with `--privsep 1`; ACLs on `/vms`, `/storage/<each>`, `/nodes/<this node>`, `/sdn/zones/localnetwork` |

*Amended during the build:* an existing token counts as already correct only
if it is privilege-separated (`privsep 1`). A token without privilege
separation carries all of the user's permissions and is reported as differing.

What "already correct" means, per item *(my call)*:
- `node.yaml`: parsed and compared by value (`vmid_range`, `bridge`;
  `request_timeout` see below). Formatting and comments are ignored.
- `authorized_keys`: exactly one non-empty, non-comment line, equal to the
  desired line after trimming surrounding whitespace.
- sudoers: the same three directives, compared line by line after trimming
  whitespace and dropping blank lines and comment lines as sudo parses them
  (`#<digits>` user-ID rules and `#include`/`#includedir` are directives, not
  comments).
- templates: by content hash, as `CheckTemplates` does. They live on
  `/etc/pve` (pmxcfs), which fixes mode and owner, so they are excluded from
  every mode/owner check and written the way `install-templates` writes them,
  not by temp-file-and-rename.
- everything else: mode, owner and content exactly.

Each item is reported as **already correct**, **create**, or **differs**. For
"differs", the plan shows a diff and setup stops unless `--replace` was given;
with `--replace` the old file is copied to `<file>.pbp-backup-<timestamp>`
first. An existing `node.yaml` whose `request_timeout` differs from 15m is
kept as is, not "differs" (*my call: it is a tuning knob*).

If every item is already correct: print **`setup has already been
completed`**, print the reply block (§5.5, without a secret), and exit 0.

`--dry-run` prints the plan and exits.

### 5.4 Phase 3 — confirm and apply

Ask "Apply these changes? [y/N]". Then apply in this order, so an
interrupted run never leaves a working key without its restriction:
`/etc/pbp` and `node.yaml` → shell → sudoers → templates → token →
`authorized_keys` **last**.

- Every file is written to a temporary file in the same directory, given its
  mode and owner, then renamed into place.
- The sudoers file is validated with `visudo -cf` before the rename, and
  `visudo -c` after; on failure the rename is undone from the backup.
- The role, user and ACLs (for `--storages`) are brought to the desired
  state on every run, with or without `--create-token`.
- The token: if `pbp@pve!proof` already exists, setup does not create a
  secret (one cannot be read back) and the reply carries none; the role and
  ACLs are still brought to the desired state. `--rotate-token` deletes and
  re-creates it, with a warning that the running daemon will lose access until
  the new secret is installed.

### 5.5 Phase 4 — prove, then reply

The self-test runs the real path, as the daemon would, minus SSH:
`sudo -u pbp env SSH_ORIGINAL_COMMAND='<version request>' sudo /usr/bin/pbp node`
must succeed, and an exec at `vmid_range[1]+1` must be refused with
`out-of-range`. *(The builder decides the exact invocation; the requirement
is that it goes through the installed sudo rule.)* It also re-reads every file
from §5.3 and checks content, mode and owner. A failure is printed loudly,
with the item, and exit is non-zero.

Then print the reply block:

```
-----BEGIN PBP NODE REPLY-----
<base64 of a small JSON object>
-----END PBP NODE REPLY-----
```

The JSON carries: format version, node name, helper version, range, bridge,
the public key it authorized, the node's ed25519 SSH host key (from
`/etc/ssh/ssh_host_ed25519_key.pub`), the API certificate's SHA-256
fingerprint (from `/etc/pve/local/pveproxy-ssl.pem` if present, else
`pve-ssl.pem`), the token id, and the token secret when one was created.
Above it: "Copy this block now; the token secret is not shown again."

*Amended during the build:* the reply is built once as a preflight (with no
secret) right after the plan, before anything is printed, prompted or
applied, so a node whose host key or API certificate cannot be read fails
with "nothing changed". A partial reply block is never printed. If the reply
cannot be built after Apply while a new secret is in hand (a race), the
secret is printed once, outside any reply markers, with the token id and the
instruction to put it in the host's token file by hand, and the command exits
non-zero.

The host key and fingerprint arrive over the root shell the admin already
trusts, which replaces `ssh-keyscan`'s trust-on-first-use.

## 6. Decisions

### 6.1 Token: a flag
The token step has the most typing (thirteen privileges, one ACL per storage),
and it only adds a role, a user and a token. `--create-token` is added by
`pbp setup` whenever the host has no token secret yet. It is idempotent and
never silently rotates a secret. (Owner agreed.)

### 6.2 Bridge: check only
Creating a bridge edits `/etc/network/interfaces` and reloads the node's
networking — exactly the third kind of damage. Setup checks it and prints the
stanza; it never creates it. (Owner agreed.)

## 7. `pbp node uninstall`

Same gate as setup (root, interactive). Shows a plan and asks. Removes
`/etc/sudoers.d/pbp` (then `visudo -c`), `/var/lib/pbp/.ssh/authorized_keys`
and the directory if empty, `/etc/pbp/node.yaml`, and sets the `pbp` shell
back to `nologin`. Removes the sudo rule and key **first**, so the access
goes before anything else. Leaves the token unless `--remove-token`
(removes token, ACLs, user and role), and never touches the templates or the
package. A file whose content is not what setup would have written is backed
up before removal. *(My calls; the owner saw these as guesses.)*

## 8. Constraints

- Go only, standard library plus the modules already in `go.mod`; ed25519 key
  generation via `golang.org/x/crypto/ssh` (already a dependency) or the
  standard library. No new runtime dependency on the node beyond what the
  README already uses (`visudo`, `usermod`, `pveum`/`pvesh`, `sshd -T`).
- All node commands by absolute path with a minimal environment, as the
  helper does today.
- No secret in argv, in a log, in the audit line, or echoed.
- `config.yaml` is written with the example's comments preserved.
- Release: **v0.3.0** (*my call*: a new feature on the 0.x line; helper and
  daemon must share major.minor, so both are upgraded together, as today).

## 9. Docs

- README "Setup" leads with the guided path: install, `pbp setup`, paste on the
  node, paste the reply back, `docker compose up -d`.
- The current manual steps move to a section "Manual setup (what `pbp setup`
  does)", kept accurate, because they are the reference for what the tool must
  produce and for admins who prefer to do it by hand.
- CHANGELOG v0.3.0; `config.example.yaml` unchanged.
- `docker-compose.yml`: `TZ: ${TZ:-Europe/London}`; `.gitignore` gains
  `.env`, `config.yaml`, `known_hosts`, `secrets/` and `logs/` if not already
  ignored, so setup's output can never be committed by accident.

## 10. Testing

- Unit tests for the planner (every item: already correct / create / differs),
  the reply encoding, the question validation, and the config writer.
- `test/nodee2e` (dev-scratch): the image no longer replays README commands;
  it runs `pbp node setup … --yes` (with stubbed `pveum`/`pvesh`/`sshd -T`),
  then the existing ten tests must still pass unchanged. New cases: a second
  run prints `setup has already been completed` and changes nothing (mtimes);
  a pre-existing differing sudoers file stops the run without `--replace`;
  `--replace` leaves a backup; setup over SSH and through sudo is refused;
  `pbp node uninstall` leaves no access (the attacker key gets nothing).
- A host-side test drives `pbp setup` end to end against that container: it
  answers the questions, pastes the reply, and gets a passing check.
- pve1 (runs production guests; only VMIDs 990000–990009 are PBP's): upgrade
  its helper to 0.3.0 (owner: no running daemon depends on it). Run
  `pbp node setup --dry-run` and then for real against the existing manual
  setup; it must report `setup has already been completed` and change nothing.
  Then `pbp node uninstall`, confirm PBP's key gets nothing, run the full
  round trip again from a fresh host directory (including `--rotate-token`
  for the existing `pbp@pve!proof`), and finish with `pbp check` and the
  live suite. The owner allows PBP's own setup on pve1 (user, files, token)
  to be removed and changed as needed; root's `authorized_keys`, sshd config,
  networking and every non-PBP guest stay untouched.

## 11. Left to the builder

- Package layout (`internal/setup` for the host, inside `internal/node` for the
  node, or similar).
- Exact wording of prompts and messages, except
  `setup has already been completed`.
- How the token commands are issued (`pveum` vs `pvesh`), as long as the result
  matches §5.3.
- The exact Docker invocation (§4.1 constraints apply).
