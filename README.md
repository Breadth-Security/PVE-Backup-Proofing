# PBP - Proxmox Backup Proofing

> "Trust but verify"
--Ronald Reagan

> ## *Proof - the act of proving, verifying, or substantiating that something is true, correct, or officially acceptable. It confirms that a claim, process, or product meets specified requirements, functions as intended, and operates within expected parameters.*
>
> In the context of backups, we all understand the fact that an untested backup <b>is not</b> a backup at all. As we gain experience, we put into place practices which speak to this reality.
>
>Proxmox Backup Server has made backups incredibly accessible, reliable, efficient, and stable. The verification of the backup snapshot written to disk is, however, not <b><i>proof</i></b> that we will have a working product after a restore.
>
>That's where PBP steps in. We are going to <i>trust</i> the backup existing verification, but we are going to <b><i>prove</i></b> that our backups will support our disaster recovery plans.

PBP (Proxmox Backup Proofing) is a small daemon that runs in a container. On a schedule it takes the newest backup of every guest from your Proxmox Backup Server storage, restores it into a temporary guest on an isolated network, boots it, checks that it is healthy inside, destroys the temporary guest, and writes the verdict into the backup's notes and into a notification.

## How it works

For each run, PBP:

1. **Preflight.** Checks the API token, SSH, the node helper (version, VMID range and bridge match your config, and it refuses what it should), the node version, that the proof bridge is really isolated and the restore storage, checks that the notification templates are installed, cleans up after any crashed run (the janitor), and then checks that the VMID range holds no guest PBP does not own.
2. **Discovery.** Picks the newest backup per guest across all configured storages. Guests whose last `PROVEN` verdict is oldest (or who were never proven) go first. A snapshot that already carries a `PBP PROVEN` line is skipped unless `restore.reproof: true`.
3. **Capacity gate.** Skips a guest if the backup does not fit in the restore storage (free space, minus space reserved by in-flight restores, minus `space_margin_percent`) or the node lacks free RAM.
4. **Protect, restore, sanitize.** Marks the backup `protected` so pruning cannot remove it mid-proof, restores it to the next free VMID in the reserved range (name `pbp-<vmid>-<timestamp>`, tag `pbp`), then puts every NIC on the isolated bridge, sets `onboot=0`, clears `protection` (a backup of a protected guest restores protected, and could then never be destroyed) and removes host passthrough. Anything that cannot be made safe is an `ERROR`; the guest is never started.
5. **Boot and check.** Starts the guest and waits up to `boot_timeout` (default 5m). Inside, within `restore.probe_timeout` (default 5m, each command bounded by `checks.command_timeout`, default 60s), it checks the system state, failed units (minus the ignore list), a read-write root filesystem, service profiles and your own per-guest checks. Linux VMs need the QEMU guest agent; containers use `pct exec`. Alpine and Gentoo containers (by their PVE `ostype`) are checked with OpenRC's `rc-status --crashed` instead of systemd, after waiting for OpenRC to finish starting its services. A VM without `systemctl` falls back to the same check.
6. **Teardown.** Stops and destroys the temporary guest with all its disks, restores the backup's original `protected` value, and annotates the backup's notes. This always runs, and the janitor repairs anything left behind by a crash.
7. **Report.** Writes a log, and sends one summary notification through your Proxmox node.

### Verdicts

| Verdict | Meaning | Says something about the backup? |
|---------|---------|-----|
| `PROVEN` | Restored, booted, all checks passed | Yes: good |
| `FAILED` | Restore failed, boot failed, or any check failed | Yes: bad |
| `BOOTED` | The guest reached running but PBP could not look inside (a VM without the guest agent, or an agent without exec rights) | Partially |
| `SKIPPED` | Not attempted (not enough space or RAM, already proven, run deadline reached) | No |
| `ERROR` | A PBP or node problem (SSH or API failure, node out of RAM, unsafe config) | No |

**Only `FAILED` indicts the backup.** `SKIPPED` and `ERROR` mean "not proven yet", and `BOOTED` means "starts, but not looked inside". Every `BOOTED` guest (no guest agent, or an agent that cannot run commands) is listed with its reason in each run summary, under "Guests PBP could not look inside (booted only)".

### Backup notes

PBP owns exactly one line in each backup's notes, placed first so it shows as the backup's comment in the Proxmox UI:

```
PBP <VERDICT> <YYYY-MM-DD HH:MM> · <short reason>
PBP PROVEN 2026-10-19 02:14 · agent ok, 3 profiles ok
PBP FAILED 2026-10-19 02:31 · failed: postgresql.service
```

Your own notes are preserved below it. A previous PBP line is replaced, never stacked. PBP only ever modifies the `notes` and `protected` attributes of a backup.

## Requirements

- Proxmox VE **9.0 or newer** and Proxmox Backup Server **4.0 or newer**. Preflight refuses older nodes.
- A PBS storage configured on the PVE node that will run the proofs (storage config is cluster-wide, so any cluster node's PBS storage works).
- A restore storage on that node with room for your largest guest, for VM images and container root filesystems.
- A dedicated, isolated bridge on that node (see below) and a reserved VMID range.
- Docker with Compose on any host that can reach the node's API (8006) and SSH (22).
- **SSH access to the PVE node as the unprivileged `pbp` user, restricted to one forced command.** PBP does not use root SSH. The key can run only the `pbp node` helper, which PBP's own binary provides on the node (see [Setup](#setup)). Some things have no API equivalent: `pct exec` (checking inside containers), the bridge's live state (IPv6, addresses, members), and arbitrary notifications (`PVE::Notify`; the notifications REST API can only send a fixed test message). The helper does these, and only these, and only for guests it is allowed to touch:
  - it runs `pct exec` only in a container whose VMID is inside the helper's `vmid_range` and which carries the `pbp` tag, and passes the command as an argument list, never through a shell,
  - it reports the one configured bridge's state, read-only,
  - it sends notifications of type `pbp` through `PVE::Notify`, and
  - it prints its own configuration and version.

  What a stolen key gets: those four things, within the range. It does not get a shell, any other command, a guest outside the range or untagged, forwarding, or root. The range, tag and bridge are read from `/etc/pbp/node.yaml` on the node, which the key cannot change. The node still trusts PBP's API token (which you scope with the privileges below), so protect the PBP host and its secrets as before.

## Setup

Two guided commands do the setup: `pbp setup` on the host that runs PBP, and `pbp node setup` on the Proxmox node, which `pbp setup` prints for you. They hand each other what the other side needs (the SSH key, the node's host key and certificate fingerprint, the API token secret) by copy and paste, so nothing is typed twice and no secret ends up in a command line or your shell history.

Before you run them, pick a VMID range and create the isolated bridge (steps 1 and 2). Setup checks both, but creates neither.

If you would rather do everything by hand, or want to see exactly what the two commands change, see [Manual setup](#manual-setup-what-pbp-setup-does). The guided commands produce that same end state.

### 1. Reserve a VMID range

Pick a range of free VMIDs, for example `990000`-`990049`, which `pbp setup` asks for and writes to `target.vmid_range`. The range size is the most guests PBP can have in flight at once.

**Exclude the range from your backup jobs.** A backup job that covers "all" guests can catch a temporary proof guest mid-run, wasting backup space and time. In Datacenter > Backup, make each job exclude the PBP VMIDs, or select guests by pool (and never put PBP guests in that pool). PBP ignores backups whose VMID is in its range (it logs them once per run) and never proofs them.

If you run more than one PBP instance against the same cluster, give **each instance its own range, with no overlap**. The janitor destroys any `pbp`-tagged guest in its range that it does not know about (leftovers from a crash), so a shared or overlapping range lets one instance destroy another instance's guests mid-proof.

> **PBP owns every guest in the reserved range that carries the tag `pbp`, and it will destroy it.** Never tag your own guests `pbp`, and never put your own guests in the range. PBP never creates, changes, starts, stops or destroys anything outside the range.

> A guest tagged `pbp` **outside** the range is never touched either. Each run reports it in an "Environment errors" section of the notification (and logs a warning), for example `VM 105 (web) is tagged pbp but outside the PBP range 990000–990049; not touched`, and the run's severity is at least `warning`. Remove the tag or move the guest to resolve it.
>
> PBP refuses to run if it finds a guest in the range that does not carry the `pbp` tag. The one exception is the leftover of a PBP restore that failed before it could be tagged: PBP's state file records the VMID as an in-flight restore, the guest is stopped, and its name (hostname for a container) matches the backup's original config, or it is still locked by the restore. PBP removes such a leftover itself, at once or through the janitor on the next run. Any other untagged guest blocks every run until you remove it: run `docker compose exec pbp pbp check`, which names the offending VMID, confirm it is not yours, and remove it on the node with `qm destroy <vmid> --purge` (VM) or `pct destroy <vmid> --purge` (container).

### 2. Create the isolated bridge

Restored guests are plugged into a bridge that has no physical ports, no bond members, no IP address and IPv6 disabled. They can talk to each other but not to your network or the node, so a restored copy of a server cannot take over its production twin's address or reach anything real. PBP verifies this at every preflight and refuses to run otherwise.

**Why IPv6 must be disabled on the bridge.** Even with no address configured, the kernel gives every interface with IPv6 enabled a link-local address (`fe80::...`). The PVE API does not show it, but a proof guest on the bridge could use it to reach the node's own SSH and web UI. Worse, a restored router or firewall guest that sends IPv6 router advertisements could give the node a SLAAC address and a route through the bridge. With `disable_ipv6` set, the node has no presence on the bridge at all.

At every preflight PBP reads the bridge's live state on the node over SSH and refuses to run unless `/proc/sys/net/ipv6/conf/<bridge>/disable_ipv6` is `1`, the bridge has no address of any kind, and its only members are interfaces of guests in the reserved VMID range (`tapN...`, `vethN...`, `fwprN...`, `fwlnN...`): no physical NIC, bond, VLAN, OVS internal port, or production guest.

Neither PBP nor `pbp setup` creates the bridge; you do. `pbp node setup` checks it before it changes anything and, if it is missing, prints the stanza below and stops. Give its name when `pbp setup` asks (`target.bridge`). PVE requires bridge names of the form vmbrN (here `vmbr99`).

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

Then apply with `ifreload -a`. Do not add `ovs_ports`, an address, or an OVS port/bond for a physical NIC. If you create the bridge in the GUI, add the `post-up` line to its stanza in `/etc/network/interfaces` afterwards.

To disable IPv6 on an existing bridge right away (the `post-up` line covers reboots and reloads), run `echo 1 > /proc/sys/net/ipv6/conf/vmbr99/disable_ipv6` on the node, then `docker compose exec pbp pbp check`.

### 3. Run `pbp setup` on the PBP host

**Docker** (the default). In a checkout of this repository, as a user who can run Docker, run:

```
docker compose build && docker run --rm -it --user 0 --no-healthcheck -e TZ="$(timedatectl show -p Timezone --value)" -v "$PWD:/setup" pbp:latest setup --dir /setup
```

This builds the image and runs setup once, in a throwaway container with the checkout mounted at `/setup`:

- `--user 0` lets setup give the files it writes to uid 65532, the user the daemon runs as.
- `-it` is needed for the questions, and for reading the node's reply without echoing it.
- `-e TZ=...` passes in the host's time zone, which setup offers as the default for the schedule. On a host without systemd use `-e TZ="$(cat /etc/timezone)"`, or name the zone yourself (`-e TZ=Europe/London`).
- On a host with SELinux enforcing, add `:z` (lower case, a label shared between containers) to the volume: `-v "$PWD:/setup:z"`. The bind mounts in `docker-compose.yml` then need `:z` too (`./config.yaml:/etc/pbp/config.yaml:ro,z` and so on), or the daemon's container is denied the files. SELinux hosts are untested.
- Rootless Docker and userns-remap are untested. If setup stops there with a `chown` error, use the [manual setup](#manual-setup-what-pbp-setup-does).

Do not start PBP with `docker compose run` or `docker compose up` before setup has written `config.yaml`. Compose creates an empty directory in place of each missing bind-mounted file, and setup then stops until you remove it.

**`.deb` host.** Install the package (see [Install from the .deb](#install-from-the-deb)), then run:

```
sudo pbp setup --deb
```

**The questions.** Setup asks these only when there is no `config.yaml` yet. Enter accepts the default in brackets, and each answer is checked as you type it:

| Question | Default | Goes into |
|----------|---------|-----------|
| Node address (hostname or IP, as this host reaches its SSH and API) | none | `target.ssh.host`, and `target.api.url` as `https://<address>:8006` |
| VMID range for proof guests | `990000-990049` | `target.vmid_range` (step 1) |
| Isolated bridge on the node | `vmbr99` | `target.bridge` (step 2) |
| Restore storage on the node | `local-zfs` | `restore.storage` |
| PBS storage(s) to prove, comma-separated | none | `sources.storages` |
| Schedule (cron) | `0 2 * * *` | `schedule` |
| Time zone for the schedule (Docker only) | the `TZ` passed in | `.env`, which `docker-compose.yml` reads |

Everything else comes from [`config.example.yaml`](config.example.yaml), with its comments, so `config.yaml` reads like the example. Edit it afterwards for the rest (checks, notifications, timeouts) with `sudoedit config.yaml`: setup leaves it read-only and owned by uid 65532. Once `config.yaml` exists, setup never asks again: it reads the file and uses its values.

Setup then creates the SSH key pair (ed25519; an existing key is reused, never replaced), prints what to run on the node, and waits:

```
On the node, as root:

1. Install pbp 0.3.0 (checksum verified):

a=$(dpkg --print-architecture) && d=$(mktemp -d) && cd "$d" && curl -fsSLO "https://git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/releases/download/v0.3.0/pbp_0.3.0_${a}.deb" && ...

2. Set up the node:

pbp node setup --vmid-range 990000-990049 --bridge vmbr99 --pubkey 'ssh-ed25519 AAAA... pbp' --storages local-zfs,pbs-main --daemon-version 0.3.0 --create-token

Paste the reply printed by pbp node setup, then press Enter:
```

Line 1 downloads the `.deb` of exactly your daemon's version from the public release page, checks it against `SHA256SUMS` and installs it. A `dev` build prints a note instead: install a matching `dev` build on the node yourself.

What setup writes, and who owns it:

| | Docker (in the checkout) | `.deb` host |
|---|---|---|
| config | `config.yaml` | `/etc/pbp/config.yaml` |
| SSH key | `secrets/pve_ssh_key` (and `.pub`) | `/etc/pbp/secrets/pve_ssh_key` (and `.pub`) |
| API token secret | `secrets/pve_token` | `/etc/pbp/secrets/pve_token` |
| node host key | `known_hosts` | `/etc/pbp/known_hosts` |
| time zone | `.env` (`TZ=<zone>`) | (the system's) |
| run logs | `logs/` | (the package's `/var/log/pbp`) |
| owner and mode | uid/gid 65532; secrets `0400` in a `0700` `secrets/`, other files `0444`, `logs/` `0750` | `root:pbp`, `0640`; `/etc/pbp/secrets` `0750` |

`config.yaml`, `.env`, `secrets/`, `known_hosts` and `logs/` are in `.gitignore`. The token secret and `known_hosts` are written only once the node's reply is pasted ([step 5](#5-paste-the-reply-back)).

### 4. Run `pbp node setup` on the node

In a root shell on the node (the console, or SSH as root), run the two lines `pbp setup` printed: first the install line, then the `pbp node setup` line. `pbp node setup`:

1. **Checks**, changing nothing: PVE 9 or later; the daemon's version is compatible with this helper; the key is `ssh-ed25519`; `sudo` is installed (Proxmox VE does not install it: `apt install sudo`; the `.deb` recommends it); the bridge exists and is isolated (by the same rule as preflight, so it can be rerun on a node in use); the VMID range holds no guest PBP does not own; sshd has `UsePAM yes` for the `pbp` user (read with `sshd -T -C user=pbp,...`, so `Match` blocks count); and each named storage exists. A failure stops it with a sentence saying what to do. A missing bridge is reported with the stanza from step 2: setup never creates a bridge or reloads networking.
2. **Shows a plan.** Every item from [Node setup](#node-setup) steps 2 to 6 (the helper config, the `pbp` shell, the sudo rule, the notification templates, the forced-command key) and the API token (role, user, token and ACLs), each marked `already correct`, `create` or `differs`, with the difference shown. Two more items are checks that setup never fixes, so a `differs` there stops it even with `--replace`: sshd must read `pbp`'s keys only from `/var/lib/pbp/.ssh/authorized_keys` (any other `AuthorizedKeysFile` entry, such as Debian's default `.ssh/authorized_keys2`, must not exist, and no `AuthorizedKeysCommand` may apply to `pbp`), and `/var/lib/pbp/.ssh` may hold only `authorized_keys` and setup's backups of it.
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

   It carries the node's name, the helper's version, the range, bridge and key it set up, the node's ed25519 SSH host key, the SHA-256 fingerprint of the API certificate, and, when the token was just created, the token secret. The secret appears only in this block, and only this once.

`pbp node setup` runs only as root, in an interactive terminal. It refuses to run from an SSH forced command (`SSH_ORIGINAL_COMMAND` set), through the `pbp` account's sudo rule, or with stdin not a terminal. It never touches root's `authorized_keys`, `/etc/ssh/sshd_config`, the network configuration or any guest.

Its options:

| Option | Effect |
|--------|--------|
| `--dry-run` | Show the checks and the plan, then stop. Nothing is changed. |
| `--yes` | Skip the `Apply these changes?` prompt. It never lifts the root and terminal requirement. |
| `--replace` | A file that exists but differs from what setup writes (a sudo rule or `authorized_keys` you edited, for example) stops setup, with the difference shown, and nothing is changed. With `--replace`, the old file is first copied to `<file>.pbp-backup-<YYYYMMDDTHHMMSS>` (for example `/etc/sudoers.d/pbp.pbp-backup-20261020T143005`), then replaced. |
| `--create-token` | Create the role, user and token. `pbp setup` adds it when the host has no token secret yet. If the token already exists, no new secret is made (PVE cannot show an existing one), and the reply carries none. The role, user and ACLs are brought to the desired state whenever `--create-token` or `--rotate-token` is set or `--storages` is given, which the line `pbp setup` prints always does. |
| `--rotate-token` | Delete the token and create it again with a new secret. Any daemon still using the old secret loses access until it gets the new one. `pbp setup` prints a line with this option when it needs it (see below). |

On a node where everything is already in place, `pbp node setup` changes nothing and prints:

```
setup has already been completed
```

followed by the reply block again, without a token secret.

### 5. Paste the reply back

Copy the whole block, including the `BEGIN` and `END` lines, into the waiting `pbp setup` and press Enter. What you paste is not echoed.

Setup refuses a reply whose range, bridge or key does not match this host's config, and then writes nothing. Otherwise it writes:

- `known_hosts`, from the host key in the reply. The key arrives over the root shell you already trust, which replaces `ssh-keyscan`'s trust on first use.
- the token secret file, when the reply carries a secret.
- `target.node` and `target.api.tls_fingerprint` in `config.yaml` (only those two values; the fingerprint is left alone when you use `ca_file`).

Then it runs the same checks as `pbp check`, except that it does not clean up after crashed runs (the janitor, which needs the daemon's state), and prints the result. In Docker that check runs inside the one-shot container, so the container must reach the node's API (8006) and SSH (22). When it passes, setup ends with:

```
Setup is complete. Start PBP: docker compose up -d
```

(`sudo systemctl start pbp` on a `.deb` host.)

**Interrupted, or something to fix?** Ctrl-C at any point keeps everything written so far. Run `pbp setup` again (the same command): it skips the questions, prints the node lines again and waits for the reply. When everything is in place and the check passes, it changes nothing and prints `setup has already been completed`. When the check fails (for example the node was reinstalled and has a new host key), it shows what failed and prints the node lines again; the new reply replaces `known_hosts`, `target.node` and the fingerprint, and setup lists each value it changed.

**Lost token secret.** If the token already exists on the node but this host has no secret for it (a rebuilt PBP host, a deleted `secrets/` directory), PVE cannot show the secret again. Setup writes nothing, explains this, and prints the `pbp node setup` line again with `--rotate-token`. Run it on the node, then `pbp setup` again and paste the new reply. Any other daemon using that token loses access until it gets the new secret.

### 6. Start

```
docker compose up -d
docker compose exec pbp pbp check
```

On a `.deb` host: `sudo systemctl start pbp`, and `sudo -u pbp PBP_CONFIG=/etc/pbp/config.yaml pbp check`.

`pbp check` validates the config and your access and runs the full preflight, without restoring anything. Fix everything it reports before you rely on the schedule.

`docker compose up` uses the image built from this checkout, stamped with the version in the top-level `VERSION` file (for example `0.3.0`; `docker compose exec pbp pbp version` shows it). The helper on the node must share its major.minor, which is why `pbp setup` installs the `.deb` of the same release there. An image built with `--build-arg VERSION=dev` is a `dev` build, which the helper refuses unless it is a `dev` build too.

The image runs as the non-root uid/gid **65532** (distroless `nonroot`), which is why setup gives that uid the files it writes. `/var/lib/pbp` and `/var/log/pbp` are created in the image owned by 65532, so the named volume `pbp-state` works as is; the bind-mounted `logs/` is created by setup with that owner. Run logs are created readable only by uid 65532 (directory `0750`, files `0600`), so on the host read them as root or as that uid, for example `sudo less logs/run-*.log`.

The time zone of the schedule is the `TZ` in `.env`, which `docker-compose.yml` reads (`TZ: ${TZ:-Europe/London}`). Change it there (`sudoedit .env`: setup leaves it read-only and owned by uid 65532), not in the compose file, and run `docker compose up -d` again.

The container's `stop_grace_period` is 11 minutes because on shutdown the daemon waits up to 10 minutes for an in-flight guest to be torn down. Do not shorten it.

### What the API token can do

`pbp node setup` creates the role `PBPProofer`, the user `pbp@pve` and the privilege-separated token `pbp@pve!proof` (see [Create an API token](#create-an-api-token) for the commands), and grants the role on the paths below. This list was confirmed against PVE 9.2 (the integration suite, see `test/integration/README.md`):

| Privilege | Path | Needed for |
|-----------|------|------------|
| `VM.Allocate` | `/vms` | restore into a new VMID, destroy with purge |
| `VM.Backup` | `/vms` | read the config inside a backup (`vzdump/extractconfig`), restore from a backup, and write `notes`/`protected` on backup volumes |
| `VM.Audit` | `/vms` | list guests (PBP must see every guest in the VMID range), read configs and status |
| `VM.Config.Network`, `VM.Config.Options`, `VM.Config.HWType` | `/vms` | sanitize: NICs, tags, name, `onboot`, `protection`, hookscript, removing `rng`/USB/PCI devices |
| `VM.PowerMgmt` | `/vms` | start and stop the temporary guest |
| `VM.GuestAgent.Audit`, `VM.GuestAgent.Unrestricted` | `/vms` | agent ping and OS info; running the checks through the agent |
| `Datastore.AllocateSpace`, `Datastore.Audit` | the restore storage and each PBS storage | allocate restored disks; list backups and read and write their notes and `protected` flag |
| `Sys.Audit` | `/nodes/<node>` | node version, memory and the bridge check |
| `SDN.Use` | `/sdn/zones/localnetwork` | see below |

`Datastore.Allocate` is **not** needed and should not be granted: on a PBS storage it allows deleting any backup.

`SDN.Use` is needed on the bridges the **original** guests use, not only on the isolated bridge: PVE checks the backed-up NIC configuration (for example `bridge=vmbr0,tag=2`) when it restores, before PBP can move the NICs to the isolated bridge. Granting it on `/sdn/zones/localnetwork` covers every local bridge and VLAN. The restored guest is never started until every NIC is on the isolated bridge.

`/vms` cannot be narrowed to the VMID range in PVE's permission model, so the token could act on every guest. The reserved range is enforced by PBP itself; the token is the second line, not the first.

Guests PBP cannot proof with this token (each ends `ERROR`, never started; the backup is not blamed):

- **Privileged containers**: restoring one needs `Sys.Modify` on `/`.
- **Containers with raw `lxc.*` keys, `devN` passthrough or a hookscript**, and **VMs with non-mapped (raw) PCI or USB passthrough**: changing those keys is reserved to `root@pam`. VMs with *mapped* devices need `Mapping.Use` on the mapping as well.

Granting `Sys.Modify` on `/` lifts the first restriction but also lets the token change node-wide settings; weigh that before doing it. Proofing privileged containers also weakens what a stolen PBP SSH key can do: the helper runs `pct exec` in PBP's tagged containers, and in a privileged container that command runs as real root (uid 0 on the node), held back only by the container's namespaces and AppArmor profile instead of an unprivileged uid mapping. If that matters more to you than proofing those backups, leave privileged containers out (see [The ignore list](#the-ignore-list)).

### Removing PBP from a node

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

To remove the helper itself afterwards, `apt remove pbp` (see the warning about `apt purge` in [Node setup](#node-setup)).

## Manual setup (what `pbp setup` does)

These are the steps `pbp setup` and `pbp node setup` carry out, for admins who prefer to do it by hand and as the reference for what the two commands must produce (a test keeps this section and the code in step). Do [Setup](#setup) steps 1 and 2 first; these steps replace Setup steps 3 to 6.

### Create an API token

On the node, create a user, a role and a **privilege-separated** token, for example user `pbp@pve` and token `proof`, giving the token id `pbp@pve!proof`. A privilege-separated token only gets the permissions that **both** the user and the token have, so grant the ACLs to both:

```
pveum user add pbp@pve
pveum role add PBPProofer --privs "VM.Allocate,VM.Audit,VM.Backup,VM.Config.HWType,VM.Config.Network,VM.Config.Options,VM.PowerMgmt,VM.GuestAgent.Audit,VM.GuestAgent.Unrestricted,Datastore.AllocateSpace,Datastore.Audit,Sys.Audit,SDN.Use"
pveum user token add pbp@pve proof --privsep 1        # prints the secret once
for p in /vms /storage/<restore storage> /storage/<each PBS storage> /nodes/<node> /sdn/zones/localnetwork; do
  pveum acl modify $p --users pbp@pve --tokens 'pbp@pve!proof' --roles PBPProofer
done
```

Put the token secret in `secrets/pve_token` (see [Configure and start](#configure-and-start)). `pbp node setup --create-token` does this step and prints the secret in its reply block.

### Create the SSH key

Create a dedicated key pair and capture the node's host key. PBP refuses a host that does not match `known_hosts`. The public key is installed on the node in the next step, for the `pbp` user, with a forced command (not in `root`'s `authorized_keys`):

```
ssh-keygen -t ed25519 -f secrets/pve_ssh_key -N '' -C pbp
ssh-keyscan -t ed25519 pve1.example.lan > known_hosts
```

### Node setup

Done once, as root on the PVE node. Step 1 is the install line `pbp setup` prints; steps 2 to 6 are what `pbp node setup` does. The daemon never does it, because the node is where the restriction lives. Replace `<PBP public key>` with the content of `secrets/pve_ssh_key.pub` (one line: `ssh-ed25519 AAAA... pbp`).

1. **Install the helper.** The helper is `pbp node`, the same binary as the daemon. Install the `.deb` (the `pbp` service stays inactive without `/etc/pbp/config.yaml`; see "Install from the .deb" for downloading it), or copy `/usr/bin/pbp` and create the `pbp` user yourself:

   ```
   apt install ./pbp_<version>_<arch>.deb
   ```

   The `.deb` creates the `pbp` user with home `/var/lib/pbp` (mode `0700`, owned by `pbp`) and shell `nologin`.

   > **Warning:** `apt purge pbp` on the node deletes the `pbp` user, and with it PBP's access. `apt remove` keeps it. A daemon and its helper must share the same major.minor version (`0.3.x` talks to `0.3.y`), so upgrade the package on the node whenever you upgrade the daemon.

2. **Write `/etc/pbp/node.yaml`.** This is the helper's own copy of the range, tag scope and bridge. It must match `target.vmid_range` and `target.bridge` in the daemon's `config.yaml` (`pbp check` fails if it does not), and `vmid_range[1]` must be below 999999999:

   ```
   install -d -m 0750 -o root -g pbp /etc/pbp
   cat > /etc/pbp/node.yaml <<'EOF'
   vmid_range: [990000, 990049]
   bridge: vmbr99
   request_timeout: 15m
   EOF
   chown root:pbp /etc/pbp/node.yaml && chmod 0640 /etc/pbp/node.yaml
   ```

   `request_timeout` (default 15m, minimum 30s) caps how long the helper lets one request run. Keep it at or above `restore.probe_timeout`, or `pbp check` warns that long probe commands are cut short. A missing or invalid `node.yaml` makes the helper refuse everything.

3. **Give `pbp` a shell**, so sshd can run the forced command. The home directory stays `0700` and owned by `pbp`. Only nodes get this; daemon-only hosts keep `nologin`:

   ```
   usermod -s /bin/sh pbp
   test "$(stat -c '%a %U' /var/lib/pbp)" = "700 pbp"
   ```

4. **Authorize the key, forced to the helper**, in `/var/lib/pbp/.ssh/authorized_keys`, as one line:

   ```
   install -d -m 0755 -o root -g root /var/lib/pbp/.ssh
   printf 'restrict,command="sudo /usr/bin/pbp node" %s\n' '<PBP public key>' > /var/lib/pbp/.ssh/authorized_keys
   chown root:root /var/lib/pbp/.ssh/authorized_keys && chmod 0644 /var/lib/pbp/.ssh/authorized_keys
   ```

   `restrict` turns off port, agent and X11 forwarding, the pty and `~/.ssh/rc`, and whatever a later OpenSSH adds to that list. The directory and the file belong to root, so the `pbp` user cannot rewrite its own forced command; sshd accepts a key file owned by root. Keep nothing else in `.ssh`, and make sure sshd reads no other key source for `pbp`: `sshd -T -C user=pbp,host=localhost,addr=127.0.0.1 | grep -i authorizedkeys` lists them, and Debian's default also reads `.ssh/authorized_keys2`, which must not exist.

5. **Add the one sudo rule**, in `/etc/sudoers.d/pbp` (Proxmox VE does not install `sudo`: `apt install sudo` first). Check it with `visudo -cf` before it is installed; a broken sudoers file locks you out of sudo:

   ```
   f=$(mktemp)
   cat > "$f" <<'EOF'
   Defaults:pbp env_reset
   Defaults:pbp env_keep = "SSH_ORIGINAL_COMMAND SSH_CONNECTION"
   pbp ALL=(root) NOPASSWD: /usr/bin/pbp node
   EOF
   visudo -cf "$f" \
       && install -m 0440 -o root -g root "$f" /etc/sudoers.d/pbp \
       && visudo -c
   rm -f "$f"
   ```

   `env_reset` with exactly that `env_keep` list is part of the design: the helper takes the request from `SSH_ORIGINAL_COMMAND` and nothing else from the environment. Do not widen it.

6. **Install the notification templates**, once, as root on the node:

   ```
   pbp node install-templates
   ```

   It writes `pbp-subject.txt.hbs` and `pbp-body.txt.hbs` into `/etc/pve/notification-templates/default/` and prints the paths. It runs only interactively as root: over SSH, and through the sudo rule, it is refused. Run it again after upgrading PBP if `pbp check` says the templates are outdated.

7. **Point PBP at it.** In `config.yaml` use `target.ssh.user: pbp` (the default), with the key and `known_hosts` from [Create the SSH key](#create-the-ssh-key). `root` is rejected. Then run `pbp check`:

   ```
   node helper version: ok (0.3.0)
   node helper config: ok (range [990000, 990049], bridge vmbr99)
   node helper request_timeout: ok (15m0s)
   node helper refusal: ok (exec in 990050 refused)
   ```

   Each failure ends with `see README "Node setup"` and names what differs: an unreachable helper, an incompatible version, a differing `vmid_range` or bridge, a `request_timeout` below the probe timeout (a warning), or a helper that did not refuse an out-of-range exec.

The `UsePAM yes` that Debian ships in `sshd_config` is what lets this locked system account log in by key; if you have turned `UsePAM` off, see Troubleshooting.

**Optional: cap concurrent logins.** To bound how many sessions a stolen key can hold open at once, add a `maxlogins` limit for `pbp`:

```
echo 'pbp hard maxlogins 8' > /etc/security/limits.d/pbp.conf
```

`pam_limits` applies it, so it needs `UsePAM yes` (Debian's default) and the `session required pam_limits.so` line Debian ships in `/etc/pam.d/sshd`. On a node with systemd-logind the count includes the session of `pbp`'s own systemd user manager, so `8` allows 7 concurrent requests. Keep it well above `workers` plus two (a `pbp check` or `pbp run` next to the daemon). A login over the limit is cut off by sshd with `There were too many logins for 'pbp'` and exit code 254 before the helper runs, so PBP cannot tell it from a guest command that failed: a cap that is too low shows up as wrongly failed checks.

### Configure and start

Create these next to `docker-compose.yml` before the first `docker compose up`:

- [ ] `config.yaml`, copied from `config.example.yaml` and edited
- [ ] `known_hosts`, from `ssh-keyscan` ([Create the SSH key](#create-the-ssh-key))
- [ ] the node set up ([Node setup](#node-setup))
- [ ] `secrets/pve_token`, containing the API token secret
- [ ] `secrets/pve_ssh_key`, the private key
- [ ] `logs/`, owned by uid 65532 (`mkdir -p logs && chown 65532:65532 logs`)
- [ ] optionally `.env` with `TZ=<your zone>` (the default is `Europe/London`)

The secret files and `config.yaml` must be readable by uid 65532 (for example `chown 65532:65532 secrets/* known_hosts config.yaml && chmod 400 secrets/*`, or `chmod 444` for the non-secret files). A bind mount hides the image's ownership: for `./logs:/var/log/pbp` run `mkdir -p logs && chown 65532:65532 logs` on the host first.

Then start PBP and check it as in [Setup step 6](#6-start).

## Install from the .deb

As an alternative to Docker, each release ships Debian packages (`amd64` and `arm64`) and tarballs. The package installs `/usr/bin/pbp`, a systemd unit (`pbp.service`) and the docs under `/usr/share/doc/pbp/`, and creates the `pbp` system user. The same `.deb` is also the node helper: `pbp setup` prints the line that installs it on the PVE node, where the `pbp` service stays inactive without a config.

1. Download the `.deb` and `SHA256SUMS` from the release and verify:

   ```
   sha256sum -c SHA256SUMS --ignore-missing
   sudo apt install ./pbp_<version>_<arch>.deb
   ```

2. Run the guided setup ([Setup](#setup) steps 3 to 5) with the `.deb` layout:

   ```
   sudo pbp setup --deb
   ```

   It writes `/etc/pbp/config.yaml`, `/etc/pbp/known_hosts` and `/etc/pbp/secrets/` (`root:pbp`, `0640`, the directory `0750`), with `/etc/pbp/...` paths in the config. The package creates `/etc/pbp` (`0750 root:pbp`) but never creates or overwrites `config.yaml`.

   **By hand** instead (after the [manual setup](#manual-setup-what-pbp-setup-does) steps for the token, key and node):

   ```
   sudo cp /usr/share/doc/pbp/config.example.yaml /etc/pbp/config.yaml   # then edit
   sudo install -d -m 0750 -o root -g pbp /etc/pbp/secrets
   sudo install -m 0640 -o root -g pbp pve_token /etc/pbp/secrets/pve_token
   sudo install -m 0640 -o root -g pbp pve_ssh_key /etc/pbp/secrets/pve_ssh_key
   ssh-keyscan -t ed25519 pve1.example.lan | sudo tee /etc/pbp/known_hosts
   sudo chown root:pbp /etc/pbp/config.yaml /etc/pbp/known_hosts
   sudo chmod 0640 /etc/pbp/config.yaml /etc/pbp/known_hosts
   ```

   In `config.yaml` set `token_secret_file` and `key_file` to `/etc/pbp/secrets/...` and `known_hosts_file: /etc/pbp/known_hosts`. The service can read `/etc/pbp` but not write to it.

3. Check, then start:

   ```
   sudo -u pbp PBP_CONFIG=/etc/pbp/config.yaml pbp check
   sudo systemctl start pbp
   ```

   `pbp check` takes the run lock, so it exits 3 ("a run is already in progress") while the daemon is mid-run; try again when the run has finished. The unit is enabled on install but only starts once `/etc/pbp/config.yaml` exists.

Logs: `journalctl -u pbp -f`, and the per-run files in `/var/log/pbp` (read them as root or `pbp`). State is in `/var/lib/pbp`.

Releases tagged `v1.2.0` produce version `1.2.0`; a pre-release tag such as `v1.2.0-rc1` produces `1.2.0~rc1`, which sorts before `1.2.0`. File names carry that Debian version (`pbp_1.2.0~rc1_amd64.deb`).

**Upgrade** by installing a newer `.deb` with `apt install ./pbp_<new>.deb`; the config and state are untouched and the running service is restarted. **On a PVE node** the same package is the helper (see [Node setup](#node-setup)). Its postinst creates the `pbp` user with the `nologin` shell and never touches sshd, sudo or `authorized_keys`; the node-only steps (`usermod -s /bin/sh pbp`, the forced-command key, the sudoers rule, `node.yaml`) are done by `pbp node setup` or by hand, never by the package, so daemon-only hosts never get a login shell. Installing a newer `.deb` on the node upgrades the helper and keeps that setup, but the daemon and the helper must share major.minor, so upgrade both.

**Remove** (`apt remove pbp`) stops and disables the service but keeps `/etc/pbp`, `/var/lib/pbp` and `/var/log/pbp` and the `pbp` user. **Purge** (`apt purge pbp`) deletes all three directories and the user. On a node that deletes the `pbp` user and with it PBP's access (and `/var/lib/pbp/.ssh`), so do not purge a node you still use for proofs.

PBP can run on the PVE host itself. It still connects over SSH as the restricted `pbp` user, but it then shares a host with the guests it restores. Prefer a separate host.

## Operating

```
docker compose up -d                              # start the daemon (scheduled runs)
docker compose exec pbp pbp check                 # preflight only; nothing is restored
docker compose exec pbp pbp run                   # one run now, all guests
docker compose exec pbp pbp run --guest 105       # one run now, only guest 105's newest backup
docker compose exec pbp pbp health                # what the Docker healthcheck runs
docker compose exec pbp pbp version               # build version and Go runtime
docker compose logs -f pbp                        # logs; per-run files are in ./logs
```

| Exit code | Meaning |
|-----------|---------|
| 0 | Success (`check` passed, or the run finished without error) |
| 1 | Failure: bad config, `check` found a problem, the run ended in `ERROR`, or `run --guest` found no backup |
| 2 | Usage error (unknown subcommand or flag, or `--guest` that is not a positive integer) |
| 3 | Busy: another run or check holds the run lock. `run` and `check` both exit 3 and print "a run is already in progress" |

Subcommands take `--config PATH`. The config path otherwise defaults to `$PBP_CONFIG`, then `/etc/pbp/config.yaml`. The state directory is `/var/lib/pbp`, overridable with `PBP_STATE_DIR`. With no config mounted, the container exits non-zero and names `/etc/pbp/config.yaml`.

### Health check

The image has a Docker `HEALTHCHECK` running `pbp health`. The container reports **unhealthy** when the last run ended in a run-level error (for example the node or SSH is unreachable, so no notification could be sent), when a scheduled run did not start, or when the current run has overrun. This is the backstop for a silent night when the node itself cannot notify you. Watch it with your usual container monitoring.

## Notifications

One summary per run is sent from the PVE node (through the `pbp node` helper over SSH, using `PVE::Notify`), so it goes to whatever notification targets and matchers you already use. Severity is `info` when everything is `PROVEN` (or skipped as already proven), `warning` for any `BOOTED`, `ERROR` or other `SKIPPED`, and `error` for any `FAILED` or a run-level failure.

To route it, create a matcher in Datacenter > Notifications > Notification Matchers with a **Match field** rule: field `type`, value `pbp`. A `hostname` field is also available.

You install the templates (`pbp-subject.txt.hbs`, `pbp-body.txt.hbs`) into `/etc/pve/notification-templates/default/` on the node with `pbp node setup`, or by hand with `pbp node install-templates` (Node setup, step 6); preflight only checks them and says to run that command if they are missing or differ from the ones in your PBP version, so run it again after upgrading PBP. PVE 9 picks up this new template type from there (verified on PVE 9.2), and `/etc/pve` survives package upgrades, so a PVE upgrade does not remove them.

If a target cannot be notified (for example a template fails to render), `PVE::Notify` only logs an `ERROR:` line; PBP treats that as a failed notification, so the run's outcome is `error` and the health check reports it. `PVE::Notify` cannot say which targets succeeded, so this is deliberately conservative: a failure of any one target marks the run's notification as failed, even if others delivered.

`notify.when` is `always` by default, so a missing nightly message is itself a signal; set it to `problems` for quiet success.

## The ignore list

A guest is `FAILED` if any systemd unit has failed, **unless** the unit is on the ignore list. Proof guests have no uplink: the bridge is isolated on purpose. Units that only make sense with a network therefore fail in a perfectly good restore. Ignored failures are still written to the log, as "ignored"; they are never silently dropped.

`checks.ignore_units` defaults to the list below. Omit the key to get it; set the key (even to `[]`) to use exactly your own list.

| Unit | Why it fails with no uplink |
|------|-----------------------------|
| `systemd-networkd-wait-online.service`, `NetworkManager-wait-online.service` | Wait for a network that never comes |
| `chronyd.service`, `chrony.service`, `systemd-timesyncd.service`, `ntp.service`, `ntpd.service`, `ntpsec.service` | No time source is reachable |
| `remote-fs.target`, `nfs-client.target`, `rpc-statd.service` | Network mounts and NFS servers are unreachable |
| `cloud-init.service`, `cloud-init-network.service`, `cloud-final.service`, `cloud-config.service` | The metadata source is unreachable |

For Windows VMs, automatic-start services that are not running count as failures, minus `checks.windows_ignore_services`. Because services can still be starting when the guest agent first answers, PBP re-checks the list every 15 seconds until none is left or `restore.probe_timeout` runs out; only the services still stopped at that point make the guest `FAILED`. The default: `sppsvc` (needs activation servers), `gupdate` and `edgeupdate` (updaters), `wuauserv` and `BITS` (Windows Update and background transfer), `W32Time` (no time source), `CDPSvc` (needs a network), and `MapsBroker`, `RemoteRegistry` and `tiledatamodelsvc` (delayed-start, disabled or self-stopping by design).

## Profiles and guest checks

PBP reads the units enabled inside each restored guest, which reflects the original host. For every enabled unit that matches a **profile**, the profile's checks run. A failing check is `FAILED`. Check kinds are `services` (must be active), `ports` (a TCP port is listening; any listed port suffices) and `commands` (must exit 0). Every command PBP runs inside a guest is bounded by `checks.command_timeout` (default 60s): a check whose command times out fails (`check timed out: <check>`). The one exception is the initial wait for boot to settle (`systemctl is-system-running --wait`). It may use most of `restore.probe_timeout`, because on the isolated proof network DHCP and wait-online services can keep a healthy guest starting for a minute or two. If the guest is still `starting` when that time runs out, the result is `ERROR` (`guest still starting after …`), not `FAILED`. All checks of one guest are bounded by `restore.probe_timeout` (default 5m): if that runs out, the guest is `ERROR` (`probe timed out`), not `FAILED`.

Built-in profiles: postgresql (`pg_isready`), mariadb and mysql (ping), nginx, apache2 and httpd (80 or 443 listening), docker (`docker info`), redis (`redis-cli ping`), sshd/ssh (22 listening), and cron.

Add your own under `checks.profiles`, keyed by a unit glob; a profile with the same name as a built-in replaces it. Add per-guest checks under `checks.guests`, matched by `vmid`, name or PVE tag, in addition to profiles:

```yaml
checks:
  profiles:
    - name: myapp
      unit: "myapp*.service"
      ports: [8080]
      commands: ["curl -sf http://127.0.0.1:8080/health"]
  guests:
    - match: { vmid: 105 }
      services: [postgresql]
```

## What "not covered by backup" means

A restore can legitimately lack things the original guest used: disks marked `backup=0`, container mount points excluded from backup, bind mounts and passthrough devices that PBP removes for safety, and network storage (NFS, CIFS, iSCSI) that the isolated bridge cannot reach.

When a failed unit can be traced to one of these (for example a `.mount` unit for an excluded disk, or a service that requires such a mount), PBP does **not** count it toward `FAILED`. It writes it to the log explicitly, for example `postgresql.service depends on /srv/pgdata (disk scsi1, backup=0): not backed up, skipped`, and lists it in the run summary and notification under **not covered by backup**. That is a feature: it tells you when a backup is missing something a service needs, which no boot test of the backup alone can show. Tracing is conservative; if PBP cannot connect a failure to a missing piece, it is a real failure.

## Troubleshooting

`pbp check` prints one line per step and ends every node helper failure with `see README "Node setup"`.

| Symptom | Cause and fix |
|---------|---------------|
| `target.ssh.user: root is no longer supported` | PBP refuses root. Set up the helper (Node setup) and use `user: pbp`. |
| `the pbp node helper is not reachable`, or `Permission denied (publickey)` | Run `pbp node setup` on the node again (the line `pbp setup` prints): it shows what differs. Otherwise: the key is not in `/var/lib/pbp/.ssh/authorized_keys` of the `pbp` user (Node setup step 4), or the file or `.ssh` directory has the wrong owner or mode (both owned by root, `.ssh` `0755`, the file `0644`). The log of sshd on the node says which. With the optional `maxlogins` cap, `There were too many logins for 'pbp'` means the cap is too low. |
| Login works but nothing runs, or `This account is currently not available` | The `pbp` user still has the `nologin` shell. Run `usermod -s /bin/sh pbp` (Node setup step 3). Also check that `UsePAM yes` is set in `sshd_config` (Debian's default); with `UsePAM no`, sshd refuses key logins for a locked account such as this one. |
| `sudo: a password is required`, or `sudo: no tty present` | The sudoers rule is missing or does not match `/usr/bin/pbp node` exactly (Node setup step 5). Check `/etc/sudoers.d/pbp` with `visudo -cf`, and that `/usr/bin/pbp` is the binary installed by the package. |
| `pbp-node: refused: bad-request` / `no request` | The helper ran without a valid request. Something other than PBP used the key, or the `env_keep` of `SSH_ORIGINAL_COMMAND` is missing from the sudoers rule. |
| `pbp-node: refused: out-of-range`, `untagged` or `not-container` | The guest is outside the helper's `vmid_range`, has no `pbp` tag, or is not an LXC container. By design. If the range itself is wrong, fix `node.yaml` and `target.vmid_range` together. |
| `vmid_range differs` or `bridge differs` | `/etc/pbp/node.yaml` and `config.yaml` disagree; the message gives both values. Make them equal. Nothing is restored until they match. |
| `helper 0.2.1 is not compatible with daemon 0.3.0` | The daemon and the helper must share major.minor. Upgrade the `.deb` on the node (or the daemon) to match. A `dev` build is compatible only with a `dev` build. |
| `helper 0.3.0 is not compatible with daemon dev` (or the reverse) | One side is a `dev` build: a binary from a plain `go build`, or an image built with `--build-arg VERSION=dev`. A `dev` build is compatible only with a `dev` build. Rebuild the image without the build argument (it then takes its version from the `VERSION` file) or with `--build-arg VERSION=<release>`, and install the matching `.deb` on the node. |
| `notification templates on the node: ... run "pbp node install-templates" as root on the node` | Install or refresh the templates (Node setup step 6). |
| `pbp-node: timeout` on long probe commands | `request_timeout` in `node.yaml` is below what the command needs; keep it at or above `restore.probe_timeout`. |
| Access stopped working after a node cleanup | `apt purge pbp` on the node deletes the `pbp` user and its `authorized_keys`. Reinstall and redo Node setup steps 3 and 4. |

## Upgrading from 0.2 to 0.3

Version 0.3.0 adds the guided setup (`pbp setup`, `pbp node setup`, `pbp node uninstall`). Nothing about a working 0.2 setup changes, but the daemon and the node helper must share major.minor, so upgrade both to 0.3.x together:

1. If you changed `TZ` in `docker-compose.yml`, put `TZ=<your zone>` in a `.env` file next to it and run `git checkout docker-compose.yml` before pulling. 0.3's compose file reads the zone from `.env`, and `pbp setup` writes `.env` only when it creates `config.yaml`, so without this the schedule falls back to `Europe/London` (and `git pull` refuses to overwrite your edited file).
2. Upgrade the daemon: `git pull && docker compose build && docker compose up -d` (or install the 0.3 `.deb` on a `.deb` host).
3. In the checkout, run the Docker one-shot from [Setup step 3](#3-run-pbp-setup-on-the-pbp-host) (or `sudo pbp setup --deb`). It finds your `config.yaml`, key and token, asks nothing, and, because the 0.2 helper on the node is not compatible with a 0.3 daemon, prints the node lines. It uses only the file names it writes itself (Docker: `secrets/pve_ssh_key`, `secrets/pve_token` and `known_hosts` in the checkout, named `/run/secrets/pve_ssh_key`, `/run/secrets/pve_token` and `/etc/pbp/known_hosts` in `config.yaml`; `.deb`: `/etc/pbp/secrets/pve_ssh_key`, `/etc/pbp/secrets/pve_token` and `/etc/pbp/known_hosts`). If your `config.yaml` names other files, it stops before changing anything and names each one: move the files to those paths, update `config.yaml` to match (`sudoedit /etc/pbp/config.yaml` on a `.deb` host), and run it again.
4. On the node, as root, run both lines: the install line upgrades the helper to 0.3, and `pbp node setup` checks the node. On a node set up by hand as the 0.2 README described, it prints `setup has already been completed` and the reply block, and changes nothing. If it shows a plan instead, the node is not quite what the 0.2 steps produce (for example a token named other than `pbp@pve!proof`, or a sudo rule you edited): read the plan before you answer, and see `--replace` in [Setup step 4](#4-run-pbp-node-setup-on-the-node).
5. Paste the reply into `pbp setup`. It refreshes `known_hosts`, `target.node` and the fingerprint from the node (and says which values it changed), then runs the check.

Until both sides are on 0.3, `pbp check` and every run fail with `helper 0.2.x is not compatible with daemon 0.3.y`.

## Upgrading from 0.1 to 0.2

Version 0.2.0 is a breaking change: PBP no longer uses root SSH. A 0.2 daemon refuses `ssh.user: root` and refuses to run until the node has the helper. To migrate:

1. Do [Node setup](#node-setup) on the node: install the 0.2 `.deb`, write `/etc/pbp/node.yaml` with the same range and bridge as your `config.yaml`, give `pbp` a shell, install the forced-command key and the sudoers rule, and run `pbp node install-templates` (PBP no longer installs the templates itself).
2. Switch `target.ssh.user` to `pbp` (or remove the line; `pbp` is the default). The existing key pair can be reused: put its public key in the `pbp` user's `authorized_keys` line from Node setup step 4.
3. Upgrade the daemon to 0.2 and run `pbp check`. Fix what it reports.
4. Remove the PBP public key from `/root/.ssh/authorized_keys` on the node. Until you do, the old key still gives root access.

If you set up a node for a 0.2.0 release candidate, redo Node setup steps 4 and 5: the `authorized_keys` line now starts with `restrict`, the `.ssh` directory and the file are owned by root (so `pbp` cannot change its own forced command), and the sudoers file is written through `mktemp`. Running step 4 again fixes the owner and mode of an existing `.ssh`.

## Configuration

See [`config.example.yaml`](config.example.yaml), which documents every key. Secrets (the API token and SSH key) are always referenced by file path, never inline. TLS to the API is verified with either `tls_fingerprint` or `ca_file`; skipping verification is not supported.

## Building

```
docker build -t pbp:latest .            # multi-stage; static binary on distroless, version from VERSION
docker build --build-arg VERSION=$(git describe --tags --always) -t pbp:latest .   # stamp another version
docker run --rm pbp:latest version      # prints the stamped version (VERSION, e.g. 0.3.0, unless overridden)
go test ./... && go vet ./...
packaging/build.sh 1.2.3 dist           # .debs, tarballs and SHA256SUMS for amd64 and arm64
```

The top-level `VERSION` file (`MAJOR.MINOR.PATCH`) is the version a Docker build uses when no `--build-arg VERSION` is given. Bump it before tagging a release: the `release` workflow fails a tag `v<version>` whose `MAJOR.MINOR.PATCH` differs from it (`v0.3.0-rc1` needs `0.3.0`). A plain `go build` without `-ldflags "-X main.version=..."` is a `dev` build.

`packaging/build.sh` needs Go, `envsubst` (gettext-base) and network access for the pinned nfpm. The `release` workflow runs it when a tag `v<version>` is pushed, runs lintian on the packages (errors fail the job) and creates the Gitea release with the packages, tarballs and `SHA256SUMS` attached. Run it by hand (workflow dispatch) for a dry run that builds but publishes nothing. It authenticates with the job's automatic token; if your Gitea refuses that (HTTP 401/403), create a token with the `write:repository` scope and store it as the repository secret `RELEASE_TOKEN`, which is used in preference when set.

## Code scanning

Workflows in [`.gitea/workflows/`](.gitea/workflows/) check every pull request and every push to `main`, and each **blocks** the pull request when it fails:

- **test**: `go vet` (with and without the `integration` tag) and `go test -race`. It first checks that it runs on exactly the Go `toolchain` pinned in `go.mod`, and that the Dockerfile builds with the same version.
- **Scanners**: Trivy, govulncheck, gosec, staticcheck and golangci-lint check the code and dependencies. gitleaks checks the **whole git history** for committed secrets; Trivy only sees the current files. The scanners also run weekly (Monday 06:17 UTC), so newly published vulnerabilities are caught in code that hasn't changed.

The Docker build runs `go vet` and `go test -race` too, so an image can't be built from code that fails them. When you bump Go, change the `toolchain` line in `go.mod` and the `FROM golang:` tag in the `Dockerfile` together; the test workflow fails if they differ. To block merging in Gitea, mark these workflows as required status checks in the branch protection for `main`.

Tool versions are pinned in the workflow files. To run them locally from the repository root:

```
# govulncheck
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

# staticcheck
go run honnef.co/go/tools/cmd/staticcheck@2026.2.2 ./...
go run honnef.co/go/tools/cmd/staticcheck@2026.2.2 -tags integration ./...

# gosec
# gosec v2.29.0 needs a Go 1.26 toolchain to analyse the code
GOTOOLCHAIN=go1.26.9 go run github.com/securego/gosec/v2/cmd/gosec@v2.29.0 ./...

# golangci-lint (config: .golangci.yml)
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run --build-tags integration ./...

# Trivy (vulnerabilities, secrets, misconfigurations)
docker run --rm -v "$PWD":/src:ro -w /src aquasec/trivy:0.76.0 fs --exit-code 1 --ignore-unfixed \
  --scanners vuln,secret,misconfig --severity UNKNOWN,LOW,MEDIUM,HIGH,CRITICAL \
  --skip-dirs .superpowers --ignorefile .trivyignore.yaml .

# gitleaks (full git history)
docker run --rm -v "$PWD":/repo:ro ghcr.io/gitleaks/gitleaks:v8.30.1 git --redact /repo

# vet and race tests
go vet ./... && go vet -tags integration ./... && go test -race ./...
```

An accepted gosec finding carries a `// #nosec <rule> -- <reason>` comment at the call site. An accepted gitleaks finding (a fake secret in a test) is allowlisted by value in `.gitleaks.toml`, and an accepted Trivy finding in `.trivyignore.yaml`.

*Note that this repo is synced from a local Gitea repo, upon commits. This work is being done by traditionally trained software developers using modern assisted coding practice (AI specification-driven generation, through SpecKit and similar tooling)*

## License

PBP is licensed under the [Apache License, Version 2.0](LICENSE).
The Go modules it depends on (golang.org/x/crypto, gopkg.in/yaml.v3,
github.com/robfig/cron) and the container base image it ships on
(distroless) are distributed under their own licenses.
