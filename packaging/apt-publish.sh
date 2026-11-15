#!/bin/sh
# Publish releases to PBP's APT repository (issue #130), on the maintainer's
# machine after packaging/mirror-release.sh: the signing key never goes to
# CI.
#
# usage: packaging/apt-publish.sh [--dry-run] [--first] TAG...
#        packaging/apt-publish.sh --init
#        packaging/apt-publish.sh [--dry-run] --page
#        packaging/apt-publish.sh --local DIR --key-file FILE DEB...
#
# For each TAG (final releases only) it downloads the GitHub release's
# .deb files, SHA256SUMS and SHA256SUMS.sig, and verifies them with the
# repository's allowed_signers. It fetches the live InRelease, verifies it
# with the published APT key (packaging/apt/pbp-archive-keyring.asc), and
# fetches and checks the Packages files it lists. --first allows an empty
# repository (InRelease not found); otherwise any failure to fetch aborts,
# so a run never publishes an index without the earlier versions. It then
# adds the packages with tools/aptindex, signs Release with gpg (InRelease
# and Release.gpg), and uploads to Cloudflare R2 in this order: the new
# packages, the by-hash index files, Packages, and last Release, Release.gpg
# and InRelease, so a client never sees an index before the files it names.
# Last, it reads everything back over HTTPS and checks it, then publishes
# the landing page as --page does. --dry-run stops before the upload and
# keeps the work directory.
#
# --page publishes only the landing page (packaging/apt/index.html, served
# at the domain's root through a Cloudflare rewrite of / to /index.html),
# when it differs from the live one, so a fix to it needs no release. The
# page must name the published key's fingerprint. It is read back from R2
# byte for byte; through the domain, markup a CDN adds right before
# </body> is allowed. The script warns when the root does not serve it.
#
# --init exports the APT key's public half (packaging/apt/, to commit),
# signs the binary keyring with the SSH release key, and uploads both.
#
# --local builds into DIR from DEB files, signed with the secret key in FILE
# (a throwaway, in a temporary GNUPGHOME), adds the landing page, and
# uploads nothing: for packaging/apt_roundtrip_test.sh.
#
# Environment: PBP_APT_KEY, the APT signing key's fingerprint (in the
# default GNUPGHOME); PBP_R2_CREDENTIALS, a file of R2_ACCOUNT_ID=,
# R2_ACCESS_KEY_ID=, R2_SECRET_ACCESS_KEY= and R2_BUCKET= lines (mode 0600);
# PBP_SIGNING_KEY, the SSH release key for --init (default
# ~/.ssh/pbp_signing). The credentials are never put on a command line.
# PBP_APT_URL, PBP_APT_KEYRING, PBP_APT_PAGE and PBP_APT_PROTO override the
# repository URL, the published key file, the landing page and curl's
# --proto for the repository, for tests.
set -eu
repo_dir=$(cd "$(dirname "$0")/.." && pwd)
signers="$repo_dir/allowed_signers"
ns=pbp-release
base_url=${PBP_APT_URL:-https://apt.breadthsecurity.com}
proto=${PBP_APT_PROTO:-=https}
gh=https://github.com/Breadth-Security/PVE-Backup-Proofing/releases/download
keyring_asc=${PBP_APT_KEYRING:-$repo_dir/packaging/apt/pbp-archive-keyring.asc}
page=${PBP_APT_PAGE:-$repo_dir/packaging/apt/index.html}
suite=stable

dry=false first=false init=false page_only=false local_dir="" key_file="" args=""
while [ $# -gt 0 ]; do
    case "$1" in
        --dry-run) dry=true ;;
        --first) first=true ;;
        --init) init=true ;;
        --page) page_only=true ;;
        --local) local_dir=${2:?--local needs a directory}; shift ;;
        --key-file) key_file=${2:?--key-file needs a file}; shift ;;
        -*) echo "unknown option $1" >&2; exit 2 ;;
        *) args="$args $1" ;;
    esac
    shift
done
usage() { sed -n '6,9p' "$0" | sed 's/^# //' >&2; exit 2; }
die() { echo "apt-publish: $*" >&2; exit 1; }
if $page_only; then
    if [ -n "$args" ] || $init || $first || [ -n "$local_dir" ] || [ -n "$key_file" ]; then usage; fi
fi

work=$(mktemp -d)
# The R2 credentials never outlive the run, a kept dry-run directory or an
# interrupted one included.
cleanup() {
    rm -f "$work/curl.cfg"
    if $dry; then echo "work directory kept: $work"; else rm -rf "$work"; fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM

sha() { sha256sum < "$1" | cut -d' ' -f1; }

# release_date prints the Release date: RFC 1123, UTC, English names.
release_date() { LC_ALL=C date -u '+%a, %d %b %Y %H:%M:%S UTC'; }

# build_index adds the .deb arguments to the tree in $1 and writes the
# suite; it prints "added"/"present" lines.
build_index() {
    root=$1; shift
    (cd "$repo_dir" && go run ./tools/aptindex -root "$root" -date "$(release_date)" -suite "$suite" "$@")
}

# sign_release signs $1/dists/$suite/Release as InRelease and Release.gpg
# with gpg key $2 and checks both with the keyring $3.
sign_release() {
    d="$1/dists/$suite"
    rm -f "$d/InRelease" "$d/Release.gpg"
    gpg --batch --yes --local-user "$2" --digest-algo SHA256 --clearsign --output "$d/InRelease" "$d/Release"
    gpg --batch --yes --local-user "$2" --digest-algo SHA256 --detach-sign --armor --output "$d/Release.gpg" "$d/Release"
    gpgv --quiet --keyring "$3" "$d/InRelease" >/dev/null 2>&1 || die "InRelease does not verify with $3"
    gpgv --quiet --keyring "$3" "$d/Release.gpg" "$d/Release" >/dev/null 2>&1 || die "Release.gpg does not verify with $3"
}

if [ -n "$local_dir" ]; then
    if [ -z "$key_file" ] || [ -z "$args" ]; then usage; fi
    GNUPGHOME="$work/gnupg"; export GNUPGHOME
    mkdir -m 700 "$GNUPGHOME"
    gpg --batch --quiet --import "$key_file"
    fpr=$(gpg --batch --with-colons --list-secret-keys | awk -F: '/^fpr/ {print $10; exit}')
    [ -n "$fpr" ] || die "no secret key in $key_file"
    mkdir -p "$local_dir"
    gpg --batch --export "$fpr" > "$local_dir/pbp-archive-keyring.gpg"
    # shellcheck disable=SC2086 # the .deb paths, split on purpose
    build_index "$local_dir" $args
    sign_release "$local_dir" "$fpr" "$local_dir/pbp-archive-keyring.gpg"
    cp "$page" "$local_dir/index.html"
    exit 0
fi

if ! $page_only; then
    [ -n "${PBP_APT_KEY:-}" ] || die "set PBP_APT_KEY to the APT signing key's fingerprint"
fi

if $init; then
    [ -z "$args" ] || usage
    sshkey=${PBP_SIGNING_KEY:-$HOME/.ssh/pbp_signing}
    mkdir -p "$work/init"
    gpg --batch --export "$PBP_APT_KEY" > "$work/init/pbp-archive-keyring.gpg"
    [ -s "$work/init/pbp-archive-keyring.gpg" ] || die "gpg has no public key $PBP_APT_KEY"
    gpg --batch --armor --export "$PBP_APT_KEY" > "$work/init/pbp-archive-keyring.asc"
    ssh-keygen -Y sign -q -f "$sshkey" -n "$ns" "$work/init/pbp-archive-keyring.gpg"
    ssh-keygen -Y verify -f "$signers" -I "$ns" -n "$ns" -s "$work/init/pbp-archive-keyring.gpg.sig" \
        < "$work/init/pbp-archive-keyring.gpg" >/dev/null || die "the keyring's SSH signature does not verify with allowed_signers"
fi

[ -n "$args" ] || $init || $page_only || usage
if ! $init && grep -q PLACEHOLDER "$keyring_asc" 2>/dev/null; then
    die "$keyring_asc is a placeholder: run packaging/apt-publish.sh --init first and commit the key"
fi

# r2_login reads the R2 credentials from their file (without running it)
# into a curl config that only this run can read; only an upload calls it.
curlcfg="$work/curl.cfg" endpoint=""
r2_login() {
    [ -z "$endpoint" ] || return 0
    creds=${PBP_R2_CREDENTIALS:?set PBP_R2_CREDENTIALS to the R2 credentials file}
    cred() { sed -n "s/^$1=//p" "$creds" | head -n 1; }
    account=$(cred R2_ACCOUNT_ID) bucket=$(cred R2_BUCKET)
    if [ -z "$account" ] || [ -z "$bucket" ]; then die "$creds lacks R2_ACCOUNT_ID or R2_BUCKET"; fi
    (umask 077 && printf 'user = "%s:%s"\n' "$(cred R2_ACCESS_KEY_ID)" "$(cred R2_SECRET_ACCESS_KEY)" > "$curlcfg")
    endpoint="https://$account.r2.cloudflarestorage.com/$bucket"
}

# put KEY FILE CACHE-CONTROL uploads FILE to R2 as KEY.
put() {
    r2_login
    case "$1" in
        *.deb) ctype=application/vnd.debian.binary-package ;;
        *.gz) ctype=application/gzip ;;
        *.gpg) ctype=application/pgp-keys ;;
        *.html) ctype="text/html; charset=utf-8" ;;
        *) ctype=text/plain ;;
    esac
    curl -sS --fail --proto =https -K "$curlcfg" --aws-sigv4 "aws:amz:auto:s3" -T "$2" \
        -H "Cache-Control: $3" -H "Content-Type: $ctype" "$endpoint/$1" -o /dev/null \
        || die "upload of $1 failed"
    echo "uploaded $1"
}
immutable="public, max-age=31536000, immutable"

# served_page FILE: FILE, as the domain served it, is the landing page.
# Markup a CDN inserted right before the page's </body> line is allowed
# (Cloudflare's bot detection adds a script there); any other difference
# is not. What R2 stores is checked byte for byte instead (r2_get).
served_page() {
    cmp -s "$1" "$page" && return 0
    n=$(grep -n '^</body>$' "$page" | head -n 1 | cut -d: -f1)
    [ -n "$n" ] || return 1
    after=$(( $(wc -l < "$page") - n ))
    head -n $((n - 1)) "$page" > "$work/page-head"; head -n $((n - 1)) "$1" > "$work/served-head"
    tail -n "$after" "$page" > "$work/page-tail"; tail -n "$after" "$1" > "$work/served-tail"
    cmp -s "$work/page-head" "$work/served-head" && cmp -s "$work/page-tail" "$work/served-tail" || return 1
    [ "$(wc -l < "$1")" -ge "$(wc -l < "$page")" ] || return 1
    tail -n $((after + 1)) "$1" | head -n 1 | grep -q '</body>$'
}

# r2_get KEY FILE downloads KEY from R2 itself, past the CDN, into FILE.
r2_get() {
    r2_login
    curl -sS --fail --proto =https -K "$curlcfg" --aws-sigv4 "aws:amz:auto:s3" -o "$2" "$endpoint/$1" \
        || die "reading back $1 from R2 failed"
}

# check_root warns when the domain's root does not serve the landing page:
# the Cloudflare rewrite of / to /index.html is missing.
check_root() {
    if curl -fsS --proto "$proto" -o "$work/root.html" "$base_url/" 2>/dev/null && served_page "$work/root.html"; then
        return 0
    fi
    echo "apt-publish: warning: $base_url/ does not serve the landing page: add the Cloudflare rewrite of / to /index.html (docs/development.md)" >&2
}

# publish_page uploads the landing page when the live one differs (or is
# missing), and reads it back; a dry run only reports. The page must name
# the fingerprint of the key in $keyring_asc.
publish_page() {
    want=$(gpg --batch --with-colons --show-keys "$keyring_asc" 2>/dev/null | awk -F: '/^fpr/ {print $10; exit}')
    got=$(sed -n 's/.*id="fingerprint"[^>]*>\([^<]*\)<.*/\1/p' "$page" | tr -d ' ')
    if [ -z "$want" ] || [ "$got" != "$want" ]; then
        die "the landing page's fingerprint (${got:-none}) is not the key's (${want:-unreadable}) in $keyring_asc"
    fi
    pstatus=$(curl -sS --proto "$proto" -o "$work/live-index.html" -w '%{http_code}' "$base_url/index.html") \
        || die "fetching the live landing page failed"
    case "$pstatus" in
        200) if served_page "$work/live-index.html"; then echo "landing page is current"; check_root; return 0; fi ;;
        404) ;;
        *) die "fetching the live landing page: HTTP $pstatus" ;;
    esac
    if $dry; then echo "landing page differs from the live one: would upload"; return 0; fi
    put index.html "$page" no-cache
    r2_get index.html "$work/stored-index.html"
    cmp -s "$work/stored-index.html" "$page" || die "the index.html in R2 differs from the upload"
    curl -fsS --proto "$proto" -o "$work/back-index.html" "$base_url/index.html" || die "reading back index.html failed"
    served_page "$work/back-index.html" || die "the served index.html is not the uploaded page (a cache?)"
    check_root
}

if $page_only; then
    publish_page
    exit 0
fi

if $init; then
    if ! $dry; then
        put pbp-archive-keyring.gpg "$work/init/pbp-archive-keyring.gpg" no-cache
        put pbp-archive-keyring.gpg.sig "$work/init/pbp-archive-keyring.gpg.sig" no-cache
    fi
    mkdir -p "$(dirname "$keyring_asc")"
    cp "$work/init/pbp-archive-keyring.asc" "$keyring_asc"
    echo "wrote $keyring_asc: commit it; the key's fingerprint is $PBP_APT_KEY"
    exit 0
fi

kr="$work/keyring.gpg"
gpg --batch --dearmor < "$keyring_asc" > "$kr"

# Download and verify each release.
debs=""
for tag in $args; do
    case "$tag" in v*) ;; *) die "$tag: a tag such as v0.10.0" ;; esac
    v=${tag#v}
    case "$v" in *-*|*~*) die "$tag is a pre-release: the repository carries final releases only" ;; esac
    d="$work/release/$tag"
    mkdir -p "$d"
    for f in SHA256SUMS SHA256SUMS.sig "pbp_${v}_amd64.deb" "pbp_${v}_arm64.deb"; do
        curl -fsSL --proto =https -o "$d/$f" "$gh/$tag/$f" || die "$tag: download of $f failed"
    done
    ssh-keygen -Y verify -f "$signers" -I "$ns" -n "$ns" -s "$d/SHA256SUMS.sig" < "$d/SHA256SUMS" >/dev/null \
        || die "$tag: SHA256SUMS.sig does not verify with allowed_signers"
    for f in "pbp_${v}_amd64.deb" "pbp_${v}_arm64.deb"; do
        grep -q "  $f\$" "$d/SHA256SUMS" || die "$tag: SHA256SUMS does not list $f"
    done
    (cd "$d" && sha256sum -c --ignore-missing --quiet SHA256SUMS) || die "$tag: checksum mismatch"
    debs="$debs $d/pbp_${v}_amd64.deb $d/pbp_${v}_arm64.deb"
done

# The live index: InRelease verified, then each Packages file it lists.
repo="$work/repo"
dists="$repo/dists/$suite"
mkdir -p "$dists"
status=$(curl -sS --proto "$proto" -o "$work/live-InRelease" -w '%{http_code}' "$base_url/dists/$suite/InRelease") \
    || die "fetching the live InRelease failed"
case "$status" in
    200)
        gpgv --quiet --keyring "$kr" --output "$work/live-Release" "$work/live-InRelease" >/dev/null 2>&1 \
            || die "the live InRelease does not verify with $keyring_asc"
        sed -n 's/^ \([0-9a-f]\{64\}\) [0-9]* \(main\/binary-[a-z0-9]*\/Packages\)$/\1 \2/p' "$work/live-Release" > "$work/live-list"
        [ -s "$work/live-list" ] || die "the live Release lists no Packages files"
        # By hash: the plain Packages may already be newer than this
        # InRelease if an earlier upload stopped halfway; the by-hash file
        # it names never changes.
        while read -r want path; do
            mkdir -p "$(dirname "$dists/$path")"
            curl -fsS --proto "$proto" -o "$dists/$path" "$base_url/dists/$suite/$(dirname "$path")/by-hash/SHA256/$want" \
                || die "fetching $path (by hash) failed"
            [ "$(sha "$dists/$path")" = "$want" ] || die "the live $path does not match its hash in InRelease"
        done < "$work/live-list"
        ;;
    404)
        $first || die "the live InRelease is not found (HTTP 404): pass --first only for an empty repository"
        ;;
    *) die "fetching the live InRelease: HTTP $status" ;;
esac

# shellcheck disable=SC2086 # the .deb paths, split on purpose
build_index "$repo" $debs | tee "$work/added"
sign_release "$repo" "$PBP_APT_KEY" "$kr"

if $dry; then
    ( publish_page ) || die "dry run: the index built and signed; only the landing page failed (above)"
    echo "dry run: nothing uploaded"
    exit 0
fi

# Upload, in order: new packages, by-hash files, Packages, then Release.
sed -n 's/^added //p' "$work/added" > "$work/added-files"
while read -r f; do
    key=$(cd "$repo" && find pool -name "$f")
    put "$key" "$repo/$key" "$immutable"
done < "$work/added-files"
for key in $(cd "$repo" && find "dists/$suite" -path '*/by-hash/SHA256/*' -type f | sort); do
    put "$key" "$repo/$key" "$immutable"
done
for key in $(cd "$repo" && find "dists/$suite/main" -path '*/by-hash' -prune -o -type f -name 'Packages*' -print | sort); do
    put "$key" "$repo/$key" no-cache
done
for f in Release Release.gpg InRelease; do
    put "dists/$suite/$f" "$dists/$f" no-cache
done

# Read back over HTTPS.
curl -fsS --proto "$proto" -o "$work/back-InRelease" "$base_url/dists/$suite/InRelease" || die "reading back InRelease failed"
cmp -s "$work/back-InRelease" "$dists/InRelease" || die "the served InRelease is not the one just uploaded (a cache?)"
gpgv --quiet --keyring "$kr" "$work/back-InRelease" >/dev/null 2>&1 || die "the served InRelease does not verify"
for key in $(cd "$repo" && find "dists/$suite/main" -path '*/by-hash' -prune -o -type f -name 'Packages*' -print); do
    curl -fsS --proto "$proto" -o "$work/back" "$base_url/$key" || die "reading back $key failed"
    [ "$(sha "$work/back")" = "$(sha "$repo/$key")" ] || die "the served $key differs from the upload"
done
while read -r f; do
    key=$(cd "$repo" && find pool -name "$f")
    curl -fsS --proto "$proto" -o "$work/back" "$base_url/$key" || die "reading back $key failed"
    [ "$(sha "$work/back")" = "$(sha "$repo/$key")" ] || die "the served $key differs from the upload"
done < "$work/added-files"
echo "published and verified:$args"
( publish_page ) || die "the packages are published and verified; only the landing page failed (above): fix it, then run --page"
