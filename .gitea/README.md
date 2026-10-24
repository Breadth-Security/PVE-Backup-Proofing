# Gitea workflows

The workflows in `workflows/` belong to PBP's source repository on its Gitea instance (`git.breadthanalytics.com`), where development, pull requests and releases happen. They run on that instance's Actions runners.

**Do not use them on the GitHub mirror** ([Breadth-Security/Proxmox-Backup-Proofing](https://github.com/Breadth-Security/Proxmox-Backup-Proofing)). Gitea pushes to the mirror; the mirror does not push back. In particular:

- **Do not copy them to `.github/workflows/`.** GitHub Actions runs only workflows in `.github/workflows/` and ignores this folder; copied there, they would run against the mirror.
- **`release.yml` in particular** talks to the Gitea API (`RELEASE_TOKEN`, `/api/v1/...`) and creates draft releases on Gitea. Releases reach GitHub only through `packaging/mirror-release.sh`, after they are signed on Gitea; see [Releasing](../docs/development.md#releasing).
- **The scanners** (gitleaks, golangci-lint, gosec, govulncheck, staticcheck, Trivy, go-toolchain) already run on every pull request and push on Gitea. Running them again on the mirror would only check the same commits twice.

Change CI here, on Gitea.

## GitHub workflows

`.github/workflows/` holds the workflows that run on the mirror's GitHub Actions (today the OSSF SLSA Go releaser, `go-ossf-slsa3-publish.yml`, with `.slsa-goreleaser.yml`). They are changed on GitHub, by pull request there, and synced back to Gitea by hand: fetch the mirror's `main` and open a Gitea pull request that fast-forwards Gitea's `main` to it (as [#87](https://git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/pulls/87) did). Sync them back before anything else lands on Gitea's `main`, or the next push from Gitea will overwrite them on GitHub.
