# Contributing

Thanks for your interest in PBP.

If you would like to contribute, just let us know and you will be added to the Gitea source repo if it's deemed appropriate.

## Where development happens

PBP is developed on a private Gitea instance. This GitHub repository is a **read-only mirror**: every push from Gitea overwrites it, and nothing done here flows back on its own.

- **Issues are welcome here**: bugs, questions and ideas. Say which version (`pbp version`), where PBP runs (Docker or `.deb`, PBP host or the node itself), and include the relevant lines of the run's `run.log` and the guest's own log file ([Logs](docs/operating.md#logs)). Remove hostnames, addresses and anything else you would rather not publish.
- **Security problems go through private vulnerability reporting**, not issues: see [SECURITY.md](SECURITY.md).
- **Pull requests cannot be merged here**, because the next mirror push would undo the merge. Open one anyway if a change is clearest as code. If it is accepted, the maintainer applies it on Gitea, crediting you as co-author, and it reaches this mirror with the next push; the pull request is then closed. For anything larger than a small fix, open an issue first, so the design can be agreed before you write it.

## Before you send a change

- **Tests:**
  - `go vet ./...` and `go test -race ./...` pass;
  - `golangci-lint run ./...` reports nothing (see [Development](docs/development.md) for the pinned linters and scanners);
  - a change that touches the node helper, `packaging/` or the SSH options also needs the node end-to-end test ([Tests](docs/development.md#tests)).
- **Docs:** a change in behaviour updates the docs in `docs/`, and gets a line in `CHANGELOG.md` under `Unreleased`.
- **Scope:** PBP touches the node only inside its VMID range and only guests tagged `pbp` ([Security model](docs/security.md)). A change that widens what it may touch needs a good reason, stated in the issue.

Contributions are licensed under the [Apache License 2.0](LICENSE), like the rest of PBP. Commits by the maintainer are signed; yours do not have to be.

## Workflows

The CI workflows live in `.gitea/workflows/` and run on Gitea only; this mirror runs no Actions. See [.gitea/README.md](.gitea/README.md).
