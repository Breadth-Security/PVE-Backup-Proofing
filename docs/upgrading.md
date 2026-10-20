# Upgrading

## Upgrading from 0.2 to 0.3

Version 0.3.0 adds the guided setup (`pbp setup`, `pbp node setup`, `pbp node uninstall`). Nothing about a working 0.2 setup changes, but the daemon and the node helper must share major.minor, so upgrade both to 0.3.x together:

1. If you changed `TZ` in `docker-compose.yml`, put `TZ=<your zone>` in a `.env` file next to it and run `git checkout docker-compose.yml` before pulling. 0.3's compose file reads the zone from `.env`, and `pbp setup` writes `.env` only when it creates `config.yaml`, so without this the schedule falls back to `Europe/London` (and `git pull` refuses to overwrite your edited file).
2. Upgrade the daemon: `git pull && docker compose build && docker compose up -d` (or install the 0.3 `.deb` on a `.deb` host).
3. In the checkout, run the Docker one-shot from [Setup step 3](setup.md#3-run-pbp-setup-on-the-pbp-host) (or `sudo pbp setup --deb`). It finds your `config.yaml`, key and token, asks nothing, and, because the 0.2 helper on the node is not compatible with a 0.3 daemon, prints the node lines. It uses only the file names it writes itself (Docker: `secrets/pve_ssh_key`, `secrets/pve_token` and `known_hosts` in the checkout, named `/run/secrets/pve_ssh_key`, `/run/secrets/pve_token` and `/etc/pbp/known_hosts` in `config.yaml`; `.deb`: `/etc/pbp/secrets/pve_ssh_key`, `/etc/pbp/secrets/pve_token` and `/etc/pbp/known_hosts`). If your `config.yaml` names other files, it stops before changing anything and names each one: move the files to those paths, update `config.yaml` to match (`sudoedit /etc/pbp/config.yaml` on a `.deb` host), and run it again.
4. On the node, as root, run both lines: the install line upgrades the helper to 0.3, and `pbp node setup` checks the node. On a node set up by hand as the 0.2 README described, it prints `setup has already been completed` and the reply block, and changes nothing. If it shows a plan instead, the node is not quite what the 0.2 steps produce (for example a token named other than `pbp@pve!proof`, or a sudo rule you edited): read the plan before you answer, and see `--replace` in [Setup step 4](setup.md#4-run-pbp-node-setup-on-the-node).
5. Paste the reply into `pbp setup`. It refreshes `known_hosts`, `target.node` and the fingerprint from the node (and says which values it changed), then runs the check.

Until both sides are on 0.3, `pbp check` and every run fail with `helper 0.2.x is not compatible with daemon 0.3.y`.

## Upgrading from 0.1 to 0.2

Version 0.2.0 is a breaking change: PBP no longer uses root SSH. A 0.2 daemon refuses `ssh.user: root` and refuses to run until the node has the helper. To migrate:

1. Do [Node setup](manual-setup.md#node-setup) on the node: install the 0.2 `.deb`, write `/etc/pbp/node.yaml` with the same range and bridge as your `config.yaml`, give `pbp` a shell, install the forced-command key and the sudoers rule, and run `pbp node install-templates` (PBP no longer installs the templates itself).
2. Switch `target.ssh.user` to `pbp` (or remove the line; `pbp` is the default). The existing key pair can be reused: put its public key in the `pbp` user's `authorized_keys` line from [Node setup](manual-setup.md#node-setup) step 4.
3. Upgrade the daemon to 0.2 and run `pbp check`. Fix what it reports.
4. Remove the PBP public key from `/root/.ssh/authorized_keys` on the node. Until you do, the old key still gives root access.

If you set up a node for a 0.2.0 release candidate, redo [Node setup](manual-setup.md#node-setup) steps 4 and 5: the `authorized_keys` line now starts with `restrict`, the `.ssh` directory and the file are owned by root (so `pbp` cannot change its own forced command), and the sudoers file is written through `mktemp`. Running step 4 again fixes the owner and mode of an existing `.ssh`.
