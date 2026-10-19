#!/bin/sh
# Container start: build the isolated bridge for real (needs NET_ADMIN), with
# one dummy member, then run sshd in the foreground logging to stderr.
set -e
ip link add vmbr99 type bridge
ip link add pbpe2e0 type dummy
ip link set pbpe2e0 master vmbr99
ip link set pbpe2e0 up
ip link set vmbr99 up
exec /usr/sbin/sshd -D -e
