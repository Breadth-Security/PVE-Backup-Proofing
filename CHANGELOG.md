# Changelog

## v0.3.0

**Since v0.3.0-rc.1:**

- `sudo pbp setup --deb` on a Proxmox VE node sets the node up in the same run: it offers `127.0.0.1` as the node address, asks before running `pbp node setup`'s checks and plan, and passes the reply internally (no second shell, nothing pasted, the token secret never shown). It offers to replace an existing token it has no secret for.
- `pbp setup` checks that the node address reaches the node that printed the reply (by the SSH host key, without logging in) before writing anything, and names the address, what it resolves to and the node's own addresses when it does not. The node's reply now carries those addresses.
- `pbp check`'s "pve api" step makes a real request.
- The daemon exits 78 on a config it cannot use, and `pbp.service` does not restart on it.
- `--rotate-token`'s plan and docs say that the new token is granted only the paths of that run.

**Guided setup.** Two commands now do what the README's "Setup" section used to describe step by step. See [docs/setup.md](docs/setup.md).

- `pbp setup` on the PBP host (Docker: a one-shot `docker run`, see [docs/setup.md](docs/setup.md#3-run-pbp-setup-on-the-pbp-host) for the exact command; `.deb` hosts: `sudo pbp setup --deb`) asks a few questions, writes `config.yaml` from `config.example.yaml` with its comments, creates the SSH key, and prints two lines to run on the node: the checksum-verified install of the matching `.deb`, and `pbp node setup`. It then reads the node's reply (without echoing it) and writes `known_hosts`, the token secret, `target.node` and `target.api.tls_fingerprint`, and runs the check. Files get the owners and modes the README already required.
- `pbp node setup`, as root in an interactive terminal on the node, checks the node, shows a plan, asks, and then sets up the helper exactly as the manual steps do (`node.yaml`, the `pbp` shell, the forced-command key, the sudo rule, the notification templates), plus the API role, user, privilege-separated token and ACLs (`--create-token`). It proves the result through the real sudo path and prints a reply block with the node's host key, the API certificate fingerprint and, once, the token secret. Options: `--dry-run`, `--yes`, `--replace` (a differing file is copied to `<file>.pbp-backup-<YYYYMMDDTHHMMSS>` first; without it setup stops), `--rotate-token`. It refuses to run from an SSH forced command (`SSH_ORIGINAL_COMMAND` set), through the `pbp` sudo rule, or without a terminal, and never creates the bridge, reloads networking, or touches root's `authorized_keys`, `sshd_config` or any guest. It needs `sudo` (Proxmox VE does not install it; the `.deb` now recommends it), and stops, without changing anything, if sshd would read `pbp`'s keys from anywhere but `/var/lib/pbp/.ssh/authorized_keys` (for example an `authorized_keys2`, another `AuthorizedKeysFile`, an `AuthorizedKeysCommand`, or an SSH user CA via `TrustedUserCAKeys` or `AuthorizedPrincipalsCommand`) or if that `.ssh` holds other files.
- Both print `setup has already been completed` when there is nothing to do.
- `pbp`'s home, `/var/lib/pbp`, now belongs to root (`0755`): the package makes it so on install and upgrade, so `pbp` cannot move or replace the `.ssh` directory that holds its forced-command key. The daemon's state moved to `/var/lib/pbp/state` (`PBP_STATE_DIR`'s default; owned by `pbp` on a `.deb` host, by uid 65532 in the image). `pbp node setup` refuses a home that is not root's.
- `pbp node setup` warns when `pbp@pve` or its tokens have permissions beyond PBP's role and paths (another role or path, a group, a second token), and re-applies an ACL entry that does not propagate.
- `pbp node uninstall` removes the sudo rule and the key first, then `node.yaml`, and sets the `pbp` shell back to `nologin`. `--remove-token` also removes the token, ACLs, user and role. `--dry-run` and `--yes` as for setup.
- `docker-compose.yml` reads the time zone from `.env` (`TZ: ${TZ:-Europe/London}`), which `pbp setup` writes; `.env` is git-ignored.
- The `.deb` no longer starts `pbp.service` from its postinst: an install only enables it (start it once with `systemctl start pbp`), and an upgrade restarts it only if it is running. Before, an upgrade started an enabled, stopped service whenever `/etc/pbp/config.yaml` existed, which on a node could start a daemon from a stray config. `pbp node setup` warns when the node also runs a PBP daemon.
- The manual steps remain, unchanged in substance, in [docs/manual-setup.md](docs/manual-setup.md).
- The README is now a short overview; the setup, operating, checks, troubleshooting, upgrading and development sections moved to `docs/`, which the `.deb` and the tarballs ship next to the README. Messages that pointed at README "Node setup" now say `see docs/setup.md`.

## v0.2.0

**Breaking change: PBP no longer uses root SSH.** The daemon talks to a restricted `pbp node` helper on the PVE node, through an SSH forced command for the unprivileged `pbp` user and one sudo rule. The helper can run `pct exec` only in tagged containers inside the configured VMID range, report the isolated bridge's state, send `pbp` notifications and print its config. See [docs/manual-setup.md](docs/manual-setup.md#node-setup).

Behaviour changes:

- `target.ssh.user` defaults to `pbp`. `root` is rejected by config validation.
- The node needs `/etc/pbp/node.yaml` (`vmid_range`, `bridge`, optional `request_timeout`, default 15m). `pbp check` and preflight fail if it differs from `target.vmid_range` and `target.bridge`.
- The daemon and the helper must share major.minor (`0.2.x` with `0.2.y`). A `dev` build is compatible only with a `dev` build.
- A Docker build without `--build-arg VERSION` (including `docker compose build`) is stamped from the new top-level `VERSION` file instead of `dev`. The release workflow fails a tag whose version disagrees with it.
- PBP no longer installs the notification templates itself. Run `pbp node install-templates` as root on the node; preflight only checks them.
- `pbp check` has new steps: node helper version, config, `request_timeout` and refusal.
- The `.deb` is unchanged for daemon-only hosts (the `pbp` user keeps the `nologin` shell). On a node, `apt purge pbp` deletes the `pbp` user and PBP's access.
- Integration tests use a separate test-only admin key (`PBP_IT_ADMIN_SSH_KEY`); see `test/integration/README.md`.
