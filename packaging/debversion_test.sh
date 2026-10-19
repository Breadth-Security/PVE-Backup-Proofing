#!/bin/sh
# Table test for packaging/debversion.sh. usage: packaging/debversion_test.sh
set -u
cd "$(dirname "$0")" || exit 1
fail=0
check() { # input expected ("INVALID" = must fail)
    out=$(./debversion.sh "$1" 2>/dev/null); rc=$?
    if [ "$2" = INVALID ]; then
        [ $rc -ne 0 ] && r=ok || r="FAIL (got $out)"
    else
        [ $rc -eq 0 ] && [ "$out" = "$2" ] && r=ok || r="FAIL (got '$out' rc=$rc)"
    fi
    case "$r" in FAIL*) fail=1 ;; esac
    printf '%-22s -> %-14s %s\n' "$1" "$2" "$r"
}
check v1.2.0 1.2.0
check 1.2.0 1.2.0
check v1.2.0-rc1 '1.2.0~rc1'
check v1.2.0-rc.1 '1.2.0~rc.1'
check v0.0.0~dev+abc1234 '0.0.0~dev+abc1234'
check v1.2.0-rc1-2 INVALID
check vfoo INVALID
check v1.2.0_x INVALID
check v INVALID
exit $fail
