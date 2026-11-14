# Install with APT

PBP's signed APT repository is the easiest way to install the `pbp` package and keep it up to date: add it once, then `apt install pbp` and `apt upgrade` work like any other package. It carries final releases (no pre-releases) for `amd64` and `arm64`, from 0.10.0 on. The package itself is the same `.deb` as on the [releases page](https://github.com/Breadth-Security/PVE-Backup-Proofing/releases) ([Install from the .deb](install-deb.md)).

APT checks every download against the repository's signing key, so the key is what you trust. It is a dedicated OpenPGP key, kept off any build server like PBP's release key.

## Add the repository

As root, on the node and on a separate PBP host alike:

```
curl -fsSLo /usr/share/keyrings/pbp-archive-keyring.gpg https://apt.breadthsecurity.com/pbp-archive-keyring.gpg
cat > /etc/apt/sources.list.d/pbp.sources <<'EOF'
Types: deb
URIs: https://apt.breadthsecurity.com
Suites: stable
Components: main
Signed-By: /usr/share/keyrings/pbp-archive-keyring.gpg
EOF
apt update && apt install pbp
```

Installing the package does not add the repository by itself: `pbp` never changes where your system gets software from.

## Check the key (optional)

The key file is signed with PBP's release key, the same one that signs every release's `SHA256SUMS` ([Verifying a release](install-deb.md#verifying-a-release)). To check it, with the repository's [`allowed_signers`](../allowed_signers):

```
curl -fsSLO https://apt.breadthsecurity.com/pbp-archive-keyring.gpg.sig
ssh-keygen -Y verify -f allowed_signers -I pbp-release -n pbp-release \
  -s pbp-archive-keyring.gpg.sig < /usr/share/keyrings/pbp-archive-keyring.gpg
```

It must print `Good "pbp-release" signature`. Then compare the key's fingerprint, `gpg --show-keys /usr/share/keyrings/pbp-archive-keyring.gpg`, with the one in [`packaging/apt/pbp-archive-keyring.asc`](../packaging/apt/pbp-archive-keyring.asc) in this repository.

## Upgrading

`apt upgrade` installs a new PBP like any package; the service is restarted if it was running.

**Upgrade the node and the PBP host in the same sitting.** The daemon and the node helper must share major.minor, in both directions ([Upgrading](upgrading.md)): on a minor version change (0.10 to 0.11), whichever side is upgraded first refuses the other, and `pbp check` and every run fail until both match. Do both outside the schedule. A patch release (0.11.0 to 0.11.1) works in either order. PBP on the node itself is one package, so it is never out of step.

For the same reason, do not add this repository to `unattended-upgrades`, which by default upgrades only Debian's own origins: an unattended upgrade of one side would stop the nightly runs until the other side is upgraded.

## Pinning a version

Every final release from 0.10.0 on stays in the repository:

```
apt install pbp=0.10.0      # install (or go back to) a given version
apt-mark hold pbp           # keep apt upgrade from moving it
apt-mark unhold pbp
```

## Removing the repository

```
rm /etc/apt/sources.list.d/pbp.sources /usr/share/keyrings/pbp-archive-keyring.gpg
apt update
```

This leaves the installed `pbp` package alone; to remove PBP itself, see [Remove](install-deb.md#remove).
