# Security model

PBP restores copies of your production guests and boots them on a production Proxmox node. This page explains what keeps that safe: what PBP may touch, how the copies are isolated, and how little the PBP host can do on the node. The setup commands enforce all of it; you do not have to configure it by hand.

## What PBP touches: the VMID range and the `pbp` tag

PBP restores into a range of VMIDs you choose (for example `990000`-`990049`), names each temporary guest `pbp-<vmid>-<timestamp>` and tags it `pbp`.

> **PBP owns every guest in the range that carries the tag `pbp`, and it will destroy it.** Never tag your own guests `pbp`, and never put your own guests in the range. PBP never creates, changes, starts, stops or destroys anything outside the range.

- **A guest tagged `pbp` outside the range** is never touched either. Each run reports it in an "Environment errors" section of the notification (and logs a warning), for example `VM 105 (web) is tagged pbp but outside the PBP range 990000–990049; not touched`, and the run's severity is at least `warning`. Remove the tag or move the guest to resolve it.
- **A guest in the range without the `pbp` tag** stops PBP: it refuses to run until you remove it. The one exception is the leftover of a PBP restore that failed before it could be tagged: PBP's state file records the VMID as an in-flight restore, the guest is stopped, and its name (hostname for a container) matches the backup's original config, or it is still locked by the restore. PBP removes such a leftover itself, at once or through the janitor on the next run. For any other untagged guest, `pbp check` names the VMID; confirm it is not yours and remove it on the node with `qm destroy <vmid> --purge` (VM) or `pct destroy <vmid> --purge` (container).
- **More than one PBP instance** against the same cluster needs **one range each, with no overlap.** The janitor destroys any `pbp`-tagged guest in its range that it does not know about (leftovers from a crash), so a shared range lets one instance destroy another's guests mid-proof.

PBP changes only two attributes of a backup: its `notes` (one verdict line) and, during a proof, `protected` (so pruning cannot remove it mid-proof; the original value is restored afterwards).

## The isolated bridge

Restored guests are plugged into a bridge that has no physical ports, no bond members, no IP address and IPv6 disabled. They can talk to each other but not to your network or the node, so a restored copy of a server cannot take over its production twin's address or reach anything real. Every NIC of a restored guest is moved to this bridge, and its `onboot` is cleared, before it is ever started. A container's `ip=dhcp` and `ip6=auto`/`ip6=dhcp` become `manual`: nothing on the bridge answers them, and the container's network service would otherwise wait for an answer and never finish booting. Static addresses are kept; anything that cannot be made safe (host passthrough PBP cannot remove, for example) ends as `ERROR` with the guest never started.

**Why IPv6 must be disabled on the bridge.** Even with no address configured, the kernel gives every interface with IPv6 enabled a link-local address (`fe80::...`). The PVE API does not show it, but a proof guest on the bridge could use it to reach the node's own SSH and web UI. Worse, a restored router or firewall guest that sends IPv6 router advertisements could give the node a SLAAC address and a route through the bridge. With `disable_ipv6` set, the node has no presence on the bridge at all.

At every preflight PBP reads the bridge's live state on the node and refuses to run unless `/proc/sys/net/ipv6/conf/<bridge>/disable_ipv6` is `1`, the bridge has no address of any kind, and its only members are interfaces of guests in the reserved VMID range (`tapN...`, `vethN...`, `fwprN...`, `fwlnN...`): no physical NIC, bond, VLAN, OVS internal port, or production guest. How to create the bridge: [Setup step 2](setup.md#2-create-the-isolated-bridge).

## How the PBP host reaches the node

PBP uses the Proxmox API with a token for almost everything. A few things have no API equivalent: `pct exec` (checking inside containers), the bridge's live state (IPv6, addresses, members), and arbitrary notifications (`PVE::Notify`; the notifications REST API can only send a fixed test message). For these, PBP logs in over SSH as the unprivileged `pbp` user. **PBP never uses root SSH.**

That login is locked to one command. The key in `/var/lib/pbp/.ssh/authorized_keys` is marked `restrict,command="sudo /usr/bin/pbp node"`, so whatever the client asks for, sshd runs only the `pbp node` helper (no shell, no forwarding, no pty), and one sudo rule lets `pbp` run exactly that command as root. The helper does these four things, and only for guests it is allowed to touch:

- it runs `pct exec` only in a container whose VMID is inside its `vmid_range` and which carries the `pbp` tag, and passes the command as an argument list, never through a shell,
- it reports the one configured bridge's state, read-only,
- it sends notifications of type `pbp` through `PVE::Notify`, and
- it prints its own configuration and version.

**What a stolen PBP key gets:** those four things, within the range. It does not get a shell, any other command, a guest outside the range or untagged, forwarding, or root. The range, tag and bridge are read from `/etc/pbp/node.yaml` on the node, which the key cannot change. The key file, its `.ssh` directory and the `pbp` account's home `/var/lib/pbp` all belong to root, so a process running as `pbp` cannot rewrite its own forced command or swap the key file. `pbp node setup` also checks that sshd reads `pbp`'s keys from nowhere else (no `authorized_keys2`, no other `AuthorizedKeysFile`, no `AuthorizedKeysCommand`, no SSH user CA) and that the sudo rule is exactly the one above.

The node still trusts PBP's API token (below), so protect the PBP host and its secrets as you would any admin credential.

## The API token

`pbp node setup` creates the role `PBPProofer`, the user `pbp@pve` and the privilege-separated token `pbp@pve!proof`, and grants the role on the paths below. This list was confirmed against PVE 9.2 by the integration suite (see `test/integration/README.md`):

| Privilege | Path | Needed for |
|-----------|------|------------|
| `VM.Allocate` | `/vms` | restore into a new VMID, destroy with purge |
| `VM.Backup` | `/vms` | read the config inside a backup (`vzdump/extractconfig`), restore from a backup, and write `notes`/`protected` on backup volumes |
| `VM.Audit` | `/vms` | list guests (PBP must see every guest in the VMID range), read configs and status |
| `VM.Config.Network`, `VM.Config.Options`, `VM.Config.HWType` | `/vms` | sanitize: NICs, tags, name, `onboot`, `protection`, hookscript, removing `rng`/USB/PCI devices |
| `VM.PowerMgmt` | `/vms` | start and stop the temporary guest |
| `VM.GuestAgent.Audit`, `VM.GuestAgent.Unrestricted` | `/vms` | agent ping and OS info; running the checks through the agent |
| `Datastore.AllocateSpace`, `Datastore.Audit` | the restore storage and each PBS storage | allocate restored disks; list backups and read and write their notes and `protected` flag |
| `Sys.Audit` | `/nodes/<node>` | node version, memory and the bridge check |
| `SDN.Use` | `/sdn/zones/localnetwork` | see below |

`Datastore.Allocate` is **not** needed and should not be granted: on a PBS storage it allows deleting any backup.

`SDN.Use` is needed on the bridges the **original** guests use, not only on the isolated bridge: PVE checks the backed-up NIC configuration (for example `bridge=vmbr0,tag=2`) when it restores, before PBP can move the NICs to the isolated bridge. Granting it on `/sdn/zones/localnetwork` covers every local bridge and VLAN. The restored guest is never started until every NIC is on the isolated bridge.

`/vms` cannot be narrowed to the VMID range in PVE's permission model, so the token could act on every guest. The reserved range is enforced by PBP itself; the token is the second line, not the first.

`pbp node setup` warns, without changing anything, when `pbp@pve` or one of its tokens has more than this: another role or another path in an ACL, membership in a group (whose permissions apply too), or a second API token. Grant extra only on purpose.

**Guests PBP cannot proof with this token** (each ends `ERROR`, never started; the backup is not blamed):

- **Privileged containers**: restoring one needs `Sys.Modify` on `/`.
- **Containers with raw `lxc.*` lines** (for example `lxc.apparmor.profile: unconfined`): PVE restores them only for `root@pam` and silently drops them for PBP's token, so the copy would not be the guest it stands for. PBP reads the backed-up config and ends `ERROR` (`raw lxc config (…) needs root@pam to restore`) before restoring anything.
- **Containers with `devN` passthrough or a hookscript**, and **VMs with non-mapped (raw) PCI or USB passthrough**: changing those keys is reserved to `root@pam`. VMs with *mapped* devices need `Mapping.Use` on the mapping as well.

Granting `Sys.Modify` on `/` lifts the first restriction but also lets the token change node-wide settings; weigh that before doing it. Proofing privileged containers also weakens what a stolen PBP SSH key can do: the helper runs `pct exec` in PBP's tagged containers, and in a privileged container that command runs as real root (uid 0 on the node), held back only by the container's namespaces and AppArmor profile instead of an unprivileged uid mapping. If that matters more to you than proofing those backups, leave them out: a privileged container that PBP cannot restore simply ends `ERROR` each run.

## Where to run PBP

**For production, run PBP on a separate host, outside the Proxmox node or cluster it proofs.** Any small VM or machine elsewhere will do: PBP itself needs little, since the restores and boots happen on the node. Running PBP on the node itself works and is supported ([PBP on the Proxmox node itself](setup.md#pbp-on-the-proxmox-node-itself)), and suits a single-node lab or a trial; for production it gives up these:

- **A warning when the node cannot send one.** The run summary is sent through the node. When the node, its SSH or its API is down, only a PBP outside it notices: its health check turns unhealthy and the scheduled run fails visibly. A PBP on the node goes down with it, and the night is silent.
- **PBP off the hypervisor.** On the node, PBP's daemon, its API token and its SSH key sit on the machine that runs your production guests. A separate host keeps that software and those secrets away from the hypervisor: a compromised PBP host still holds the API token (so protect it), but it is not a shell on the hypervisor, and a compromised node does not also hand over PBP's own host.
- **Independence from node maintenance and failure.** Upgrades and reboots of the node do not interrupt it, and losing a node does not take PBP's config and state with it: a tool whose job is proving your disaster recovery should not depend on the machine it is checking.
- **Independence from the cluster.** In a cluster, another node of the same cluster is not separate enough: it shares the cluster's health, network and maintenance. Run PBP outside it.

## What setup never touches

`pbp setup` and `pbp node setup` never create the bridge or reload networking, never touch root's `authorized_keys`, `/etc/ssh/sshd_config` or any guest, and never replace a file they did not write without `--replace` (which backs the old file up first). Both run only as root in an interactive terminal; the node side refuses to run from an SSH forced command or through the `pbp` account's sudo rule, so a stolen PBP key cannot reach them.
