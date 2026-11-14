# APT Repository and Unattended Setup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `apt install pbp` from a signed repository at `https://apt.breadthsecurity.com`, and `pbp setup` / `pbp node setup` that run end to end from flags without a terminal.

**Architecture:**
- **APT:** a pure-Go package `internal/aptrepo` (reads `.deb` control data, keeps the `Packages` stanzas, writes a suite with by-hash index files and `Release`). A thin CLI, `tools/aptindex`, wraps it, and `packaging/apt-publish.sh` does the rest: it verifies the release, fetches and verifies the live index, calls the CLI, signs with `gpg`, uploads to R2 with `curl --aws-sigv4`, and reads back.
- **Unattended:** answers come from flags through the same validators as the interactive questions (`internal/hostsetup`). One `unattended` switch flows through `cmd/pbp`'s setup and node-gate code, so no stdin read happens.

**Tech Stack:** Go (standard library only: `archive/tar`, `compress/gzip`, `crypto/sha256`), POSIX sh, `gpg`/`gpgv`, `curl` ≥ 7.75, `ssh-keygen -Y`.

**Spec:** `specs/2026-11-14-apt-repository-and-unattended-setup-design.md` (issues #130, #131).

## Global Constraints

- **Repository URL:** `https://apt.breadthsecurity.com`. One suite and one component (`stable`, `main`), architectures `amd64 arm64`.
- **`Release` fields:**
  - `Origin: PBP`, `Label: PBP`, `Suite: stable`, `Codename: stable`, `Architectures: amd64 arm64`, `Components: main`, `Acquire-By-Hash: yes`, `Date` (RFC 1123, UTC);
  - SHA256 hashes only.
- **Pool path:** `pool/main/p/pbp/pbp_<version>_<arch>.deb`. Final releases only: a version containing `~` is refused.
- **A published file never changes:** the same version and arch with a different SHA256 is an error, and the same SHA256 is a no-op.
- **Keys and secrets:**
  - The signing key never goes to CI. `$PBP_APT_KEY` is a key fingerprint; the R2 credentials come from the file `$PBP_R2_CREDENTIALS`, never from a command line or the environment.
  - Secrets are passed as file paths. A curl config file built from the credentials is mode 0600 and removed on exit.
- **Upload order:**
  1. pool;
  2. by-hash index files;
  3. `Packages` and `Packages.gz`;
  4. `Release`, `Release.gpg`, `InRelease`.
- **`Cache-Control`:** `public, max-age=31536000, immutable` for pool and by-hash files; `no-cache` for everything else.
- **`--unattended`:**
  - never reads stdin;
  - on `pbp node setup` and `pbp node uninstall`, lifts only "stdin is not a terminal" and requires `--yes`;
  - `SSH_ORIGINAL_COMMAND` set, `SUDO_USER=pbp` and non-root stay refused.
- **Exit codes:** the new exit code `4` means "setup waits for the node's reply". The existing ones are 0, 1, 2 and 3.
- **Answer defaults for `--unattended`:**
  - `--vmid-range` `990000-990049`, `--bridge` `vmbr99`, `--restore-storage` `local-zfs`, `--schedule` `0 2 * * *`, `--tz` the host's;
  - `--address` defaults to `127.0.0.1` on the node itself and is otherwise required;
  - `--storages` is required.
- **Token rotation** in unattended mode needs an explicit `--rotate-token`.
- **`--reply-file`:** created with mode 0600; an existing path, including a symlink, is refused.
- **No new Go module dependencies.**
- **The repository's rules:** no lab host names in repo files; tests and builds on the dev test host (`rt`); signed commits; Fable review before the PR.

## Review Focus

1. **A failed fetch of the live index** (a timeout or 5xx, or a 404 when the repository is not new) must abort the publish. It must never publish an index without the earlier versions. Pinned by `apt-publish.sh`, which requires `InRelease` to verify, plus `--first` for an empty repository (Task 3).
2. **An upload interrupted halfway** must leave a repository APT still accepts: the old `Release` keeps pointing at by-hash files that are never deleted. Pinned by `TestWriteSuiteKeepsOldByHash` (Task 2).
3. **A `--reply-file` path that already exists or is a symlink** must be refused without writing the secret. Pinned in Task 5.
4. **Stdin with content under `--unattended`** (a piped `y` or `n`) must not be read, and must not change the outcome. Pinned in Tasks 5 and 6.
5. **Flags that match an existing `config.yaml`** must go ahead, and flags that differ must stop and name both values. Pinned in Task 4.

---

### Task 1: `internal/aptrepo`: read a `.deb` and keep an index

**Files:**
- Create: `internal/aptrepo/deb.go`, `internal/aptrepo/index.go`
- Test: `internal/aptrepo/deb_test.go`, `internal/aptrepo/index_test.go` (the tests build small `.deb` files in Go: `ar` with `debian-binary`, `control.tar.gz`, `data.tar.gz`)

**Interfaces:**
- **Produces:**
  - `type Field struct{ Name, Value string }`
  - `type Stanza []Field`, with `func (s Stanza) Get(name string) string`
  - `type Deb struct{ Control Stanza; Size int64; SHA256 string }`
  - `func ReadDeb(path string) (Deb, error)`
  - `func ParsePackages(r io.Reader) ([]Stanza, error)`
  - `func FormatPackages(st []Stanza) []byte`
  - `type Index struct{ Stanzas []Stanza }`
  - `func (ix *Index) Add(d Deb, filename string) (added bool, err error)`

- [ ] **Step 1: Write the failing tests**
  - **`TestReadDeb`:** a built `.deb` with control fields `Package: pbp`, `Version: 0.10.0`, `Architecture: amd64` gives `Get("Version") == "0.10.0"`, and `Size` and `SHA256` equal to the file's.
  - **`TestReadDebRejects`:**
    - a file that is not an `ar` archive;
    - a `.deb` with no `control.tar.*`;
    - `control.tar.xz`, with the error naming the member.
  - **`TestParseFormatRoundTrip`:** a two-stanza `Packages` text with a multi-line `Description` (continuation lines start with a space) survives `Parse` then `Format` byte for byte.
  - **`TestIndexAdd`:**
    - Adding gives a stanza with the control fields, then `Filename: pool/main/p/pbp/pbp_0.10.0_amd64.deb`, `Size`, `SHA256`, in that order.
    - The same deb again gives `added == false` and no error.
    - The same version and arch with another SHA256 gives an error containing `already published with a different SHA256`.
    - Version `0.11.0~rc.1` gives an error containing `pre-release`.

- [ ] **Step 2: Run them to make sure they fail**
  Run: `scratchpad/rt go test ./internal/aptrepo/`
  Expected: build failure, undefined `ReadDeb`.

- [ ] **Step 3: Implement**
  - **`ReadDeb`** parses the `ar` format (8-byte magic, 60-byte headers, 2-byte alignment) and opens `control.tar.gz` (gzip) or `control.tar` (plain), then reads `./control`. It hashes the whole file.
  - **`Add`** matches existing stanzas on `Package`, `Version` and `Architecture`.

- [ ] **Step 4: Run them to make sure they pass**
  Run: `scratchpad/rt go test ./internal/aptrepo/`
  Expected: `ok`.

- [ ] **Step 5: Commit**
  `git commit -S -m "aptrepo: read .deb control data and keep a Packages index (#130)"`, with the attribution trailer.

### Task 2: write a suite, and the `tools/aptindex` CLI

**Files:**
- Create: `internal/aptrepo/suite.go`, `tools/aptindex/main.go`
- Test: `internal/aptrepo/suite_test.go`, `tools/aptindex/main_test.go`

**Interfaces:**
- **Consumes:** Task 1.
- **Produces:**
  - `type Suite struct{ Name string; Archs []string; Date time.Time; Packages map[string][]Stanza }` (keyed by arch)
  - `func WriteSuite(root string, s Suite) error`, which writes under `root/dists/<Name>/`:
    - `main/binary-<arch>/Packages` and `Packages.gz`;
    - `main/binary-<arch>/by-hash/SHA256/<hex>` for both;
    - the unsigned `Release`.
  - **The CLI:** `aptindex -root DIR -date RFC1123 -suite stable [deb…]`
    - It reads `DIR/dists/stable/main/binary-*/Packages` if present.
    - It copies each new deb to `DIR/pool/main/p/pbp/` and adds it.
    - It writes the suite, and prints `added <file>` or `present <file>` for each deb.

- [ ] **Step 1: Write the failing tests**
  - **`TestWriteSuite`:** the `Release` contains each of the Global Constraints fields and `Acquire-By-Hash: yes`.
    - Its `SHA256:` section lists `main/binary-amd64/Packages` and `Packages.gz`, and the same for arm64, with sizes and hashes that match the files.
    - Each by-hash file exists and has the bytes its name hashes to.
  - **`TestWriteSuiteDeterministic`:** two runs with the same `Date` and input give byte-identical trees. `Packages.gz` is written with no name and a zero mtime in its gzip header.
  - **`TestWriteSuiteKeepsOldByHash`:** running again with one more stanza leaves the first run's by-hash files in place.
  - **`TestWriteSuiteEmptyArch`:** an arch with no packages gets an empty `Packages`, which is still listed.
  - **`TestAptindexCLI`:** builds two debs, runs `main` with `-root` set to a temp dir, then again with the same debs. The second run prints `present` twice and leaves `Packages` unchanged.

- [ ] **Step 2: Run them to make sure they fail**
  Run: `scratchpad/rt go test ./internal/aptrepo/ ./tools/aptindex/`
  Expected: build failure.

- [ ] **Step 3: Implement**
  - The `Release` text, exactly (the stanza fields in this order, then the hash lines):
    ```
    Origin: PBP
    Label: PBP
    Suite: stable
    Codename: stable
    Date: <Date in time.RFC1123 with "UTC">
    Architectures: amd64 arm64
    Components: main
    Acquire-By-Hash: yes
    SHA256:
     <hex> <size> main/binary-amd64/Packages
    ```
  - Hash lines are sorted by path.
  - The CLI's `run(args []string, stdout io.Writer) error` is what the test calls; `main` wraps it.

- [ ] **Step 4: Run them to make sure they pass**
  Run: `scratchpad/rt go test ./internal/aptrepo/ ./tools/aptindex/`
  Expected: `ok`.

- [ ] **Step 5: Check a real package.** On the test host, run `packaging/build.sh` with a test version, then `go run ./tools/aptindex` on its two `.deb` files.
  Expected:
  - `Packages` shows `Package: pbp`, `Architecture: amd64` and `arm64`, and the `Depends` and `Recommends` from `packaging/nfpm.yaml`.
  - `dpkg-deb -f` on the same file agrees field for field.

- [ ] **Step 6: Commit**
  `git commit -S -m "aptrepo: write a signed-ready suite with by-hash indexes; aptindex CLI (#130)"`

### Task 3: `packaging/apt-publish.sh`, the key files, and the APT round trip

**Files:**
- Create:
  - `packaging/apt-publish.sh`;
  - `packaging/apt/pbp-archive-keyring.asc` (placeholder until the owner creates the key; the script refuses a placeholder);
  - `packaging/apt_roundtrip_test.sh`.
- Modify: `docs/development.md` (the release steps and the one-time setup).

**Interfaces:**
- **Consumes:** the `tools/aptindex` CLI.
- **Produces:**
  - **The usage:** `packaging/apt-publish.sh [--dry-run] [--first] TAG…` and `packaging/apt-publish.sh --init`.
  - **The credentials file format:**
    ```
    R2_ACCOUNT_ID=…
    R2_ACCESS_KEY_ID=…
    R2_SECRET_ACCESS_KEY=…
    R2_BUCKET=…
    ```
  - **`--local DIR`:** builds into DIR and does not upload. This is for the round-trip test.
  - **`--key-file FILE`** (only with `--local`): signs with a throwaway exported key in a temporary `GNUPGHOME` instead of `$PBP_APT_KEY`.
  - **`--dry-run`:** runs every step except the upload and the read-back, and prints the temporary directory it leaves behind.

- [ ] **Step 1: Write `packaging/apt_roundtrip_test.sh`** (the failing test). On a Debian host, as an unprivileged user:
  1. Make a throwaway `GNUPGHOME` and key.
  2. Run `apt-publish.sh --local "$tmp/repo" --key-file <throwaway export> <two test debs>`.
  3. Run `apt-get` with isolated `Dir::State`, `Dir::Cache` and `Dir::Etc::sourcelist` pointing at `file://$tmp/repo`, and `signed-by` set to the throwaway key, through `update` and then `download pbp=<test version>`.
  4. Check that the downloaded file's SHA256 equals the input's.
  5. Then append a byte to `Packages`, and check that `apt-get update` exits non-zero.

  Nothing is installed.

- [ ] **Step 2: Run it to make sure it fails**
  Run: `scratchpad/rt sh packaging/apt_roundtrip_test.sh`
  Expected: FAIL, `apt-publish.sh: not found`.

- [ ] **Step 3: Implement `apt-publish.sh`**, in the style of `mirror-release.sh` (`set -eu`, usage from the header comment):
  1. **Download and verify the release** from the GitHub release: `pbp_<v>_{amd64,arm64}.deb`, `SHA256SUMS` and `SHA256SUMS.sig`. It verifies with `ssh-keygen -Y verify` against `allowed_signers` (namespace `pbp-release`) and `sha256sum -c`, and refuses a tag with `-rc` (Debian `~`).
  2. **Fetch the live index:** `InRelease`, verified with `gpgv --keyring <the published keyring>`. Without `--first`, any HTTP status other than 200 aborts, naming the status. With `--first`, a 404 for `InRelease` means an empty repository.
  3. **Fetch the `Packages` files** that `InRelease` lists, and check each one's SHA256 against it.
  4. **Build the index** with `go run ./tools/aptindex -root "$work" -date "$(date -u '+%a, %d %b %Y %H:%M:%S UTC')"` on the debs.
  5. **Sign:** `gpg --local-user "$PBP_APT_KEY" --clearsign` for `InRelease`, and `--detach-sign --armor` for `Release.gpg`. Then `gpgv` checks both.
  6. **Upload, in the Global Constraints order:**
     - with `curl --fail -K "$curlcfg" --aws-sigv4 "aws:amz:auto:s3" -T <file> -H "Cache-Control: …" "https://$R2_ACCOUNT_ID.r2.cloudflarestorage.com/$R2_BUCKET/<key>"`;
     - `$curlcfg` is a mode-0600 temp file holding `user = "<id>:<secret>"`, removed by a trap.
  7. **Read back** from `https://apt.breadthsecurity.com`: `InRelease` (with `gpgv`), each `Packages` file (its hash against `InRelease`) and each new deb (size and SHA256).
  8. **`--init`:**
     - exports the key from `$PBP_APT_KEY` to `pbp-archive-keyring.gpg` (binary) and `.asc`;
     - signs `pbp-archive-keyring.gpg` with `ssh-keygen -Y sign -n pbp-release` (`$PBP_SIGNING_KEY`, as `sign-release.sh`);
     - uploads both, with `no-cache`;
     - writes the `.asc` to `packaging/apt/` for committing.

- [ ] **Step 4: Run it to make sure it passes**
  Run: `scratchpad/rt sh packaging/apt_roundtrip_test.sh`
  Expected: `PASS` lines for download, hash and tamper.
  Then run `shellcheck packaging/apt-publish.sh packaging/apt_roundtrip_test.sh`: clean.

- [ ] **Step 5: Update `docs/development.md`**
  - The release steps gain step 6, `packaging/apt-publish.sh vX.Y.Z`, after `mirror-release.sh`.
  - A new "APT repository (one-time setup)" section covers the owner's four steps from the spec, and the credentials file format.

- [ ] **Step 6: Commit**
  `git commit -S -m "apt-publish.sh: verify, index, sign, upload in order, read back; round-trip test (#130)"`

### Task 4: answers from flags (`internal/hostsetup`)

**Files:**
- Modify: `internal/hostsetup/questions.go`, to extract each question's check into a named function the interactive path also uses.
- Create: `internal/hostsetup/flags.go`
- Test: `internal/hostsetup/flags_test.go`

**Interfaces:**
- **Produces:**
  - `type Flags struct{ Address, VMIDRange, Bridge, RestoreStorage, Storages, Schedule, TZ string }`
  - `func FromFlags(f Flags, l Layout, defaultTZ, defaultAddr string) (Answers, error)`
  - `func Conflicts(cfg config.Config, f Flags) []string`

- [ ] **Step 1: Write the failing tests**
  - **`TestFromFlagsDefaults`:** `Flags{Storages: "pbs-main"}` with `defaultAddr "127.0.0.1"` and the deb layout gives:
    - `Address "127.0.0.1"`, `VMIDRange {990000, 990049}`, `Bridge "vmbr99"`, `RestoreStorage "local-zfs"`;
    - `PBSStorages ["pbs-main"]`, `Schedule "0 2 * * *"`, `TZ ""`.
  - **`TestFromFlagsRequired`:**
    - `Flags{}` gives an error containing `--storages is required`;
    - with `defaultAddr ""` and no `Address`, an error containing `--address is required`.
  - **`TestFromFlagsInvalid`:** each bad value gives an error naming its flag and the same rule text as the interactive message:
    - `Address "https://x"`;
    - `VMIDRange "5-1"`;
    - `Bridge "-x"`;
    - `RestoreStorage "local-"`;
    - `Storages "pbs,1x"`;
    - `Schedule "nope"`;
    - `TZ "Local"` (Docker layout).
  - **`TestConflicts`:**
    - a config with bridge `vmbr99` and `Flags{Bridge: "vmbr77"}` gives `["--bridge vmbr77 differs from target.bridge vmbr99 in config.yaml"]`;
    - equal flags and empty flags give nil;
    - every flag except `TZ` is compared.

- [ ] **Step 2: Run them to make sure they fail**
  Run: `scratchpad/rt go test ./internal/hostsetup/`
  Expected: build failure.

- [ ] **Step 3: Implement.**
  - Extract `checkBridge`, `checkSchedule` and `checkTZ` beside the existing `validAddress`, `parseRange`, `validStorage` and `parseStorages`.
  - `AskWith` calls the same functions. `TestAskReasks` and its siblings must still pass unchanged.

- [ ] **Step 4: Run them to make sure they pass**
  Run: `scratchpad/rt go test ./internal/hostsetup/`
  Expected: `ok`.

- [ ] **Step 5: Commit**
  `git commit -S -m "hostsetup: answers from flags through the interactive checks; flag/config conflicts (#131)"`

### Task 5: the node gate, `pbp node setup --unattended --reply-file`, and `pbp node uninstall --unattended`

**Files:**
- Modify: `cmd/pbp/nodesetup.go`:
  - `nodeGateReason(allowNoTTY bool)`;
  - the gate moves after flag parsing in `nodeSetup` and `nodeUninstall`;
  - the new flags.
- Test: `cmd/pbp/nodesetup_test.go`

**Interfaces:**
- **Produces:**
  - `func (a *app) nodeGateReason(allowNoTTY bool) string`
  - `func (a *app) setupGate(sub string, allowNoTTY bool) bool`
  - `func writeReplyFile(path string, rep nodeproto.SetupReply) error`, which uses `nodeproto.EncodeReply` and opens with `O_CREATE|O_EXCL|O_WRONLY|O_NOFOLLOW`, mode 0600.

- [ ] **Step 1: Write the failing tests** (on `newSetupRig`)
  - **`TestNodeSetupUnattendedNoTTY`:** with `isTerminal` false, `setupArgs("--yes", "--unattended", "--reply-file", p)` exits 0.
    - `p` has mode 0600, and `nodeproto.ReadReply(p)` gives a reply with a non-empty `TokenSecret`.
    - stdout does not contain the secret.
    - stdin `"n\n"` was not read: the result is the same as with empty stdin.
  - **`TestNodeGateUnattendedKeepsOtherRefusals`:** `TestNodeSetupGateRefuses`'s cases for SSH, sudo-from-pbp and not-root, now also with `--yes --unattended`, still exit `exitUsage` and leave the snapshot unchanged.
  - **`TestNodeSetupUnattendedNeedsYes`:** `--unattended` without `--yes` exits `exitUsage`, with `--unattended requires --yes`.
  - **`TestNodeSetupReplyFileRefusesExisting`:** an existing file, or a symlink to `/dev/null`, makes the run exit 1 with nothing written to the target. *Ruling for the executor:* do the check before applying, so that the node is not changed.
  - **`TestNodeUninstallUnattended`:** non-terminal stdin with `--yes --unattended --remove-token` exits 0 and removes the items.

- [ ] **Step 2: Run them to make sure they fail**
  Run: `scratchpad/rt go test ./cmd/pbp/ -run 'Unattended|ReplyFile|GateRefuses'`
  Expected: FAIL, unknown flag `-unattended`.

- [ ] **Step 3: Implement.**
  - `nodeRun` gets `noStdin bool`: under `--unattended`, nothing reads `o.in`, because `--yes` already answers the only prompt.
  - `offerLocalNode` calls `nodeGateReason(false)` for now; Task 6 passes its flag through.

- [ ] **Step 4: Run them to make sure they pass**
  Run: `scratchpad/rt go test ./cmd/pbp/`
  Expected: `ok`. The existing gate tests still pass.

- [ ] **Step 5: Commit**
  `git commit -S -m "pbp node setup/uninstall --unattended: no terminal needed, other refusals kept; --reply-file 0600 (#131)"`

### Task 6: `pbp setup --unattended`, `--node-reply` and exit code 4

**Files:**
- Modify: `cmd/pbp/setup.go`:
  - the flags;
  - `askAndWrite` takes the answers from `hostsetup.FromFlags` when unattended;
  - `offerLocalNode(onNode, in, unattended)`;
  - `setUpLocalNode`, which never calls `Confirm` when unattended: rotation only with `--rotate-token`, else exit 1 with the existing message plus `pass --rotate-token to replace it`;
  - the separate-host branch: print `hostsetup.NodeLines` alone and return `exitWaitReply`, or read `--node-reply`.
- Modify: `cmd/pbp/main.go` (`exitWaitReply = 4`), `cmd/pbp/setup.go` `setupUsage`.
- Test: `cmd/pbp/setup_unattended_test.go`

**Interfaces:**
- **Consumes:** `hostsetup.Flags`, `FromFlags` and `Conflicts` (Task 4); `nodeGateReason(true)` (Task 5).
- **Produces:**
  - `const exitWaitReply = 4`
  - the flags `--unattended`, `--address`, `--vmid-range`, `--bridge`, `--restore-storage`, `--storages`, `--schedule`, `--tz`, `--node-reply`, `--rotate-token`.

- [ ] **Step 1: Write the failing tests**
  - **`TestSetupUnattendedOnNode`** (`newLocalRig`, `isTerminal` false, stdin `"n\nn\n"`):
    - `setup --deb --unattended --storages pbs1` exits 0;
    - the config holds `127.0.0.1` and the defaults;
    - the token file is written;
    - stdout never contains `Set up this node here?` or `[y/N]`.
  - **`TestSetupUnattendedSeparateHost`** (`newSetupEnv`):
    - `--unattended --address pve.example.lan --storages pbs1` exits 4. Its stdout is exactly `hostsetup.NodeLines(cfg, pub, version, false)` plus a newline (the install line and the `pbp node setup` line), with nothing before or after.
    - Then, with the same flags plus `--node-reply <file with e.reply(nil)>`, it exits 0 and the check passes.
    - With `--node-reply -` and the reply on stdin, it also exits 0.
  - **`TestSetupUnattendedMissing`:** with no `--storages`, it exits 1 with `--storages is required`, and no `config.yaml` is written.
  - **`TestSetupUnattendedConflict`:** with an existing config and `--bridge vmbr77`, it exits 1 naming both values.
  - **`TestSetupUnattendedRotation`:** with the token existing and no secret on the host:
    - without `--rotate-token`, it exits 1 with `pass --rotate-token`;
    - with it, it exits 0.
  - **`TestSetupFlagsNeedUnattended`:** `--storages pbs1` without `--unattended` exits `exitUsage` with `answer flags need --unattended`. *My call*, so an interactive run never half-uses flags.

- [ ] **Step 2: Run them to make sure they fail**
  Run: `scratchpad/rt go test ./cmd/pbp/ -run 'SetupUnattended|SetupFlags'`
  Expected: FAIL.

- [ ] **Step 3: Implement**, as in the Files list.

- [ ] **Step 4: Run them to make sure they pass**
  Run: `scratchpad/rt go test -race ./...`
  Expected: all `ok`.

- [ ] **Step 5: Commit**
  `git commit -S -m "pbp setup --unattended: answers from flags, on-node end to end, exit 4 and --node-reply for a separate host (#131)"`

### Task 7: docs and CHANGELOG

**Files:**
- Create: `docs/install-apt.md`
- Modify:
  - `docs/install-deb.md`: point to APT;
  - `README.md`: trial step 1 offers APT, Getting started links the unattended setup, Documentation table row;
  - `docs/setup.md`: a new "Unattended setup" section with both flows and the example from the spec;
  - `docs/operating.md`: exit code 4;
  - `docs/security.md`: the APT key and the unattended gate;
  - `docs/troubleshooting.md`: rows for `--storages is required`, the conflict message and `pass --rotate-token`;
  - `CHANGELOG.md`: `## Unreleased`.

- [ ] **Step 1: Write the docs.** `docs/install-apt.md` covers:
  - adding the repository, using the spec's "User side" block verbatim;
  - checking the key: the `ssh-keygen -Y verify` of `pbp-archive-keyring.gpg.sig`, and the fingerprint, filled in after `--init`;
  - upgrading the node first, then the host, and why not to add the origin to `unattended-upgrades`;
  - pinning (`apt install pbp=<version>`, `apt-mark hold pbp`);
  - removing the repository.

- [ ] **Step 2: Check them.** Every command in the docs matches the implemented flags (`grep` each flag name in `cmd/pbp/setup.go` and `nodesetup.go`), and every relative link resolves (`ls` each path; check each anchor against a heading).
  Run: a small loop over `grep -o '](docs/[^)#]*' …`.
  Expected: no missing files.

- [ ] **Step 3: Commit**
  `git commit -S -m "Docs: APT install, unattended setup, exit code 4, the APT key in the security model (#130, #131)"`

### Task 8: live verification (owner's approval and window on the test node)

- [ ] **Step 1: Separate-host flow.** A Docker-layout `pbp setup --unattended` in a temp directory on the dev test host, against the test node, with a **separate** VMID range and without `--rotate-token`.
  - The node side runs `pbp node setup --yes --unattended --reply-file` in a root SSH session; that is an admin's own session, not a forced command.
  - *Ruling for the executor:* this rotates nothing, because the token's secret is new for the temp host only if the token does not exist. On a node where it exists, stop at the rotation message and report it, rather than rotating the production daemon's token.
- [ ] **Step 2: On-node flow.** Only if the owner provides a node without PBP (for example the second test node) or approves an uninstall and reinstall window. Otherwise record it as not done.
- [ ] **Step 3: APT, after the owner's one-time setup.**
  1. `packaging/apt-publish.sh --init`, then `packaging/apt-publish.sh --first v0.10.0`.
  2. Run `apt-get update && apt-get download pbp=0.10.0` with isolated dirs on the dev test host, and check the hash.
  3. Install on the test node with `apt install pbp=0.10.0`, which is the version already installed, so nothing changes, and confirm that `apt` sees the origin.
- [ ] **Step 4:** Record the results in the PR description.
