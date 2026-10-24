# Gitea workflows

The workflows in `workflows/` belong to PBP's source repository on its Gitea instance (`git.breadthanalytics.com`), where development, pull requests and releases happen. They run on that instance's Actions runners.

**Do not use them on the GitHub mirror** ([Breadth-Security/Proxmox-Backup-Proofing](https://github.com/Breadth-Security/Proxmox-Backup-Proofing)). Gitea pushes to the mirror; the mirror does not push back. In particular:

- **Do not copy them to `.github/workflows/`.** GitHub Actions runs only workflows in `.github/workflows/` and ignores this folder; copied there, they would run against the mirror.
- **`release.yml` in particular** talks to the Gitea API (`RELEASE_TOKEN`, `/api/v1/...`) and creates draft releases on Gitea. Releases reach GitHub only through `packaging/mirror-release.sh`, after they are signed on Gitea; see [Releasing](../docs/development.md#releasing).
- **The scanners** (gitleaks, golangci-lint, gosec, govulncheck, staticcheck, Trivy, go-toolchain) already run on every pull request and push on Gitea. Running them again on the mirror would only check the same commits twice.

Change CI here, on Gitea.

## GitHub workflows

`.github/workflows/` holds the workflows that run on the mirror's GitHub Actions: today the OSSF SLSA Go releaser, `go-ossf-slsa3-publish.yml`, with its build configuration in `.slsa-goreleaser-*.yml` at the repository root. Gitea Actions does not run them: it uses `.gitea/workflows/` instead whenever that folder exists.

**Change them here, on Gitea**, like any other file, and let the push mirror carry them to GitHub. Do not merge anything on GitHub: the mirror force-pushes Gitea's branches on every sync, so a change made only there is lost. The mirror's GitHub token needs permission to change workflows (classic token: the `workflow` scope; fine-grained: Workflows read and write); without it GitHub rejects every push that touches `.github/workflows/`, and the mirror stops syncing.

To try a change before merging it, push its branch to Gitea, let the mirror sync it, and run the workflow on that branch on GitHub:

```sh
gh workflow run go-ossf-slsa3-publish.yml -R Breadth-Security/Proxmox-Backup-Proofing --ref <branch>
gh run watch -R Breadth-Security/Proxmox-Backup-Proofing
```

The run uses the branch's version of the workflow, but GitHub dispatches only a workflow that is already on `main`, so a brand-new workflow has to land there first.
