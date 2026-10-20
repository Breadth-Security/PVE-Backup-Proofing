#!/bin/sh
# Tests packaging/scripts/postinst's service handling with a fake systemctl
# (and fake getent, adduser, install) on a temp copy of the script whose
# /run/systemd/system, /etc/pbp and /var/lib/pbp point into a temp dir.
# The script must never start pbp.service: an install only enables it, and
# an upgrade only try-restarts it (restarts it if it is running).
# usage: packaging/postinst_test.sh
set -u
cd "$(dirname "$0")" || exit 1
fail=0
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
for c in getent adduser install; do
    printf '#!/bin/sh\nexit 0\n' > "$tmp/bin/$c"
done
# systemctl logs its arguments; is-enabled and is-active answer from the
# FAKE_ENABLED and FAKE_ACTIVE variables.
cat > "$tmp/bin/systemctl" <<'EOS'
#!/bin/sh
echo "$*" >> "$FAKE_LOG"
case "$1" in
is-enabled) [ "${FAKE_ENABLED:-0}" = 1 ] ;;
is-active) [ "${FAKE_ACTIVE:-0}" = 1 ] ;;
*) exit 0 ;;
esac
EOS
chmod +x "$tmp/bin/"*

# check NAME OLD-VERSION CONFIG(0/1) MARKER(0/1) ENABLED ACTIVE EXPECTED-LOG
check() {
    r="$tmp/case"
    rm -rf "$r"
    mkdir -p "$r/run/systemd/system" "$r/etc/pbp" "$r/var/lib/pbp"
    [ "$3" = 1 ] && : > "$r/etc/pbp/config.yaml"
    [ "$4" = 1 ] && : > "$r/var/lib/pbp/.disabled-by-remove"
    sed -e "s#/run/systemd/system#$r/run/systemd/system#g" \
        -e "s#/etc/pbp#$r/etc/pbp#g" -e "s#/var/lib/pbp#$r/var/lib/pbp#g" \
        scripts/postinst > "$tmp/postinst"
    : > "$tmp/log"
    PATH="$tmp/bin:$PATH" FAKE_LOG="$tmp/log" FAKE_ENABLED="$5" FAKE_ACTIVE="$6" \
        sh "$tmp/postinst" configure "$2" >/dev/null 2>&1
    rc=$?
    got=$(tr '\n' ';' < "$tmp/log")
    if [ $rc -eq 0 ] && [ "$got" = "$7" ]; then
        res=ok
    else
        res="FAIL (rc=$rc, got '$got')"
        fail=1
    fi
    printf '%-40s %s\n' "$1" "$res"
}
check "install, no config" "" 0 0 0 0 "daemon-reload;enable pbp.service;"
check "install with a stray config" "" 1 0 0 0 "daemon-reload;enable pbp.service;"
check "upgrade, enabled, inactive, config" 0.2.0 1 0 1 0 "daemon-reload;try-restart pbp.service;"
check "upgrade, running" 0.2.0 1 0 1 1 "daemon-reload;try-restart pbp.service;"
check "upgrade, no config" 0.2.0 0 0 1 0 "daemon-reload;try-restart pbp.service;"
check "reinstall after remove, config" 0.2.0 1 1 0 0 "daemon-reload;enable pbp.service;try-restart pbp.service;"
if grep -Eq 'systemctl (start|restart) ' scripts/postinst; then
    echo "FAIL: postinst starts or restarts pbp.service unconditionally"
    fail=1
fi
exit $fail
