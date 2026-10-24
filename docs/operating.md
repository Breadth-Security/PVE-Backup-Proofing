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
docker compose logs -f pbp                        # logs; each run's folder is in ./logs (see Logs)
```

On a `.deb` host (see [Install from the .deb](install-deb.md)):

```
sudo systemctl start pbp                                          # start the daemon (scheduled runs)
sudo -u pbp PBP_CONFIG=/etc/pbp/config.yaml pbp check             # preflight only; nothing is restored
sudo -u pbp PBP_CONFIG=/etc/pbp/config.yaml pbp run               # one run now, all guests
sudo -u pbp PBP_CONFIG=/etc/pbp/config.yaml pbp run --guest 105   # one run now, only guest 105's newest backup
sudo -u pbp PBP_CONFIG=/etc/pbp/config.yaml pbp health            # the same health check as the Docker image's
pbp version                                                       # build version and Go runtime
journalctl -u pbp -f                                              # logs; each run's folder is in /var/log/pbp (see Logs)
```

Run these as `pbp`, the user the service runs as: it can read `/etc/pbp`, and the state directory `/var/lib/pbp/state` is its own. As root, `pbp check`, `pbp run` and `pbp daemon` refuse and say so (`refusing to run as root: /var/lib/pbp/state belongs to pbp …`, with the command to run instead), because files root writes there or in `/var/log/pbp` would lock the service out of them; `pbp health` and `pbp version` only read, and work either way. The run folders in `/var/log/pbp` are readable by root and `pbp` (for example `sudo less /var/log/pbp/*/run.log`).

`run --guest` proofs one guest's newest backup across the configured storages, VM or container alike, and sends the usual notification and backup note. Like a scheduled run, it skips a backup that already carries a `PBP PROVEN` line ("already proven") unless `restore.reproof: true` is set in `config.yaml`. A backup of a template is always skipped ("template (not bootable)").

A run on demand that is interrupted (Ctrl-C, or its SSH session dropping) still tears its guest down and records the result. Under Docker, it runs inside the PBP container through `docker compose exec`, so it ends abruptly if that container is stopped or restarted meanwhile: let it finish first. Anything it leaves behind is cleaned up by the next run's janitor.

## Logs

Each run writes its own folder under `logging.dir` (`/var/log/pbp` on a `.deb` host, `./logs` with Docker), named with the time the run started and what started it (`schedule` or `manual`):

```
/var/log/pbp/
  2026-10-21_02-00-00_schedule/
    run.log
    VM102.log
    CT101.log
```

`run.log` is the run as a whole: preflight, discovery, one line per guest result and a closing summary. Each guest PBP works on also gets its own file, `VM<vmid>.log` or `CT<vmid>.log`, with one line per step: restore, start, checks, teardown. **When a guest failed, start with its own file.** When a PVE task fails (restore, start), the file holds the tail of the task's log; the reason in `run.log` and the notification is just the task's error line.

```
2026-10-21 02:00:00 EDT run started (schedule) · PBP 0.8.2 · node pve1
02:00:02 VM102 started (VM102.log)
02:01:31 VM102 FAILED · failed: sssd-nss.socket, sssd-pam.socket
03:33:54 run finished in 1h33m · ❌ severity error
03:33:54 ✅ 13 proven · ☑️ 6 previously proven · ❌ 9 failed · ⚠️ 9 error · ⚠️ 4 booted · ⏭️ 3 skipped
```

The markers are the same in the notification: ✅ proven, ☑️ previously proven (skipped because an earlier run proved the backup), ❌ `FAILED` (the backup has a problem), ⚠️ `ERROR` or `BOOTED` (not proven either way), ⏭️ skipped for any other reason. The notification's subject leads with the run's severity (✅, ⚠️ or ❌) and its body with the counts line.

```
02:00:36 restore done in 33s
02:00:37 started; waiting for the guest (up to 5m0s)
02:01:21 running after 44s (Rocky Linux)
02:01:26 verdict FAILED · failed: sssd-nss.socket, sssd-pam.socket
```

Only warnings and errors carry a level; a problem PBP itself has (not the guest) is also written to `run.log`, with the guest's label in front. Times are in the time zone of the host (or container; see [Schedule and time zone](#schedule-and-time-zone)). A file's first line carries the date and zone, other lines just the time. The service log (`journalctl -u pbp`, `docker compose logs`) stays short: the run starting, the preflight and discovery lines, one line per guest result and the summary.

`logging.retain_runs` (default 30) is the number of run folders kept; older ones are removed after each run. Single `run-*.log` files from earlier versions count toward the same limit until they age out. Anything else in `logging.dir` is left alone. If `logging.dir` cannot be written (for example a Docker bind mount with the wrong owner), the run still runs and notifies, logging to the service log only, and the notification has no `Logs:` line.

The notification names the run's folder (`Logs: /var/log/pbp/2026-10-21_02-00-00_schedule/`). With Docker that is the path inside the container, which `docker-compose.yml` maps to `./logs` on the host.

## Excluding guests

PBP proofs the newest backup of every guest it finds. To leave guests out, list them under `selection.exclude` in `config.yaml`:

```yaml
selection:
  exclude:
    guests: [104, CT109, VM5000, 900-999]
    tags: [no-proof]
```

- **`guests`:** a VMID (`104`, a VM or a container with that number), a label for one kind (`VM104`, `CT109`), or an inclusive range (`900-999`).
- **`tags`:** a guest is excluded if its live config or its backed-up config carries one of these tags (case does not matter). Tagging a guest in the Proxmox UI takes effect at the next run; a guest that no longer exists is judged by the tags in its backup.

An excluded guest is never restored, nothing is reserved for it, and its backup's notes are left alone. It shows in the run summary as `SKIPPED · excluded by config (<rule>)`, for example `excluded by config (tag no-proof)`, and it does not raise the run's severity. `pbp run --guest N` refuses an excluded guest, names the rule and exits 1: edit the config to proof it. A malformed entry stops PBP from loading the config, naming the entry (the daemon exits 78; `run`, `check` and `health` exit 1).

The daemon reads `config.yaml` when it starts, so restart it after editing (`sudo systemctl restart pbp`, or `docker compose restart pbp`); `pbp run --guest` reads the file each time.

## Schedule and time zone

### The schedule

`schedule` in `config.yaml` says when the daemon starts a run; `pbp setup` asks for it, with `0 2 * * *` (02:00 every day) as the default. It is a standard cron expression: five fields, separated by spaces.

```
┌───────────── minute        0-59
│ ┌─────────── hour          0-23
│ │ ┌───────── day of month  1-31
│ │ │ ┌─────── month         1-12 or JAN-DEC
│ │ │ │ ┌───── day of week   0-6 (Sunday is 0) or SUN-SAT
│ │ │ │ │
0 2 * * *
```

Each field takes `*` (every value), a number, a list (`1,15`), a range (`1-5`) or a step (`*/6`, every 6). A few examples:

| Schedule | Runs |
|----------|------|
| `0 2 * * *` | every day at 02:00 (the default) |
| `30 3 * * 1-5` | Monday to Friday at 03:30 |
| `0 4 * * SUN` | every Sunday at 04:00 |
| `0 3 1 * *` | at 03:00 on the first day of each month |
| `15 1 * * MON,THU` | Monday and Thursday at 01:15 |
| `0 */6 * * *` | every six hours, on the hour |
| `@daily` | every day at midnight (also `@weekly`, `@monthly`, `@hourly`) |

[crontab.guru](https://crontab.guru) explains any five-field expression in plain words and is handy for trying one out. PBP's cron does not take a seconds field (six fields are refused) or the `L`, `W` and `#` extensions some cron implementations offer. `pbp setup`, `pbp check` and the daemon all refuse an invalid schedule and say why.

A run takes as long as the proofs it has to do, and the next one starts only when that one has finished: a scheduled run that finds the previous one still going is skipped. One that finds the lock held by a `pbp check` or `pbp run` you started waits for it, checking every minute for up to 15 minutes, and is skipped only if it is still held then. `restore.max_run_duration` stops a run from starting new proofs once it has run that long: the guests still waiting are `SKIPPED · run deadline`. It does not cut short a proof in progress, which still finishes its restore, boot and checks (and a large restore is not bounded by any timeout), so a run can end well after the limit. `pbp health` reports a run as overrunning only after `max_run_duration` plus `restore.boot_timeout`, `restore.probe_timeout` and 10 minutes.

### The time zone

The time zone decides when the schedule fires. It also sets the time in each backup's note (`db01 · PBP PROVEN 2026-10-19 02:14 · …`, shown without a zone) and in the daemon's log. Run IDs (`20261019T020000.000Z-schedule`) are always in UTC.

Where PBP gets it:

- **Docker:** the `TZ` in `.env` next to `docker-compose.yml`. `pbp setup` asks for it and writes it. Without it, the compose file falls back to `Europe/London`.
- **`.deb` host:** the host's own time zone (`timedatectl` shows it). PBP has no setting of its own for it.

**Use a name from the tz database, in the form `Area/City`**, or `UTC`:

| Zone | Standard time | Daylight saving time |
|------|---------------|----------------------|
| `Etc/UTC` | UTC | none |
| `Europe/London` | UTC+0 | UTC+1 |
| `Europe/Paris`, `Europe/Berlin`, `Europe/Amsterdam` | UTC+1 | UTC+2 |
| `America/New_York`, `America/Toronto` | UTC−5 | UTC−4 |
| `America/Chicago` | UTC−6 | UTC−5 |
| `America/Denver` | UTC−7 | UTC−6 |
| `America/Phoenix` | UTC−7 | none |
| `America/Los_Angeles` | UTC−8 | UTC−7 |
| `America/Sao_Paulo` | UTC−3 | none |
| `Asia/Kolkata` | UTC+5:30 | none |
| `Asia/Singapore` | UTC+8 | none |
| `Asia/Tokyo` | UTC+9 | none |
| `Australia/Sydney` | UTC+10 | UTC+11 |

For every zone, run `timedatectl list-timezones` on any systemd host, or see the [list of tz database time zones](https://en.wikipedia.org/wiki/List_of_tz_database_time_zones). Avoid abbreviations and offsets even where they are accepted: `EST` is a fixed UTC−5 with no daylight saving, `Etc/GMT-2` means UTC**+**2 (the sign is inverted), and `PST` or `+02:00` are refused.

**To change it:**

- **Docker:** `sudoedit .env` (setup leaves it read-only), set `TZ=Area/City`, then `docker compose up -d`. A `TZ` exported in the shell that runs `docker compose` takes precedence over `.env`, so unset it there or make it match.
- **`.deb` host:** `sudo timedatectl set-timezone Area/City` changes the whole host. To change it for PBP only, run `sudo systemctl edit pbp` and add:

  ```
  [Service]
  Environment=TZ=Area/City
  ```

  Then run `sudo systemctl restart pbp` either way.
- **For the schedule alone:** start it with `CRON_TZ=`, for example `schedule: "CRON_TZ=America/New_York 0 3 * * *"` (03:00 New York time, whatever zone the host or container is in). The notes and the log keep the process's own zone.

**Daylight saving.** On the day clocks go forward, a time that does not exist that night is skipped. On the day they go back, a time that happens twice runs twice: the second run skips the backups the first one proved (unless `restore.reproof: true`) and tries the others again. With the default `0 2 * * *`, that means:

| Zone | Skipped | Runs twice |
|------|---------|------------|
| US and Canadian zones | the March night clocks go forward | none |
| Central Europe (`Europe/Paris`, `Europe/Berlin`, …) | the last Sunday in March | the last Sunday in October |
| `Australia/Sydney` | the first Sunday in October | the first Sunday in April |
| `Europe/London`, `Etc/UTC`, zones without daylight saving | none | none |

To avoid both, schedule outside 01:00 to 03:00 (for example `0 3 * * *` or later, which is safe in all of the zones above), or use `Etc/UTC`.

## Exit codes

| Exit code | Meaning |
|-----------|---------|
| 0 | Success (`check` passed, or the run finished without error) |
| 1 | Failure: bad config (including a malformed `selection.exclude` entry), `check` found a problem, the run's severity is `error` (any guest `FAILED`, or a run-level error), or `run --guest` found no backup or names an excluded guest |
| 2 | Usage error (unknown subcommand or flag, or `--guest` that is not a positive integer) |
| 3 | Busy: another run or check holds the run lock. `run` and `check` both exit 3 and print "a run is already in progress" |
| 78 | `daemon` only: the config is missing or unusable (for example a `config.yaml` that `pbp setup` has not finished, or a malformed `selection.exclude` entry), or the daemon runs as root while its state or log directory belongs to the service user. `pbp.service` does not restart on it; fix the config (run `pbp setup` again) and start the service |

Subcommands take `--config PATH`. The config path otherwise defaults to `$PBP_CONFIG`, then `/etc/pbp/config.yaml`. The state directory is `/var/lib/pbp/state`, overridable with `PBP_STATE_DIR`. Errors and what to do about them: [Troubleshooting](troubleshooting.md).

## Health check

`pbp health` reports a problem when the last run ended in a run-level error (for example the node or SSH is unreachable, so no notification could be sent, or the run was interrupted because PBP was stopped or crashed mid-run), when a scheduled run did not start, or when the current run has overrun (it ran past `max_run_duration` plus `boot_timeout`, `probe_timeout` and 10 minutes, or past 24 hours without `max_run_duration`; a restore still in progress, which no timeout bounds, extends that up to the 24 hours). It reads the schedule in the daemon's time zone: the daemon records its `TZ`, when set, at start. It prints `ok` and exits 0 when all is well, and otherwise prints the reason and exits 1. This is the backstop for a silent night when the node itself cannot notify you. When the daemon starts again after it was killed or crashed during a run, it marks that run as interrupted (so `pbp health` reports it) and, if it can reach the node, sends a notification about it and cleans up the guests the run left behind, within 15 minutes; otherwise the next run's janitor does. If a `pbp run` holds the lock at that moment, that run's own start marks it instead.

- **Docker:** the image's `HEALTHCHECK` runs it, so the container reports **unhealthy**. Watch that with your usual container monitoring.
- **`.deb` host:** run `sudo -u pbp PBP_CONFIG=/etc/pbp/config.yaml pbp health` from your monitoring (a check script, or a systemd timer that alerts on failure).

## Notifications

One summary per run is sent from the PVE node (through the `pbp node` helper over SSH, using `PVE::Notify`), so it goes to whatever notification targets and matchers you already use. Severity is `info` when everything is `PROVEN` (or skipped as already proven, as a template or as excluded by config), `warning` for any `BOOTED`, `ERROR` or other `SKIPPED`, or for an environment error (such as a `pbp`-tagged guest outside the VMID range), and `error` for any `FAILED` or a run-level failure. The body lists the problems first (`FAILED`, `ERROR`, `BOOTED`, then `SKIPPED` by reason), then ☑️ PREVIOUSLY PROVEN (guests whose newest backup an earlier run proved) and ✅ PROVEN, one guest per line with its name (`VM110: db01`), containers first, then by VMID.

To route it, create a matcher in Datacenter > Notifications > Notification Matchers with a **Match field** rule: field `type`, value `pbp`. A `hostname` field is also available.

You install the templates (`pbp-subject.txt.hbs`, `pbp-body.txt.hbs`) into `/etc/pve/notification-templates/default/` on the node with `pbp node setup`, or by hand with `pbp node install-templates` ([Node setup](manual-setup.md#node-setup), step 6); preflight only checks them and says to run that command if they are missing or differ from the ones in your PBP version, so run it again after upgrading PBP. PVE 9 picks up this new template type from there (verified on PVE 9.2), and `/etc/pve` survives package upgrades, so a PVE upgrade does not remove them.

If a target cannot be notified (for example a template fails to render), `PVE::Notify` only logs an `ERROR:` line; PBP treats that as a failed notification, so the run's outcome is `error` and the health check reports it. `PVE::Notify` cannot say which targets succeeded, so this is deliberately conservative: a failure of any one target marks the run's notification as failed, even if others delivered.

`notify.when` is `always` by default, so a missing nightly message is itself a signal; set it to `problems` for quiet success.

## Backup notes

PBP owns exactly one line in each backup's notes. It goes first, so it shows as the backup's comment in PVE and PBS, and it starts with the guest's name, so searching the backup lists by name still finds the backup:

```
<name> · PBP <VERDICT> <YYYY-MM-DD HH:MM> · <short reason>
db01 · PBP PROVEN 2026-10-19 02:14 · agent ok, 3 profiles ok
db01 · PBP FAILED 2026-10-19 02:31 · failed: postgresql.service
```

The name comes from the backed-up config (`name` for a VM, `hostname` for a container), or is the label (`VM105`) when it has none. Your own notes are preserved below the line. When their first line is just the guest's name, as vzdump's default notes template (`{{guestname}}`) writes it, PBP's line takes its place instead of repeating it. Any other first line, such as a custom template's (`db01 (105) nightly`), moves to the second line. A previous PBP line is replaced, never stacked.

PBP 0.5 to 0.7 wrote the line without the name, pushing the name out of the comment. Each run puts the name back in front of such a line, keeping its verdict, on every backup in `sources.storages` (not only the newest of each guest), except those of excluded guests. It does so after the proofs, up to 100 backups per run and within `restore.max_run_duration`, so with many old backups the first runs after upgrading take a few minutes longer and finish the rest over the following nights. `run.log` counts them (`notes: added the guest name in front of the PBP line on N older backups (0 failed, M left for the next run)`). A `SKIPPED` guest gets no line: its notes are left as they were. PBP only ever modifies the `notes` and `protected` attributes of a backup.
