# Install from the .deb

As an alternative to Docker, each release ships Debian packages (`amd64` and `arm64`) and tarballs. The package installs `/usr/bin/pbp`, a systemd unit (`pbp.service`) and the docs under `/usr/share/doc/pbp/`, and creates the `pbp` system user. The same `.deb` is also the node helper: `pbp setup` prints the line that installs it on the PVE node, where the package never starts the `pbp` service.

1. Download the `.deb` and `SHA256SUMS` from the release and verify:

   ```
   sha256sum -c SHA256SUMS --ignore-missing
   sudo apt install ./pbp_<version>_<arch>.deb
   ```

2. Run the guided setup ([Setup](setup.md) steps 3 to 5) with the `.deb` layout:

   ```
   sudo pbp setup --deb
   ```

   It writes `/etc/pbp/config.yaml`, `/etc/pbp/known_hosts` and `/etc/pbp/secrets/` (`root:pbp`, `0640`, the directory `0750`), with `/etc/pbp/...` paths in the config. The package creates `/etc/pbp` (`0750 root:pbp`) but never creates or overwrites `config.yaml`.

   **By hand** instead (after the [manual setup](manual-setup.md) steps for the token, key and node):

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

   `pbp check` takes the run lock, so it exits 3 ("a run is already in progress") while the daemon is mid-run; try again when the run has finished. The package enables the unit on install but never starts it: you start it, once, with `systemctl start pbp` (it does nothing without `/etc/pbp/config.yaml`). After that it starts at boot.

Logs: `journalctl -u pbp -f`, and the per-run files in `/var/log/pbp` (read them as root or `pbp`). State is in `/var/lib/pbp/state`.

Releases tagged `v1.2.0` produce version `1.2.0`; a pre-release tag such as `v1.2.0-rc1` produces `1.2.0~rc1`, which sorts before `1.2.0`. File names carry that Debian version (`pbp_1.2.0~rc1_amd64.deb`).

**Upgrade** by installing a newer `.deb` with `apt install ./pbp_<new>.deb`; the config and state are untouched, and the service is restarted if it is running (a stopped one stays stopped). **On a PVE node** the same package is the helper (see [Node setup](manual-setup.md#node-setup)). Its postinst creates the `pbp` user with the `nologin` shell, keeps its home `/var/lib/pbp` root's (`0755`, also on an upgrade) and never touches sshd, sudo or `authorized_keys`; the node-only steps (`usermod -s /bin/sh pbp`, the forced-command key, the sudoers rule, `node.yaml`) are done by `pbp node setup` or by hand, never by the package, so daemon-only hosts never get a login shell. Installing a newer `.deb` on the node upgrades the helper and keeps that setup, but the daemon and the helper must share major.minor, so upgrade both. `pbp node setup` warns when the node also runs a PBP daemon (`/etc/pbp/config.yaml` exists or `pbp.service` is active): the daemon runs as `pbp`, which owns `/var/lib/pbp`.

**Remove** (`apt remove pbp`) stops and disables the service but keeps `/etc/pbp`, `/var/lib/pbp` and `/var/log/pbp` and the `pbp` user. **Purge** (`apt purge pbp`) deletes all three directories and the user. On a node that deletes the `pbp` user and with it PBP's access (and `/var/lib/pbp/.ssh`), so do not purge a node you still use for proofs.

PBP can run on the PVE host itself. It still connects over SSH as the restricted `pbp` user, but it then shares a host with the guests it restores. Prefer a separate host.
