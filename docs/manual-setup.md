# Manual setup (what `pbp setup` does)

These are the steps `pbp setup` and `pbp node setup` carry out, for admins who prefer to do it by hand and as the reference for what the two commands must produce (a test keeps this section and the code in step). Do [Setup](setup.md) steps 1 and 2 first; these steps replace Setup steps 3 to 6.

## Create an API token

On the node, create a user, a role and a **privilege-separated** token, for example user `pbp@pve` and token `proof`, giving the token id `pbp@pve!proof`. A privilege-separated token only gets the permissions that **both** the user and the token have, so grant the ACLs to both:

```
pveum user add pbp@pve
pveum role add PBPProofer --privs "VM.Allocate,VM.Audit,VM.Backup,VM.Config.HWType,VM.Config.Network,VM.Config.Options,VM.PowerMgmt,VM.GuestAgent.Audit,VM.GuestAgent.Unrestricted,Datastore.AllocateSpace,Datastore.Audit,Sys.Audit,SDN.Use"
pveum user token add pbp@pve proof --privsep 1        # prints the secret once
for p in /vms /storage/<restore storage> /storage/<each PBS storage> /nodes/<node> /sdn/zones/localnetwork; do
  pveum acl modify $p --users pbp@pve --tokens 'pbp@pve!proof' --roles PBPProofer
done
```

Put the token secret in `secrets/pve_token` in the Docker checkout, or `/etc/pbp/secrets/pve_token` on a `.deb` host (see [Configure and start](#configure-and-start)). `pbp node setup --create-token` does this step and prints the secret in its reply block.

## Create the SSH key

Create a dedicated key pair and capture the node's host key (paths for the Docker checkout; on a `.deb` host use `/etc/pbp/secrets/pve_ssh_key` and `/etc/pbp/known_hosts`). PBP refuses a host that does not match `known_hosts`, so check the scanned key against one you already trust (for example `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` on the node). The public key is installed on the node in the next step, for the `pbp` user, with a forced command (not in `root`'s `authorized_keys`):

```
ssh-keygen -t ed25519 -f secrets/pve_ssh_key -N '' -C pbp
ssh-keyscan -t ed25519 pve-node.example.lan > known_hosts
```

## Node setup

Done once, as root on the PVE node. Step 1 is the install line `pbp setup` prints; steps 2 to 6 are what `pbp node setup` does. The daemon never does it, because the node is where the restriction lives. Replace `<PBP public key>` with the content of `secrets/pve_ssh_key.pub` (one line: `ssh-ed25519 AAAA... pbp`).

1. **Install the helper.** The helper is `pbp node`, the same binary as the daemon. Install the `.deb` (the package never starts the `pbp` service; see [Install from the .deb](install-deb.md) for downloading it), or copy `/usr/bin/pbp` and create the `pbp` user yourself:

   ```
   apt install ./pbp_<version>_<arch>.deb
   ```

   The `.deb` creates the `pbp` user with shell `nologin` and home `/var/lib/pbp`, which belongs to root (`0755 root:root`) so that `pbp` cannot move or replace `.ssh` in it; the daemon's state, on a host that runs it, is in `/var/lib/pbp/state` (`0700`, owned by `pbp`). Without the package, create these yourself: `install -d -m 0755 -o root -g root /var/lib/pbp` and `install -d -m 0700 -o pbp -g pbp /var/lib/pbp/state`.

   > **Warning:** `apt purge pbp` on the node deletes the `pbp` user, and with it PBP's access. `apt remove` keeps it. A daemon and its helper must share the same major.minor version (`0.3.x` talks to `0.3.y`), so upgrade the package on the node whenever you upgrade the daemon.

2. **Write `/etc/pbp/node.yaml`.** This is the helper's own copy of the range, tag scope and bridge. It must match `target.vmid_range` and `target.bridge` in the daemon's `config.yaml` (`pbp check` fails if it does not), and `vmid_range[1]` must be below 999999999:

   ```
   install -d -m 0750 -o root -g pbp /etc/pbp
   cat > /etc/pbp/node.yaml <<'EOF'
   vmid_range: [990000, 990049]
   bridge: vmbr99
   request_timeout: 15m
   EOF
   chown root:pbp /etc/pbp/node.yaml && chmod 0640 /etc/pbp/node.yaml
   ```

   `request_timeout` (default 15m, minimum 30s) caps how long the helper lets one request run. Keep it at or above `restore.probe_timeout`, or `pbp check` warns that long probe commands are cut short. A missing or invalid `node.yaml` makes the helper refuse everything.

3. **Give `pbp` a shell**, so sshd can run the forced command. The home directory stays `0755` and owned by root. Only nodes get this; daemon-only hosts keep `nologin`:

   ```
   usermod -s /bin/sh pbp
   test "$(stat -c '%a %U:%G' /var/lib/pbp)" = "755 root:root"
   ```

4. **Authorize the key, forced to the helper**, in `/var/lib/pbp/.ssh/authorized_keys`, as one line:

   ```
   install -d -m 0755 -o root -g root /var/lib/pbp/.ssh
   printf 'restrict,command="sudo /usr/bin/pbp node" %s\n' '<PBP public key>' > /var/lib/pbp/.ssh/authorized_keys
   chown root:root /var/lib/pbp/.ssh/authorized_keys && chmod 0644 /var/lib/pbp/.ssh/authorized_keys
   ```

   `restrict` turns off port, agent and X11 forwarding, the pty and `~/.ssh/rc`, and whatever a later OpenSSH adds to that list. The directory and the file belong to root, so the `pbp` user cannot rewrite its own forced command; sshd accepts a key file owned by root. Keep nothing else in `.ssh`, and make sure sshd reads no other key source for `pbp`: `sshd -T -C user=pbp,host=localhost,addr=127.0.0.1 | grep -i authorizedkeys` lists them, and Debian's default also reads `.ssh/authorized_keys2`, which must not exist. `TrustedUserCAKeys` and `AuthorizedPrincipalsCommand` must be `none` for `pbp` (a `Match User pbp` block can set that): a certificate from a user CA logs in without the forced command.

5. **Add the one sudo rule**, in `/etc/sudoers.d/pbp`. Proxmox VE does not include `sudo`; the `pbp` package brings it along as a recommended package, but if you copied the binary instead, or apt is set not to install recommends, run `apt install sudo` first. Check it with `visudo -cf` before it is installed; a broken sudoers file locks you out of sudo:

   ```
   f=$(mktemp)
   cat > "$f" <<'EOF'
   Defaults:pbp env_reset
   Defaults:pbp env_keep = "SSH_ORIGINAL_COMMAND SSH_CONNECTION"
   pbp ALL=(root) NOPASSWD: /usr/bin/pbp node
   EOF
   visudo -cf "$f" \
       && install -m 0440 -o root -g root "$f" /etc/sudoers.d/pbp \
       && visudo -c
   rm -f "$f"
   ```

   `env_reset` with exactly that `env_keep` list is part of the design: the helper takes the request from `SSH_ORIGINAL_COMMAND` and nothing else from the environment. Do not widen it.

6. **Install the notification templates**, once, as root on the node:

   ```
   pbp node install-templates
   ```

   It writes `pbp-subject.txt.hbs` and `pbp-body.txt.hbs` into `/etc/pve/notification-templates/default/` and prints the paths. It runs only interactively as root: over SSH, and through the sudo rule, it is refused. Run it again after upgrading PBP if `pbp check` says the templates are outdated.

7. **Point PBP at it.** In `config.yaml` use `target.ssh.user: pbp` (the default), with the key and `known_hosts` from [Create the SSH key](#create-the-ssh-key). `root` is rejected. Then run `pbp check`:

   ```
   node helper version: ok (0.6.1)
   node helper config: ok (range [990000, 990049], bridge vmbr99)
   node helper request_timeout: ok (15m0s)
   node helper refusal: ok (exec in 990050 refused)
   ```

   Each failure ends with `see docs/setup.md` and names what differs: an unreachable helper, an incompatible version, a differing `vmid_range` or bridge, a `request_timeout` below the probe timeout (a warning), or a helper that did not refuse an out-of-range exec.

The `UsePAM yes` that Debian ships in `sshd_config` is what lets this locked system account log in by key; if you have turned `UsePAM` off, see [Troubleshooting](troubleshooting.md).

**Optional: cap concurrent logins.** To bound how many sessions a stolen key can hold open at once, add a `maxlogins` limit for `pbp`:

```
echo 'pbp hard maxlogins 8' > /etc/security/limits.d/pbp.conf
```

`pam_limits` applies it, so it needs `UsePAM yes` (Debian's default) and the `session required pam_limits.so` line Debian ships in `/etc/pam.d/sshd`. On a node with systemd-logind the count includes the session of `pbp`'s own systemd user manager, so `8` allows 7 concurrent requests. Keep it well above `workers` plus two (a `pbp check` or `pbp run` next to the daemon). A login over the limit is cut off by sshd with `There were too many logins for 'pbp'` and exit code 254 before the helper runs, so PBP cannot tell it from a guest command that failed: a cap that is too low shows up as wrongly failed checks.

## Configure and start

**`.deb` host:** see [Install from the .deb](install-deb.md), "By hand instead of `pbp setup`".

**Docker:** create these next to `docker-compose.yml` before the first `docker compose up`:

- [ ] `config.yaml`, copied from `config.example.yaml` and edited
- [ ] `known_hosts`, from `ssh-keyscan` ([Create the SSH key](#create-the-ssh-key))
- [ ] the node set up ([Node setup](#node-setup))
- [ ] `secrets/pve_token`, containing the API token secret
- [ ] `secrets/pve_ssh_key`, the private key
- [ ] `logs/`, owned by uid 65532 (`mkdir -p logs && chown 65532:65532 logs`)
- [ ] optionally `.env` with `TZ=<your zone>` (the default is `Europe/London`)

The secret files and `config.yaml` must be readable by uid 65532 (for example `chown 65532:65532 secrets/* known_hosts config.yaml && chmod 400 secrets/*`, or `chmod 444` for the non-secret files). A bind mount hides the image's ownership: for `./logs:/var/log/pbp` run `mkdir -p logs && chown 65532:65532 logs` on the host first.

Then start PBP and check it as in [Setup step 6](setup.md#6-start-and-check).
