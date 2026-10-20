# Setup

Two guided commands do the setup: `pbp setup` on the host that runs PBP, and `pbp node setup` on the Proxmox node, which `pbp setup` prints for you. They hand each other what the other side needs (the SSH key, the node's host key and certificate fingerprint, the API token secret) by copy and paste, so nothing is typed twice and no secret ends up in a command line or your shell history.

Before you run them, pick a VMID range and create the isolated bridge (steps 1 and 2). Setup checks both, but creates neither.

If you would rather do everything by hand, or want to see exactly what the two commands change, see [Manual setup](manual-setup.md). The guided commands produce that same end state.

## 1. Reserve a VMID range

Pick a range of free VMIDs, for example `990000`-`990049`, which `pbp setup` asks for and writes to `target.vmid_range`. The range size is the most guests PBP can have in flight at once.

**Exclude the range from your backup jobs.** A backup job that covers "all" guests can catch a temporary proof guest mid-run, wasting backup space and time. In Datacenter > Backup, make each job exclude the PBP VMIDs, or select guests by pool (and never put PBP guests in that pool). PBP ignores backups whose VMID is in its range (it logs them once per run) and never proofs them.

If you run more than one PBP instance against the same cluster, give **each instance its own range, with no overlap**. The janitor destroys any `pbp`-tagged guest in its range that it does not know about (leftovers from a crash), so a shared or overlapping range lets one instance destroy another instance's guests mid-proof.

> **PBP owns every guest in the reserved range that carries the tag `pbp`, and it will destroy it.** Never tag your own guests `pbp`, and never put your own guests in the range. PBP never creates, changes, starts, stops or destroys anything outside the range.

> A guest tagged `pbp` **outside** the range is never touched either. Each run reports it in an "Environment errors" section of the notification (and logs a warning), for example `VM 105 (web) is tagged pbp but outside the PBP range 990000–990049; not touched`, and the run's severity is at least `warning`. Remove the tag or move the guest to resolve it.
>
> PBP refuses to run if it finds a guest in the range that does not carry the `pbp` tag. The one exception is the leftover of a PBP restore that failed before it could be tagged: PBP's state file records the VMID as an in-flight restore, the guest is stopped, and its name (hostname for a container) matches the backup's original config, or it is still locked by the restore. PBP removes such a leftover itself, at once or through the janitor on the next run. Any other untagged guest blocks every run until you remove it: run `docker compose exec pbp pbp check`, which names the offending VMID, confirm it is not yours, and remove it on the node with `qm destroy <vmid> --purge` (VM) or `pct destroy <vmid> --purge` (container).

## 2. Create the isolated bridge

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

## 3. Run `pbp setup` on the PBP host

**Docker** (the default). In a checkout of this repository, as a user who can run Docker, run:

```
docker compose build && docker run --rm -it --user 0 --no-healthcheck -e TZ="$(timedatectl show -p Timezone --value)" -v "$PWD:/setup" pbp:latest setup --dir /setup
```

This builds the image and runs setup once, in a throwaway container with the checkout mounted at `/setup`:

- `--user 0` lets setup give the files it writes to uid 65532, the user the daemon runs as.
- `-it` is needed for the questions, and for reading the node's reply without echoing it.
- `-e TZ=...` passes in the host's time zone, which setup offers as the default for the schedule. On a host without systemd use `-e TZ="$(cat /etc/timezone)"`, or name the zone yourself (`-e TZ=Europe/London`).
- On a host with SELinux enforcing, add `:z` (lower case, a label shared between containers) to the volume: `-v "$PWD:/setup:z"`. The bind mounts in `docker-compose.yml` then need `:z` too (`./config.yaml:/etc/pbp/config.yaml:ro,z` and so on), or the daemon's container is denied the files. SELinux hosts are untested.
- Rootless Docker and userns-remap are untested. If setup stops there with a `chown` error, use the [manual setup](manual-setup.md).

Do not start PBP with `docker compose run` or `docker compose up` before setup has written `config.yaml`. Compose creates an empty directory in place of each missing bind-mounted file, and setup then stops until you remove it.

**`.deb` host.** Install the package (see [Install from the .deb](install-deb.md)), then run:

```
sudo pbp setup --deb
```

**PBP on the Proxmox node itself.** When `sudo pbp setup --deb` runs on a Proxmox VE node, it does both halves in one shell. The address question offers `127.0.0.1`, which always reaches the node, whatever its host name resolves to. Then, instead of printing the two lines for the node, setup asks `Set up this node here? [y/N]`. With yes, it runs `pbp node setup`'s checks, shows the same plan and asks the same `Apply these changes? [y/N]`, applies it, and hands the reply (token secret included) straight to its own second half: nothing is pasted and the secret is never shown. If the API token already exists on the node and this host has no secret for it, setup asks whether to replace it (`--rotate-token`). A node file that differs from what setup writes stops it, as with `pbp node setup`; `sudo pbp setup --deb --replace` then backs it up and replaces it. The node-side rules still apply: root, an interactive terminal, not from an SSH forced command or the `pbp` sudo rule; where they do not hold, setup says so and falls back to the two lines. Answer no to keep the two-shell flow.

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

Everything else comes from [`config.example.yaml`](../config.example.yaml), with its comments, so `config.yaml` reads like the example. Edit it afterwards for the rest (checks, notifications, timeouts) with `sudoedit config.yaml`: setup leaves it read-only and owned by uid 65532. Once `config.yaml` exists, setup never asks again: it reads the file and uses its values.

Setup then creates the SSH key pair (ed25519; an existing key is reused, never replaced), prints what to run on the node, and waits:

```
On the node, as root:

1. Install pbp 0.3.0 (checksum verified):

(a=$(dpkg --print-architecture) && d=$(mktemp -d) && cd "$d" && curl -fsSLO "https://git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/releases/download/v0.3.0/pbp_0.3.0_${a}.deb" && ...; rc=$?; rm -rf "$d"; exit $rc)

2. Set up the node:

pbp node setup --vmid-range 990000-990049 --bridge vmbr99 --pubkey 'ssh-ed25519 AAAA... pbp' --storages local-zfs,pbs-main --daemon-version 0.3.0 --create-token

Paste the reply printed by pbp node setup, then press Enter:
```

Line 1 downloads the `.deb` of exactly your daemon's version from the public release page into a temporary directory, checks it against `SHA256SUMS`, installs it and removes the directory. It runs in a subshell, so your shell stays where it was. A `dev` build prints a note instead: install a matching `dev` build on the node yourself.

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

## 4. Run `pbp node setup` on the node

In a root shell on the node (the console, or SSH as root), run the two lines `pbp setup` printed: first the install line, then the `pbp node setup` line. `pbp node setup`:

1. **Checks**, changing nothing: PVE 9 or later; the daemon's version is compatible with this helper; the key is `ssh-ed25519`; `sudo` is installed (Proxmox VE does not install it: `apt install sudo`; the `.deb` recommends it); the bridge exists and is isolated (by the same rule as preflight, so it can be rerun on a node in use); the VMID range holds no guest PBP does not own; sshd has `UsePAM yes` for the `pbp` user (read with `sshd -T -C user=pbp,...`, so `Match` blocks count); and each named storage exists. A failure stops it with a sentence saying what to do. A missing bridge is reported with the stanza from step 2: setup never creates a bridge or reloads networking.
2. **Shows a plan.** Every item from [Node setup](manual-setup.md#node-setup) steps 2 to 6 (the helper config, the `pbp` shell, the sudo rule, the notification templates, the forced-command key) and the API token (role, user, token and ACLs), each marked `already correct`, `create` or `differs`, with the difference shown. Two more items are checks that setup never fixes, so a `differs` there stops it even with `--replace`: sshd must read `pbp`'s keys only from `/var/lib/pbp/.ssh/authorized_keys` (any other `AuthorizedKeysFile` entry, such as Debian's default `.ssh/authorized_keys2`, must not exist, no `AuthorizedKeysCommand` may apply to `pbp`, and no SSH user CA (`TrustedUserCAKeys`) or `AuthorizedPrincipalsCommand` either, since a certificate would log in without the forced command), and `/var/lib/pbp/.ssh` may hold only `authorized_keys` and setup's backups of it.
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
| `--rotate-token` | Delete the token and create it again with a new secret. Any daemon still using the old secret loses access until it gets the new one. PVE drops a removed token's ACLs, so the new token is granted only the paths this run sets: `/vms`, the node, the SDN zone and the storages in `--storages` (the line `pbp setup` prints lists every storage in `config.yaml`). `pbp setup` prints a line with this option when it needs it (see below). |

On a node where everything is already in place, `pbp node setup` changes nothing and prints:

```
setup has already been completed
```

followed by the reply block again, without a token secret.

## 5. Paste the reply back

Copy the whole block, including the `BEGIN` and `END` lines, into the waiting `pbp setup` and press Enter. What you paste is not echoed.

Setup refuses a reply whose range, bridge or key does not match this host's config, and then writes nothing.

It also checks that the node address you gave reaches that node: it connects to it on the SSH port and compares the host key it sees with the one in the reply, without logging in. If they differ, the address (through DNS or `/etc/hosts`) leads to another machine; setup then writes nothing but the token secret (shown only once), names the address, what it resolves to and the node's own addresses, and asks you to fix `target.ssh.host` and `target.api.url`. If nothing answers on the address, setup carries on and the check at the end shows the failure.

Otherwise it writes:

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

## 6. Start

```
docker compose up -d
docker compose exec pbp pbp check
```

On a `.deb` host: `sudo systemctl start pbp`, and `sudo -u pbp PBP_CONFIG=/etc/pbp/config.yaml pbp check`.

`pbp check` validates the config and your access and runs the full preflight, without restoring anything. Fix everything it reports before you rely on the schedule.

`docker compose up` uses the image built from this checkout, stamped with the version in the top-level `VERSION` file (for example `0.3.0`; `docker compose exec pbp pbp version` shows it). The helper on the node must share its major.minor, which is why `pbp setup` installs the `.deb` of the same release there. An image built with `--build-arg VERSION=dev` is a `dev` build, which the helper refuses unless it is a `dev` build too.

The image runs as the non-root uid/gid **65532** (distroless `nonroot`), which is why setup gives that uid the files it writes. `/var/lib/pbp` (with the state in `/var/lib/pbp/state`) and `/var/log/pbp` are created in the image owned by 65532, so the named volume `pbp-state` works as is; the bind-mounted `logs/` is created by setup with that owner. Run logs are created readable only by uid 65532 (directory `0750`, files `0600`), so on the host read them as root or as that uid, for example `sudo less logs/run-*.log`.

The time zone of the schedule is the `TZ` in `.env`, which `docker-compose.yml` reads (`TZ: ${TZ:-Europe/London}`). Change it there (`sudoedit .env`: setup leaves it read-only and owned by uid 65532), not in the compose file, and run `docker compose up -d` again. A `TZ` exported in the shell that runs `docker compose` takes precedence over `.env`, so unset it there (or make it match).

The container's `stop_grace_period` is 11 minutes because on shutdown the daemon waits up to 10 minutes for an in-flight guest to be torn down. Do not shorten it.

## What the API token can do

`pbp node setup` creates the role `PBPProofer`, the user `pbp@pve` and the privilege-separated token `pbp@pve!proof` (see [Create an API token](manual-setup.md#create-an-api-token) for the commands), and grants the role on the paths below. This list was confirmed against PVE 9.2 (the integration suite, see `test/integration/README.md`):

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

`pbp node setup` warns, without changing anything, when `pbp@pve` or one of its tokens has more than this: another role or another path in an ACL, membership in a group (whose permissions apply too), or a second API token. Grant extra only on purpose (see privileged containers below). An ACL entry for these paths that does not propagate covers only the path itself, so setup sets it again.

`/vms` cannot be narrowed to the VMID range in PVE's permission model, so the token could act on every guest. The reserved range is enforced by PBP itself; the token is the second line, not the first.

Guests PBP cannot proof with this token (each ends `ERROR`, never started; the backup is not blamed):

- **Privileged containers**: restoring one needs `Sys.Modify` on `/`.
- **Containers with raw `lxc.*` keys, `devN` passthrough or a hookscript**, and **VMs with non-mapped (raw) PCI or USB passthrough**: changing those keys is reserved to `root@pam`. VMs with *mapped* devices need `Mapping.Use` on the mapping as well.

Granting `Sys.Modify` on `/` lifts the first restriction but also lets the token change node-wide settings; weigh that before doing it. Proofing privileged containers also weakens what a stolen PBP SSH key can do: the helper runs `pct exec` in PBP's tagged containers, and in a privileged container that command runs as real root (uid 0 on the node), held back only by the container's namespaces and AppArmor profile instead of an unprivileged uid mapping. If that matters more to you than proofing those backups, leave privileged containers out (see [The ignore list](checks.md#the-ignore-list)).

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

To remove the helper itself afterwards, `apt remove pbp` (see the warning about `apt purge` in [Node setup](manual-setup.md#node-setup)).
