# Dedicated proofing node: findings and test plan (issue #30)

Branch `dedicatedPVEProofingHostEnhancement`. Scope (owner, 2026-10-21): **one source cluster**, proofed on a separate Proxmox VE host. Multi-cluster support (several clusters' backups on one datastore) is out of scope. A dedicated host will be put online for testing when needed.

## Setup being tested

```
production PVE (cluster)  ──backups──▶  PBS datastore  ◀──same datastore──  proofing PVE (standalone)
                                                                                  ▲
                                                                       PBP daemon (separate host or on it)
```

PBP points only at the proofing node: `target.node`, `target.api.url`, `target.ssh.host`, the isolated bridge, the restore storage and the VMID range are all on that node. `sources.storages` names the proofing node's storage IDs for the shared PBS datastore. Production nodes run nothing of PBP: no helper, no token, no bridge.

## How PBP reaches PBS

PBP never connects to PBS. It calls the proofing node's PVE API with its `pbp@pve` token, and the node talks to PBS with the credentials in its own storage definition (`username` in `storage.cfg`, the secret in `/etc/pve/priv`). Where PBP runs (Docker, another host, the node itself) does not change the PBS side.

Today, on pve1, PBP proofs through the same storage (`PBS-qemu`, user `pbs@pbs`) that the backup jobs write through. That user owns the backup groups, so setting `protected` and writing notes are allowed. On a dedicated node, the admin chooses the credentials when adding the datastore; question 1 below is about that choice.

## What the code already settles

- **Discovery** lists backups through the proofing node's own storage view (`/nodes/<node>/storage/<id>/content`), so it needs nothing from production.
- **Guests are identified by kind and VMID across all configured storages** (`discovery.NewestPerGuest`). With one source cluster, that is correct: a replica storage of the same datastore counts as the same guest. Several clusters would collide (out of scope).
- **Restored disks** all go to `restore.storage`, so the source's storage IDs do not need to exist on the proofing node.
- **Sanitize** removes hookscripts, host passthrough and bind mounts. Mapped devices that do not exist end `ERROR` (unverified on a node without them).
- **The VMID range and the stray-tag check** apply to the proofing node only; nothing else runs there.

## Open questions, with hypotheses

| # | Question | Hypothesis | How to test |
|---|----------|------------|-------------|
| 1 | Can PBP set `protected` and write notes through the proofing node's PBS storage? | Only if the proofing node's PBS credentials own the backup groups (same PBS user or token as production), or hold `Datastore.Modify` (which also allows deleting backups). With a different read-only user, every proof ends `ERROR · protect backup`. | Add the datastore with (a) a separate `DatastoreReader` user, (b) the production backup owner's credentials. Proof one guest each way; read the result and the PBS task log. |
| 2 | Are the verdicts visible from production? | Yes: notes and `protected` are stored on PBS. | After 1(b), read the backup's notes in the production node's storage view. |
| 3 | Does a restore fail when the backed-up NIC's bridge or VLAN (`bridge=vmbr0,tag=2`) does not exist on the proofing node? | No: restore does not check bridges, and PBP moves every NIC to the isolated bridge before start. `SDN.Use` on `/sdn/zones/localnetwork` still applies. | Proof a guest whose backup uses a bridge name the proofing node does not have. |
| 4 | Does a VM with `cpu: host` (or a CPU model the proofing node lacks) boot? | Usually yes with `host`; a named model newer than the proofing node's CPU fails to start (`ERROR · start failed`). | Proof a `cpu: host` VM and, if one exists, a VM with a recent named model. |
| 5 | Machine type and PVE version differences | A pinned `machine:` newer than the proofing node's QEMU fails to start. The proofing node should run the same or a newer PVE. | Compare versions; proof a VM with a pinned machine type if one exists. |
| 6 | Encrypted backups | Need the encryption key in the proofing node's storage config, otherwise the restore fails. | Proof an encrypted backup with and without the key (pve1's PBS-qemu prints "Using encryption key", so its backups are encrypted). |
| 7 | Notifications | They go out through the proofing node's `PVE::Notify`, so its notification targets must be configured. | Run once and receive the summary. |
| 8 | Template install and node setup | `pbp setup` / `pbp node setup` work unchanged against the proofing node. | Run the guided setup against it from the PBP host. |

## Results on pve2 (2026-10-21)

The test setup: **pve2**, a standalone PVE 9.2.18 host on the same CPU model as pve1. Its `PBS-qemu` storage points at the same datastore, as `pbs@pbs` (the backups' owner), with the same encryption key. It has a new isolated bridge `vmbr99` and pbp 0.4.0 with node setup. The PBP host is **dev-scratch** (0.4.0 `.deb`, config written by the guided setup).

| # | Result |
|---|--------|
| 1 | **With the owner's credentials (`pbs@pbs`, `DatastorePowerUser`): works.** `protected` was set and cleared, and the notes were written (VM108, CT120, VM102). **With a separate read-only token** (`pbp-ro@pbs!pve2`, `DatastoreReader` only), in a second storage `PBS-qemu-ro` on pve2: listing and restore access work (444 backups listed), but PBS refuses `protected` and notes with `permission check failed - missing Datastore.Modify\|Datastore.Backup on /datastore/pbs_cic-nas`. The proof ends `ERROR` before anything is restored. **Bug found and fixed on this branch:** PBP then tried to undo the refused protect and left an in-flight job that the janitor would retry, and fail, on every run. Now a refusal (`pve.PermissionDenied`) leaves nothing to undo, and the reason says which rights the storage's PBS user lacks. |
| 2 | **Yes.** pve1's view of the same backups shows pve2's verdict lines, with `protected=0` afterwards. Both nodes share the notes, so another PBP instance treats a backup proven on pve2 as "already proven". |
| 3 | Not testable with the current backups: they all use `vmbr0`, which pve2 also has (as an OVS bridge). |
| 4 | **`cpu: host` works** (VM108 PROVEN). **A named model works** (VM102 `x86-64-v2-AES` booted; it FAILED on the same sssd units as on pve1, which is consistent). Both hosts have the same CPU model, so a CPU mismatch is not tested. |
| 5 | **A pinned machine type works** when the proofing node runs the same PVE: VM115 (`machine: pc-q35-9.0`, Windows, 466 GiB) restored in 11m49s and was running after 43s. The proof ended `ERROR` for an unrelated reason: the Windows guest agent's `guest-exec-status` timed out 27s after boot. That is a Windows probe issue, filed separately. |
| 6 | **Works with the key:** the backups are encrypted, and pve2's storage has the key. Without the key, the restore cannot decrypt; that case is not tested. |
| 7 | **Works:** pve2's `mail-to-root` target sent each summary (the run outcome was not `error`). |
| 8 | **Works:** `pbp setup --deb` on dev-scratch plus `pbp node setup` on pve2, unchanged. `pbp check` passes. One network issue was found and fixed: pve2 still had an unused corosync address (`10.10.0.2/24`) on a port without a link, so its replies to dev-scratch (`10.10.0.1`) went nowhere. Setup reported it clearly ("could not reach … the check below shows whether it works"). |

## Host requirements for the test

- Proxmox VE 9.x, same or newer than production; enough RAM and disk for the largest guest to be proofed.
- The production PBS datastore added as a storage (same datastore; first with a separate read-only user, for question 1).
- The encryption key, if the backups are encrypted.
- An isolated bridge (Setup step 2) and a restore storage.
- A notification target.
- Reachable from the PBP host (API on 8006, SSH on 22).

## Expected deliverables

- Docs: a "Dedicated proofing node" section, linked from "Where to run PBP" in `docs/security.md` and from `docs/setup.md`, covering:
  - the setup;
  - the PBS credentials needed (the answer to question 1, with the security trade-off);
  - what production then needs (nothing from PBP);
  - the requirements above.
- Code only where the tests show it is needed. Candidates:
  - a clear `pbp check` or run message when the PBS storage refuses `protected` or notes;
  - naming the CPU or machine type in a start failure.
- CHANGELOG.
