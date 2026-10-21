# Proofing on a dedicated node

By default PBP restores and boots its proof copies on a production Proxmox node. It can instead use a **separate Proxmox VE host that only proofs**: the copies never run on production hardware, production nodes need nothing from PBP (no helper, no API token, no isolated bridge), and proofs no longer compete with production guests for RAM, storage and I/O. If that host is the standby hardware your disaster recovery plan would restore to, every proof also proves that restore path.

This page covers **one source cluster** whose backups are proofed on one dedicated node. Proofing backups of several clusters from one datastore is not supported yet: guests are identified by kind and VMID, so two clusters' VM 100 would be treated as one guest.

```
Production PVE  ── Backup jobs ──▶  PBS datastore  ◀── same datastore ──  Proofing PVE
                                                                          ▲
                                                                          │
                                                                          PBP (its own host, or on the proofing host directly)
```

PBP itself is unchanged: it talks only to the proofing node, which is its `target.node`. Where to run PBP is a separate choice: [Where to run PBP](security.md#where-to-run-pbp).

## What the proofing node needs

- **Proxmox VE as new as production, or newer.** A VM with a pinned machine type (`machine: pc-q35-9.0`) needs a QEMU at least that new, and a VM with a named CPU model needs a CPU that supports it. `cpu: host` works on any CPU.
- **Room for the largest guest you want proofed:** free RAM and space on the restore storage. PBP skips a guest that does not fit.
- **The production datastore added as a PBS storage,** with the right PBS credentials (next section) and, for encrypted backups, the same encryption key.
- **An isolated bridge and a free VMID range** ([Setup](setup.md) steps 1 and 2), and a **restore storage**.
- **A notification target:** the run summary is sent through the proofing node (Datacenter > Notifications).
- **Reachable from the PBP host:** API on port 8006, SSH on port 22.

## Connecting the proofing node to PBS

PBP never connects to PBS itself. It asks the proofing node's API, and the node talks to PBS with the credentials of its own PBS storage (the user or API token you enter when you add the storage). Before each proof PBP marks the backup `protected`, and afterwards it writes the verdict into the backup's notes. **PBS allows both only for:**

- the **owner** of the backup (the PBS user or token your backup jobs use), with `Datastore.Backup`; or
- an identity with **`Datastore.Modify`** on the datastore. Of PBS's standard roles only `DatastoreAdmin` (and `Admin`) has it, and it also allows deleting any backup.

A **read-only identity (`DatastoreReader`) lists and restores backups but cannot proof them:** PBS refuses the change, and every proof ends

```
ERROR · protect backup: PBS refused: the storage's PBS user needs Datastore.Backup as the backup's owner, or Datastore.Modify
```

before anything is restored.

**Use the same PBS user or token that your backup jobs use** when you add the datastore to the proofing node. The proofing node then holds exactly the PBS rights your production nodes already have: `DatastoreBackup` lets it create backups, and `DatastorePowerUser` also lets it prune its own. Treat the proofing node like a production node as far as PBS is concerned: same admin access, same patching. Granting a separate identity `DatastoreAdmin` instead would give it more than the owner has, including deleting any backup, so it is rarely the better choice.

To find the owner, look at a backup group in the PBS web UI (Datastore > Content, the "Owner" column), or at the `username` of the PBS storage on a production node (`/etc/pve/storage.cfg`).

## Setting it up

1. **Add the datastore** on the proofing node: Datacenter > Storage > Add > Proxmox Backup Server, with the same server, datastore and namespace as production, the owner's credentials, and the encryption key (upload the key file, or paste it). Check that the storage lists your backups.
2. **Choose a VMID range and create the isolated bridge** on the proofing node: [Setup](setup.md) steps 1 and 2.
3. **Run `pbp setup` on the PBP host** ([Setup](setup.md) step 3). Answer with the proofing node's address, its bridge and restore storage, and **its** storage ID for the datastore.
4. **Run the two node lines on the proofing node** and paste the reply back ([Setup](setup.md) steps 4 and 5). Production nodes are not involved.
5. **Start PBP and run `pbp check`** ([Setup](setup.md) step 6).
6. **If PBP proofed on a production node before, retire that setup:** stop the old daemon, and remove PBP from the production node with [`pbp node uninstall`](setup.md#removing-pbp-from-a-node). Do not run both. They see the same backups and share their notes, so the second simply skips what the first proved, and both would compete for the same backups.

## Proofing on your disaster-recovery standby

**For production, make the proofing node the hardware your disaster recovery plan would restore to:** a standby host, a cold spare, or the machine at a second site. A proof there is a rehearsal of the real restore, every night:

- **It proves the path you would actually use:** that host's CPU, storage, PVE version, its access to the PBS datastore, and the encryption key and PBS credentials it holds. A proof on a production node only shows that the backup restores where it came from.
- **It catches drift on hardware that otherwise sits idle.** A missing or rotated key, expired credentials, a failing disk, an outdated PVE: each shows up as a failed or erroring proof the next morning, not in the middle of an outage. Treat such a failure as a finding about your disaster recovery, not only about one backup.
- **It keeps proofing load off production**, and production nodes need nothing from PBP.

What the standby then has to meet every night is the list in [What the proofing node needs](#what-the-proofing-node-needs): as new a PVE as production, room for your largest guests, the datastore with the owner's credentials and the encryption key.

**A cold spare that is normally off** must be running for the run window. PBP does not power hosts on: wake it shortly before the schedule (wake-on-LAN or IPMI from another host's timer, or a BIOS wake time) and let it power down afterwards if you like. If it is not up when the run starts, the run fails at preflight; because the summary is sent through the proofing node, no notification can be sent then, so watch `pbp health` from your monitoring ([Health check](operating.md#health-check)). Leave enough margin for the host to boot and for PBS, if it is woken too, to be ready.

**A standby at another site** needs the PBS datastore from there: the same PBS over the WAN, or a PBS at that site that receives the backups through a sync job (see [Proofing from a sync target](#good-to-know)). The run summary goes out through that node's notification targets, so configure them for the site.

**Proofing borrows the standby's capacity:** a VMID range, space on the restore storage and RAM, for the length of the run. Size the schedule and `restore.max_run_duration` so the window ends well before the standby might be needed, remembering that `max_run_duration` only stops new proofs from starting: proofs in progress at that point still restore, boot and are checked, so leave room for your largest guests after it, and pause PBP (`sudo systemctl stop pbp`, or `docker compose stop`) during a failover test or a real failover, so it does not compete with the guests you are bringing up.

## Good to know

- **Verdicts are visible everywhere.** Notes and the `protected` flag are stored on PBS, so production nodes see the proofing node's verdict lines in their own view of the backups.
- **Bridges and VLANs.** Every NIC of a restored guest is moved to the isolated bridge before it starts, so the proofing node does not need production's bridges for the proof. (Every backup in the test setup used a bridge that both hosts had, so a bridge missing on the proofing node was not tested.)
- **Mapped devices** (PCI or USB mappings) that the proofing node lacks end the proof `ERROR`, with the guest never started.
- **Proofing from a sync target.** If the proofing node reads a PBS that receives its backups through a sync job (for example an offsite copy), PBP proofs that copy: the verdict lines and `protected` flag change on the sync target, not on the PBS that made the backups, so look for the verdicts there. While a proof runs, the copy is protected, so pruning on the sync target skips it.
- **Encrypted backups** restore only with the key in the proofing node's storage.

Tested with Proxmox VE 9.2 and PBS 4.2: a standalone proofing node, PBP on a separate host, the owner's credentials (works) and a `DatastoreReader` token (refused as described above).
