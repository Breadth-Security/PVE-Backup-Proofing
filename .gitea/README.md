# Gitea workflows

The workflows in `workflows/` belong to PBP's source repository on its Gitea instance (`git.breadthanalytics.com`), where development, pull requests and releases happen. They run on that instance's Actions runners.

**Do not use them on the GitHub mirror** ([Breadth-Security/Proxmox-Backup-Proofing](https://github.com/Breadth-Security/Proxmox-Backup-Proofing)). The mirror is a one-way push mirror from Gitea, so anything changed on GitHub is overwritten by the next sync. In particular:

- **Do not copy them to `.github/workflows/`.** GitHub Actions runs only workflows in `.github/workflows/` and ignores this folder, so the mirror runs nothing today. Copied there, they would run against the mirror.
- **`release.yml` in particular** talks to the Gitea API (`RELEASE_TOKEN`, `/api/v1/...`) and creates draft releases on Gitea. Releases reach GitHub only through `packaging/mirror-release.sh`, after they are signed on Gitea; see [Releasing](../docs/development.md#releasing).
- **The scanners** (gitleaks, golangci-lint, gosec, govulncheck, staticcheck, Trivy, go-toolchain) already run on every pull request and push on Gitea. Running them again on the mirror would only check the same commits twice.

Change CI here, on Gitea.
