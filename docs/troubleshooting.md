# Troubleshooting

`pbp check` prints one line per step and ends every node helper failure with `see docs/setup.md`.

| Symptom | Cause and fix |
|---------|---------------|
| `target.ssh.user: root is no longer supported` | PBP refuses root. Set up the helper ([Node setup](manual-setup.md#node-setup)) and use `user: pbp`. |
| `the pbp node helper is not reachable`, or `Permission denied (publickey)` | Run `pbp node setup` on the node again (the line `pbp setup` prints): it shows what differs. Otherwise: the key is not in `/var/lib/pbp/.ssh/authorized_keys` of the `pbp` user ([Node setup](manual-setup.md#node-setup) step 4), or the file or `.ssh` directory has the wrong owner or mode (both owned by root, `.ssh` `0755`, the file `0644`). The log of sshd on the node says which. With the optional `maxlogins` cap, `There were too many logins for 'pbp'` means the cap is too low. |
| Login works but nothing runs, or `This account is currently not available` | The `pbp` user still has the `nologin` shell. Run `usermod -s /bin/sh pbp` ([Node setup](manual-setup.md#node-setup) step 3). Also check that `UsePAM yes` is set in `sshd_config` (Debian's default); with `UsePAM no`, sshd refuses key logins for a locked account such as this one. |
| `sudo: a password is required`, or `sudo: no tty present` | The sudoers rule is missing or does not match `/usr/bin/pbp node` exactly ([Node setup](manual-setup.md#node-setup) step 5). Check `/etc/sudoers.d/pbp` with `visudo -cf`, and that `/usr/bin/pbp` is the binary installed by the package. |
| `pbp-node: refused: bad-request` / `no request` | The helper ran without a valid request. Something other than PBP used the key, or the `env_keep` of `SSH_ORIGINAL_COMMAND` is missing from the sudoers rule. |
| `pbp-node: refused: out-of-range`, `untagged` or `not-container` | The guest is outside the helper's `vmid_range`, has no `pbp` tag, or is not an LXC container. By design. If the range itself is wrong, fix `node.yaml` and `target.vmid_range` together. |
| `vmid_range differs` or `bridge differs` | `/etc/pbp/node.yaml` and `config.yaml` disagree; the message gives both values. Make them equal. Nothing is restored until they match. |
| `helper 0.2.1 is not compatible with daemon 0.3.0` | The daemon and the helper must share major.minor. Upgrade the `.deb` on the node (or the daemon) to match. A `dev` build is compatible only with a `dev` build. |
| `helper 0.3.0 is not compatible with daemon dev` (or the reverse) | One side is a `dev` build: a binary from a plain `go build`, or an image built with `--build-arg VERSION=dev`. A `dev` build is compatible only with a `dev` build. Rebuild the image without the build argument (it then takes its version from the `VERSION` file) or with `--build-arg VERSION=<release>`, and install the matching `.deb` on the node. |
| `notification templates on the node: ... run "pbp node install-templates" as root on the node` | Install or refresh the templates ([Node setup](manual-setup.md#node-setup) step 6). |
| `pbp-node: timeout` on long probe commands | `request_timeout` in `node.yaml` is below what the command needs; keep it at or above `restore.probe_timeout`. |
| Access stopped working after a node cleanup | `apt purge pbp` on the node deletes the `pbp` user and its `authorized_keys`. Reinstall and redo [Node setup](manual-setup.md#node-setup) steps 3 and 4. |
