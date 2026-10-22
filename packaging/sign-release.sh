#!/bin/sh
# Sign the SHA256SUMS of published releases and attach SHA256SUMS.sig
# (issue #37). Run it on the maintainer's machine after the release workflow
# has published a release: the signing key never goes to CI.
#
# usage: packaging/sign-release.sh [--replace] [--dry-run] TAG...
#        packaging/sign-release.sh [--replace] [--dry-run] --all
#
# For each release it downloads every file, checks them all against
# SHA256SUMS (and that SHA256SUMS lists them all), signs SHA256SUMS with the
# key in namespace pbp-release, verifies the signature with the repository's
# allowed_signers, uploads it, then downloads the published signature and
# verifies that too. A release that already has SHA256SUMS.sig is only
# verified, unless --replace is given. --dry-run stops before signing.
#
# The key is $PBP_SIGNING_KEY (default ~/.ssh/pbp_signing). When ssh-agent
# holds it, it signs through the agent; otherwise ssh-keygen asks for the
# passphrase. Gitea credentials come from `git credential fill`.
set -eu

repo_dir=$(cd "$(dirname "$0")/.." && pwd)
signers="$repo_dir/allowed_signers"
key=${PBP_SIGNING_KEY:-$HOME/.ssh/pbp_signing}
host=git.breadthanalytics.com
api="https://$host/api/v1/repos/tdiana/Proxmox-Backup-Proofing"
ns=pbp-release

replace=false
dry=false
all=false
tags=""
for arg in "$@"; do
    case "$arg" in
        --replace) replace=true ;;
        --dry-run) dry=true ;;
        --all) all=true ;;
        -*) echo "unknown option $arg" >&2; exit 2 ;;
        *) tags="$tags $arg" ;;
    esac
done
if ! $all && [ -z "$tags" ]; then
    sed -n '6,7p' "$0" | sed 's/^# //' >&2
    exit 2
fi
for f in "$key.pub" "$signers"; do
    [ -r "$f" ] || { echo "cannot read $f" >&2; exit 1; }
done

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
chmod 700 "$tmp"

# Credentials go to curl through a config file, never the command line.
cred=$(printf 'protocol=https\nhost=%s\n\n' "$host" | git credential fill)
user=$(printf '%s\n' "$cred" | sed -n 's/^username=//p')
pass=$(printf '%s\n' "$cred" | sed -n 's/^password=//p')
[ -n "$user" ] && [ -n "$pass" ] || { echo "no Gitea credentials from git credential fill" >&2; exit 1; }
(umask 077; printf 'user = "%s:%s"\n' "$user" "$pass" > "$tmp/curlrc")
unset cred pass
gitea() { curl -fsS -K "$tmp/curlrc" "$@"; }

# Sign through the agent when it holds the key (no passphrase prompt).
sign_with="$key"
if ssh-add -L 2>/dev/null | grep -qF "$(cut -d' ' -f2 "$key.pub")"; then
    sign_with="$key.pub"
fi

verify() { # dir: SHA256SUMS.sig against SHA256SUMS
    (cd "$1" && ssh-keygen -Y verify -f "$signers" -I "$ns" -n "$ns" -s SHA256SUMS.sig < SHA256SUMS)
}

if $all; then
    tags=$(gitea "$api/releases?limit=100" | jq -r '.[].tag_name')
fi

failed=0
for tag in $tags; do
    echo "== $tag"
    dir="$tmp/$tag"
    mkdir -p "$dir"
    if ! gitea "$api/releases/tags/$tag" > "$dir.json"; then
        echo "   no release for $tag" >&2; failed=1; continue
    fi
    id=$(jq -r .id "$dir.json")
    sig_id=$(jq -r '.assets[] | select(.name == "SHA256SUMS.sig") | .id' "$dir.json")

    jq -r '.assets[] | select(.name != "SHA256SUMS.sig") | "\(.name)\t\(.browser_download_url)"' "$dir.json" |
        while IFS="$(printf '\t')" read -r name url; do
            gitea -o "$dir/$name" "$url"
        done
    [ -f "$dir/SHA256SUMS" ] || { echo "   release has no SHA256SUMS" >&2; failed=1; continue; }

    if [ -n "$sig_id" ] && ! $replace; then
        gitea -o "$dir/SHA256SUMS.sig" "$(jq -r '.assets[] | select(.name == "SHA256SUMS.sig") | .browser_download_url' "$dir.json")"
        if verify "$dir" >/dev/null 2>&1; then
            echo "   already signed; signature good"
        else
            echo "   already signed, but the signature does NOT verify (use --replace)" >&2; failed=1
        fi
        continue
    fi

    # Every file is listed and matches, before anything is signed.
    if ! (cd "$dir" && sha256sum --quiet -c SHA256SUMS); then
        echo "   files do not match SHA256SUMS; not signed" >&2; failed=1; continue
    fi
    unlisted=$(cd "$dir" && for f in *; do
        [ "$f" = SHA256SUMS ] || awk -v f="$f" '$2 == f || $2 == "*" f { found = 1 } END { exit !found }' SHA256SUMS || echo "$f"
    done)
    if [ -n "$unlisted" ]; then
        echo "   files missing from SHA256SUMS: $unlisted; not signed" >&2; failed=1; continue
    fi

    if $dry; then
        echo "   files match SHA256SUMS; would sign"
        continue
    fi
    rm -f "$dir/SHA256SUMS.sig"
    ssh-keygen -q -Y sign -f "$sign_with" -n "$ns" "$dir/SHA256SUMS"
    verify "$dir" >/dev/null

    if [ -n "$sig_id" ]; then
        gitea -X DELETE "$api/releases/$id/assets/$sig_id" >/dev/null
    fi
    gitea -X POST -F "attachment=@$dir/SHA256SUMS.sig" "$api/releases/$id/assets?name=SHA256SUMS.sig" >/dev/null

    # What users download must verify too.
    url=$(gitea "$api/releases/$id/assets" | jq -r '.[] | select(.name == "SHA256SUMS.sig") | .browser_download_url')
    rm -f "$dir/SHA256SUMS.sig"
    gitea -o "$dir/SHA256SUMS.sig" "$url"
    if verify "$dir" >/dev/null 2>&1; then
        echo "   signed; published signature verifies"
    else
        echo "   uploaded signature does NOT verify" >&2; failed=1
    fi
done
exit $failed
