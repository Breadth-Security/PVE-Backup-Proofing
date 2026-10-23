# Security policy

PBP restores backups on Proxmox VE nodes and installs a helper there that runs as root through one sudo rule, so a vulnerability in it can matter a great deal. Please report one privately.

## Reporting a vulnerability

**Use GitHub's private vulnerability reporting:** the repository's **Security** tab → **Report a vulnerability**. Do not open a public issue or pull request for a vulnerability.

Include what you can of:

- the affected version (`pbp version`) and where it runs (PBP host, Proxmox node, Docker or `.deb`);
- what an attacker needs first (a stolen SSH key, the API token, a guest under their control, access to the PBP host);
- what they gain, and how to reproduce it;
- any fix you have in mind.

You will get an acknowledgement within 7 days. Once a fix is released, the report is credited in the CHANGELOG unless you ask otherwise.

## Supported versions

Only the newest release gets security fixes. PBP's daemon and node helper must share major.minor, so upgrade both sides as [Upgrading](docs/upgrading.md) describes.

## What is in scope

- the daemon (`pbp daemon`, `pbp run`, `pbp check`, `pbp setup`);
- the node helper (`pbp node`), its SSH forced command, its sudo rule, and `pbp node setup` / `uninstall`;
- the packages, the release files and their signatures, and the install line `pbp setup` prints.

[Security model](docs/security.md) explains what PBP is allowed to touch on the node (its VMID range and `pbp`-tagged guests only), what the API token and SSH key can and cannot do, and the guests PBP deliberately does not proof. A way around any of those limits is in scope.

## Verifying releases

Every release's `SHA256SUMS` is signed with PBP's SSH signing key, fingerprint `SHA256:HzEvo7r8x719x/XzBa5JDQFsFdsw79kNXIdaUt2a7a8`. See [Verifying a release](docs/install-deb.md#verifying-a-release).
