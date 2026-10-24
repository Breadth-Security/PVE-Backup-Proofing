#!/usr/bin/env bash
# Runs the pbp node helper end-to-end test (build tag nodee2e) on a Docker
# host given as the argument or $PBP_E2E_HOST: rsyncs the repo to /root/pbp there, builds
# the .deb with packaging/build.sh, and runs the test, which installs that
# .deb in a throwaway Debian 13 container and talks to it over real SSH.
#
# usage: test/nodee2e/run.sh ssh-host     (or set PBP_E2E_HOST)
# The host needs Docker, Go, rsync, an OpenSSH client and envsubst
# (gettext-base). Never point it at a Proxmox node.
set -euo pipefail

host="${1:-${PBP_E2E_HOST:-}}"
[ -n "$host" ] || { echo "usage: test/nodee2e/run.sh ssh-host (or set PBP_E2E_HOST)" >&2; exit 2; }
root="$(cd "$(dirname "$0")/../.." && pwd)"

# Never run this on a Proxmox node: it builds images, starts containers and
# rsyncs --delete into /root/pbp. A PVE node has /etc/pve.
if ssh "$host" 'test -e /etc/pve'; then
    echo "run.sh: $host has /etc/pve, it looks like a Proxmox node; refusing to run" >&2
    exit 2
fi

rsync -a --delete \
    --exclude .git --exclude .superpowers --exclude .remember --exclude dist \
    "$root/" "$host:/root/pbp/"

# shellcheck disable=SC2016 # expanded on the remote host
script='
set -euo pipefail
cd /root/pbp
command -v envsubst >/dev/null || { echo "envsubst missing: apt install gettext-base" >&2; exit 2; }
version="0.0.0~e2e"
out="$(mktemp -d)"
trap "rm -rf \"$out\"" EXIT
packaging/build.sh "$version" "$out" >/dev/null
PBP_E2E_DEB="$out/pbp_${version}_amd64.deb" PBP_E2E_VERSION="$version" \
    go test -count=1 -tags nodee2e ./test/nodee2e/ -v
'
ssh "$host" "bash -lc $(printf '%q' "$script")"
