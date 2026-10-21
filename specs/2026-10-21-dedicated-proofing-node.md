# Dedicated proofing node: findings and test plan (issue #30)

Branch `dedicatedPVEProofingHostEnhancement`. Scope (owner, 2026-10-21): **one source cluster**, proofed on a separate Proxmox VE host. Multi-cluster support (several clusters' backups on one datastore) is out of scope. A dedicated host will be put online for testing when needed.

## Setup being tested

```
production PVE (cluster)  ──backups──▶  PBS datastore  ◀──same datastore──  proofing PVE (standalone)
                                                                                  ▲
                                                                       PBP daemon (separate host or on it)
```

PBP points only at the proofing node: `target.node`, `target.api.url`, `target.ssh.host`, the isolated bridge, the restore storage and the VMID range are all on that node. `sources.storages` names the proofing node's storage IDs for the shared PBS datastore. Production nodes run nothing of PBP: no helper, no token, no bridge.

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
