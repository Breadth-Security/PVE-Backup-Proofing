Main functional requirements

1. The ability to pull from a Proxmox Backup Server storage repository, identified by a storage entity of type "backup" on any proxmox host
2. For any LXC or VM backup, the ability to restore to a temporary host, and verify that the container or VM starts successfully. 
3. The temporary host should be programatically created, isolated from any existing networks, and destroyed after use
4. Guest VM's without the Guest Agent installed should be noted and reported to the user in the main log output. 
5. For guest VM's with Guest Agent installed, the status of the guest agent should be read in order to verify a successful startup. 
6. For LXC's (containers), the startup should be verified by reading available logs of the LXC directly, using available pct status and pct exec commands. 
7. The distribution method for the product should be a docker container, compose enabled with example docker-compose.yml file

All defined specifications should link back to one of these primary functional requirements. 