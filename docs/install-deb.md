# Install from the .deb

As an alternative to Docker, each release ships Debian packages (`amd64` and `arm64`) and tarballs on the [releases page](https://github.com/BreadthAnalytics/proxmox-backup-proofing/releases). The package installs `/usr/bin/pbp`, a systemd unit (`pbp.service`) and the docs under `/usr/share/doc/pbp/`, and creates the `pbp` system user. The same package is also the node helper: `pbp setup` prints the line that installs it on the Proxmox node.

## Install

1. **Download and verify** the package, `SHA256SUMS` and its signature `SHA256SUMS.sig` (here `0.8.1`; use the version you want). You need PBP's public signing key, in the repository's [`allowed_signers`](../allowed_signers) file (`curl -fsSLO https://raw.githubusercontent.com/BreadthAnalytics/proxmox-backup-proofing/main/allowed_signers`); check its fingerprint first ([Verifying a release](#verifying-a-release)):

   ```
   v=0.8.1; a=$(dpkg --print-architecture)
   base=https://github.com/BreadthAnalytics/proxmox-backup-proofing/releases/download/v$(echo "$v" | tr '~' -)
   curl -fsSLO "$base/pbp_${v}_${a}.deb" && curl -fsSLO "$base/SHA256SUMS" && curl -fsSLO "$base/SHA256SUMS.sig"
   ssh-keygen -Y verify -f allowed_signers -I pbp-release -n pbp-release -s SHA256SUMS.sig < SHA256SUMS
   sha256sum -c SHA256SUMS --ignore-missing
   sudo apt install "./pbp_${v}_${a}.deb"
   ```

   The first check must print `Good "pbp-release" signature for pbp-release with ED25519 key SHA256:HzEvo7r8x719x/XzBa5JDQFsFdsw79kNXIdaUt2a7a8`, the second `pbp_<version>_<arch>.deb: OK`. Stop if either fails.

   Pre-releases are tagged like `v0.8.1-rc.2` and their files carry the Debian version `0.8.1~rc.2` (`pbp_0.8.1~rc.2_amd64.deb`), which sorts before `0.8.1`, so the final release installs over a pre-release as an upgrade. GitHub serves a `~` in a file name as `.`, so download a pre-release's package as `pbp_0.8.1.rc.2_${a}.deb` and save it under the name `SHA256SUMS` lists: `curl -fsSL -o "pbp_${v}_${a}.deb" "$base/pbp_$(echo "$v" | tr '~' .)_${a}.deb"`.

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

## Verifying a release

Each release's `SHA256SUMS` is signed with PBP's SSH signing key, so you can check that a release came from the maintainer and not just that the download is intact (`SHA256SUMS` alone cannot tell you that: it sits on the same release page as the packages). Every release from `v0.2.0-rc.1` on carries `SHA256SUMS.sig`.

- **The key** is in the repository's [`allowed_signers`](../allowed_signers) file, and its fingerprint is `SHA256:HzEvo7r8x719x/XzBa5JDQFsFdsw79kNXIdaUt2a7a8` (ED25519). Before trusting a downloaded copy, compare its fingerprint with one from a second source: an installed PBP's `pbp setup` install line (the key is built in), the GitHub repository, where commits and tags signed with this key, such as `v0.8.1`, show as **Verified** (click the badge to see the key's fingerprint), or a clone you already trust. `grep '^pbp-release' allowed_signers | cut -d' ' -f3- | ssh-keygen -lf -` prints the fingerprint.
- **Checking** needs only `ssh-keygen` (from `openssh-client`, present on every Proxmox node): `ssh-keygen -Y verify -f allowed_signers -I pbp-release -n pbp-release -s SHA256SUMS.sig < SHA256SUMS`, then `sha256sum -c SHA256SUMS --ignore-missing` for the files you downloaded.
- **The node install line** that `pbp setup` prints does both checks itself, with the key built into your PBP, so the key never comes from the page it verifies.
- **Tags and commits** made after 2026-10-22 (the first signed tag is `v0.8.1`) are signed with the same key as `tdiana@breadthanalytics.com`; merge commits made in Gitea's web UI are not. Check a tag with `git -c gpg.ssh.allowedSignersFile=allowed_signers tag -v v<version>`.
- **A new release is a draft** until it is signed: the release workflow publishes it as a draft, and the maintainer's signing step makes it public, so a release is never visible without `SHA256SUMS.sig`.

