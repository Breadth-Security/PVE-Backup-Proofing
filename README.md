# pbv

# Proxmox Backup Verify

> Trust but verify
--Ronald Reagan

> ## *Verification - In scientific methodology, verification involves gathering empirical data, performing research, or conducting experiments to demonstrate the truth of a hypothesis or model.*
>
> In the context of backups, we all understand (if not practice) the fact that an untested backup is not a backup at all. Proxmox Backup Server has made backups incredibly accessible, reliable, efficient, and stable. The validation of the backup snapshot written to disk is, however, not a validation that we will have a working product after a restore. 
>
>That's where PBV steps in. We are going to trust the backup verification, but we are going to validate that our backups will support our disaster recovery plans. 