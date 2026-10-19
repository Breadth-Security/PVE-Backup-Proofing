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

# Step 4: authorized_keys, one line, forced command and no forwarding.
ARG PBP_PUBKEY
RUN test -n "$PBP_PUBKEY" \
    && install -d -m 0700 -o pbp -g pbp /var/lib/pbp/.ssh \
    && printf 'command="sudo /usr/bin/pbp node",no-pty,no-agent-forwarding,no-port-forwarding,no-X11-forwarding,no-user-rc %s\n' "$PBP_PUBKEY" \
        > /var/lib/pbp/.ssh/authorized_keys \
    && chown pbp:pbp /var/lib/pbp/.ssh/authorized_keys \
    && chmod 0600 /var/lib/pbp/.ssh/authorized_keys

# Step 5: the one sudo rule, checked with visudo before it is installed.
COPY node/sudoers /tmp/sudoers-pbp
RUN visudo -cf /tmp/sudoers-pbp \
    && install -m 0440 -o root -g root /tmp/sudoers-pbp /etc/sudoers.d/pbp \
    && rm /tmp/sudoers-pbp \
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
