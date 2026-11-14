# APT repository and unattended setup: design

Status: draft for the owner's approval. Issues #130 (APT repository) and #131 (unattended setup), part of #127 (outreach): "make trying it cheap", and the helper-scripts rule "never prompt without an escape hatch".

## Intent

Two things stand between a Proxmox admin and a running PBP: a manual download for every install and upgrade, and a setup that only works by answering questions at a terminal. This change removes both:

1. **An APT repository**, so that `apt install pbp` and `apt upgrade` work, signed so that APT itself verifies every download.
2. **Unattended setup**, so that `pbp setup` and `pbp node setup` run end to end from flags, with no terminal. Automation tools (Ansible, cloud-init, a future helper script) can then set PBP up.

## Decisions (owner, 2026-11-14)

- **Hosting:** Cloudflare R2, a public bucket on the custom domain `apt.breadthsecurity.com`.
- **Signing:** a new, dedicated OpenPGP key (APT cannot use the SSH release key), kept on the maintainer's machine and never in CI, as with the SSH release key.
- **Unattended scope:** both PBP on the node itself and PBP on a separate host.
- **Node gate:** an explicit flag lifts only the "stdin must be a terminal" check. SSH forced commands, the `pbp` account's sudo rule and non-root stay refused, flag or not.
- **One branch for both:** `aptRepository`.

## Goals

- `apt install pbp` from `https://apt.breadthsecurity.com`, for amd64 and arm64, final releases only.
- The repository's trust is anchored in the existing release trust: the APT key's fingerprint is published in the signed repository, and the key file is signed with the SSH release key, so `allowed_signers` vouches for it.
- Publishing a release to APT is one local script run after `mirror-release.sh`. It verifies before it uploads, and verifies again after.
- `pbp setup --unattended …` never reads stdin and never prompts. Any answer it needs and lacks is an error naming the flag.
- Nothing about the interactive flows changes.

## Non-goals

- Pre-releases in APT (they stay downloads on the releases page). A `testing` suite can come later.
- Docker images in a registry.
- Creating the isolated bridge (a separate item of #127; setup still checks it and refuses without it).
- Automatic upgrades. `unattended-upgrades` ignores third-party origins by default, and the docs say why it should stay that way (see version skew).

## Anti-goals (a failure even if it works)

- A signing key in CI, or a secret passed on a command line.
- An unattended mode that a stolen PBP SSH key, or the `pbp` account, could drive.
- An index uploaded before the files it lists, so that `apt update` sees hash mismatches.

## Part 1: the APT repository

### Layout

A flat, standard Debian repository with one suite and one component:

```
https://apt.breadthsecurity.com/
  pbp-archive-keyring.gpg              binary OpenPGP public key (for signed-by)
  pbp-archive-keyring.gpg.sig          the key file signed with the SSH release key
  dists/stable/InRelease               clearsigned Release
  dists/stable/Release, Release.gpg    Release and its detached signature
  dists/stable/main/binary-amd64/Packages, Packages.gz
  dists/stable/main/binary-arm64/Packages, Packages.gz
  dists/stable/main/binary-*/by-hash/SHA256/<hash>   the same index files, by hash
  pool/main/p/pbp/pbp_<version>_<arch>.deb
```

- `Release` sets `Acquire-By-Hash: yes`, so a client mid-update never mixes an old `Release` with a new `Packages`.
- `Release` lists SHA256 hashes only; APT has required SHA256 for years. It also sets `Origin: PBP`, `Label: PBP`, `Suite: stable`, `Codename: stable`, `Architectures: amd64 arm64`, `Components: main` and `Date`.
- *My call:* every final release from 0.10.0 on stays in the pool and in `Packages`, so `apt install pbp=<version>` can pin or downgrade. Older releases are not back-filled.

### User side

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

- **Checking the key**, optional but documented: `ssh-keygen -Y verify` of `pbp-archive-keyring.gpg.sig` with `allowed_signers`, as for release files, and comparing the fingerprint with the one in the repository's docs.
- **The PBP package does not add the repository itself.** No `postinst` writes APT sources, because installing a package should not change where the system gets software from.

### The publish tool

**`packaging/apt-publish.sh TAG…`**, run on the maintainer's machine after `mirror-release.sh`:

1. **Download and verify:** the release's `.deb` files, `SHA256SUMS` and `SHA256SUMS.sig` from the GitHub release. It verifies the signature with `allowed_signers` and the checksums, as `mirror-release.sh` does, and refuses a pre-release version (`~`).
2. **Fetch the current index:** `Packages` for each architecture, from the live repository. An empty repository is the first run.
3. **Build the new index:** `go run ./tools/aptindex` adds the new `.deb` files to it and writes `Packages`, `Packages.gz`, the by-hash copies and `Release`.
   - It reads each `.deb`'s control file itself (the `ar` and `tar` formats), so it needs no Debian tools.
   - A version that is already present with the same SHA256 is a no-op. The same version with a different hash is an error: a published file never changes.
4. **Sign:** with `gpg` and the key `$PBP_APT_KEY` (a fingerprint), as `InRelease` (clearsigned) and `Release.gpg` (detached). Then `gpgv` checks both against the published keyring.
5. **Upload, in order:**
   1. the pool files;
   2. the by-hash index files;
   3. `Packages` and `Packages.gz`;
   4. last, `Release`, `Release.gpg` and `InRelease`.

   The upload uses `curl --aws-sigv4` against R2's S3 API, so no extra tool is needed. The R2 access key is read from a file (`$PBP_R2_CREDENTIALS`), never from the command line or the environment.
   - Pool and by-hash files get `Cache-Control: public, max-age=31536000, immutable`.
   - `InRelease`, `Release*`, `Packages*` and the keyring get `Cache-Control: no-cache`.
6. **Read back:** fetch `InRelease` over HTTPS, verify it with `gpgv`, check that the `Packages` hashes it lists match what is served, and check that each new `.deb` is served with the right size and SHA256.

Flags: `--dry-run` stops before uploading and leaves the built tree in a temporary directory for inspection. `--init` uploads the keyring files to an empty bucket.

### One-time setup (owner)

1. Create the OpenPGP key: ed25519, no expiry or a long one (*my call*), user ID `PBP APT repository <…>`. Back it up offline.
2. Create the R2 bucket. Give it public access on the custom domain `apt.breadthsecurity.com`, and leave the `r2.dev` URL off.
3. Create an R2 API token with Object Read & Write on that bucket only, saved to a file with mode 0600.
4. Run `packaging/apt-publish.sh --init`, then `packaging/apt-publish.sh v0.10.0`.

### Version skew

The daemon and the node helper must share major.minor. With APT on both hosts, `apt upgrade` on the PBP host can move the daemon to a new minor version before the node.

- `pbp check` already fails clearly when this happens.
- The docs say to upgrade the node first, then the host, and why automatic upgrades should not include this repository.
- A node running PBP on itself upgrades both halves in one package, so it is never skewed.

## Part 2: unattended setup

### `pbp setup --unattended`

`--unattended` means:
- never read stdin and never prompt;
- take every answer from flags, using the interactive default where one exists;
- confirm the plan automatically;
- fail, naming the flag, when an answer is needed and missing.

The answer flags:

| Flag | Interactive question | Default when the flag is absent |
|------|----------------------|---------------------------------|
| `--address` | Node address | `127.0.0.1` on the node itself; otherwise required |
| `--vmid-range` | VMID range | `990000-990049` |
| `--bridge` | Isolated bridge | `vmbr99` |
| `--restore-storage` | Restore storage | `local-zfs` |
| `--storages` | PBS storages, comma-separated | required |
| `--schedule` | Schedule (cron) | `0 2 * * *` |
| `--tz` | Time zone (Docker only) | the host's |

- **Validation:** each value goes through the same check as the interactive answer: `config.ValidStorageID`, the range rule, the bridge name rule and the cron parser.
- **An existing `config.yaml`:** the flags are not applied, as now. If a flag disagrees with the file, setup stops and names both values, rather than ignoring the flag silently (*my call*).
- **On the node itself** (`--deb` on a PVE node), the run goes end to end:
  - "Set up this node here?" is yes;
  - "Apply these changes?" is yes;
  - a node file that differs still needs `--replace`, as now;
  - "Replace the token?" needs the explicit `--rotate-token`, never `--unattended` alone: a rotation cuts off any other daemon using the token.
- **On a separate host,** without `--node-reply`, setup writes the config and the SSH key, prints only the node line on stdout, and exits with a new code **4** (setup waits for the node's reply). With `--node-reply FILE` (or `-` for stdin), it reads the reply from there instead of the paste, finishes, and runs the check.

### `pbp node setup --unattended`

- **`--unattended` lifts exactly one gate check: "stdin is not a terminal".** It requires `--yes`. These stay refused, flag or not:
  - `SSH_ORIGINAL_COMMAND` set;
  - `SUDO_USER=pbp`;
  - not root.

  So the `pbp` account, or a stolen PBP key, still cannot drive setup. An admin's own SSH session, or Ansible with `become`, is not a forced command and is allowed.
- **`--reply-file FILE`** writes the reply block, token secret included, to `FILE`. It is created with mode 0600, owned by root, and an existing file is refused. Otherwise the reply goes to stdout, as now.
- **The same rules apply to `pbp node uninstall --unattended`**, so automation can also undo it.

### Example: a separate host, end to end

```
# PBP host
pbp setup --deb --unattended --address pve1.example.lan --storages pbs-main   # exit 4, prints the node line
# node, as root (an admin session or automation, not the pbp account)
pbp node setup <the printed arguments> --yes --unattended --reply-file /root/pbp-reply
# copy /root/pbp-reply to the PBP host over a secure channel, then:
pbp setup --deb --unattended --node-reply /root/pbp-reply && shred -u /root/pbp-reply
```

## Docs

- **New `docs/install-apt.md`:** the repository, checking the key, upgrading node-first, pinning, removing the repository.
- **`docs/install-deb.md`:** points to the APT install as the easier path.
- **README:** "Try it on one guest" step 1 offers APT; the getting-started steps link to the unattended setup.
- **`docs/setup.md`:** a new "Unattended setup" section with both flows and exit code 4.
- **`docs/operating.md`:** exit code 4.
- **`docs/security.md`:**
  - the APT key, and what it can and cannot do (it can publish a package APT will install as root: protect it like the release key);
  - the unattended gate.
- **`docs/development.md`:** the release steps gain `apt-publish.sh`, and the one-time setup.
- **CHANGELOG.**

## Tests

- **`tools/aptindex`:**
  - the fields of a `Packages` stanza from a real nfpm-built `.deb`;
  - `Release` hashes, sizes and the by-hash paths;
  - adding a version that is already present (same hash: no-op; different hash: error);
  - a pre-release refused;
  - output that is byte-identical on a rerun with the same `Date`.
- **An APT round trip in a Debian container on the test host** (not in unit tests):
  - build a throwaway repository signed with a throwaway key;
  - `apt update && apt install pbp` from it, checking the installed version;
  - then a tampered `Packages` must make `apt update` fail.
- **`pbp setup --unattended`:**
  - no read from stdin, whatever is on it;
  - the defaults;
  - a missing `--storages` or `--address` named in the error;
  - a flag that disagrees with an existing config;
  - exit 4 and a node line alone on stdout;
  - `--node-reply` from a file and from `-`;
  - on-node mode never prompts;
  - `--rotate-token` required for a rotation.
- **The node gate:**
  - with `--unattended`, a non-terminal stdin is allowed;
  - `SSH_ORIGINAL_COMMAND`, `SUDO_USER=pbp` and non-root are still refused;
  - `--unattended` without `--yes` is a usage error;
  - `--reply-file` mode 0600, and an existing file refused.
- **Live, on the test node:**
  - an on-node unattended setup from a clean state;
  - the separate-host flow with a reply file;
  - `apt install` from the real repository after the first publish.

## Left to the builder

- Internal structure of `tools/aptindex` and how `apt-publish.sh` calls it.
- Exact wording of messages, within the docs' style.
- Whether `--unattended` and `--yes` are both accepted on `pbp setup` (with `--yes` meaning the same), as long as the docs name one.
