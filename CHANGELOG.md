# Changelog

## v0.3.0

**Guided setup.** Two commands now do what README "Setup" used to describe step by step. See README "Setup".

- `pbp setup` on the PBP host (Docker: a one-shot `docker run`, see the README for the exact command; `.deb` hosts: `sudo pbp setup --deb`) asks a few questions, writes `config.yaml` from `config.example.yaml` with its comments, creates the SSH key, and prints two lines to run on the node: the checksum-verified install of the matching `.deb`, and `pbp node setup`. It then reads the node's reply (without echoing it) and writes `known_hosts`, the token secret, `target.node` and `target.api.tls_fingerprint`, and runs the check. Files get the owners and modes the README already required.
- `pbp node setup`, as root in an interactive terminal on the node, checks the node, shows a plan, asks, and then sets up the helper exactly as the manual steps do (`node.yaml`, the `pbp` shell, the forced-command key, the sudo rule, the notification templates), plus the API role, user, privilege-separated token and ACLs (`--create-token`). It proves the result through the real sudo path and prints a reply block with the node's host key, the API certificate fingerprint and, once, the token secret. Options: `--dry-run`, `--yes`, `--replace` (a differing file is copied to `<file>.pbp-backup-<YYYYMMDDTHHMMSS>` first; without it setup stops), `--rotate-token`. It refuses to run from an SSH forced command (`SSH_ORIGINAL_COMMAND` set), through the `pbp` sudo rule, or without a terminal, and never creates the bridge, reloads networking, or touches root's `authorized_keys`, `sshd_config` or any guest.
- Both print `setup has already been completed` when there is nothing to do.
- `pbp node uninstall` removes the sudo rule and the key first, then `node.yaml`, and sets the `pbp` shell back to `nologin`. `--remove-token` also removes the token, ACLs, user and role. `--dry-run` and `--yes` as for setup.
- `docker-compose.yml` reads the time zone from `.env` (`TZ: ${TZ:-Europe/London}`), which `pbp setup` writes; `.env` is git-ignored.
- The manual steps remain in README "Manual setup (what `pbp setup` does)", unchanged in substance.

### Upgrading from 0.2

The daemon and the node helper must share major.minor: **upgrade both to 0.3.x together**. If you changed `TZ` in `docker-compose.yml`, first put `TZ=<your zone>` in a `.env` file next to it and run `git checkout docker-compose.yml` before pulling: the compose file now reads the zone from `.env`, which `pbp setup` writes only when it creates `config.yaml`, so otherwise the schedule falls back to `Europe/London`. Then upgrade the daemon, run `pbp setup` in the checkout (or `sudo pbp setup --deb`), and run the two lines it prints on the node. A node set up by hand per the 0.2 README reports `setup has already been completed`; paste the reply back. See README "Upgrading from 0.2 to 0.3".

## v0.2.0

**Breaking change: PBP no longer uses root SSH.** The daemon talks to a restricted `pbp node` helper on the PVE node, through an SSH forced command for the unprivileged `pbp` user and one sudo rule. The helper can run `pct exec` only in tagged containers inside the configured VMID range, report the isolated bridge's state, send `pbp` notifications and print its config. See README "Node setup".

Behaviour changes:

- `target.ssh.user` defaults to `pbp`. `root` is rejected by config validation.
- The node needs `/etc/pbp/node.yaml` (`vmid_range`, `bridge`, optional `request_timeout`, default 15m). `pbp check` and preflight fail if it differs from `target.vmid_range` and `target.bridge`.
- The daemon and the helper must share major.minor (`0.2.x` with `0.2.y`). A `dev` build is compatible only with a `dev` build.
- A Docker build without `--build-arg VERSION` (including `docker compose build`) is stamped from the new top-level `VERSION` file instead of `dev`. The release workflow fails a tag whose version disagrees with it.
- PBP no longer installs the notification templates itself. Run `pbp node install-templates` as root on the node; preflight only checks them.
- `pbp check` has new steps: node helper version, config, `request_timeout` and refusal.
- The `.deb` is unchanged for daemon-only hosts (the `pbp` user keeps the `nologin` shell). On a node, `apt purge pbp` deletes the `pbp` user and PBP's access.
- Integration tests use a separate test-only admin key (`PBP_IT_ADMIN_SSH_KEY`); see `test/integration/README.md`.

### Migrating from 0.1

1. Set up the helper on the node as described in README "Node setup": install the 0.2 `.deb`, write `/etc/pbp/node.yaml`, `usermod -s /bin/sh pbp`, add the forced-command `authorized_keys` line and the sudoers rule, and run `pbp node install-templates`.
2. Set `target.ssh.user: pbp` (or remove the line).
3. Upgrade the daemon to 0.2 and run `pbp check`.
4. Remove PBP's public key from `/root/.ssh/authorized_keys` on the node.

If you tested a 0.2.0 release candidate, redo README "Node setup" step 4: the `authorized_keys` line is now `restrict,command="sudo /usr/bin/pbp node" <key>`, and `/var/lib/pbp/.ssh` (`0755`) and `authorized_keys` (`0644`) are owned by root so the `pbp` user cannot rewrite its forced command. An optional `maxlogins` cap for `pbp` is documented there too. The request format changed after the release candidates (the daemon now sends the time it still allows, `--timeout-ms`, instead of an absolute `--deadline`), so a release-candidate helper refuses a 0.2.0 daemon's requests as `bad-request` and the other way round: upgrade both together.
