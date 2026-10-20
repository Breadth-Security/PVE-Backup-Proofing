# A throwaway "Proxmox node" for the pbp node helper end-to-end test: Debian 13
# with sshd and sudo and PBP installed from its .deb, but NOT set up: the test
# sets it up with `pbp node setup`, as an admin would. Proxmox tools are
# stubs; the bridge is a real Linux bridge built at start (node/entrypoint.sh).
#
# Build context (assembled by nodee2e_test.go): this file, node/, pbp.deb and
# pve-ssl.pem (a self-signed certificate the test generates).
FROM debian:trixie

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        openssh-server sudo iproute2 procps adduser \
    && rm -rf /var/lib/apt/lists/*

# Install the helper from the package, and nothing more.
COPY pbp.deb /tmp/pbp.deb
RUN DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends /tmp/pbp.deb && rm /tmp/pbp.deb

# The node starts unconfigured: no helper config, no shell for pbp, no key,
# no sudo rule (docs/manual-setup.md "Node setup" steps 2-6 are setup's job).
RUN test ! -e /etc/pbp/node.yaml \
    && test "$(getent passwd pbp | cut -d: -f7)" = /usr/sbin/nologin \
    && test ! -e /var/lib/pbp/.ssh \
    && test ! -e /etc/sudoers.d/pbp

# sshd host keys (setup's reply carries the ed25519 one) and its run dir.
# The uninstall test makes many failing logins in a row: without penalties
# each one is refused at authentication instead of the source being shut out.
RUN ssh-keygen -A && install -d -m 0755 /run/sshd \
    && printf 'PerSourcePenalties no\n' > /etc/ssh/sshd_config.d/pbp-e2e.conf

# Stub Proxmox: /etc/pve (guest configs, the node certificate, the template
# dir), pvesh, pveum (state under /var/lib/pbp-e2e), pct, ovs-vsctl, and perl
# standing in for PVE::Notify (the real perl is diverted, so it must come last).
COPY node/lxc/ /etc/pve/lxc/
COPY pve-ssl.pem /etc/pve/local/pve-ssl.pem
RUN install -d -m 0755 /etc/pve/notification-templates/default /var/log/pbp-e2e /var/lib/pbp-e2e \
    && install -d -m 0700 /etc/pve/priv \
    && touch /etc/pve/priv/token.cfg \
    && printf 'root@pam\n' > /var/lib/pbp-e2e/users \
    && printf 'PVEAuditor\tDatastore.Audit,Sys.Audit,VM.Audit\t1\n' > /var/lib/pbp-e2e/roles \
    && : > /var/lib/pbp-e2e/tokens \
    && : > /var/lib/pbp-e2e/acls
COPY node/stubs/pvesh /usr/bin/pvesh
COPY node/stubs/pveum /usr/sbin/pveum
COPY node/stubs/pct /usr/sbin/pct
COPY node/stubs/ovs-vsctl /usr/bin/ovs-vsctl
COPY node/entrypoint.sh /usr/local/sbin/pbp-e2e-entrypoint
RUN dpkg-divert --local --rename --add /usr/bin/perl
COPY node/stubs/perl /usr/bin/perl

ENTRYPOINT ["/usr/local/sbin/pbp-e2e-entrypoint"]
