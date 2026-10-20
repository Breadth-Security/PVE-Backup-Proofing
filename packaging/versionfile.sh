#!/bin/sh
# Read and check the top-level VERSION file, the version a build without an
# explicit version is stamped with (the Docker image, `docker compose build`).
#
# usage: packaging/versionfile.sh [debian-version]
#   Prints VERSION if it is a plain MAJOR.MINOR.PATCH (no "v", no suffix),
#   else fails. Given a Debian version from packaging/debversion.sh (such as
#   1.2.0~rc1), it also fails unless that version's MAJOR.MINOR.PATCH, before
#   any "~" or "+" suffix, equals VERSION. The release workflow uses this so a
#   tag cannot publish packages whose number disagrees with VERSION.
# VERSION_FILE overrides the file's path (for tests).
set -eu

f="${VERSION_FILE:-$(dirname "$0")/../VERSION}"
[ -r "$f" ] || { echo "cannot read $f" >&2; exit 1; }
v=$(tr -d '[:space:]' < "$f")
if ! printf '%s\n' "$v" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; then
    echo "VERSION must be MAJOR.MINOR.PATCH (e.g. 0.2.0), got: '$v'" >&2
    exit 1
fi
if [ $# -ge 1 ]; then
    base="${1%%[~+]*}"
    if [ "$base" != "$v" ]; then
        echo "version $1 does not match VERSION $v; update VERSION before tagging" >&2
        exit 1
    fi
fi
printf '%s\n' "$v"
