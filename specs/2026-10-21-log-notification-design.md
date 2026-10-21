# Run logs and notification summary (issue #26)

Agreed with the owner on issue #26 (comment 254), 2026-10-21. Branch `logAndNotificationRefinement`.

## Why

Last night's run log on pve1 (`run-20261021T060000.100Z-schedule.log`) showed the gap: nearly every record was written at 03:33:53, when the run ended, one "guest result" line per guest. Nothing was logged while a guest was restored, booted and checked, so its 9 `ERROR`s could not be explained, and PVE task failures appeared only as task IDs followed by a raw task log folded into the reason. The notification listed every guest in VMID order with these unreadable reasons.

## Goals

1. **Layout (owner's comment on #26).** One folder per run, named with the run's start time in the host's or container's time zone (the zone the schedule uses, #19), holding the run's own log and one file per guest:

   ```
   /var/log/pbp/
     2026-10-21_02-00-00_schedule/
       run.log
       VM102.log
       CT101.log
   ```

   - `logging.retain_runs` counts folders.
   - Old `run-*.log` files count toward the same limit until they age out.

2. **Readable text files, not JSON.** The service log (`journalctl -u pbp` / `docker compose logs`) stays short: run started, one line per guest result, and a summary line at the end.

   `run.log`:
   ```
   2026-10-21 02:00:00 EDT run started (schedule) · PBP 0.4.0 · node pve1
   02:00:02 preflight ok (1.8s)
   02:00:02 discovery: 48 backups on PBS-qemu; 3 already proven, 45 to proof
   02:00:02 VM102 started (VM102.log)
   02:01:31 VM102 FAILED · failed: sssd-nss.socket, sssd-pam.socket
   02:01:43 VM104 SKIPPED · insufficient node memory
   03:33:54 run finished in 1h33m · 13 proven, 9 failed, 4 booted, 9 skipped, 9 error · severity error
   ```

   `VM102.log`, one line per step:
   ```
   2026-10-21 02:00:02 EDT backup PBS-qemu:backup/vm/102/2026-10-20T06:00:02Z (taken 2026-10-20 02:00, 20h old, 32 GiB)
   02:00:02 capacity ok: 32 GiB of 410 GiB free on pool_qemu; RAM 4.4 GiB of 21 GiB free
   02:00:03 backup marked protected (was not)
   02:00:03 restore to 990001 …
   02:00:36 restore done in 33s
   02:00:36 made safe: net0: bridge=vmbr0,tag=2 → bridge=pbpbr0; onboot: 1 → 0
   02:00:37 started; waiting for the guest (up to 5m)
   02:01:21 running after 44s (Rocky Linux)
   02:01:23 failed units: sssd-nss.socket, sssd-pam.socket
   02:01:25 check ssh:ports:22 ok
   02:01:26 verdict FAILED · failed: sssd-nss.socket, sssd-pam.socket
   02:01:38 teardown done in 12s: destroyed 990001, protected set back to false
   02:01:38 notes: PBP FAILED 2026-10-21 02:01 · failed: sssd-nss.socket, sssd-pam.socket
   ```

   When a PVE task fails (restore, start), the guest log gets the tail of the task log; the reason keeps only the task's error line, not the task ID.

3. **Notification.** Problems first, a short subject, one guest ID style (`VM102` / `CT101`) everywhere, the log folder's path.

   ```
   Subject: PBP pve1: 9 failed, 9 error, 4 booted, 4 skipped · 13 proven

   PBP run on pve1, Tue 2026-10-21 02:00 EDT (scheduled), 1h33m
   Logs: /var/log/pbp/2026-10-21_02-00-00_schedule/

   FAILED: the backup has a problem
     VM102 web01 · backup Oct 20 02:00 · failed: sssd-nss.socket, sssd-pam.socket
     VM107 db02 · backup Oct 20 02:00 · root filesystem read-only
   ERROR: PBP could not finish; says nothing about the backup
     CT101 dns01 · backup Oct 20 02:00 · restore failed: <the task's error line>
   BOOTED: started, not looked inside
     VM117 win01 · backup Oct 20 02:00 · guest agent missing
   SKIPPED
     insufficient node memory: VM104, VM105, VM115, VM126
     already proven: VM100, CT110, CT127
   PROVEN
     VM108, CT109, VM109, VM110, VM111, VM112, VM113, CT116, CT117, CT119, CT120, CT124, VM137

   Not covered by backup
     VM102: Found nfs-home.mount depends on /nfs/home (network storage, //cic-nas/homes): not backed up, skipped
   ```

   - The subject drops zero counts; a clean night reads `PBP pve1: all 13 proven`.
   - "Not covered" lines name their guest.
   - The environment-error and run-error sections are unchanged.
   - `notify.when` stays `always | problems`.
   - The node's templates must be reinstalled after an upgrade (preflight already says so).

4. **Consistency.** The same reason text appears in the guest log, `run.log`, the notification and the backup's note line (the note truncates long reasons, as today). All times are in one zone: the process's local zone (`TZ` / the host's zone).

## Non-goals

- #29 (containers stuck in `starting`), 2-second restore failures, the `protected` flag that could not be set back: separate issues, once these logs can explain them.
- New config options. `notify.when` keeps its two values.
- Machine-readable (JSON) run logs.

## Decisions made while planning (builder's calls; the owner can overturn them)

- **Reasons stay stable.** A skip reason stays `insufficient node memory` (no figures in it) so the notification can group identical reasons; the figures go in the guest log.
- **"started" lines** (`VM102 started (VM102.log)`) go to `run.log` only, not to the service log, which gets only results.
- **Warnings and errors in a guest file** (a failed destroy, a failed unprotect, a panic) are also written to `run.log` and the service log, prefixed with the guest's label, so no PBP fault is visible only in a guest file.
- **Step lines are INFO**, including a failed restore and its task log: a guest's verdict is not a PBP fault. ERROR stays for PBP faults (as today).
- **Levels:** only `WARN` and `ERROR` are printed in the files; INFO lines carry no level.
- **Dates:** a file's first line, and the first line after the date changes, carry the full date and zone; other lines carry `15:04:05`.
- **Notification lines are not column-aligned** (`VM102 web01 · backup Oct 20 02:00 · reason`): mail clients use proportional fonts.
- **Subject counts** include `skipped` only for skips that raise the severity (not already proven, not templates). A run error leads the subject: `PBP pve1: run error · 0 proven`. Nothing proofed and nothing wrong reads `nothing to proof`.
- **Docker:** the notification shows the container's path (`/var/log/pbp/...`), which `docker-compose.yml` maps to `./logs`; the docs say so.
- **The system-state line** in the sample guest log is dropped: the probe does not return it, and the failed units carry the same information.
- **Folder-name collisions** (two runs in the same second) get `-2`, `-3`, … appended; a run never writes into another run's folder.
