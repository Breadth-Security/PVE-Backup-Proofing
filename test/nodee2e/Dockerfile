# A throwaway "Proxmox node" for the pbp node helper end-to-end test: Debian 13
# with sshd and sudo, PBP installed from its .deb, and the node set up exactly
# as README "Node setup" (design §5) says. Proxmox tools are stubs; the bridge
# is a real Linux bridge built at start (see node/entrypoint.sh).
#
# Build context (assembled by nodee2e_test.go): this file, node/, pbp.deb.
FROM debian:trixie

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        openssh-server sudo iproute2 procps adduser \
    && rm -rf /var/lib/apt/lists/*

# Step 1: install the helper from the package.
COPY pbp.deb /tmp/pbp.deb
RUN DEBIAN_FRONTEND=noninteractive apt-get install -y /tmp/pbp.deb && rm /tmp/pbp.deb

# Step 2: /etc/pbp/node.yaml.
COPY node/node.yaml /tmp/node.yaml
RUN install -m 0640 -o root -g pbp /tmp/node.yaml /etc/pbp/node.yaml && rm /tmp/node.yaml

# Step 3: a shell for the forced command; home stays 0700 and owned by pbp.
RUN usermod -s /bin/sh pbp \
    && test "$(stat -c '%a %U' /var/lib/pbp)" = "700 pbp"

# Step 4: authorized_keys, one line, restrict plus the forced command, owned
# by root so pbp cannot rewrite it.
ARG PBP_PUBKEY
RUN test -n "$PBP_PUBKEY" \
    && install -d -m 0755 -o root -g root /var/lib/pbp/.ssh \
    && printf 'restrict,command="sudo /usr/bin/pbp node" %s\n' "$PBP_PUBKEY" \
        > /var/lib/pbp/.ssh/authorized_keys \
    && chown root:root /var/lib/pbp/.ssh/authorized_keys \
    && chmod 0644 /var/lib/pbp/.ssh/authorized_keys

# Step 5: the one sudo rule, checked with visudo before it is installed.
# (README writes it through mktemp; a build has no other users, so the copied
# file is staged in /root.)
COPY node/sudoers /root/sudoers-pbp
RUN visudo -cf /root/sudoers-pbp \
    && install -m 0440 -o root -g root /root/sudoers-pbp /etc/sudoers.d/pbp \
    && rm /root/sudoers-pbp \
    && visudo -c

# Stub Proxmox: container configs, pct, ovs-vsctl, and perl standing in for
# PVE::Notify (the real perl is diverted, so it must come last).
COPY node/lxc/ /etc/pve/lxc/
COPY node/stubs/pct /usr/sbin/pct
COPY node/stubs/ovs-vsctl /usr/bin/ovs-vsctl
COPY node/entrypoint.sh /usr/local/sbin/pbp-e2e-entrypoint
RUN install -d -m 0755 /var/log/pbp-e2e /run/sshd \
    && dpkg-divert --local --rename --add /usr/bin/perl
COPY node/stubs/perl /usr/bin/perl

EXPOSE 22
ENTRYPOINT ["/usr/local/sbin/pbp-e2e-entrypoint"]
