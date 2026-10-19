# Integration tests against a real Proxmox VE node

These tests run PBP's real code (API client, SSH, target, run lifecycle,
janitor, notification templates) against a live PVE 9 node. They are opt-in:
build tag `integration`, and every test skips unless `PBP_IT_CONFIG` is set.

```
PBP_IT_CONFIG=/path/to/config.yaml \
PBP_IT_LXC=110 PBP_IT_VM_AGENT=100 PBP_IT_VM_NOAGENT=138 \
PBP_IT_BROKEN_VMID=990009 \
PBP_IT_BROKEN_TEMPLATE=NFS-iso:vztmpl/debian-12-standard_12.12-1_amd64.tar.zst \
PBP_IT_BROKEN_STORAGE=PBS-qemu \
go test -tags integration ./test/integration/ -v -timeout 90m
```

The node runs production guests. The tests only create, start and destroy
guests inside `target.vmid_range`, only on the isolated bridge, and only
change `notes` and `protected` on the backups they proof (restoring
`protected`). The one exception is `TestBrokenBackupFails`, which creates its
own backup and deletes exactly that backup again.

## Environment

| Variable | Used by | Meaning |
|----------|---------|---------|
| `PBP_IT_CONFIG` | all | A PBP config file for the node (never committed). Use `workers: 1`. |
| `PBP_IT_LXC` | `TestProofSmallLXC`, `TestCrashRecovery` | VMID of a small container whose newest backup is proofed. |
| `PBP_IT_VM_AGENT` | `TestProofVMWithAgent` | VMID of a VM with `agent: 1` and a working guest agent inside. |
| `PBP_IT_VM_NOAGENT` | `TestProofVMWithoutAgent` | VMID of a VM without a guest agent. |
| `PBP_IT_BROKEN_VMID` | `TestBrokenBackupFails` | A free VMID inside the range for the throwaway container. |
| `PBP_IT_BROKEN_TEMPLATE` | `TestBrokenBackupFails` | A systemd-based (Debian/Ubuntu) `vztmpl` volume. |
| `PBP_IT_BROKEN_STORAGE` | `TestBrokenBackupFails` | A PBS storage listed in `sources.storages` to back it up to. |
| `PBP_IT_BROKEN_STORAGE` | `TestProofAlpine*`, `TestProofProtectedGuest` | Also where those tests put their throwaway backups. |
| `PBP_IT_ALPINE_TEMPLATE` | `TestProofAlpine*` | An Alpine (OpenRC) `vztmpl` volume. |
| `PBP_IT_ALPINE_VMID` | `TestProofAlpineHealthy` | A free VMID inside the range for the throwaway container. |
| `PBP_IT_ALPINE_CRASH_VMID` | `TestProofAlpineCrashed` | A free VMID inside the range for the throwaway container. |
| `PBP_IT_PROTECTED_VMID` | `TestProofProtectedGuest` | A free VMID inside the range for the throwaway container. |
| `PBP_IT_PROTECTED_TEMPLATE` | `TestProofProtectedGuest` | A systemd-based (Debian/Ubuntu) `vztmpl` volume. |
| `PBP_IT_LIVE_NOTIFY=1` | `TestNotificationDelivers` | Send one real notification through the node's targets. |

Pick small guests: every proof restores the full backup to `restore.storage`.
The values above are pve1's: 110 `dnstest` (unprivileged CT, 0.7 GB), 100
`delta1` (VM with a working agent, 33 GB), 138 `netbootTesting` (VM without
`agent`). VM 125 has `agent: 1` but no agent running inside, so it gives
`BOOTED` and is not a valid `PBP_IT_VM_AGENT`.

## What each test does

- **TestCheck**: the preflight of `pbp check` (PVE version, isolated bridge,
  restore storage, template install, janitor, no foreign guest in the range),
  and that the API reports the bridge as `bridge`/`OVSBridge`.
- **TestProofSmallLXC**: proofs the container's newest backup. Expects
  `PROVEN`, or `FAILED` naming real units.
- **TestProofVMWithAgent**: proofs the VM; expects `PROVEN` or `FAILED` with
  the agent used (not `BOOTED`).
- **TestProofVMWithoutAgent**: expects `BOOTED` / `guest agent missing`.
- **TestNotificationRenders**: installs the templates, then renders a summary
  that fills every section with the node's own proxmox-notify. It renders
  into an in-memory notification config whose only enabled target is a
  webhook to a one-shot listener on the node's loopback; the built-in
  `mail-to-root` target and `default-matcher` are disabled in that config,
  and the script refuses to send otherwise. Nothing is delivered, and the
  node's notification config is not changed.
- **TestNotificationDelivers** (only with `PBP_IT_LIVE_NOTIFY=1`): sends one
  real notification. A person must confirm it arrived and rendered.
- **TestBrokenBackupFails**: creates a container at `PBP_IT_BROKEN_VMID`
  (unprivileged, rootfs on the restore storage, NIC on the isolated bridge),
  enables `pbp-it-broken.service` (`ExecStart=/bin/false`), backs it up with
  `vzdump --mode stop`, destroys the container (an untagged guest in the range
  makes PBP refuse to run), proofs the backup and expects `FAILED` naming the
  unit. The backup is deleted with `pvesm free` at the end.
- **TestProofAlpineHealthy**: a throwaway Alpine container (OpenRC probe,
  chosen by ostype `alpine`); checks `rc-status --crashed` on the source
  (exit 1, empty output when nothing crashed) and expects `PROVEN`.
- **TestProofAlpineCrashed**: a throwaway Alpine container whose default
  runlevel holds `pbp-it-crash` (an `openrc-run` script with
  `command=/bin/sleep`, `command_args=1`, `command_background=no`, so OpenRC
  marks it crashed a second after start; `rc-status --crashed` then lists it
  and exits 0). Expects `FAILED` naming the service. **Fails as of
  2026-10-19**: the OpenRC probe does not wait for the default runlevel, so it
  runs before the service starts and reports `PROVEN`.
- **TestProofProtectedGuest**: a throwaway Debian container with
  `protection: 1` (and `nesting=1`, as the GUI sets it; without it
  systemd-logind fails in an unprivileged Debian 12 container). PBP must
  clear protection on its restored copy so it can be destroyed; expects
  `PROVEN` and an empty range.
- **TestCrashRecovery**: builds `pbp`, starts `pbp run --guest PBP_IT_LXC`
  (with `notify.when: problems`, only the storage holding that backup), kills
  it with SIGKILL once its job reaches stage `started`, checks the guest and
  the protected flag were left behind, then runs preflight on the same state
  directory: the janitor must destroy the guest and restore `protected`.

The throwaway tests (`TestBrokenBackupFails`, `TestProofAlpine*`,
`TestProofProtectedGuest`) build their source container inside the range,
and PBP never proofs backups whose VMID is in its range. They proof with the
range narrowed to the side of the source VMID with more room (e.g. 990009 ->
`[990000, 990008]`); the range-empty checks still cover the whole range.

After each proof the tests assert: exactly one result, no guest left in the
range, `protected` equal to its value before the run, notes line 1 a PBP
verdict line, the previous user notes (without any old PBP line) unchanged
below it, and no in-flight jobs left in the state store.

Proof tests run with `restore.reproof: true` so they can be repeated, use a
fresh state directory each, and hand the run summary to a capturing notifier
instead of the node, so they send no notifications. They also hide backups of
other guest kinds with the same VMID (VMIDs get reused: a run for
`--guest N` proofs every kind with that VMID).

## Node setup (done once on pve1, 2026-10-19)

**Isolated bridge `vmbr99`** (Linux bridge, no ports, no address). Backup of
the network config first, then:

```
cp -p /etc/network/interfaces /root/interfaces.pbp-backup-<timestamp>
pvesh create /nodes/pve1/network --iface vmbr99 --type bridge --autostart 1
diff -u /etc/network/interfaces /etc/network/interfaces.new   # only the vmbr99 stanza
pvesh set /nodes/pve1/network                                  # applies with ifreload
```

The API reports it as `type: bridge`, `bridge_ports: ""`, no address.

Preflight also requires IPv6 disabled on the bridge (see the main README):
add `post-up echo 1 > /proc/sys/net/ipv6/conf/vmbr99/disable_ipv6` to the
`vmbr99` stanza and run that `echo` once. **Not yet done on pve1**: until it
is, `pbp check` and the suite refuse the bridge with `bridge is not isolated`.

**User, role, token.** User `pbp@pve`, role `PBPProofer`, token
`pbp@pve!proof` (privilege separated). A privilege-separated token gets the
intersection of the user's and the token's permissions, so the ACLs are
granted to both:

```
pveum user add pbp@pve
pveum role add PBPProofer --privs "VM.Allocate,VM.Audit,VM.Backup,VM.Config.HWType,VM.Config.Network,VM.Config.Options,VM.PowerMgmt,VM.GuestAgent.Audit,VM.GuestAgent.Unrestricted,Datastore.AllocateSpace,Datastore.Audit,Sys.Audit,SDN.Use"
pveum user token add pbp@pve proof --privsep 1
for p in /vms /storage/pool_qemu /storage/PBS-qemu /storage/PBS-Baremetal /nodes/pve1 /sdn/zones/localnetwork; do
  pveum acl modify $p --users pbp@pve --tokens 'pbp@pve!proof' --roles PBPProofer
done
```

This is the minimal set found empirically: the suite passes with it, and it
started from a larger set (`Datastore.Allocate`, `VM.Config.CPU/Memory/Disk/
CDROM/Cloudinit` were removed without any test failing). `VM.Backup` (backup
config and volume access) and `SDN.Use` on `/sdn/zones/localnetwork` (restore
checks the original NIC bridges and VLANs) were found missing by the suite.

**Notification templates.** `pbp-subject.txt.hbs` and `pbp-body.txt.hbs` in
`/etc/pve/notification-templates/default/`, installed by PBP's preflight.

**Test config.** `restore.storage: pool_qemu`, `sources.storages:
[PBS-Baremetal, PBS-qemu]` (encrypted; the node holds the key), bridge
`vmbr99`, range `[990000, 990009]`, `workers: 1`, `boot_timeout: 5m`, TLS
pinned to the pveproxy certificate's SHA-256, SSH `known_hosts` from
`ssh-keyscan` cross-checked against an existing trusted entry.
