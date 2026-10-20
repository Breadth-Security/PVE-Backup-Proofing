# Operating

## Everyday commands

On a Docker host, in your checkout:

```
docker compose up -d                              # start the daemon (scheduled runs)
docker compose exec pbp pbp check                 # preflight only; nothing is restored
docker compose exec pbp pbp run                   # one run now, all guests
docker compose exec pbp pbp run --guest 105       # one run now, only guest 105's newest backup
docker compose exec pbp pbp health                # what the Docker healthcheck runs
docker compose exec pbp pbp version               # build version and Go runtime
docker compose logs -f pbp                        # logs; per-run files are in ./logs
```

On a `.deb` host (see [Install from the .deb](install-deb.md)):

```
sudo systemctl start pbp                                          # start the daemon (scheduled runs)
sudo -u pbp PBP_CONFIG=/etc/pbp/config.yaml pbp check             # preflight only; nothing is restored
sudo -u pbp PBP_CONFIG=/etc/pbp/config.yaml pbp run               # one run now, all guests
sudo -u pbp PBP_CONFIG=/etc/pbp/config.yaml pbp run --guest 105   # one run now, only guest 105's newest backup
sudo -u pbp PBP_CONFIG=/etc/pbp/config.yaml pbp health            # the same health check as the Docker image's
pbp version                                                       # build version and Go runtime
journalctl -u pbp -f                                              # logs; per-run files are in /var/log/pbp
```

Run these as `pbp`, the user the service runs as: it can read `/etc/pbp`, and the state directory `/var/lib/pbp/state` is its own. Run as root, they would leave root-owned state files the daemon cannot write. The per-run files in `/var/log/pbp` are readable by root and `pbp` (for example `sudo less /var/log/pbp/run-*.log`).

`run --guest` proofs one guest's newest backup across the configured storages, VM or container alike, and sends the usual notification and backup note. Like a scheduled run, it skips a backup that already carries a `PBP PROVEN` line ("already proven") unless `restore.reproof: true` is set in `config.yaml`.

## Exit codes

| Exit code | Meaning |
|-----------|---------|
| 0 | Success (`check` passed, or the run finished without error) |
| 1 | Failure: bad config, `check` found a problem, the run ended in `ERROR`, or `run --guest` found no backup |
| 2 | Usage error (unknown subcommand or flag, or `--guest` that is not a positive integer) |
| 3 | Busy: another run or check holds the run lock. `run` and `check` both exit 3 and print "a run is already in progress" |
| 78 | `daemon` only: the config is missing or unusable (for example a `config.yaml` that `pbp setup` has not finished). `pbp.service` does not restart on it; fix the config (run `pbp setup` again) and start the service |

Subcommands take `--config PATH`. The config path otherwise defaults to `$PBP_CONFIG`, then `/etc/pbp/config.yaml`. The state directory is `/var/lib/pbp/state`, overridable with `PBP_STATE_DIR`. Errors and what to do about them: [Troubleshooting](troubleshooting.md).

## Health check

`pbp health` reports a problem when the last run ended in a run-level error (for example the node or SSH is unreachable, so no notification could be sent), when a scheduled run did not start, or when the current run has overrun. It prints `ok` and exits 0 when all is well, and otherwise prints the reason and exits 1. This is the backstop for a silent night when the node itself cannot notify you.

- **Docker:** the image's `HEALTHCHECK` runs it, so the container reports **unhealthy**. Watch that with your usual container monitoring.
- **`.deb` host:** run `sudo -u pbp PBP_CONFIG=/etc/pbp/config.yaml pbp health` from your monitoring (a check script, or a systemd timer that alerts on failure).

## Notifications

One summary per run is sent from the PVE node (through the `pbp node` helper over SSH, using `PVE::Notify`), so it goes to whatever notification targets and matchers you already use. Severity is `info` when everything is `PROVEN` (or skipped as already proven), `warning` for any `BOOTED`, `ERROR` or other `SKIPPED`, and `error` for any `FAILED` or a run-level failure.

To route it, create a matcher in Datacenter > Notifications > Notification Matchers with a **Match field** rule: field `type`, value `pbp`. A `hostname` field is also available.

You install the templates (`pbp-subject.txt.hbs`, `pbp-body.txt.hbs`) into `/etc/pve/notification-templates/default/` on the node with `pbp node setup`, or by hand with `pbp node install-templates` ([Node setup](manual-setup.md#node-setup), step 6); preflight only checks them and says to run that command if they are missing or differ from the ones in your PBP version, so run it again after upgrading PBP. PVE 9 picks up this new template type from there (verified on PVE 9.2), and `/etc/pve` survives package upgrades, so a PVE upgrade does not remove them.

If a target cannot be notified (for example a template fails to render), `PVE::Notify` only logs an `ERROR:` line; PBP treats that as a failed notification, so the run's outcome is `error` and the health check reports it. `PVE::Notify` cannot say which targets succeeded, so this is deliberately conservative: a failure of any one target marks the run's notification as failed, even if others delivered.

`notify.when` is `always` by default, so a missing nightly message is itself a signal; set it to `problems` for quiet success.

## Backup notes

PBP owns exactly one line in each backup's notes, placed first so it shows as the backup's comment in the Proxmox UI:

```
PBP <VERDICT> <YYYY-MM-DD HH:MM> · <short reason>
PBP PROVEN 2026-10-19 02:14 · agent ok, 3 profiles ok
PBP FAILED 2026-10-19 02:31 · failed: postgresql.service
```

Your own notes are preserved below it. A previous PBP line is replaced, never stacked. PBP only ever modifies the `notes` and `protected` attributes of a backup.
