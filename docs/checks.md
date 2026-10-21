# Checks inside the guest

Once a restored guest has booted, PBP looks inside it: the system state, failed units, a read-write root filesystem, and service checks. This page explains what counts as a failure, how to add your own checks, and what PBP deliberately does not blame on a backup. The settings live under `checks:` in `config.yaml`.

## The ignore list

A guest is `FAILED` if any systemd unit has failed, **unless** the unit is on the ignore list. Proof guests have no uplink: the bridge is isolated on purpose. Units that only make sense with a network therefore fail in a perfectly good restore. Ignored failures are still written to the log, as "ignored"; they are never silently dropped.

PBP's built-in list is below. Units you put in `checks.ignore_units` are **added** to it, so you only list your own, and new built-in entries in later PBP versions apply without editing your config. To use exactly your own lists (Linux and Windows) instead, set `checks.use_default_ignores: false`.

| Unit | Why it fails with no uplink |
|------|-----------------------------|
| `systemd-networkd-wait-online.service`, `NetworkManager-wait-online.service` | Wait for a network that never comes |
| `chronyd.service`, `chrony.service`, `systemd-timesyncd.service`, `ntp.service`, `ntpd.service`, `ntpsec.service` | No time source is reachable |
| `remote-fs.target`, `nfs-client.target`, `rpc-statd.service` | Network mounts and NFS servers are unreachable |
| `cloud-init.service`, `cloud-init-network.service`, `cloud-final.service`, `cloud-config.service` | The metadata source is unreachable |

For Windows VMs, automatic-start services that are not running count as failures, minus `checks.windows_ignore_services`. Because services can still be starting when the guest agent first answers, PBP re-checks the list every 15 seconds until none is left or `restore.probe_timeout` runs out; only the services still stopped at that point make the guest `FAILED`. For each of them the guest's log file says why it stopped, from the newest Service Control Manager event since boot and the service's exit code, for example `Bonjour Service stopped: event 7024: The Bonjour Service service terminated with the following service-specific error: … (service-specific exit code 0xFFFFFFFF)`, or `not started since boot`. The default: `sppsvc` (needs activation servers), `gupdate` and `edgeupdate` (updaters), `wuauserv` and `BITS` (Windows Update and background transfer), `W32Time` (no time source), `CDPSvc` (needs a network), and `MapsBroker`, `RemoteRegistry`, `tiledatamodelsvc` and `WslInstaller` (delayed-start, disabled or self-stopping by design; `WslInstaller` checks or installs WSL at boot on Windows 11, then exits). Services you put in `checks.windows_ignore_services` are added to this list (names compare ignoring case), for example a third-party service that cannot run on the isolated network, such as `Bonjour Service`.

## Profiles and guest checks

PBP reads the units enabled inside each restored guest, which reflects the original host. For every enabled unit that matches a **profile**, the profile's checks run. A failing check is `FAILED`. Check kinds are `services` (must be active), `ports` (a TCP port is listening; any listed port suffices) and `commands` (must exit 0). Every command PBP runs inside a guest is bounded by `checks.command_timeout` (default 60s): a check whose command times out fails (`check timed out: <check>`). The one exception is the initial wait for boot to settle (`systemctl is-system-running --wait`). It may use most of `restore.probe_timeout`, because on the isolated proof network DHCP and wait-online services can keep a healthy guest starting for a minute or two. If the guest is still `starting` when that time runs out, the result is `ERROR` (`guest still starting after …`), not `FAILED`. All checks of one guest are bounded by `restore.probe_timeout` (default 5m): if that runs out, the guest is `ERROR` (`probe timed out`), not `FAILED`.

Built-in profiles: postgresql (`pg_isready`), mariadb and mysql (ping), nginx, apache2 and httpd (80 or 443 listening), docker (`docker info`), redis (`redis-cli ping`), sshd/ssh (22 listening), and cron.

Add your own under `checks.profiles`, keyed by a unit glob; a profile with the same name as a built-in replaces it. Add per-guest checks under `checks.guests`, matched by `vmid`, name or PVE tag, in addition to profiles:

```yaml
checks:
  profiles:
    - name: myapp
      unit: "myapp*.service"
      ports: [8080]
      commands: ["curl -sf http://127.0.0.1:8080/health"]
  guests:
    - match: { vmid: 105 }
      services: [postgresql]
```

## Containers without nesting

In an unprivileged container without `features: nesting=1`, systemd 252 and later cannot set up the mount namespaces that sandboxed units use (`PrivateTmp=`, `ProtectSystem=` and similar). Those units fail with `226/NAMESPACE` ("Failed to set up mount namespacing … Permission denied"), and so do systemd's own mounts such as `tmp.mount` and `dev-mqueue.mount`. Proxmox warns about it when such a container starts: "Systemd 252 detected. You may need to enable nesting."

The production container has the same failures, so they say nothing about the backup. PBP therefore ends such a container `BOOTED`, not `FAILED`, naming the units: `container without nesting: 4 units cannot start sandboxed (systemd-logind.service, …); enable nesting=1`. It counts a unit this way only when its failure is the namespace denial: a service that exited `226/NAMESPACE`, one of systemd's API mounts, or a socket whose service failed so. Any other failed unit still makes the guest `FAILED`.

To fix it, enable nesting on the production container (Options > Features > Nesting, or `pct set <vmid> --features nesting=1`) and restart it; the next backup is then proven normally.

## What "not covered by backup" means

A restore can legitimately lack things the original guest used: disks marked `backup=0`, container mount points excluded from backup, bind mounts and passthrough devices that PBP removes for safety, and network storage (NFS, CIFS, iSCSI) that the isolated bridge cannot reach.

When a failed unit can be traced to one of these (for example a `.mount` unit for an excluded disk, or a service that requires such a mount), PBP does **not** count it toward `FAILED`. It writes it to the log explicitly, for example `postgresql.service depends on /srv/pgdata (disk scsi1, backup=0): not backed up, skipped`, and lists it in the run summary and notification under **not covered by backup**. That is a feature: it tells you when a backup is missing something a service needs, which no boot test of the backup alone can show. Tracing is conservative; if PBP cannot connect a failure to a missing piece, it is a real failure.
