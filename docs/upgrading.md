# Upgrading

PBP has two parts, and you upgrade them together: the daemon on the PBP host (the Docker image or the `.deb`) and the helper on the Proxmox node (the same `.deb`). They must share major.minor: `0.3.x` works with `0.3.y`, but not with `0.4.z`. Until both sides match, `pbp check` and every run fail with `helper <version> is not compatible with daemon <version>`, and nothing is restored. Read the [changelog](../CHANGELOG.md) for the release first.

Your `config.yaml`, secrets, `known_hosts`, state and logs are kept. A new release may add config keys with defaults; compare your file with [`config.example.yaml`](../config.example.yaml) to see them.

1. **Upgrade the daemon.**
   - Docker: in the checkout, `git pull && docker compose build && docker compose up -d`.
   - `.deb` host: download the new `.deb` and `SHA256SUMS` from the release, then `sha256sum -c SHA256SUMS --ignore-missing && sudo apt install ./pbp_<version>_<arch>.deb`. The service is restarted if it was running (see [Install from the .deb](install-deb.md)).
2. **Run `pbp setup` again**: the Docker one-shot from [Setup step 3](setup.md#3-run-pbp-setup-on-the-pbp-host), or `sudo pbp setup --deb`. With `config.yaml` in place it asks nothing. When the node's helper is older, the check fails and setup prints the node lines for the new version and waits for the reply.
3. **On the node, as root, run both lines.** The install line installs the `.deb` of the daemon's exact version. `pbp node setup` then brings the node to what this version expects (for example new notification templates), shows a plan for anything that changed, and prints the reply block; on a node that is already right it prints `setup has already been completed`.
4. **Paste the reply** into `pbp setup`. It runs the check and tells you when it passes.

On a patch release the daemon and the old helper are still compatible, so step 2 may report `setup has already been completed` straight away. Run the install line on the node anyway (`pbp setup` prints it whenever it prints the node lines; otherwise download and install the matching `.deb` as in step 1), so both sides stay on the same release.
