#!/bin/sh
# Table test for packaging/release-notes.sh against a fixture CHANGELOG, plus
# a check of the repository's own CHANGELOG for the VERSION file's release.
# usage: packaging/release-notes_test.sh
set -u
cd "$(dirname "$0")" || exit 1
fail=0
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
gh=https://github.com/Breadth-Security/PVE-Backup-Proofing/blob

cat > "$tmp/CHANGELOG.md" <<'EOF'
# Changelog

## Unreleased

Next things.

## v1.2.0

**A minor release: upgrade both sides.**

- Fix `a | b` with [docs](docs/x.md#y), [abs](https://example.com/z), [anchor](#v110), [readme](./README.md) and ✅.
- ![logo](docs/logo.png) `[not](a-link)`


## v1.1.0

## v1.0.0

Old.
EOF
cat > "$tmp/no-unreleased.md" <<'EOF'
# Changelog

## Unreleased

## v1.0.0

Old.
EOF

check() { # name changelog tag expected-file-or-"FAIL" [message-substring]
    out=$(CHANGELOG_FILE="$2" ./release-notes.sh "$3" 2>"$tmp/err"); rc=$?
    if [ "$4" = FAIL ]; then
        if [ $rc -ne 0 ] && grep -qF -- "${5:-}" "$tmp/err"; then r=ok; else r="FAIL (rc=$rc out='$out' err='$(cat "$tmp/err")')"; fi
    elif [ $rc -eq 0 ] && [ "$out" = "$(cat "$4")" ]; then
        r=ok
    else
        r="FAIL (rc=$rc err='$(cat "$tmp/err")')"
        printf '%s\n' "$out" > "$tmp/got"
        diff "$4" "$tmp/got" | sed 's/^/    /'
    fi
    case "$r" in FAIL*) fail=1 ;; esac
    printf '%-34s %s\n' "$1" "$r"
}

cat > "$tmp/want-1.2.0" <<EOF
**A minor release: upgrade both sides.**

- Fix \`a | b\` with [docs]($gh/v1.2.0/docs/x.md#y), [abs](https://example.com/z), [anchor](#v110), [readme]($gh/v1.2.0/README.md) and ✅.
- ![logo]($gh/v1.2.0/docs/logo.png) \`[not](a-link)\`
EOF
sed 's#/blob/v1.2.0/#/blob/v1.2.0-rc.1/#g' "$tmp/want-1.2.0" > "$tmp/want-1.2.0-rc.1"
printf 'Next things.\n' > "$tmp/want-next"
printf 'Old.\n' > "$tmp/want-old"

check "final release"                "$tmp/CHANGELOG.md" v1.2.0 "$tmp/want-1.2.0"
check "pre-release, final section"   "$tmp/CHANGELOG.md" v1.2.0-rc.1 "$tmp/want-1.2.0-rc.1"
check "pre-release, Unreleased"      "$tmp/CHANGELOG.md" v1.3.0-rc.1 "$tmp/want-next"
check "last section"                 "$tmp/CHANGELOG.md" v1.0.0 "$tmp/want-old"
check "no section"                   "$tmp/CHANGELOG.md" v1.3.0 FAIL "## v1.3.0"
check "empty section"                "$tmp/CHANGELOG.md" v1.1.0 FAIL "## v1.1.0"
check "pre-release, nothing to use"  "$tmp/no-unreleased.md" v2.0.0-rc.1 FAIL "## Unreleased"
check "heading prefix is no match"   "$tmp/CHANGELOG.md" v1.2 FAIL "not a release tag"
check "tag without v"                "$tmp/CHANGELOG.md" 1.2.0 FAIL "not a release tag"
check "no tag"                       "$tmp/CHANGELOG.md" "" FAIL "usage"
check "missing changelog"            "$tmp/none.md" v1.2.0 FAIL "none.md"

# The repository's own CHANGELOG has notes for the VERSION file's release (or
# Unreleased ones while it is being written), with no relative link left.
v=$(cat ../VERSION)
if out=$(./release-notes.sh "v$v-rc.0" 2>&1) && [ -n "$out" ]; then
    if printf '%s\n' "$out" | grep -qE '\]\((docs/|\./|[A-Za-z0-9_-]+\.md)'; then
        printf 'repository CHANGELOG v%s       FAIL: relative link left\n' "$v"; fail=1
    else
        printf 'repository CHANGELOG v%s       ok\n' "$v"
    fi
else
    printf 'repository CHANGELOG v%s       FAIL: %s\n' "$v" "$out"; fail=1
fi
exit $fail
