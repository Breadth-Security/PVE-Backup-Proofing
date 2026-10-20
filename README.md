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

## Requirements

- Proxmox VE **9.0 or newer** and Proxmox Backup Server **4.0 or newer**. Preflight refuses older nodes.
- A PBS storage configured on the PVE node that will run the proofs (storage config is cluster-wide, so any cluster node's PBS storage works).
- A restore storage on that node with room for your largest guest, for VM images and container root filesystems.
- A dedicated, isolated bridge on that node (see below) and a reserved VMID range.
- Docker with Compose on any host that can reach the node's API (8006) and SSH (22).
- **SSH access to the PVE node as the unprivileged `pbp` user, restricted to one forced command.** PBP does not use root SSH. The key can run only the `pbp node` helper, which PBP's own binary provides on the node (see [Setup](docs/setup.md)). Some things have no API equivalent: `pct exec` (checking inside containers), the bridge's live state (IPv6, addresses, members), and arbitrary notifications (`PVE::Notify`; the notifications REST API can only send a fixed test message). The helper does these, and only these, and only for guests it is allowed to touch:
  - it runs `pct exec` only in a container whose VMID is inside the helper's `vmid_range` and which carries the `pbp` tag, and passes the command as an argument list, never through a shell,
  - it reports the one configured bridge's state, read-only,
  - it sends notifications of type `pbp` through `PVE::Notify`, and
  - it prints its own configuration and version.

  What a stolen key gets: those four things, within the range. It does not get a shell, any other command, a guest outside the range or untagged, forwarding, or root. The range, tag and bridge are read from `/etc/pbp/node.yaml` on the node, which the key cannot change. The node still trusts PBP's API token (which you scope with the privileges below), so protect the PBP host and its secrets as before.

## Getting started

1. Pick a free VMID range for proof guests and create an isolated bridge on the node ([Setup](docs/setup.md) steps 1 and 2). PBP checks both but creates neither.
2. On the PBP host, run `pbp setup` (Docker: the one-shot command in [Setup step 3](docs/setup.md#3-run-pbp-setup-on-the-pbp-host); a `.deb` host: `sudo pbp setup --deb`). It asks a few questions, writes `config.yaml` and the SSH key, and prints two lines to run on the node.
3. On the node, as root, run those two lines: the first installs the matching `pbp` package, the second (`pbp node setup`) sets up the restricted helper and the API token, and prints a reply block.
4. Paste the reply back into `pbp setup`. It writes the node's host key, certificate fingerprint and token secret, and checks everything.
5. Start PBP: `docker compose up -d` (or `sudo systemctl start pbp`).

## Documentation

| Document | What it covers |
|----------|----------------|
| [Setup](docs/setup.md) | The guided setup step by step, what the API token can do, removing PBP from a node |
| [Manual setup](docs/manual-setup.md) | What `pbp setup` and `pbp node setup` do, as commands you can run by hand |
| [Install from the .deb](docs/install-deb.md) | Running PBP from the Debian package instead of Docker |
| [Operating](docs/operating.md) | Commands, exit codes, the health check, notifications, backup notes |
| [Checks inside the guest](docs/checks.md) | The ignore list, service profiles and per-guest checks, "not covered by backup" |
| [Troubleshooting](docs/troubleshooting.md) | What `pbp check` failures mean and how to fix them |
| [Upgrading](docs/upgrading.md) | Upgrading the daemon and the node helper together |
| [Development](docs/development.md) | Building, releases and the code scanners |
| [Changelog](CHANGELOG.md) | What changed in each release |

## Configuration

See [`config.example.yaml`](config.example.yaml), which documents every key. Secrets (the API token and SSH key) are always referenced by file path, never inline. TLS to the API is verified with either `tls_fingerprint` or `ca_file`; skipping verification is not supported.

*Note that this repo is synced from a local Gitea repo, upon commits. This work is being done by traditionally trained software developers using modern assisted coding practice (AI specification-driven generation, through SpecKit and similar tooling)*

## License

PBP is licensed under the [Apache License, Version 2.0](LICENSE).
The Go modules it depends on (golang.org/x/crypto, gopkg.in/yaml.v3,
github.com/robfig/cron) and the container base image it ships on
(distroless) are distributed under their own licenses.
