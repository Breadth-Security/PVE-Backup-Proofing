#!/bin/sh
# Prints the release notes for a tag: its section of CHANGELOG.md, without the
# "## v<version>" heading (the release title names the version). The release
# workflow sets them as the release's description (issue #105).
#
# usage: packaging/release-notes.sh v<version>      (v1.2.0, v1.2.0-rc.1)
#   CHANGELOG_FILE overrides the CHANGELOG path (for tests).
#
# A pre-release uses its final version's section if there is one, else
# "## Unreleased". Fails when there are no notes to use, so a release cannot
# ship without them.
#
# Relative links (docs/x.md#y) become absolute GitHub links pinned to the
# tag: a release page has no repository base, and readers see releases on the
# public GitHub mirror, never on the private Gitea. Links inside code spans,
# absolute links and in-page anchors are left as they are.
set -u
repo=https://github.com/Breadth-Security/PVE-Backup-Proofing

die() { echo "release-notes: $*" >&2; exit 1; }

if [ $# -ne 1 ] || [ -z "$1" ]; then
    die "usage: $0 v<version>"
fi
tag=$1
printf '%s\n' "$tag" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' ||
    die "$tag is not a release tag (v<major>.<minor>.<patch>[-pre])"
final=${tag#v}
final=${final%%-*}
changelog=${CHANGELOG_FILE:-$(dirname "$0")/../CHANGELOG.md}
[ -r "$changelog" ] || die "cannot read $changelog"

has_heading() { # heading: whether the CHANGELOG has it (trailing blanks ignored)
    awk -v h="$1" '{ sub(/[ \t\r]+$/, "") } $0 == h { found = 1; exit } END { exit !found }' "$changelog"
}

section() { # heading: the section's lines, blank lines trimmed at both ends
    awk -v h="$1" '
        { line = $0; sub(/[ \t\r]+$/, "", line) }
        /^## / { if (in_sec) exit; in_sec = (line == h); next }
        in_sec { lines[++n] = line }
        END {
            first = 1; while (first <= n && lines[first] == "") first++
            last = n;  while (last >= first && lines[last] == "") last--
            for (i = first; i <= last; i++) print lines[i]
        }' "$changelog"
}

notes=$(section "## v$final")
if [ -z "$notes" ]; then
    case "$tag" in
        *-*)
            notes=$(section "## Unreleased")
            [ -n "$notes" ] ||
                die "$changelog has no notes for $tag: no \"## v$final\" section and \"## Unreleased\" is empty" ;;
        *)
            if has_heading "## v$final"; then
                die "the \"## v$final\" section of $changelog is empty: write the release notes there in the release PR"
            fi
            die "$changelog has no \"## v$final\" section: move the Unreleased notes there in the release PR" ;;
    esac
fi

printf '%s\n' "$notes" | awk -v base="$repo/blob/$tag" '
    # Rewrites the targets of inline links and images in s.
    function links(s,   out, t) {
        out = ""
        while (match(s, /\]\([^)]*\)/)) {
            t = substr(s, RSTART + 2, RLENGTH - 3)
            out = out substr(s, 1, RSTART + 1)
            if (t != "" && t !~ /^([A-Za-z][A-Za-z0-9+.-]*:|#|\/)/) {
                sub(/^\.\//, "", t)
                t = base "/" t
            }
            out = out t ")"
            s = substr(s, RSTART + RLENGTH)
        }
        return out s
    }
    {
        # Code spans sit between backticks: parts 2, 4, ... are code.
        n = split($0, part, "`")
        line = ""
        for (i = 1; i <= n; i++) {
            if (i > 1) line = line "`"
            line = line (i % 2 ? links(part[i]) : part[i])
        }
        print line
    }'
