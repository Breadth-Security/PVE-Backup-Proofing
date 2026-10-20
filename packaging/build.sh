#!/usr/bin/env bash
# Build release artifacts: static binaries, .deb packages, tarballs and
# SHA256SUMS for amd64 and arm64.
#
# usage: packaging/build.sh <version> <outdir>
#   version: Debian-valid, without a leading "v" (e.g. 1.2.0, 0.0.0~dev+abc123)
set -euo pipefail

NFPM_VERSION="v2.48.0"

if [ $# -ne 2 ]; then
    echo "usage: $0 <version> <outdir>" >&2
    exit 2
fi
version="$1"
outdir="$2"

case "$version" in
    [0-9]*) ;;
    *) echo "version must start with a digit (no leading v): $version" >&2; exit 2 ;;
esac

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
mkdir -p "$outdir"
outdir="$(cd "$outdir" && pwd)"
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT

export VERSION="$version"
# Debian requires a changelog, even for native packages.
CHANGELOG="$stage/changelog.gz"
export CHANGELOG
{
    printf 'pbp (%s) unstable; urgency=medium\n\n  * Release %s.\n\n' "$version" "$version"
    printf ' -- Tony D <tony.diana@gmail.com>  %s\n' "$(date -R)"
} | gzip -9n > "$CHANGELOG"
for arch in amd64 arm64; do
    bin="$stage/$arch/pbp"
    mkdir -p "$stage/$arch"
    # Same build as the Dockerfile.
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
        -ldflags "-s -w -X main.version=${version}" -o "$bin" ./cmd/pbp

    # shellcheck disable=SC2016 # envsubst takes a literal list of variable names
    ARCH="$arch" BIN="$bin" envsubst '${ARCH} ${VERSION} ${BIN} ${CHANGELOG}' \
        < packaging/nfpm.yaml > "$stage/nfpm-$arch.yaml"
    go run "github.com/goreleaser/nfpm/v2/cmd/nfpm@${NFPM_VERSION}" package \
        --config "$stage/nfpm-$arch.yaml" --packager deb \
        --target "$outdir/pbp_${version}_${arch}.deb"

    tdir="$stage/pbp_${version}_linux_${arch}"
    mkdir -p "$tdir"
    cp "$bin" packaging/pbp.service README.md LICENSE config.example.yaml "$tdir/"
    cp -r docs "$tdir/"
    tar --sort=name --owner=0 --group=0 --numeric-owner --mtime='UTC 1970-01-01' \
        -czf "$outdir/pbp_${version}_linux_${arch}.tar.gz" -C "$stage" "pbp_${version}_linux_${arch}"
done

(cd "$outdir" && sha256sum pbp_"${version}"_* > SHA256SUMS)
echo "built in $outdir:"
ls -l "$outdir"
