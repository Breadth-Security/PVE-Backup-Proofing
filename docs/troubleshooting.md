# Troubleshooting

Most problems show up in one of two places: while `pbp setup` or `pbp node setup` runs, or in `pbp check` (which also runs at the start of every scheduled run). `pbp check` prints one line per step and stops at the first part that fails; node helper failures end with `see docs/setup.md`. Run it on the PBP host:

```
docker compose exec pbp pbp check                       # Docker
sudo -u pbp PBP_CONFIG=/etc/pbp/config.yaml pbp check   # .deb
```

When a guest in a run failed or errored, look first at that guest's own log file in the run's folder (`VM102.log` or `CT101.log` under `logging.dir`; see [Logs](operating.md#logs)): it has a line for every step and, for a failed PVE task, the tail of the task log.

**The first fix to try for anything on the node side** is to run the `pbp node setup` line again (rerun `pbp setup` on the PBP host to print it). It shows, as a plan, everything on the node that differs from what PBP needs, and fixes it after asking; it changes nothing that is already right.

## During setup

| Message | Cause and fix |
|---------|---------------|
| `the address does not reach that node: … answers SSH with a different host key than node …` | The node address you gave `pbp setup` leads to another machine, usually through DNS or `/etc/hosts`. The message names the address, what it resolves to and the node's own addresses. Set `target.ssh.host` and the host in `target.api.url` in `config.yaml` to one of those addresses (or `127.0.0.1` when PBP runs on the node itself) with `sudoedit`, then run `pbp setup` again. |
| `The token pbp@pve!proof already exists on the node and its secret cannot be read back` | PVE shows a token's secret only once, and this host does not have it. Run the `--rotate-token` line setup prints on the node, then `pbp setup` again. See [Lost token secret](setup.md#lost-token-secret). |
| `pbp node setup: refused: run it interactively as root, not over SSH (…)` | `pbp node setup` and `pbp node uninstall` run only as root in an interactive terminal. The reason in brackets says what is missing: run it in a root shell (the console, or `ssh -t root@node`), not through a script or a pipe. |
| `this node has pbp …, the PBP host runs …: install pbp … on the node first` | The node's `pbp` package is not the daemon's version. Run the install line `pbp setup` printed (line 1) before the `pbp node setup` line. |
| `sudo is not installed …` | PBP's helper needs one sudo rule, and Proxmox VE does not include `sudo`. The `pbp` package normally installs it as a recommended package; it is missing when apt is set not to install recommends (`APT::Install-Recommends "false"`), or when the `pbp` binary was copied without the package. Run `apt install sudo` on the node, then the setup line again. |
| `the bridge … does not exist on this node`, or `the bridge … has …` | Create the bridge as in [Setup step 2](setup.md#2-create-the-isolated-bridge), including the `post-up` line that disables IPv6. The message says what is wrong (no such bridge, a port, an address, IPv6 enabled); a missing bridge is reported with the stanza to add. |
| `guests not owned by PBP sit in the VMID range …` (setup), `guest not owned by PBP in VMID range` (check) | A guest without the `pbp` tag sits in the VMID range. Make sure it is not yours, then remove it, or choose another range ([Security model](security.md#what-pbp-touches-the-vmid-range-and-the-pbp-tag)). |
| `these differ from what setup writes: …; rerun with --replace …` | A file on the node (often a sudo rule or `authorized_keys` edited by hand) is not what setup writes. Read the difference in the plan; if setup's version is right, rerun with `--replace`, which backs the old file up first. |
| `… names files where pbp setup does not keep them …` | An existing `config.yaml` points its key, token or `known_hosts` at other paths. Move the files to the paths the message gives and update `config.yaml` to match, then run `pbp setup` again. |
| `config.yaml is a directory (Docker creates one when the file is missing)` | `docker compose` ran before setup wrote `config.yaml`. Remove the empty directory (and any other empty directories Compose made for missing files) and run setup again. |
| `no node reply found: -----BEGIN PBP NODE REPLY----- marker missing` | The paste did not include the reply block. Copy the whole block, from the `BEGIN` line to the `END` line, and run `pbp setup` again. |

## In `pbp check` and runs

| Message | Cause and fix |
|---------|---------------|
| `pve api …: FAIL` | The API is not reachable at `target.api.url`, the TLS fingerprint does not match the node's certificate, or the token is wrong. Check the address, then run `pbp setup` again so it refreshes the fingerprint from the node. |
| `ssh …: FAIL: … knownhosts: key mismatch` | The address answers SSH with a host key other than the one in `known_hosts`: either the address leads to another machine (see the first row above), or the node was reinstalled. Run `pbp setup` again; a new reply replaces `known_hosts`. |
| `the pbp node helper is not reachable`, or `Permission denied (publickey)` | Run the `pbp node setup` line again: it shows what differs. The usual causes are a missing or edited key in `/var/lib/pbp/.ssh/authorized_keys`, wrong owners or modes there, or a reinstalled `pbp` package after an `apt purge`. The sshd log on the node says which. With the optional `maxlogins` cap, `There were too many logins for 'pbp'` means the cap is too low. |
| `This account is currently not available`, or login works but nothing runs | The `pbp` user still has the `nologin` shell, or sshd has `UsePAM no` (sshd then refuses key logins for a locked account such as this one). Run the `pbp node setup` line again; it checks both. |
| `sudo: a password is required`, or `sudo: no tty present` | The sudo rule is missing or does not match `/usr/bin/pbp node` exactly. Run the `pbp node setup` line again. |
| `helper 0.3.1 is not compatible with daemon 0.4.0` | The daemon and the node helper must share major.minor. Upgrade both: [Upgrading](upgrading.md). |
| `… is not compatible with daemon dev` (or the reverse) | One side is a `dev` build: a binary from a plain `go build`, or an image built with `--build-arg VERSION=dev`. A `dev` build works only with a `dev` build. Rebuild the image without that argument (it then takes its version from the `VERSION` file) and install the matching `.deb` on the node. |
| `vmid_range differs` or `bridge differs` | `/etc/pbp/node.yaml` on the node and `config.yaml` disagree; the message gives both values. Make them equal: change `config.yaml`, then run `pbp setup` and the node line again. Nothing is restored until they match. |
| `restore storage … is not active and enabled`, or `… does not hold … content` | The restore storage in `config.yaml` (`restore.storage`) is disabled or not available on the node, or is not set up for VM images and container volumes (`images` and `rootdir` content). Pick an active storage that can (`pvesm status` on the node lists them), set it with `sudoedit`, and run the `pbp node setup` line again so the token gets access to it. |
| `bridge is not isolated` | Something changed on the bridge since setup: a port, an address, or IPv6 enabled (after a reload without the `post-up` line, for example). The message says which; fix it as in [Setup step 2](setup.md#2-create-the-isolated-bridge). |
| `notification templates on the node: … run "pbp node install-templates" as root on the node` | The templates are missing or older than this PBP version. Run the `pbp node setup` line again (or `pbp node install-templates` as root on the node). |
| `pbp-node: refused: out-of-range`, `untagged` or `not-container` | The helper refused a guest outside its `vmid_range`, without the `pbp` tag, or not an LXC container. That is by design and protects your guests; if the range itself is wrong, fix `config.yaml` and rerun setup. |
| `pbp-node: refused: bad-request` / `no request` | Something other than PBP used the key, or the sudo rule lost its `env_keep` of `SSH_ORIGINAL_COMMAND`. Run the `pbp node setup` line again. |
| `pbp-node: timeout` on long probe commands | `request_timeout` in `/etc/pbp/node.yaml` is shorter than the command needs; keep it at or above `restore.probe_timeout`. |
| `guest still starting after 4m30s (waiting on: <units>)` | The guest booted, but systemd still had jobs queued when the probe ran out of time (`restore.probe_timeout`). The units named are what boot waits on; the guest's log file has the full job list. A unit that waits on the network can wait for ever on the isolated bridge; if it is only slow, raise `restore.probe_timeout`. |
| `BOOTED · container without nesting: … enable nesting=1` | An unprivileged container without `nesting=1`: its systemd cannot start sandboxed units, in production as in the copy. Enable nesting on the container; see [Containers without nesting](checks.md#containers-without-nesting). |
| `ERROR · raw lxc config (…) needs root@pam to restore` | The container's config has raw `lxc.*` lines, which PVE drops when PBP's token restores, so PBP does not proof it. See [Security model](security.md#the-api-token). |
| `a run is already in progress` (exit 3) | Another run or check holds the run lock, usually the daemon's scheduled run. Wait for it to finish. |

## A run on demand

| Message | Cause and fix |
|---------|---------------|
| `no backup found for guest N` (`pbp run --guest N`) | None of the PBS storages in `sources.storages` holds a backup of guest N, or its VMID is inside PBP's own range (those are never proofed). |
| The guest is `SKIPPED` as `template (not bootable)` | The backup is of a VM or container template (`template: 1` in its stored config). A template cannot be started, so PBP never proofs it; this is by design and does not make the run a `warning`. To prove a template's contents, proof a guest cloned from it. |
| The guest is `SKIPPED` as `already proven` | Its newest backup already carries a `PBP PROVEN` line. Set `restore.reproof: true` in `config.yaml` to proof it again, or wait for a newer backup. |

## The service (`.deb` hosts)

| Symptom | Cause and fix |
|---------|---------------|
| `pbp.service` stopped with exit code 78 | The config is missing pieces or invalid, for example a `config.yaml` that `pbp setup` has not finished. `journalctl -u pbp` shows the reason. Fix it (run `sudo pbp setup --deb` again), then `sudo systemctl start pbp`. The service does not restart on its own for this. |
| `pbp.service` does not start at all (condition failed) | There is no `/etc/pbp/config.yaml` yet. Run `sudo pbp setup --deb` first. |
| Access to the node stopped working after a cleanup | `apt purge pbp` on the node deletes the `pbp` user and its key. Reinstall the package and run the `pbp node setup` line again. |
