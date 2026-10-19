# Changelog

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
