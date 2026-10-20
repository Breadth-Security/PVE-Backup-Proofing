#!/bin/sh
# Container start: build the isolated bridge for real (needs NET_ADMIN), with
# one dummy member named like a proof guest's port (VMID 990005, inside the
# test range), then run sshd in the foreground logging to stderr.
#
# IPv6 must be off on the bridge, as README's post-up line does on a node.
# Docker mounts /proc/sys read-only, so the test starts the container with
# --sysctl net.ipv6.conf.default.disable_ipv6=1 and the new bridge inherits
# it; the write below covers a runtime that allows it, and the check makes
# sure one of the two worked.
set -e
ip link add vmbr99 type bridge
ip link add tap990005i0 type dummy
ip link set tap990005i0 master vmbr99
ip link set tap990005i0 up
echo 1 2>/dev/null > /proc/sys/net/ipv6/conf/vmbr99/disable_ipv6 || true
if [ "$(cat /proc/sys/net/ipv6/conf/vmbr99/disable_ipv6)" != 1 ]; then
    echo "entrypoint: cannot disable IPv6 on vmbr99" >&2
    exit 1
fi
ip link set vmbr99 up
exec /usr/sbin/sshd -D -e
