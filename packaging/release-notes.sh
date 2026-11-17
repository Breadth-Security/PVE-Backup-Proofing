#!/bin/sh
# Prints the release notes for a tag: its section of CHANGELOG.md, without the
# "## v<version>" heading (the release title names the version). The release
# workflow sets them as the release's description (issue #105).
#
# usage: packaging/release-notes.sh v<version>      (v1.2.0, v1.2.0-rc.1)
#   CHANGELOG_FILE overrides the CHANGELOG path (for tests).
#
# A pre-release uses its final version's section when that has notes, else
# "## Unreleased". Fails when there are no notes to use, so a release cannot
# ship without them.
#
# Readers see releases on the public GitHub mirror, and a release page has no
# repository base, so inline links to repository files (docs/x.md#y, ./x,
# /x) become absolute GitHub links pinned to the tag. Absolute links, in-page
# anchors, code spans and fenced code blocks are left as they are. The notes
# fail instead of shipping when, outside code, a link is still relative (such
# as ../x), a reference-style link definition or an HTML link is used, or the
# private Gitea host is named.
#
# Issue numbers in the CHANGELOG are Gitea's, which public readers cannot
# open, and GitHub would link "#N" to its own, unrelated issue or PR. So
# parentheses citing only Gitea issues ("(issue #140)", "(issues #1, #2)",
# "(#63)", "(part of #127)") are dropped, "GitHub #N" is kept, and any other
# "#N" outside code fails the notes.
set -u
repo=https://github.com/Breadth-Security/PVE-Backup-Proofing
gitea=git.breadthanalytics.com

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

# Both awk programs track fenced code blocks (``` or ~~~, indented at most
# three spaces): a "## " line inside one is code, not a heading.
fences='
    function fence_line(line,   m) {
        if (!match(line, /^ ? ? ?(```|~~~)/)) return 0
        m = substr(line, RSTART + RLENGTH - 3, 3)
        if (fence == "") fence = m
        else if (m == fence) fence = ""
        return 1
    }'

has_heading() { # heading: whether the CHANGELOG has it
    awk -v h="$1" "$fences"'
        { sub(/\r$/, ""); key = $0; sub(/[ \t]+$/, "", key) }
        fence == "" && key == h { found = 1; exit }
        { fence_line($0) }
        END { exit !found }' "$changelog"
}

section() { # heading: the section's lines, blank lines trimmed at both ends;
    # exits 3 when the section ends inside a code fence
    awk -v h="$1" "$fences"'
        { sub(/\r$/, ""); line = $0; key = line; sub(/[ \t]+$/, "", key) }
        fence == "" && /^## / { if (in_sec) exit; in_sec = (key == h); next }
        in_sec { lines[++n] = line }
        { fence_line(line) }
        END {
            if (in_sec && fence != "") exit 3
            first = 1; while (first <= n && lines[first] ~ /^[ \t]*$/) first++
            last = n;  while (last >= first && lines[last] ~ /^[ \t]*$/) last--
            for (i = first; i <= last; i++) print lines[i]
        }' "$changelog"
}

notes_for() { # heading: its notes, or die on an unclosed fence
    s=$(section "$1"); rc=$?
    [ $rc -eq 3 ] && die "the \"$1\" section of $changelog ends inside an unclosed code fence"
    [ $rc -eq 0 ] || die "reading $changelog failed"
    printf '%s' "$s"
}

notes=$(notes_for "## v$final") || exit 1
if [ -z "$notes" ]; then
    case "$tag" in
        *-*)
            notes=$(notes_for "## Unreleased") || exit 1
            [ -n "$notes" ] ||
                die "$changelog has no notes for $tag: no \"## v$final\" notes and \"## Unreleased\" is empty" ;;
        *)
            if has_heading "## v$final"; then
                die "the \"## v$final\" section of $changelog is empty: write the release notes there in the release PR"
            fi
            die "$changelog has no \"## v$final\" section: move the Unreleased notes there in the release PR" ;;
    esac
fi

out=$(printf '%s\n' "$notes" | awk -v base="$repo/blob/$tag" -v gitea="$gitea" "$fences"'
    function bad(msg) { print "release-notes: " msg > "/dev/stderr"; err = 1 }
    # A repository-relative target, made absolute; others as they are.
    function fix(t,   angle) {
        gsub(/^[ \t]+|[ \t]+$/, "", t)
        angle = (t ~ /^<.*>$/)
        if (angle) t = substr(t, 2, length(t) - 2)
        if (t != "" && t !~ /^([A-Za-z][A-Za-z0-9+.-]*:|#|\/\/|\.\.(\/|$))/) {
            sub(/^\.?\//, "", t)
            t = base "/" t
        }
        return angle ? "<" t ">" : t
    }
    # Drops parentheses citing only Gitea issues, and the Gitea part of one
    # that also cites a GitHub issue: GitHub would link #N to its own item.
    function refs(s) {
        gsub(/\((issues?|PRs?) #[0-9]+(, #[0-9]+)*(,| and) /, "(", s)
        gsub(/ ?\((issues?|PRs?|part of|see) #[0-9]+((,| and|, and) #[0-9]+)*\)/, "", s)
        gsub(/ ?\(#[0-9]+((,| and|, and) #[0-9]+)*\)/, "", s)
        return s
    }
    # Rewrites the targets of inline links and images in s.
    function links(s,   out, t) {
        out = ""
        while (match(s, /\]\([^)]*\)/)) {
            t = fix(substr(s, RSTART + 2, RLENGTH - 3))
            if (t !~ /^<?(https?:\/\/|mailto:|#)/) bad("relative link left: (" t "); link to a file in the repository")
            out = out substr(s, 1, RSTART + 1) t ")"
            s = substr(s, RSTART + RLENGTH)
        }
        return out s
    }
    {
        if (fence_line($0) || fence != "") { print; next }
        # Code spans sit between backticks: the even parts, except a last
        # part after an unmatched backtick, which is plain text.
        n = split($0, part, "`")
        line = ""; plain = ""
        for (i = 1; i <= n; i++) {
            if (i > 1) line = line "`"
            if (i % 2 == 0 && !(i == n && n % 2 == 0)) {
                line = line part[i]
            } else {
                p = links(refs(part[i]))
                line = line p; plain = plain " " p
            }
        }
        if (plain ~ /^ ? ? ? ?\[[^]]*\]:/) bad("reference-style link definition: use an inline link")
        if (tolower(plain) ~ /href=|src=/) bad("HTML link or image: use Markdown")
        if (index(plain, gitea)) bad("names the private Gitea host " gitea " outside code: " $0)
        # Any other #N would link to an unrelated GitHub item (link targets
        # and GitHub references aside).
        rest = plain
        gsub(/\]\([^)]*\)/, "]()", rest)
        gsub(/GitHub #[0-9]+/, "", rest)
        if (rest ~ /(^|[^A-Za-z0-9&\/])#[0-9]/)
            bad("Gitea issue reference outside \"(issue #N)\": reword it, or cite it as (issue #N) to drop it: " $0)
        print line
    }
    END { exit err ? 3 : 0 }') || die "no notes for $tag: fix the CHANGELOG as said above"
printf '%s\n' "$out"
