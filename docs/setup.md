# Setup

Two guided commands set PBP up: `pbp setup` on the host that runs PBP, and `pbp node setup` on the Proxmox node. `pbp setup` prints the `pbp node setup` line for you; you run it on the node, and paste the block it prints back into `pbp setup`. That copy and paste carries everything each side needs from the other (the SSH key, the node's host key and certificate fingerprint, the API token secret), so nothing is typed twice and no secret ends up in a command line or your shell history.

- **PBP on its own host** (recommended for production): two shells, one on the PBP host and a root shell on the node. Follow steps 1 to 6 below.
- **PBP on the Proxmox node itself** (a single-node lab or a trial): one shell. Do steps 1 and 2, then see [PBP on the Proxmox node itself](#pbp-on-the-proxmox-node-itself).

For production, run PBP on a separate host, outside the Proxmox node or cluster it proofs: [Where to run PBP](security.md#where-to-run-pbp) explains why. To restore and boot the proof copies on a separate Proxmox host as well (a standby or spare instead of a production node), the same steps apply to that host; [Proofing on a dedicated node](dedicated-node.md) covers what it needs first, above all its PBS credentials.

Before you start, get PBP onto the PBP host: a checkout of this repository (Docker), or the `.deb` package ([Install from the .deb](install-deb.md)). If you would rather do everything by hand, or want to see exactly what the two commands change, see [Manual setup](manual-setup.md); the guided commands produce that same end state. Why the setup looks the way it does is in [Security model](security.md).

## 1. Choose a VMID range

Pick a range of free VMIDs for the temporary proof guests, for example `990000`-`990049`. There is nothing to create: `pbp setup` asks for the range and writes it to `target.vmid_range`, and `pbp node setup` checks that no guest of yours is in it. The range size is the most guests PBP can have in flight at once.

- **Keep your own guests out of the range, and never tag them `pbp`.** PBP owns every `pbp`-tagged guest in its range and will destroy it, and refuses to run while an untagged guest sits in the range (details: [Security model](security.md#what-pbp-touches-the-vmid-range-and-the-pbp-tag)).
- **Exclude the range from your backup jobs.** A backup job that covers "all" guests can catch a temporary proof guest mid-run, wasting backup space and time. In Datacenter > Backup, make each job exclude the PBP VMIDs, or select guests by pool (and never put PBP guests in that pool). PBP ignores backups whose VMID is in its range.
- **More than one PBP instance** against the same cluster needs one range each, with no overlap.

## 2. Create the isolated bridge

Restored guests run on a bridge of their own that has no physical ports, no IP address and IPv6 disabled, so a copy of a production server can never reach your network or the node ([why](security.md#the-isolated-bridge)). PBP checks this bridge at every preflight and refuses to run otherwise, but neither `pbp setup` nor `pbp node setup` creates it: you do, once, on the node. PVE requires bridge names of the form vmbrN; the examples use `vmbr99`, which is also `pbp setup`'s default.

**Linux bridge.** Add a stanza to `/etc/network/interfaces` on the node, then apply it with `ifreload -a`:

```
auto vmbr99
iface vmbr99 inet manual
        bridge-ports none
        bridge-stp off
        bridge-fd 0
        post-up echo 1 > /proc/sys/net/ipv6/conf/vmbr99/disable_ipv6
```

No `address` line, no gateway, `bridge-ports none` so that nothing is attached, and the `post-up` line disables IPv6 on the bridge every time it comes up.

**Open vSwitch bridge.** Create an `OVSBridge` with no `ovs_ports` and no address, either in the GUI (Node > System > Network > Create > OVS Bridge, leaving "Bridge ports" empty), or in `/etc/network/interfaces`:

```
auto vmbr99
iface vmbr99 inet manual
        ovs_type OVSBridge
        post-up echo 1 > /proc/sys/net/ipv6/conf/vmbr99/disable_ipv6
```

Then apply with `ifreload -a`. Do not add `ovs_ports`, an address, or an OVS port/bond for a physical NIC.

**Created it in the GUI?** The GUI cannot add the `post-up` line: add it to the bridge's stanza in `/etc/network/interfaces` afterwards, and run `echo 1 > /proc/sys/net/ipv6/conf/vmbr99/disable_ipv6` once to disable IPv6 right away (the `post-up` line covers reboots and reloads).

If the bridge is missing or not isolated, `pbp node setup` stops before changing anything and says why (a missing bridge is reported with the stanza above). Setup never creates a bridge or reloads networking.

## 3. Run `pbp setup` on the PBP host

**Docker.** In your checkout of this repository, as a user who can run Docker:

```
docker compose build && docker run --rm -it --user 0 --no-healthcheck -e TZ="$(timedatectl show -p Timezone --value)" -v "$PWD:/setup" pbp:latest setup --dir /setup
```

This builds the image and runs setup once, in a throwaway container with the checkout mounted at `/setup`:

- `--user 0` lets setup give the files it writes to uid 65532, the user the daemon runs as.
- `-it` is needed for the questions, and for reading the node's reply without echoing it.
- `-e TZ=...` passes in the host's time zone, which setup offers as the default for the schedule. On a host without systemd use `-e TZ="$(cat /etc/timezone)"`, or name the zone yourself (`-e TZ=Europe/London`).
- On a host with SELinux enforcing, add `:z` (lower case, a label shared between containers) to the volume: `-v "$PWD:/setup:z"`. The bind mounts in `docker-compose.yml` then need `:z` too (`./config.yaml:/etc/pbp/config.yaml:ro,z` and so on), or the daemon's container is denied the files. SELinux hosts are untested.
- Rootless Docker and userns-remap are untested. If setup stops there with a `chown` error, use the [manual setup](manual-setup.md).

Do not start PBP with `docker compose run` or `docker compose up` before setup has written `config.yaml`: Compose creates an empty directory in place of each missing bind-mounted file, and setup then stops until you remove it.

**`.deb` host.** After [installing the package](install-deb.md):

```
sudo pbp setup --deb
```

### The questions

Setup asks these only when there is no `config.yaml` yet. Enter accepts the default in brackets, and each answer is checked as you type it:

| Question | Default | Goes into |
|----------|---------|-----------|
| Node address (hostname or IP, as this host reaches its SSH and API) | none (`127.0.0.1` on the node itself) | `target.ssh.host`, and `target.api.url` as `https://<address>:8006` |
| VMID range for proof guests | `990000-990049` | `target.vmid_range` (step 1) |
| Isolated bridge on the node | `vmbr99` | `target.bridge` (step 2) |
| Restore storage on the node | `local-zfs` | `restore.storage`: active, and able to hold VM images and container volumes |
| PBS storage(s) to prove, comma-separated | none | `sources.storages`: the names of the PBS storages as the node knows them |
| Schedule (cron) | `0 2 * * *` | `schedule`: five cron fields (minute, hour, day of month, month, day of week); see [The schedule](operating.md#the-schedule) |
| Time zone for the schedule (Docker only; a `.deb` host uses its own zone) | the `TZ` passed in | `.env`, which `docker-compose.yml` reads; an `Area/City` name such as `Europe/Berlin` (see [The time zone](operating.md#the-time-zone)) |

Everything else comes from [`config.example.yaml`](../config.example.yaml), with its comments, so `config.yaml` reads like the example. Edit it afterwards for the rest (checks, notifications, timeouts) with `sudoedit config.yaml` (Docker) or `sudoedit /etc/pbp/config.yaml` (`.deb`): setup leaves it read-only. Once `config.yaml` exists, setup never asks again: it reads the file and uses its values.

### What it prints

Setup creates the SSH key pair (ed25519; an existing key is reused, never replaced), prints what to run on the node, and waits:

```
On the node, as root:

1. Install pbp 0.8.1 (signature and checksum verified):

(a=$(dpkg --print-architecture) && d=$(mktemp -d) && cd "$d" && curl -fsSLO "https://github.com/BreadthAnalytics/proxmox-backup-proofing/releases/download/v0.8.1/pbp_0.8.1_${a}.deb" && ...; rc=$?; rm -rf "$d"; exit $rc)

2. Set up the node:

pbp node setup --vmid-range 990000-990049 --bridge vmbr99 --pubkey 'ssh-ed25519 AAAA... pbp' --storages local-zfs,pbs-main --daemon-version 0.8.1 --create-token

Paste the reply printed by pbp node setup, then press Enter:
```

Line 1 downloads the `.deb` of exactly your daemon's version from the public release page into a temporary directory, checks it against `SHA256SUMS`, installs it and removes the directory. It runs in a subshell, so your shell stays where it was. A `dev` build prints a note instead: install a matching `dev` build on the node yourself.

### What it writes

| | Docker (in the checkout) | `.deb` host |
|---|---|---|
| config | `config.yaml` | `/etc/pbp/config.yaml` |
| SSH key | `secrets/pve_ssh_key` (and `.pub`) | `/etc/pbp/secrets/pve_ssh_key` (and `.pub`) |
| API token secret | `secrets/pve_token` | `/etc/pbp/secrets/pve_token` |
| node host key | `known_hosts` | `/etc/pbp/known_hosts` |
| time zone | `.env` (`TZ=<zone>`) | (the system's) |
| run logs | `logs/` | `/var/log/pbp` (the package's) |
| owner and mode | uid/gid 65532; secrets `0400` in a `0700` `secrets/`, other files `0444`, `logs/` `0750` | `root:pbp`, `0640`; `/etc/pbp/secrets` `0750` |

In the Docker checkout, `config.yaml`, `.env`, `secrets/`, `known_hosts` and `logs/` are in `.gitignore`. The token secret and `known_hosts` are written only once the node's reply is pasted ([step 5](#5-paste-the-reply-back)).

## 4. Run `pbp node setup` on the node

In a root shell on the node (the console, or SSH as root), run the two lines `pbp setup` printed: first the install line, then the `pbp node setup` line. `pbp node setup`:

1. **Checks**, changing nothing: PVE 9 or later; the daemon's version is compatible with this helper; the key is `ssh-ed25519`; `sudo` is installed (Proxmox VE does not include it; installing the `pbp` package brings it along as a recommended package, unless apt is set not to install recommends); the bridge exists and is isolated (by the same rule as preflight, so it can be rerun on a node in use); the VMID range holds no guest PBP does not own; sshd has `UsePAM yes` for the `pbp` user; and each named storage exists. A failure stops it with a sentence saying what to do.
2. **Shows a plan.** Every item from [Node setup](manual-setup.md#node-setup) steps 2 to 6 (the helper config, the `pbp` shell, the sudo rule, the notification templates, the forced-command key) and the API token (role, user, token and ACLs), each marked `already correct`, `create` or `differs`, with the difference shown. Two more items are checks that setup never fixes, so a `differs` there stops it even with `--replace`: sshd must read `pbp`'s keys only from `/var/lib/pbp/.ssh/authorized_keys` (no `authorized_keys2`, other `AuthorizedKeysFile`, `AuthorizedKeysCommand`, SSH user CA or `AuthorizedPrincipalsCommand`), and `/var/lib/pbp/.ssh` may hold only `authorized_keys` and setup's backups of it. Warnings name permissions `pbp@pve` has beyond what PBP needs.
3. **Asks** `Apply these changes? [y/N]`. Anything but `y` or `yes` changes nothing.
4. **Applies** the plan, writing each file to a temporary file and renaming it into place. The sudo rule is checked with `visudo` first. The key goes in last, so an interrupted run never leaves a working key without its restriction.
5. **Proves it**, the way the daemon will use it: as `pbp`, through the sudo rule, the helper must answer a version request and refuse a command for a VMID just above the range. It also re-reads every file.
6. **Prints the reply block**:

   ```
   Copy this block now; the token secret is not shown again.
   -----BEGIN PBP NODE REPLY-----
   eyJmb3JtYXQiOjEs...
   -----END PBP NODE REPLY-----
   ```

   It carries the node's name and addresses, the helper's version, the range, bridge and key it set up, the node's ed25519 SSH host key, the SHA-256 fingerprint of the API certificate, and, when the token was just created, the token secret. The secret appears only in this block, and only this once.

On a node where everything is already in place, `pbp node setup` changes nothing, prints `setup has already been completed` and the reply block again, without a token secret.

`pbp node setup` runs only as root, in an interactive terminal; it refuses to run from an SSH forced command (`SSH_ORIGINAL_COMMAND` set), through the `pbp` account's sudo rule, or with stdin not a terminal. Its options:

| Option | Effect |
|--------|--------|
| `--dry-run` | Show the checks and the plan, then stop. Nothing is changed. |
| `--yes` | Skip the `Apply these changes?` prompt. It never lifts the root and terminal requirement. |
| `--replace` | A file that exists but differs from what setup writes (a sudo rule or `authorized_keys` you edited, for example) stops setup, with the difference shown, and nothing is changed. With `--replace`, the old file is first copied to `<file>.pbp-backup-<YYYYMMDDTHHMMSS>` (for example `/etc/sudoers.d/pbp.pbp-backup-20261020T143005`), then replaced. |
| `--create-token` | Create the role, user and token. `pbp setup` adds it when the host has no token secret yet. If the token already exists, no new secret is made (PVE cannot show an existing one), and the reply carries none. The role, user and ACLs are brought to the desired state whenever `--create-token` or `--rotate-token` is set or `--storages` is given, which the line `pbp setup` prints always does. |
| `--rotate-token` | Delete the token and create it again with a new secret. Any daemon still using the old secret loses access until it gets the new one. PVE drops a removed token's ACLs, so the new token is granted only the paths this run sets: `/vms`, the node, the SDN zone and the storages in `--storages` (the line `pbp setup` prints lists every storage in `config.yaml`). `pbp setup` prints a line with this option when it needs it (see [Lost token secret](#lost-token-secret)). |

## 5. Paste the reply back

Copy the whole block, including the `BEGIN` and `END` lines, into the waiting `pbp setup` and press Enter. What you paste is not echoed.

Setup first checks the reply: its range, bridge and key must match this host's config. It also checks that the node address you gave really reaches that node, by connecting to it on the SSH port and comparing the host key it sees with the one in the reply (without logging in). If the address leads to another machine (through DNS or `/etc/hosts`), setup writes nothing but the token secret (shown only once), names the address, what it resolves to and the node's own addresses, and asks you to fix `target.ssh.host` and `target.api.url`. If nothing answers on the address, setup carries on and the check at the end shows the failure.

Otherwise it writes:

- `known_hosts`, from the host key in the reply. The key arrives over the root shell you already trust, which replaces `ssh-keyscan`'s trust on first use.
- the token secret file, when the reply carries a secret.
- `target.node` and `target.api.tls_fingerprint` in `config.yaml` (only those two values; the fingerprint is left alone when you use `ca_file`).

Then it runs the same checks as `pbp check` (except the janitor, which needs the daemon's state) and prints the result. In Docker that check runs inside the one-shot container, so the container must reach the node's API (8006) and SSH (22). When it passes, setup ends with `Setup is complete. Start PBP: docker compose up -d` (or `sudo systemctl start pbp` on a `.deb` host).

## 6. Start and check

**Docker:**

```
docker compose up -d
docker compose exec pbp pbp check
```

**`.deb` host:**

```
sudo systemctl start pbp
sudo -u pbp PBP_CONFIG=/etc/pbp/config.yaml pbp check
```

`pbp check` validates the config and your access and runs the full preflight, without restoring anything. Fix everything it reports ([Troubleshooting](troubleshooting.md)) before you rely on the schedule. To try one proof right away, `pbp run --guest <vmid>` ([Operating](operating.md)).

### Docker notes

- `docker compose up` uses the image built from your checkout, stamped with the version in the top-level `VERSION` file (`docker compose exec pbp pbp version` shows it). The helper on the node must share its major.minor, which is why `pbp setup` installs the `.deb` of the same release there.
- The image runs as the non-root uid/gid **65532** (distroless `nonroot`), which is why setup gives that uid the files it writes. Run logs are readable only by uid 65532 (directory `0750`, files `0600`), so on the host read them as root, for example `sudo less logs/*/run.log`.
- The schedule's time zone is the `TZ` in `.env`, which `docker-compose.yml` reads (`TZ: ${TZ:-Europe/London}`). Valid names, how to change it, and what daylight saving does to the schedule: [The time zone](operating.md#the-time-zone).
- The container's `stop_grace_period` is 25 minutes. On shutdown the daemon starts no further proofs and gives those in flight up to 10 minutes to finish; after that it cancels them, and their teardown and the run's notification can take about 11 minutes more. Do not shorten it, or a stop can kill PBP mid-teardown and leave a temporary guest behind.

## PBP on the Proxmox node itself

This suits a single-node lab or a trial; for production, run PBP on a separate host ([Where to run PBP](security.md#where-to-run-pbp)).

PBP can run on the node it proofs on: install the `.deb` there ([Install from the .deb](install-deb.md)), do steps 1 and 2, then run, in a root shell on the node:

```
sudo pbp setup --deb
```

It does both halves in that one shell:

1. The address question offers `127.0.0.1`, which always reaches the node, whatever its host name resolves to. Take it.
2. Instead of printing the two lines for the node, setup asks `Set up this node here? [y/N]`.
3. With yes, it runs `pbp node setup`'s checks, shows the same plan and asks the same `Apply these changes? [y/N]`, then applies it and hands the reply (token secret included) straight to its own second half: nothing is pasted and the secret is never shown.
4. If the API token already exists on the node and this host has no secret for it, setup asks `Replace the token?` (the same as `--rotate-token`).
5. It runs the check, and ends with `Setup is complete. Start PBP: sudo systemctl start pbp`.

A node file that differs from what setup writes stops it, as with `pbp node setup`; `sudo pbp setup --deb --replace` then backs it up and replaces it. The node-side rules still apply (root, an interactive terminal, not from an SSH forced command or the `pbp` sudo rule); where they do not hold, setup says so and falls back to printing the two lines. Answer no to keep the two-shell flow.


## If something goes wrong

**Interrupted, or something to fix.** Ctrl-C at any point keeps everything written so far. Run `pbp setup` again (the same command): it skips the questions, prints the node lines again and waits for the reply. When everything is in place and the check passes, it changes nothing and prints `setup has already been completed`. When the check fails (for example the node was reinstalled and has a new host key), it shows what failed and prints the node lines again; the new reply replaces `known_hosts`, `target.node` and the fingerprint, and setup lists each value it changed.

### Lost token secret

If the token already exists on the node but this host has no secret for it (a rebuilt PBP host, a deleted `secrets/` directory, or a node an earlier setup created the token on), PVE cannot show the secret again. Setup writes nothing, explains this, and prints the `pbp node setup` line again with `--rotate-token`. Run it on the node, then `pbp setup` again and paste the new reply. Any other daemon using that token loses access until it gets the new secret.

The same applies when this host has a secret but the node rejects it (HTTP 401: the token was replaced from elsewhere, or the file holds the wrong value): setup says so and prints the `--rotate-token` line.

**Other errors** are listed in [Troubleshooting](troubleshooting.md).

## Removing PBP from a node

`pbp node uninstall`, as root on the node, undoes `pbp node setup`. Like setup, it runs only as root in an interactive terminal, shows a plan and asks before changing anything. It removes, in this order, so that access goes first:

1. the sudo rule `/etc/sudoers.d/pbp` (then checks sudo with `visudo -c`),
2. `/var/lib/pbp/.ssh/authorized_keys`, and the `.ssh` directory if that leaves it empty,
3. `/etc/pbp/node.yaml`,
4. and sets the `pbp` shell back to `nologin`.

A file whose content is not what setup writes is backed up to `<file>.pbp-backup-<YYYYMMDDTHHMMSS>` before it is removed. The notification templates, `/etc/pbp` itself and the package are left alone. A node with nothing to remove prints `nothing to remove`.

| Option | Effect |
|--------|--------|
| `--remove-token` | Also remove the API token, its ACLs, the `pbp@pve` user and the `PBPProofer` role. Without it they are kept. |
| `--dry-run` | Show the plan and stop. |
| `--yes` | Skip the prompt. |

To remove the package itself afterwards, `apt remove pbp`. Do not `apt purge` a node you still use: purging deletes the `pbp` user and with it PBP's access.
