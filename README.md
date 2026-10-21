# PBP - Proxmox Backup Proofing

> "Trust, but verify"
--Ronald Reagan

> ## *Proof - the act of proving, verifying, or substantiating that something is true, correct, or officially acceptable. It confirms that a claim, process, or product meets specified requirements, functions as intended, and operates within expected parameters.*
>
> In the context of backups, we all understand the fact that an untested backup <b>is not</b> a backup at all. As we gain experience, we put into place practices which speak to this reality.
>
>Proxmox Backup Server has made backups incredibly accessible, reliable, efficient, and stable. The verification of the backup snapshot written to disk is, however, not <b><i>proof</i></b> that we will have a working product after a restore.
>
>That's where PBP steps in. We are going to <i>trust</i> the backup existing verification, but we are going to <b><i>prove</i></b> that our backups will support our disaster recovery plans.

PBP (Proxmox Backup Proofing) is a small daemon that runs in a container or directly on a Debian host. On a schedule it takes the newest backup of every guest from your Proxmox Backup Server storage, restores it into a temporary guest on an isolated network, boots it, checks that it is healthy inside, destroys the temporary guest, and writes the verdict into the backup's notes and into a notification.

## How it works

For each run, PBP:

1. **Preflight.** Checks the API token and SSH, the node helper (its version, and that its VMID range and bridge match your config), the node's PVE version, that the proof bridge is really isolated, the restore storage and the notification templates. It cleans up after any crashed run (the janitor), and checks that the VMID range holds no guest PBP does not own.
2. **Discovery.** Picks the newest backup of each guest across all configured PBS storages, leaving out guests you [exclude](docs/operating.md#excluding-guests). Guests whose last `PROVEN` verdict is oldest (or who were never proven) go first. A backup that already carries a `PBP PROVEN` line is skipped unless `restore.reproof: true`.
3. **Templates.** A backup of a VM or container template (`template: 1` in the config stored in the backup) is skipped: a template cannot be started, so it cannot be proven. Nothing is restored and the backup is not touched.
4. **Capacity gate.** Skips a guest if the backup does not fit in the restore storage (free space, minus space reserved by restores in progress, minus `space_margin_percent` of the restore storage's total size) or the node lacks free RAM.
5. **Protect, restore, sanitize.** Marks the backup `protected` so pruning cannot remove it mid-proof, and restores it to the next free VMID in the reserved range, tagged `pbp` (a VM is also named `pbp-<original VMID>-<backup time, UTC, YYYYMMDDHHMM>`; a container keeps its hostname). It then puts every NIC on the isolated bridge (a container's DHCP and IPv6 autoconfiguration become `manual`, since nothing answers them there), sets `onboot=0`, clears `protection` (a backup of a protected guest restores protected, and could then never be destroyed) and removes host passthrough. Anything that cannot be made safe is an `ERROR`; the guest is never started.
6. **Boot and check.** Starts the guest and waits up to `restore.boot_timeout` (default 5m). Inside, within `restore.probe_timeout` (default 5m; each command at most `checks.command_timeout`, default 60s), it checks the system state, failed units (minus the [ignore list](docs/checks.md#the-ignore-list)), a read-write root filesystem, service profiles and your own per-guest checks. Linux VMs need the QEMU guest agent; containers are checked with `pct exec`. Alpine and Gentoo containers are checked with OpenRC instead of systemd (a service of the runlevel that is stopped or crashed fails the guest), as is a VM without `systemctl`; they get the read-only-root and per-guest checks too, but not the built-in service profiles, which are written for systemd.
7. **Teardown.** Stops and destroys the temporary guest with all its disks, restores the backup's original `protected` value, and writes the verdict into the backup's notes. This always runs, and the janitor repairs anything a crash left behind.
8. **Report.** Writes a log, and sends one summary notification through your Proxmox node.

### Verdicts

| Verdict | Meaning | Says something about the backup? |
|---------|---------|-----|
| `PROVEN` | Restored, booted, all checks passed | Yes: good |
| `FAILED` | Restore failed, boot failed, or any check failed | Yes: bad |
| `BOOTED` | The guest reached running but PBP could not look inside (a VM without the guest agent, or an agent without exec rights), or a container without `nesting=1` whose systemd cannot start its sandboxed units ([details](docs/checks.md#containers-without-nesting)) | Partially |
| `SKIPPED` | Not attempted (excluded by config, a template, not enough space or RAM, already proven, run deadline reached) | No |
| `ERROR` | A PBP or node problem (SSH or API failure, node out of RAM, a guest that cannot be made safe) | No |

**Only `FAILED` indicts the backup.** `SKIPPED` and `ERROR` mean "not proven yet", and `BOOTED` means "starts, but not looked inside". Every `BOOTED` guest is listed with its reason in each run summary, under "BOOTED: started, not looked inside".

## Requirements

- **Proxmox VE 9.0 or newer** and **Proxmox Backup Server 4.0 or newer**.
- **On the node that runs the proofs:**
  - a PBS storage holding the backups (storage config is cluster-wide, so a PBS storage of any cluster node works);
  - a restore storage with room for your largest guest, for VM disks and container volumes;
  - an isolated bridge for the proof guests, which you create once ([Setup step 2](docs/setup.md#2-create-the-isolated-bridge));
  - a free range of VMIDs, which you only choose ([Setup step 1](docs/setup.md#1-choose-a-vmid-range));
  - `sudo` (Proxmox VE does not include it; the `pbp` package installs it as a recommended package, or run `apt install sudo` if apt is set not to install recommends);
  - a root shell, once, to run `pbp node setup`.
- **A host for PBP** that reaches the node's API (port 8006) and SSH (port 22): any host with Docker and Compose, or a Debian-based host for the `.deb` package (amd64 or arm64). **For production, run PBP on a separate host, outside the Proxmox node or cluster it proofs** ([why](docs/security.md#where-to-run-pbp)). Running it on the node itself works and is supported for a single-node lab or a trial.

PBP never logs in to the node as root. It uses an API token with a narrow role, and SSH only as an unprivileged `pbp` user that is locked to one restricted helper command. What that means, and what a stolen key could and could not do: [Security model](docs/security.md).

## Getting started

1. **Get PBP** on the PBP host: clone this repository (Docker), or install the `.deb` from the [releases page](https://git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/releases) ([Install from the .deb](docs/install-deb.md)).
2. **Choose a free VMID range and create the isolated bridge** on the node ([Setup](docs/setup.md) steps 1 and 2). PBP checks both, but you choose the range and create the bridge.
3. **Run `pbp setup`** on the PBP host: the Docker one-shot command in [Setup step 3](docs/setup.md#3-run-pbp-setup-on-the-pbp-host), or `sudo pbp setup --deb`. It asks a few questions, writes `config.yaml` and the SSH key, and prints two lines to run on the node.
4. **On the node, as root, run those two lines.** The first installs the matching `pbp` package; the second, `pbp node setup`, sets up the restricted helper and the API token, and prints a reply block.
5. **Paste the reply** back into `pbp setup`. It writes the node's host key, certificate fingerprint and token secret, and checks everything.
6. **Start PBP**: `docker compose up -d`, or `sudo systemctl start pbp`.

Running PBP on the Proxmox node itself? `sudo pbp setup --deb` there does steps 3 to 5 in one shell, with nothing to paste: see [PBP on the Proxmox node itself](docs/setup.md#pbp-on-the-proxmox-node-itself).

## Documentation

| Document | What it covers |
|----------|----------------|
| [Setup (Docker or .deb)](docs/setup.md) | The guided setup step by step, PBP on the node itself, removing PBP from a node |
| [Install from the .deb](docs/install-deb.md) | Downloading and installing the Debian package, upgrading and removing it |
| [Manual setup](docs/manual-setup.md) | What `pbp setup` and `pbp node setup` do, as commands you can run by hand |
| [Operating](docs/operating.md) | Everyday commands, a run on demand, exit codes, the health check, notifications, backup notes |
| [Checks inside the guest](docs/checks.md) | The ignore list, service profiles and per-guest checks, "not covered by backup" |
| [Proofing on a dedicated node](docs/dedicated-node.md) | Restoring and booting the proof copies on a separate Proxmox host instead of a production node, and the PBS credentials it needs |
| [Security model](docs/security.md) | What PBP may touch, the isolated bridge, the restricted node helper, the API token's privileges, where to run PBP |
| [Troubleshooting](docs/troubleshooting.md) | What setup and `pbp check` errors mean and how to fix them |
| [Upgrading](docs/upgrading.md) | Upgrading the daemon and the node helper together |
| [Development](docs/development.md) | Building, tests, releases and the code scanners |
| [Changelog](CHANGELOG.md) | What changed in each release |

## Configuration

`pbp setup` writes `config.yaml` for you; [`config.example.yaml`](config.example.yaml) documents every key, for anything you change afterwards. Secrets (the API token and SSH key) are always referenced by file path, never inline. TLS to the API is verified with either `tls_fingerprint` or `ca_file`; skipping verification is not supported.

*Note that this repo is synced from a local Gitea repo, upon commits. This work is being done by traditionally trained software developers using modern assisted coding practice (AI specification-driven generation, through SpecKit and similar tooling)*

## License

PBP is licensed under the [Apache License, Version 2.0](LICENSE).
The Go modules it depends on (golang.org/x/crypto, gopkg.in/yaml.v3,
github.com/robfig/cron) and the container base image it ships on
(distroless) are distributed under their own licenses.
