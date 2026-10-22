# Install from the .deb

As an alternative to Docker, each release ships Debian packages (`amd64` and `arm64`) and tarballs on the [releases page](https://git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/releases). The package installs `/usr/bin/pbp`, a systemd unit (`pbp.service`) and the docs under `/usr/share/doc/pbp/`, and creates the `pbp` system user. The same package is also the node helper: `pbp setup` prints the line that installs it on the Proxmox node.

## Install

1. **Download and verify** the package and `SHA256SUMS` of a release (here `0.6.1`; use the version you want):

   ```
   v=0.6.1; a=$(dpkg --print-architecture)
   base=https://git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/releases/download/v$v
   curl -fsSLO "$base/pbp_${v}_${a}.deb" && curl -fsSLO "$base/SHA256SUMS"
   sha256sum -c SHA256SUMS --ignore-missing
   sudo apt install "./pbp_${v}_${a}.deb"
   ```

   Pre-releases are tagged like `v0.6.1-rc.2` and their files carry the Debian version `0.6.1~rc.2` (`pbp_0.6.1~rc.2_amd64.deb`), which sorts before `0.6.1`, so the final release installs over a pre-release as an upgrade.

2. **Set it up.** Choose the VMID range and create the bridge first ([Setup](setup.md) steps 1 and 2), then run:

   ```
   sudo pbp setup --deb
   ```

   and continue with [Setup](setup.md) steps 4 and 5. On the Proxmox node itself, the same command sets up the node in the same shell: [PBP on the Proxmox node itself](setup.md#pbp-on-the-proxmox-node-itself).

   It writes `/etc/pbp/config.yaml`, `/etc/pbp/known_hosts` and `/etc/pbp/secrets/` (`root:pbp`, `0640`; the directory `0750`). The service can read `/etc/pbp` but not write to it. The package creates `/etc/pbp` but never creates or overwrites `config.yaml`.

3. **Start it and check:**

   ```
   sudo systemctl start pbp
   sudo -u pbp PBP_CONFIG=/etc/pbp/config.yaml pbp check
   ```

   The package enables the unit on install but never starts it: you start it once, and after that it starts at boot. Without a usable `/etc/pbp/config.yaml` it does not start (`ConditionPathExists`), or stops with exit code 78 and is not restarted until the config is fixed. `pbp check` takes the run lock, so it exits 3 ("a run is already in progress") while the daemon is mid-run; try again when the run has finished.

Everyday commands for a `.deb` host (a run on demand, logs, the health check): [Operating](operating.md).

**By hand instead of `pbp setup`** (after the [manual setup](manual-setup.md) steps for the token, key and node):

```
sudo cp /usr/share/doc/pbp/config.example.yaml /etc/pbp/config.yaml   # then edit
sudo install -d -m 0750 -o root -g pbp /etc/pbp/secrets
sudo install -m 0640 -o root -g pbp pve_token /etc/pbp/secrets/pve_token
sudo install -m 0640 -o root -g pbp pve_ssh_key /etc/pbp/secrets/pve_ssh_key
ssh-keyscan -t ed25519 pve-node.example.lan | sudo tee /etc/pbp/known_hosts
sudo chown root:pbp /etc/pbp/config.yaml /etc/pbp/known_hosts
sudo chmod 0640 /etc/pbp/config.yaml /etc/pbp/known_hosts
```

In `config.yaml` set `token_secret_file` and `key_file` to `/etc/pbp/secrets/...` and `known_hosts_file: /etc/pbp/known_hosts`.

## Where things are

| | Path |
|---|---|
| config, secrets, `known_hosts` | `/etc/pbp/` |
| state (in-flight proofs, the run lock) | `/var/lib/pbp/state` (owned by `pbp`) |
| per-run logs (one folder per run) | `/var/log/pbp` (readable by root and `pbp`) |
| service log | `journalctl -u pbp` |

## Upgrade

Install the newer `.deb` the same way (`sudo apt install ./pbp_<new>_<arch>.deb`). Config, secrets and state are kept, and the service is restarted if it was running (a stopped one stays stopped). The daemon and the helper on the node must share major.minor, so upgrade the node too: see [Upgrading](upgrading.md).

## On a Proxmox node

On the node, the package is the helper. Its install script creates the `pbp` user with the `nologin` shell and keeps the user's home `/var/lib/pbp` owned by root (`0755`, also on an upgrade), and it never touches sshd, sudo or `authorized_keys`. The node-only steps (the `pbp` login shell, the forced-command key, the sudo rule, `node.yaml`) are done by `pbp node setup` (or [by hand](manual-setup.md#node-setup)), never by the package, so a host that only runs the daemon never gets a `pbp` login.

`pbp node setup` warns when the node also runs a PBP daemon (`/etc/pbp/config.yaml` exists or `pbp.service` is active), except in the [on-node setup](setup.md#pbp-on-the-proxmox-node-itself) you chose on purpose. For production, a separate PBP host is recommended: see [Where to run PBP](security.md#where-to-run-pbp).

## Remove

`apt remove pbp` stops and disables the service but keeps `/etc/pbp`, `/var/lib/pbp`, `/var/log/pbp` and the `pbp` user. `apt purge pbp` deletes all three directories and the user. On a node, that deletes the `pbp` user and with it PBP's access (and `/var/lib/pbp/.ssh`), so do not purge a node you still use for proofs; to undo the node setup cleanly, use [`pbp node uninstall`](setup.md#removing-pbp-from-a-node). Purge also removes `/etc/sudoers.d/pbp` when it is exactly the rule setup wrote. The PVE user `pbp@pve`, its token and the `PBPProofer` role stay (removing the package says so): remove them with `pbp node uninstall --remove-token` before purging.
