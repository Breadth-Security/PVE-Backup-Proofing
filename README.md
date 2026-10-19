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

1. **Preflight.** Checks the API token, SSH, the node version, that the proof bridge is really isolated, the restore storage, that the VMID range holds no guest PBP does not own, and installs the notification templates.
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
- **Root-equivalent SSH access to the PVE node.** This is stated plainly: PBP holds root-equivalent access to one PVE node. It is needed because:
  - `pct exec` (checking inside containers) has no API equivalent,
  - the bridge's live state (IPv6, addresses, members) is not in the API, and
  - arbitrary notifications can only be sent with `PVE::Notify` on the node itself; the notifications REST API can only send a fixed test message.

  Use a dedicated key, pin the host key in `known_hosts`, and treat the PBP host and its secrets accordingly. A restricted wrapper is a possible later improvement, not part of v1.

## Setup

### 1. Reserve a VMID range

Pick a range of free VMIDs, for example `990000`-`990049`, set as `target.vmid_range`. The range size is the most guests PBP can have in flight at once.

**Exclude the range from your backup jobs.** A backup job that covers "all" guests can catch a temporary proof guest mid-run, wasting backup space and time. In Datacenter > Backup, make each job exclude the PBP VMIDs, or select guests by pool (and never put PBP guests in that pool). PBP ignores backups whose VMID is in its range (it logs them once per run) and never proofs them.

If you run more than one PBP instance against the same cluster, give **each instance its own range, with no overlap**. The janitor destroys any `pbp`-tagged guest in its range that it does not know about (leftovers from a crash), so a shared or overlapping range lets one instance destroy another instance's guests mid-proof.

> **PBP owns every guest in the reserved range that carries the tag `pbp`, and it will destroy it.** Never tag your own guests `pbp`, and never put your own guests in the range. PBP never creates, changes, starts, stops or destroys anything outside the range.

> A guest tagged `pbp` **outside** the range is never touched either. Each run reports it in an "Environment errors" section of the notification (and logs a warning), for example `VM 105 (web) is tagged pbp but outside the PBP range 990000–990049; not touched`, and the run's severity is at least `warning`. Remove the tag or move the guest to resolve it.
>
> PBP refuses to run if it finds a guest in the range that does not carry the `pbp` tag, unless its state file records that VMID as the target of a restore PBP started. A restore that fails part way can leave a partial, untagged guest behind; PBP removes that one itself (at once, or through the janitor on the next run), as long as it is stopped. Any other untagged guest blocks every run until you remove it: run `docker compose exec pbp pbp check`, which names the offending VMID, confirm it is not yours, and remove it on the node with `qm destroy <vmid> --purge` (VM) or `pct destroy <vmid> --purge` (container).

### 2. Create the isolated bridge

Restored guests are plugged into a bridge that has no physical ports, no bond members, no IP address and IPv6 disabled. They can talk to each other but not to your network or the node, so a restored copy of a server cannot take over its production twin's address or reach anything real. PBP verifies this at every preflight and refuses to run otherwise.

**Why IPv6 must be disabled on the bridge.** Even with no address configured, the kernel gives every interface with IPv6 enabled a link-local address (`fe80::...`). The PVE API does not show it, but a proof guest on the bridge could use it to reach the node's own SSH and web UI. Worse, a restored router or firewall guest that sends IPv6 router advertisements could give the node a SLAAC address and a route through the bridge. With `disable_ipv6` set, the node has no presence on the bridge at all.

At every preflight PBP reads the bridge's live state on the node over SSH and refuses to run unless `/proc/sys/net/ipv6/conf/<bridge>/disable_ipv6` is `1`, the bridge has no address of any kind, and its only members are interfaces of guests in the reserved VMID range (`tapN...`, `vethN...`, `fwprN...`, `fwlnN...`): no physical NIC, bond, VLAN, OVS internal port, or production guest.

PBP does not create the bridge; you do. Name it in `target.bridge`. PVE requires bridge names of the form vmbrN (here `vmbr99`).

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

### 3. Create an API token

On the node, create a user, a role and a **privilege-separated** token, for example user `pbp@pve` and token `proof`, giving the token id `pbp@pve!proof`. A privilege-separated token only gets the permissions that **both** the user and the token have, so grant the ACLs to both:

```
pveum user add pbp@pve
pveum role add PBPProofer --privs "VM.Allocate,VM.Audit,VM.Backup,VM.Config.HWType,VM.Config.Network,VM.Config.Options,VM.PowerMgmt,VM.GuestAgent.Audit,VM.GuestAgent.Unrestricted,Datastore.AllocateSpace,Datastore.Audit,Sys.Audit,SDN.Use"
pveum user token add pbp@pve proof --privsep 1        # prints the secret once
for p in /vms /storage/<restore storage> /storage/<each PBS storage> /nodes/<node> /sdn/zones/localnetwork; do
  pveum acl modify $p --users pbp@pve --tokens 'pbp@pve!proof' --roles PBPProofer
done
```

This list was confirmed against PVE 9.2 (the integration suite, see `test/integration/README.md`):

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

Granting `Sys.Modify` on `/` lifts the first restriction but also lets the token change node-wide settings; weigh that before doing it.

Put the token secret in a file (see Secrets below).

### 4. Create the SSH key

Create a dedicated key pair, install the public key in `root`'s `authorized_keys` on the node, and capture the node's host key. PBP refuses a host that does not match `known_hosts`:

```
ssh-keygen -t ed25519 -f secrets/pve_ssh_key -N '' -C pbp
ssh-copy-id -i secrets/pve_ssh_key.pub root@pve1.example.lan
ssh-keyscan -t ed25519 pve1.example.lan > known_hosts
```

### 5. Configure and start

Copy `config.example.yaml` to `config.yaml` and edit it. Put the API token secret in `secrets/pve_token`. Then:

```
docker compose up -d
docker compose exec pbp pbp check
```

`pbp check` validates the config and your access and runs the full preflight, without restoring anything. Fix everything it reports before you rely on the schedule.

Secrets and volumes: the image runs as the non-root uid/gid **65532** (distroless `nonroot`). The secret files and `config.yaml` must be readable by that user (for example `chown 65532:65532 secrets/* known_hosts config.yaml && chmod 400 secrets/*`, or `chmod 444` for the non-secret files). `/var/lib/pbp` and `/var/log/pbp` are created in the image owned by 65532, so the named volume `pbp-state` works as is. A bind mount hides that ownership: for `./logs:/var/log/pbp` run `mkdir -p logs && chown 65532:65532 logs` on the host first. Run logs are created readable only by uid 65532 (directory `0750`, files `0600`), so on the host read them as root or as that uid, for example `sudo less logs/run-*.log`.

The container's `stop_grace_period` is 11 minutes because on shutdown the daemon waits up to 10 minutes for an in-flight guest to be torn down. Do not shorten it.

## Operating

```
docker compose up -d                              # start the daemon (scheduled runs)
docker compose exec pbp pbp check                 # preflight only; nothing is restored
docker compose exec pbp pbp run                   # one run now, all guests
docker compose exec pbp pbp run --guest 105       # one run now, only guest 105's newest backup
docker compose exec pbp pbp health                # what the Docker healthcheck runs
docker compose logs -f pbp                        # logs; per-run files are in ./logs
```

| Exit code | Meaning |
|-----------|---------|
| 0 | Success (`check` passed, or the run finished without error) |
| 1 | Failure: bad config, `check` found a problem, the run ended in `ERROR`, or `run --guest` found no backup |
| 2 | Usage error (unknown subcommand or flag) |
| 3 | Busy: another run or check holds the run lock. `run` and `check` both exit 3 and print "a run is already in progress" |

Subcommands take `--config PATH`. The config path otherwise defaults to `$PBP_CONFIG`, then `/etc/pbp/config.yaml`. The state directory is `/var/lib/pbp`, overridable with `PBP_STATE_DIR`. With no config mounted, the container exits non-zero and names `/etc/pbp/config.yaml`.

### Health check

The image has a Docker `HEALTHCHECK` running `pbp health`. The container reports **unhealthy** when the last run ended in a run-level error (for example the node or SSH is unreachable, so no notification could be sent), when a scheduled run did not start, or when the current run has overrun. This is the backstop for a silent night when the node itself cannot notify you. Watch it with your usual container monitoring.

## Notifications

One summary per run is sent from the PVE node (over SSH, using `PVE::Notify`), so it goes to whatever notification targets and matchers you already use. Severity is `info` when everything is `PROVEN` (or skipped as already proven), `warning` for any `BOOTED`, `ERROR` or other `SKIPPED`, and `error` for any `FAILED` or a run-level failure.

To route it, create a matcher in Datacenter > Notifications > Notification Matchers with a **Match field** rule: field `type`, value `pbp`. A `hostname` field is also available.

PBP installs its templates (`pbp-subject.txt.hbs`, `pbp-body.txt.hbs`) into `/etc/pve/notification-templates/default/` on the node during preflight, and updates them if missing or changed. PVE 9 picks up this new template type from there (verified on PVE 9.2), and `/etc/pve` survives package upgrades, so nothing needs reinstalling after an upgrade.

If a target cannot be notified (for example a template fails to render), `PVE::Notify` only logs an `ERROR:` line; PBP treats that as a failed notification, so the run's outcome is `error` and the health check reports it.

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

## Configuration

See [`config.example.yaml`](config.example.yaml), which documents every key. Secrets (the API token and SSH key) are always referenced by file path, never inline. TLS to the API is verified with either `tls_fingerprint` or `ca_file`; skipping verification is not supported.

## Building

```
docker build -t pbp:latest .            # multi-stage; static binary on distroless
go test ./... && go vet ./...
```

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
docker run --rm -v "$PWD":/src:ro aquasec/trivy:0.76.0 fs --exit-code 1 --ignore-unfixed \
  --scanners vuln,secret,misconfig --skip-dirs /src/.superpowers /src

# gitleaks (full git history)
docker run --rm -v "$PWD":/repo:ro ghcr.io/gitleaks/gitleaks:v8.30.1 git --redact /repo

# vet and race tests
go vet ./... && go vet -tags integration ./... && go test -race ./...
```

An accepted gosec finding carries a `// #nosec <rule> -- <reason>` comment at the call site.

*Note that this repo is synced from a local Gitea repo, upon commits. This work is being done by traditionally trained software developers using modern assisted coding practice (AI specification-driven generation, through SpecKit and similar tooling)*

## License

PBP is licensed under the [Apache License, Version 2.0](LICENSE).
The Go modules it depends on (golang.org/x/crypto, gopkg.in/yaml.v3,
github.com/robfig/cron) and the container base image it ships on
(distroless) are distributed under their own licenses.
