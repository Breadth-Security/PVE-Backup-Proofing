# Development

## Building

```
docker build -t pbp:latest .            # multi-stage; static binary on distroless, version from VERSION
docker build --build-arg VERSION=$(git describe --tags --always) -t pbp:latest .   # stamp another version
docker run --rm pbp:latest version      # prints the stamped version (the VERSION file, unless overridden)
go test ./... && go vet ./...
packaging/build.sh 1.2.3 dist           # .debs, tarballs and SHA256SUMS for amd64 and arm64
```

The top-level `VERSION` file (`MAJOR.MINOR.PATCH`) is the version a Docker build uses when no `--build-arg VERSION` is given. Bump it before tagging a release: the `release` workflow fails a tag `v<version>` whose `MAJOR.MINOR.PATCH` differs from it (`v0.3.0-rc.1` needs `0.3.0`). A plain `go build` without `-ldflags "-X main.version=..."` is a `dev` build.

`packaging/build.sh` needs Go, `envsubst` (gettext-base) and network access for the pinned nfpm. The `release` workflow runs it when a tag `v<version>` is pushed, runs lintian on the packages (errors fail the job) and creates the Gitea release with the packages, tarballs and `SHA256SUMS` attached. Run it by hand (workflow dispatch) for a dry run that builds but publishes nothing. It authenticates with the job's automatic token; if your Gitea refuses that (HTTP 401/403), create a token with the `write:repository` scope and store it as the repository secret `RELEASE_TOKEN`, which is used in preference when set.

## Releasing

1. Bump `VERSION`, move the CHANGELOG's `Unreleased` section to `v<version>`, and set the install examples in the docs to the new version (in the release's PR).
2. After the merge, on `main`: `git tag -a v<version> -m "PBP v<version>"` and `git push GiteaMain v<version>`. With `tag.gpgsign` set, the tag is signed with your SSH signing key; check it with `git tag -v v<version>`.
3. Wait for the `release` workflow to publish the release.
4. **Sign it at once**, on your machine: `packaging/sign-release.sh v<version>`. It downloads every file of the release, checks them against `SHA256SUMS`, signs `SHA256SUMS` (namespace `pbp-release`), uploads `SHA256SUMS.sig` and checks that the published signature verifies with [`allowed_signers`](../allowed_signers). Until it has run, the node install line that `pbp setup` prints fails (it requires the signature), so do not leave a release unsigned. `--dry-run` checks the files without signing; `--all` covers every release, skipping (and re-verifying) those already signed; `--replace` re-signs.

The signing key never goes to CI: it is the maintainer's SSH key `~/.ssh/pbp_signing` (`PBP_SIGNING_KEY` overrides the path), loaded into `ssh-agent` or unlocked with its passphrase when the script asks. Its public half is in `allowed_signers` and built into PBP (`internal/hostsetup.ReleaseSigner`; a test keeps the two equal). Commits and tags are signed with the same key under `gpg.format ssh`; merges made in Gitea's web UI are not (accepted: the signed tags and `SHA256SUMS.sig` carry the release's provenance).

**Changing the key** (rotation, or after a compromise): generate a new key, replace both lines in `allowed_signers` and `ReleaseSigner`, register it on Gitea (and GitHub) as a signing key, re-sign the releases with `packaging/sign-release.sh --replace --all`, and say so in the CHANGELOG and the release notes with both fingerprints. A PBP built before the change verifies only signatures by the old key, so its printed install line fails for releases signed by the new one: install the new release by hand once, verifying with the new `allowed_signers`. Keep an offline backup of the private key and its passphrase.

## Tests

- **Unit tests:** `go test -race ./...` (and `go vet` with the `integration` and `nodee2e` tags, which CI also runs).
- **Node end-to-end test** (`test/nodee2e`): sets up a throwaway Debian container as a fake Proxmox node with the real `.deb`, `pbp setup` and `pbp node setup`, then attacks it over real SSH with PBP's key. It needs a Docker host with Go: `test/nodee2e/run.sh [ssh-host]` copies the checkout there and runs it. Never point it at a Proxmox node.
- **Integration suite** (`test/integration`): proofs real backups on a real PVE node. See [`test/integration/README.md`](../test/integration/README.md) for what it needs and what it touches.

CI compiles and vets both suites but runs neither: one needs Docker, the other a real node. They are the only tests of the forced command, the sudo rule and real PVE behaviour, so run the node end-to-end test before merging anything that touches `internal/node`, `internal/nodesetup`, `packaging/` or the SSH options, and the integration suite before a release.

## Code scanning

Workflows in [`.gitea/workflows/`](../.gitea/workflows/) check every pull request and every push to `main`, and each **blocks** the pull request when it fails:

- **go-toolchain**: `go vet` (with no build tag, and with the `integration` and `nodee2e` tags) and `go test -race`. It first checks that it runs on exactly the Go `toolchain` pinned in `go.mod`, and that the Dockerfile builds with the same version.
- **Scanners**: Trivy, govulncheck, gosec, staticcheck and golangci-lint check the code and dependencies. gitleaks checks the **whole git history** for committed secrets; Trivy only sees the current files. The scanners also run weekly (Monday 06:17 UTC), so newly published vulnerabilities are caught in code that hasn't changed.

The Docker build runs `go vet` and `go test -race` too, so an image can't be built from code that fails them. When you bump Go, change the `toolchain` line in `go.mod` and the `FROM golang:` tag in the `Dockerfile` together; the go-toolchain workflow fails if they differ. To block merging in Gitea, mark these workflows as required status checks in the branch protection for `main`.

Tool versions are pinned in the workflow files. To run them locally from the repository root:

```
# govulncheck
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

# staticcheck
go run honnef.co/go/tools/cmd/staticcheck@2026.2.2 ./...
go run honnef.co/go/tools/cmd/staticcheck@2026.2.2 -tags integration ./...
go run honnef.co/go/tools/cmd/staticcheck@2026.2.2 -tags nodee2e ./...

# gosec
# gosec v2.29.0 needs a Go 1.26 toolchain to analyse the code
GOTOOLCHAIN=go1.26.10 go run github.com/securego/gosec/v2/cmd/gosec@v2.29.0 ./...

# golangci-lint (config: .golangci.yml)
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run --build-tags integration ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run --build-tags nodee2e ./...

# Trivy (vulnerabilities, secrets, misconfigurations)
docker run --rm -v "$PWD":/src:ro -w /src aquasec/trivy:0.76.1 fs --exit-code 1 --ignore-unfixed \
  --scanners vuln,secret,misconfig --severity UNKNOWN,LOW,MEDIUM,HIGH,CRITICAL \
  --skip-dirs .superpowers --ignorefile .trivyignore.yaml .

# gitleaks (full git history)
docker run --rm -v "$PWD":/repo:ro ghcr.io/gitleaks/gitleaks:v8.30.1 git --redact /repo

# vet and race tests
go vet ./... && go vet -tags integration ./... && go vet -tags nodee2e ./... && go test -race ./...
```

An accepted gosec finding carries a `// #nosec <rule> -- <reason>` comment at the call site. An accepted gitleaks finding (a fake secret in a test) is allowlisted by value in `.gitleaks.toml`, and an accepted Trivy finding in `.trivyignore.yaml`.
