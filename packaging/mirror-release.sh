#!/bin/sh
# Copy signed Gitea releases to the GitHub mirror (issue #83). Development
# and the release workflow stay on a private Gitea instance; GitHub is where
# everyone else downloads. Run it after
# packaging/sign-release.sh, on the maintainer's machine.
#
# usage: packaging/mirror-release.sh [--dry-run] [--replace] TAG...
#        packaging/mirror-release.sh [--dry-run] [--replace] --all
#
# For each release it downloads the Gitea release's files, requires a
# SHA256SUMS.sig that verifies with the repository's allowed_signers and
# files that all match SHA256SUMS, then creates the GitHub release as a draft
# with the same files and description, downloads them back from GitHub,
# checks them the same way, and only then publishes it. A GitHub release
# that already exists is only checked (and published if it is a draft whose
# files verify); --replace deletes it (keeping the tag) and copies it again,
# for when the Gitea release changed (re-signed after a key change, a new
# description). Afterwards the newest final release is marked GitHub's
# "latest", whatever order the copies were made in. GitHub replaces "~" in file names with ".", so the files of
# a pre-release (pbp_0.3.0~rc.2_amd64.deb) are served as
# pbp_0.3.0.rc.2_amd64.deb; they are renamed back before checking, since
# the signed SHA256SUMS names them with "~". The push mirror must already have
# pushed the tag. --all copies oldest first, so GitHub's "latest" ends on the
# newest release.
#
# Needs gh logged in with access to $PBP_GITHUB_REPO (default
# Breadth-Security/PVE-Backup-Proofing); Gitea credentials come from
# `git credential fill`.
set -eu

repo_dir=$(cd "$(dirname "$0")/.." && pwd)
signers="$repo_dir/allowed_signers"
gh_repo=${PBP_GITHUB_REPO:-Breadth-Security/PVE-Backup-Proofing}
host=git.breadthanalytics.com
api="https://$host/api/v1/repos/tdiana/PVE-Backup-Proofing"
ns=pbp-release

dry=false
replace=false
all=false
tags=""
for arg in "$@"; do
    case "$arg" in
        --dry-run) dry=true ;;
        --replace) replace=true ;;
        --all) all=true ;;
        -*) echo "unknown option $arg" >&2; exit 2 ;;
        *) tags="$tags $arg" ;;
    esac
done
if ! $all && [ -z "$tags" ]; then
    sed -n '7,8p' "$0" | sed 's/^# //' >&2
    exit 2
fi
[ -r "$signers" ] || { echo "cannot read $signers" >&2; exit 1; }
gh auth status >/dev/null 2>&1 || { echo "gh is not logged in (gh auth login)" >&2; exit 1; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
chmod 700 "$tmp"

# Gitea credentials go to curl through a config file, never the command line.
cred=$(printf 'protocol=https\nhost=%s\n\n' "$host" | git credential fill)
user=$(printf '%s\n' "$cred" | sed -n 's/^username=//p')
pass=$(printf '%s\n' "$cred" | sed -n 's/^password=//p')
[ -n "$user" ] && [ -n "$pass" ] || { echo "no Gitea credentials from git credential fill" >&2; exit 1; }
esc() { printf '%s' "$1" | sed 's/[\\"]/\\&/g'; }
(umask 077; printf 'user = "%s:%s"\n' "$(esc "$user")" "$(esc "$pass")" > "$tmp/curlrc")
unset cred pass
gitea() { curl -fsS -K "$tmp/curlrc" "$@"; }

check() { # dir: signature and checksums, every file listed
    (cd "$1" &&
        ssh-keygen -Y verify -f "$signers" -I "$ns" -n "$ns" -s SHA256SUMS.sig < SHA256SUMS >/dev/null 2>&1 &&
        sha256sum --quiet -c SHA256SUMS &&
        for f in *; do
            case "$f" in SHA256SUMS | SHA256SUMS.sig) continue ;; esac
            awk -v f="$f" '$2 == f || $2 == "*" f { found = 1 } END { exit !found }' SHA256SUMS || exit 1
        done)
}

unmangle() { # dir: give files GitHub renamed ("~" -> ".") their SHA256SUMS names
    (cd "$1" && awk '{ n = $2; sub(/^\*/, "", n); print n }' SHA256SUMS | while read -r n; do
        g=$(printf '%s' "$n" | tr '~' '.')
        if [ "$g" != "$n" ] && [ -f "$g" ] && [ ! -e "$n" ]; then mv "$g" "$n"; fi
    done)
}

if $all; then
    page=1
    while :; do
        got=$(gitea "$api/releases?limit=50&page=$page" | jq -r '.[] | select(.draft | not) | .tag_name')
        [ -n "$got" ] || break
        tags="$tags $got"
        page=$((page + 1))
    done
    # Gitea lists newest first; copy oldest first.
    tags=$(printf '%s\n' $tags | awk '{ l[NR] = $0 } END { for (i = NR; i > 0; i--) print l[i] }')
fi

failed=0
for tag in $tags; do
    echo "== $tag"
    src="$tmp/$tag/gitea" dst="$tmp/$tag/github"
    mkdir -p "$src" "$dst"
    if ! gitea "$api/releases/tags/$tag" > "$tmp/$tag/release.json"; then
        echo "   no Gitea release for $tag" >&2; failed=1; continue
    fi
    if [ "$(jq -r .draft "$tmp/$tag/release.json")" = true ]; then
        echo "   still a draft on Gitea: sign it first (packaging/sign-release.sh $tag)" >&2; failed=1; continue
    fi
    if ! gh api "repos/$gh_repo/git/ref/tags/$tag" >/dev/null 2>&1; then
        echo "   tag not on GitHub yet: let the push mirror sync" >&2; failed=1; continue
    fi

    if $replace && ! $dry && gh release view "$tag" -R "$gh_repo" >/dev/null 2>&1; then
        gh release delete "$tag" -R "$gh_repo" --yes >/dev/null # the tag stays
        echo "   deleted the GitHub release to copy it again"
    fi
    if gh release view "$tag" -R "$gh_repo" >/dev/null 2>&1; then
        gh release download "$tag" -R "$gh_repo" -D "$dst" >/dev/null
        unmangle "$dst"
        if check "$dst"; then
            echo "   already on GitHub; signature and checksums good"
            if [ "$(gh release view "$tag" -R "$gh_repo" --json isDraft --jq .isDraft)" = true ] && ! $dry; then
                gh release edit "$tag" -R "$gh_repo" --draft=false --latest=false >/dev/null
                echo "   published (was a draft)"
            fi
        else
            echo "   already on GitHub, but its files do NOT verify: delete it there and copy again" >&2; failed=1
        fi
        continue
    fi

    # A failed download fails this release only, not the whole run.
    if ! jq -r '.assets[] | "\(.name)\t\(.browser_download_url)"' "$tmp/$tag/release.json" |
        while IFS="$(printf '\t')" read -r name url; do
            gitea -o "$src/$name" "$url"
        done; then
        echo "   downloading the Gitea release's files failed" >&2; failed=1; continue
    fi
    if [ ! -f "$src/SHA256SUMS.sig" ] || ! check "$src"; then
        echo "   Gitea release is not signed, or its files do not verify; not copied" >&2; failed=1; continue
    fi
    if $dry; then
        echo "   signed and verified on Gitea; would copy"
        continue
    fi

    jq -r '.body // ""' "$tmp/$tag/release.json" > "$tmp/$tag/notes.md"
    pre=""
    [ "$(jq -r .prerelease "$tmp/$tag/release.json")" = true ] && pre=--prerelease
    # shellcheck disable=SC2086 # $pre is empty or one flag
    if ! gh release create "$tag" -R "$gh_repo" --draft --verify-tag $pre --title "$tag" \
        --notes-file "$tmp/$tag/notes.md" "$src"/* >/dev/null; then
        echo "   creating the GitHub release failed (rerun with --replace)" >&2; failed=1; continue
    fi

    # What users download from GitHub must verify too.
    gh release download "$tag" -R "$gh_repo" -D "$dst" >/dev/null
    unmangle "$dst"
    if check "$dst"; then
        gh release edit "$tag" -R "$gh_repo" --draft=false --latest=false >/dev/null
        echo "   copied; GitHub files verify; published"
    else
        echo "   copied, but the GitHub files do NOT verify; left as a draft" >&2; failed=1
    fi
done

# GitHub's "latest" is the newest final release on Gitea, if it is copied.
if ! $dry; then
    newest=$(gitea "$api/releases?limit=50" | jq -r '[.[] | select((.draft or .prerelease) | not)][0].tag_name // empty')
    if [ -n "$newest" ] && gh release view "$newest" -R "$gh_repo" --json isDraft --jq .isDraft 2>/dev/null | grep -qx false; then
        gh release edit "$newest" -R "$gh_repo" --latest >/dev/null
    fi
fi
exit $failed
