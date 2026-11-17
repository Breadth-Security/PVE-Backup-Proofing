# Containers with devices: refuse up front, drop device-only raw lines, keep virtual devices (#140, #141)

Status: approved by the owner, 2026-11-17. One branch implements both issues.

## Why

A guest that uses host devices is a permanent `ERROR` today, whatever PBP's grants are:

- A container with **raw `lxc.*` lines** is refused before restoring, because PVE drops those lines for PBP's token and the copy would not be the guest it stands for. Most such lines in the field only pass devices through: GPU render nodes, USB serial sticks, `/dev/net/tun`.
- A container with **`devN`** is restored in full, then fails when Sanitize removes `devN`, because only `root@pam` may change it. A 19 GiB container took 3½ minutes to restore before failing with "cannot make the copy safe".
- **VMs** with raw host passthrough have the same late failure.

This work is for PBP users in general, not one lab. Their containers are mostly unprivileged. The common device users are Tailscale/WireGuard (`/dev/net/tun`), media servers with an iGPU (`/dev/dri`), home automation with a USB stick (`ttyUSB*`, `ttyACM*`) and Frigate with a Coral.

**Success:**
- A container whose only problem is device-only raw lines, or `devN` naming a purely virtual device, gets proofed. Its devices are listed under "not covered by the proof".
- A guest PBP still cannot proof ends `ERROR` before anything is reserved or restored. Its reason names every blocking key and says only `root@pam` can change it.

## Goals

1. **#140, refuse up front.** Before reserving or restoring, check the backed-up config (`orig`) for keys whose removal from the copy is `root@pam`-only, and end `ERROR` right away with a reason that names them.
2. **#141 option 2, keep virtual devices.** A container `devN` naming a device on the keep-list stays in the copy as restored. It is not removed, so no `root@pam` step is needed.
3. **#141 option 1, device-only raw lines.** A container whose raw `lxc.*` lines are all device-only is restored; PVE drops the lines. Each dropped line is recorded as a missing piece, and failed units are traced to the dropped devices through their journal.

## Non-goals

- **Real hardware in the copy:** GPUs and render nodes, USB or serial devices, disks, the Coral. The owner decided "purely virtual devices for now". Sharing render nodes may be revisited later, and is out of scope here.
- **Stripping keys with the root node helper.** It would work for everything, but it widens what root does on the node for PBP.
- **Mapped VM devices** (`hostpciN`/`usbN` with `mapping=`, `virtiofsN`, `rngN` with `/dev/hwrng`). Removing them needs `Mapping.Use`, a grant, not `root@pam`. Their handling does not change in this work.
- **Privileged containers:** the existing `Sys.Modify` check and reason are unchanged. The docs gain one sentence noting that a privileged container with devices also needs it.

## Anti-goals: failures even if the code works

- **The copy gets a device it could use to reach production or real hardware.** The keep-list is exact-path and conservative, and every rule here errs towards removing or refusing.
- **A guest is reported PROVEN when a service failed for a reason other than a dropped device.** Tracing must be conservative: an exact device path match, or the unit stays untraced.
- **A false `ERROR` that a grant would fix, worded as "only root@pam can".** Only keys PVE's code actually reserves to `root@pam` (verified below) may get that reason.

## Facts this rests on (verified against PVE 9.2 source, pve-container and qemu-server)

- **Raw lines on restore.** `PVE::LXC::Create::sanitize_and_merge_config`, restricted (any caller but `root@pam`): the whole `lxc` key is skipped with a warning ("skipping custom lxc options, restore manually as root"). Every other key from the backed-up config is merged, except `rootfs`, `mpN`, `unusedN`, `template`, `digest`, `snapshots`, `unprivileged` and `parent`.
- **`devN` on restore.** `devN` is merged without a permission check. Changing or deleting it is refused for everyone but `root@pam` (`check_ct_modify_config_perm`: "configuring device passthrough is only allowed for root@pam").
- **Container `hookscript`.** Changing or deleting it is `root@pam`-only ("changing the hookscript is only allowed for root@pam").
- **VM keys** (`PVE::API2::Qemu`). These options are `root@pam`-only to delete:
  - `hookscript`, `parallelN` and `ivshmem`: in none of the option groups, so `$check_vm_modify_config_perm` dies with "only root can set …".
  - `serialN` whose value is not `socket`: "only root can delete … for real devices".
  - `usbN` with `host=` other than `spice`: `check_usb_perm`, "only root can set … for real devices".
  - `hostpciN` with `host=`, or with `romfile=`: `check_hostpci_perm`. It runs on the old value when deleting, so `romfile` blocks even a mapped device.
  - `args` is already refused as unsafe.
- **VM restore** (`PVE::QemuServer::check_restore_permissions` → `check_mapping_access`, called from the restore API and every restore path):
  - For any caller but `root@pam`, a raw `usbN` (`host=` other than spice) or a raw `hostpciN` (`host=`) is refused when the restore starts, not when it is removed afterwards.
  - Mapped `usbN`, `hostpciN` and `virtiofsN`, and `rngN` with `/dev/hwrng`, need `Mapping.Use` at restore.
  - `romfile`, `serialN`, `parallelN`, `ivshmem` and `hookscript` are not checked at restore; only their removal is `root@pam`-only.
- **API tokens** never pass a `root@pam` check (the authuser of a token is never the bare `root@pam`), so no PBP grant changes any of the above.
- **Failure tracing.** Inside a container udev does not run, so `dev-*.device` units mostly don't exist and services declare no dependency on their device. Unit properties cannot connect a failure to a missing device, which is why the journal is used.

These were read from the PVE source, which is the confirmation; a fake test can only check PBP against the same reading. For the build, `serialN`, `parallelN` and `ivshmem` must also be checked against what Sanitize removes today (`rePCIEtc`, `isVirtualDevice`, the `ivshmem` case), so the refusal covers exactly what Sanitize would otherwise fail on.

**No live VM test is needed.** With the up-front refusal, PBP never sends such a VM to PVE, so PVE's handling of it no longer affects the result.

## Design

### 1. Up-front refusal (#140)

A new pure function in `internal/sanitize` takes a guest kind and its backed-up config. It returns the `root@pam`-only removals the copy would need, each with key and detail (the device path or value). For a container it also returns the raw `lxc.*` lines that are not device-only (section 3).

Inputs and rules:

| Kind | Key | Blocks when |
|---|---|---|
| LXC | `devN` | its path is not on the keep-list (section 2) |
| LXC | `hookscript` | present |
| LXC | raw `lxc.*` | any line is not device-only (section 3) |
| QEMU | `hookscript` | present |
| QEMU | `parallelN` | present |
| QEMU | `ivshmem` | present |
| QEMU | `serialN` | value is not `socket` |
| QEMU | `usbN` | `host=` set and not `spice` (or a bare first field that is not `spice`, as `isVirtualDevice` reads it) |
| QEMU | `hostpciN` | `host=` set, or `romfile=` set |

**Where it runs:** `internal/run/runner.go`, where today's raw `lxc.*` check is: after the template check, before the privileged check and before any reservation. It replaces that check, so a container gets one reason listing every blocker.

**Reason format** (one line, keys in config order, at most three named, then "and N more", as `keyList` does for bind mounts):

`PBP cannot proof dev1 (/dev/ttyUSB0), hookscript: only root@pam can restore or remove them (docs/security.md#guests-pbp-cannot-proof)`

When raw lines block, the reason names their keys as today. Example: `PBP cannot proof raw lxc settings (lxc.apparmor.profile), dev1 (/dev/ttyUSB0): only root@pam can restore or remove them (…)`. The builder may settle the exact wording within these rules:
- It names the blocking keys and their device paths.
- It says only `root@pam` can, without claiming whether PVE refuses at restore or at removal: for raw `hostpciN`/`usbN` it is the restore, for the rest the removal.
- It links the same docs anchor.
- It stays under the reason length cap.

**Notes and logging:** the notes are written as for the raw `lxc.*` case today (`annotate` with `noteName`). No restore, no reservation.

**Safety net:** Sanitize keeps removing these keys exactly as today, so a config PBP misread still fails safe at sanitize time.

### 2. Keep-list for container devices (#141 option 2)

A `devN` whose path (first field of the value, before any `,uid=`/`gid=`/`mode=`/`deny-write=` options) is exactly one of these is left in the copy:

```
/dev/net/tun
/dev/fuse
/dev/null
/dev/zero
/dev/full
/dev/random
/dev/urandom
```

**Matching rules:**
- The path must match exactly, with no prefix matching, no globbing and no symlink resolution.
- The options are kept as restored.
- The list lives in one place in `internal/sanitize`, with a comment saying why each entry has no reach outside the container.
- Deliberately excluded, with a comment saying why:
  - `/dev/loop*` and `/dev/loop-control`: shared with the host.
  - `/dev/kvm`: may come later on request.
  - Everything else.

**Sanitize:** a keep-listed `devN` is no longer removed and records no missing piece. The copy has the device.

**Run log:** the guest's log says that each kept device was kept, e.g. `kept dev0 (/dev/net/tun) in the copy: a virtual device`.

**Mixed containers:** a container with a kept `devN` and a blocking one ends `ERROR` from section 1, naming only the blocking one.

### 3. Device-only raw lines (#141 option 1)

A raw `lxc.*` line is **device-only** when it is one of these:
- `lxc.cgroup2.devices.allow: …` or `lxc.cgroup.devices.allow: …`, any value.
- `lxc.mount.entry: <source> <target> <fstype> <options> …` meeting all three conditions:
  - fstype is `none`, and the options include `bind` or `rbind`;
  - the source is on the **character-device list** below. A bind of a block device (`/dev/sdb1`, `/dev/disk/by-id/…`, `/dev/zvol/…`, `/dev/mapper/…`) is data, not a device, so it blocks; so does any `/dev` source not on the list;
  - the source is matched by path after `path.Clean`, with no symlink resolution.

**Character-device list** (a source path equal to an entry, or under an entry ending in `/`):

```
/dev/tty*            (ttyUSB*, ttyACM*, ttyS*, ttyAMA*; the glob is a name prefix)
/dev/serial/
/dev/dri/
/dev/bus/usb/
/dev/usb/
/dev/net/tun
/dev/fuse
/dev/apex_*
/dev/accel/
/dev/video*
/dev/snd/
/dev/hidraw*
/dev/input/
/dev/nvidia*
/dev/kfd
/dev/ppp
/dev/i2c-*
/dev/gpiochip*
```

This is a list of what may pass, not of what blocks: a device class it does not know ends `ERROR` as today and can be added on request. Allow lines (`devices.allow`) count as device-only whatever their type, including `b` and `a`. On their own they mount nothing, so dropping them cannot remove data.

**Every other raw line is not device-only and blocks** (section 1). That includes:
- `lxc.cgroup2.devices.deny`: dropping it would give the copy more access than the guest has.
- A mount entry of a device as a filesystem, e.g. `/dev/sdb1 mnt/data ext4`: that is data, not a device.
- A mount entry from outside `/dev`.
- `lxc.autodev`, `lxc.hook.*`, `lxc.apparmor.*`, `lxc.idmap`, `lxc.cap.*` and anything unknown.

A line that cannot be parsed blocks.

**When all of a container's raw lines are device-only,** PBP restores it and PVE drops them.

`MissingFromOriginal` records one missing piece per dropped line:
- **Mount entry:** kind `passthrough`, `Key` the raw key (`lxc.mount.entry`), `Path` the path **inside the container**, i.e. the target with a leading `/` (`dev/ttyACM0` → `/dev/ttyACM0`), and `Detail` the host source when it differs (e.g. `/dev/serial/by-id/usb-…`). The guest sees and logs the target, so the target is what tracing matches and what the hint names.
- **Allow line:** kind `passthrough`, `Key` the raw key, `Detail` the rule, e.g. `c 188:* rwm`, with no `Path`.
- **Repeated identical lines** collapse into one piece.

The guest's log states once that PVE dropped the raw lines, which the token cannot restore, and lists them.

**Tracing** (`internal/probe/missing.go`): a new step runs for failed units still untraced after the existing mount and dependency tracing.
- For each such unit, read its recent journal in the copy, for example `journalctl -u <unit> -b --no-pager -n 50 -o cat`.
- Find device paths in the journal lines: tokens starting with `/dev/`, ended by a character that cannot be part of a path (whitespace, quote, `:`, `,`, `)`, end of line).
- A found path is **explained** by a mount-entry piece when it equals the piece's `Path`, or lies under it (`pathWithin`, whole segments). That second case covers directory binds such as `/dev/serial/by-id`, whose services log `/dev/serial/by-id/usb-…-port0`. `/dev/ttyUSB10` is not under `/dev/ttyUSB1`.
- **And the found path must be absent in the copy:** `test -e <path>` exits 1, as `traceMount` already requires for disks. A path that exists in the copy did not go missing, so it cannot explain the failure.
- Only then is the unit covered by that piece.
- **Not covered:**
  - Allow-line pieces have no path and never cover a unit.
  - A journal read that fails, exits non-zero or finds no match leaves the unit untraced, as today.
- The journal step runs only when the guest has device pieces with a path. Other guests run no extra commands.
- Covered units are reported like bind-mount ones, in "not covered by the proof", and are not failures.

**FAILED hint:** when a guest with dropped device pieces still ends FAILED on untraced units, the reason gains a short hint naming the dropped devices and `ignore_units`, as the nesting hint does. Example:

`failed: zigbee2mqtt.service · the copy has no /dev/ttyUSB0, /dev/ttyACM0 (raw lxc lines dropped); if a unit needs them, list it in checks.guests ignore_units`

Its wording is the builder's call within the reason cap: it names the device paths (or "device rules" when only allow lines were dropped) and names `checks.guests` `ignore_units`.

### 4. Interaction with existing checks

**Order in the runner:**
1. Readable config.
2. Exclusion.
3. Template.
4. The new refusal (section 1).
5. Privileged / `Sys.Modify`.
6. Capacity and reservation.

A privileged container with device-only lines and no `Sys.Modify` ends with the privileged reason, as today.

- Bind-mount handling (#129) is unchanged. A container may have bind mounts, kept virtual devices and device-only raw lines at once.
- `missingPieces` dedup by `Kind+Key` must not collapse two different raw lines that share a key such as `lxc.mount.entry`. Dedup for device pieces uses `Kind+Key+Path+Detail`.

## Tests (the builder writes them; these are the cases that must exist)

**Sanitize, pure functions:**
- The refusal function returns nothing for:
  - a clean container;
  - a container with only keep-listed `devN`;
  - a container with only device-only raw lines;
  - a VM with `serial0: socket`, `usb0: spice` or `usb0: host=spice`.
- It returns the keys for:
  - `dev0: /dev/ttyUSB0`, and `dev0: /dev/net/tun2` (no prefix match);
  - container and VM `hookscript`;
  - `parallel0`;
  - `ivshmem`;
  - `serial0: /dev/ttyS0`;
  - `usb0: host=1234:5678`;
  - `hostpci0: 0000:01:00.0`, `hostpci0: host=…`, and `hostpci0: mapping=gpu,romfile=x.rom`.
- `hostpci0: mapping=gpu` is not refused.
- Device-only classification:
  - every example in section 3, both ways;
  - a privileged container's real set from #141 (`devices.allow: a`, `c 188:* rwm`, `c 189:* rwm`, `c 10:200 rwm`, and binds of `/dev/serial/by-id`, `ttyUSB0/1`, `ttyACM0/1`, `/dev/net/tun`), all device-only;
  - a `devices.deny` line blocks;
  - an `ext4` mount of `/dev/sdb1` blocks, and so does a `none bind` entry of `/dev/sdb1`, `/dev/disk/by-id/x` or `/dev/zvol/rpool/x`;
  - `/dev/ttyUSB0`, `/dev/serial/by-id`, `/dev/dri` and `/dev/bus/usb` binds are device-only, and `/dev/sdb1` with fstype `none` is not;
  - `lxc.hook.autodev` blocks;
  - an unparseable mount entry blocks.
- Sanitize keeps keep-listed `devN` and still removes `/dev/ttyUSB0`.
- `MissingFromOriginal` records one piece per distinct dropped line, with `Path` or `Detail` as above.

**Runner** (`internal/run`, with the PVE fake):
- For each blocking key: `ERROR` with the reason, no reservation, no restore call, and notes written.
- A container with blocking `devN` **and** non-device raw lines gets one combined reason.
- A container with device-only lines and a kept `devN` restores and is proofed. Its log lists the device pieces and the kept device.
- A privileged container with device-only lines and no `Sys.Modify` gets the privileged reason.

**Probe tracing:**
- A failed unit whose journal names `/dev/ttyUSB0` (absent in the copy) is covered.
- A directory piece `/dev/serial/by-id` covers a unit whose journal names `/dev/serial/by-id/usb-x-port0`.
- A path present in the copy (`test -e` exits 0) does not cover.
- `/dev/ttyUSB10` in the journal does not cover a `/dev/ttyUSB1` piece.
- A piece whose source differs from its target is matched on the target.
- A journal read error leaves the unit untraced.
- No journal command runs when there are no device pieces with a path.
- A FAILED guest with device pieces carries the hint.

**PVE fake:** restoring a container as the token drops `lxc` lines and keeps `devN`, as PVE does. Deleting `devN` or `hookscript`, or the VM keys above, returns PVE's 403 or 500 with PVE's message.

## Docs

- **`docs/security.md`, "Guests PBP cannot proof":**
  - Rewrite the raw `lxc.*` bullet: device-only lines are now proofed without the devices (what counts as device-only), other lines still block.
  - Rewrite the `devN`/hookscript/raw passthrough bullet: the up-front refusal and its reason; virtual devices on the keep-list are kept.
  - Note that privileged containers with devices also need `Sys.Modify`.
- **`docs/checks.md`:** dropped devices appear under "not covered by the proof"; how journal tracing works, and that `ignore_units` handles units it cannot trace.
- **`docs/troubleshooting.md`:** rows for the new refusal reason and the FAILED hint.
- **`CHANGELOG.md`:** an entry under the next version.

## Live verification (the owner's test node, after the unit tests pass)

These need the owner's go-ahead for any grant changes:
- **The privileged container with only `dev0: /dev/net/tun`** (#140's example) is proofed with `Sys.Modify` granted, and ends with the privileged reason without it.
- **The privileged container with the device-only raw lines above** (#141's example) is restored with `Sys.Modify`. Its devices are listed, and its result (PROVEN, or FAILED with the hint) is examined.
- **An unprivileged test container** with `dev0: /dev/net/tun` and one device-only raw line, created in PBP's own VMID range or as a throwaway with the owner's approval, is proofed without extra grants.
- **The PVE behaviour behind section 1:**
  - restored `devN` starts unchanged in the copy;
  - PVE's restore warning lists the dropped lines.

## Left to the builder

- Function and type names, file placement inside `internal/sanitize` and `internal/probe`.
- Exact reason and log wording, within the rules above.
- The journal line count (around 50) and time window.
- Whether the device-only parser lives next to `RawLXCKeys` or in a new file.

## Calls made during design (not the owner's words)

- The keep-list contents, including the standard pseudo-devices, and excluding `/dev/kvm` and loop devices.
- `devices.deny`, `autodev`, hooks and block-device filesystem mounts block, rather than count as device-only.
- Journal tracing uses only exact device paths. Allow-line rules never cover a unit.
- One combined reason when several blockers exist.
- `romfile` on a mapped `hostpciN` blocks, following PVE's code.
- Mount-entry sources must be on a character-device list; block-device binds and unknown `/dev` sources block. Allow lines of any type are device-only.
- A missing piece records the target path inside the container. Tracing accepts a journal path equal to or under it, and only when that path is absent in the copy.
- The up-front reason says "only root@pam can restore or remove them", because PVE refuses raw `hostpciN`/`usbN` at restore and the other keys at removal.
- No live VM test: PBP refuses those VMs before PVE sees them.
