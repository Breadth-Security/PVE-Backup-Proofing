# PBV - Proxmox Backup Validation tool

> "Trust but verify"
--Ronald Reagan

> ## *Validation - the act of proving, verifying, or substantiating that something is true, correct, or officially acceptable. It confirms that a claim, process, or product meets specified requirements, functions as intended, and operates within expected parameters.*
>
> In the context of backups, we all understand the fact that an untested backup <b>is not</b> a backup at all. As we gain experience, we put into place practices which speak to this reality. 
>
>Proxmox Backup Server has made backups incredibly accessible, reliable, efficient, and stable. The validation of the backup snapshot written to disk is, however, not a validation that we will have a working product after a restore. 
>
>That's where PBV steps in. We are going to <i>trust</i> the backup existing verification, but we are going to <i>validate</i> that our backups will support our disaster recovery plans. 