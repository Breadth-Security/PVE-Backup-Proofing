#!/bin/sh
# Table test for packaging/versionfile.sh, plus a check that the repository's
# own VERSION file is valid. usage: packaging/versionfile_test.sh
set -u
cd "$(dirname "$0")" || exit 1
fail=0
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
check() { # VERSION-content debversion-or-"" expected ("INVALID" = must fail)
    printf '%s\n' "$1" > "$tmp/VERSION"
    if [ -n "$2" ]; then
        out=$(VERSION_FILE="$tmp/VERSION" ./versionfile.sh "$2" 2>/dev/null); rc=$?
    else
        out=$(VERSION_FILE="$tmp/VERSION" ./versionfile.sh 2>/dev/null); rc=$?
    fi
    if [ "$3" = INVALID ]; then
        [ $rc -ne 0 ] && r=ok || r="FAIL (got $out)"
    else
        [ $rc -eq 0 ] && [ "$out" = "$3" ] && r=ok || r="FAIL (got '$out' rc=$rc)"
    fi
    case "$r" in FAIL*) fail=1 ;; esac
    printf '%-8s %-20s -> %-8s %s\n' "$1" "${2:-(none)}" "$3" "$r"
}
check 0.2.0 "" 0.2.0
check 10.20.30 "" 10.20.30
check 0.2 "" INVALID
check v0.2.0 "" INVALID
check 0.2.0-rc1 "" INVALID
check 0.02.0 "" INVALID
check dev "" INVALID
check "" "" INVALID
check 0.2.0 0.2.0 0.2.0
check 0.2.0 '0.2.0~rc1' 0.2.0
check 0.2.0 '0.2.0+build5' 0.2.0
check 0.2.0 0.2.1 INVALID
check 0.2.0 '0.3.0~rc1' INVALID
check 0.2.0 '0.0.0~dev+abc1234' INVALID
# The repository's own VERSION file.
out=$(./versionfile.sh 2>&1); rc=$?
if [ $rc -eq 0 ]; then
    printf 'repository VERSION %s ok\n' "$out"
else
    printf 'repository VERSION FAIL: %s\n' "$out"; fail=1
fi
exit $fail
