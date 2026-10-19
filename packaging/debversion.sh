#!/bin/sh
# Map a release tag to a Debian package version.
#   v1.2.0      -> 1.2.0
#   v1.2.0-rc1  -> 1.2.0~rc1   (the first "-" becomes "~" so it sorts before 1.2.0)
# The result is used for the .deb, the tarballs and every release asset name.
# Prints the version, or an error on stderr and exit 1 if the result is not a
# valid Debian upstream version for a native package (no hyphen allowed).
#
# usage: packaging/debversion.sh <tag-or-version>
set -eu

v="${1#v}"
# Replace only the first hyphen.
case "$v" in
    *-*) v="${v%%-*}~${v#*-}" ;;
esac
if ! printf '%s\n' "$v" | grep -Eq '^[0-9][0-9A-Za-z.+~]*$'; then
    echo "not a valid Debian version: $1 (mapped to: $v)" >&2
    exit 1
fi
printf '%s\n' "$v"
