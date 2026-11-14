#!/bin/sh
# APT round trip for packaging/apt-publish.sh (issue #130): build a throwaway
# repository signed with a throwaway key (apt-publish.sh --local), then check
# that apt-get accepts it and downloads the exact package, and that it
# rejects a tampered index and a tampered InRelease. Run it on a Debian
# host with apt-get, dpkg-deb and gpg. Nothing is installed: apt-get works
# in a private state and cache directory.
set -eu
repo_dir=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

# A throwaway OpenPGP key, exported for apt-publish.sh --key-file.
export GNUPGHOME="$tmp/gnupg"
mkdir -m 700 "$GNUPGHOME"
gpg --batch --quiet --pinentry-mode loopback --passphrase '' \
    --quick-gen-key 'PBP APT round-trip test <test@example.invalid>' ed25519 sign never
gpg --batch --quiet --pinentry-mode loopback --passphrase '' --armor --export-secret-keys > "$tmp/key.asc"
unset GNUPGHOME

# Two small packages, built with dpkg-deb (gzip, as nfpm writes them).
for arch in amd64 arm64; do
    d="$tmp/pkg-$arch"
    mkdir -p "$d/DEBIAN" "$d/usr/share/doc/pbp"
    printf 'round trip %s\n' "$arch" > "$d/usr/share/doc/pbp/README"
    printf 'Package: pbp\nVersion: 0.0.1\nArchitecture: %s\nMaintainer: Test <test@example.invalid>\nDescription: PBP round-trip test\n' "$arch" > "$d/DEBIAN/control"
    dpkg-deb -Zgzip --root-owner-group --build "$d" "$tmp/pbp_0.0.1_$arch.deb" >/dev/null
done

"$repo_dir/packaging/apt-publish.sh" --local "$tmp/repo" --key-file "$tmp/key.asc" \
    "$tmp/pbp_0.0.1_amd64.deb" "$tmp/pbp_0.0.1_arm64.deb"

mkdir -p "$tmp/apt/state/lists/partial" "$tmp/apt/cache/archives/partial" "$tmp/apt/etc" "$tmp/dl"
echo "deb [signed-by=$tmp/repo/pbp-archive-keyring.gpg] file://$tmp/repo stable main" > "$tmp/apt/etc/sources.list"
aptget() {
    apt-get -q -o Dir::State="$tmp/apt/state" -o Dir::Cache="$tmp/apt/cache" \
        -o Dir::Etc::SourceList="$tmp/apt/etc/sources.list" -o Dir::Etc::SourceParts=/dev/null \
        -o APT::Sandbox::User=root -o Debug::NoLocking=1 -o APT::Architecture=amd64 "$@"
}

aptget update >"$tmp/update.log" 2>&1 || { cat "$tmp/update.log"; fail "apt-get update on the signed repository"; }
grep -q '^W:' "$tmp/update.log" && { cat "$tmp/update.log"; fail "apt-get update warned"; }
(cd "$tmp/dl" && aptget download pbp=0.0.1 >/dev/null 2>&1) || fail "apt-get download pbp=0.0.1"
want=$(sha256sum < "$tmp/pbp_0.0.1_amd64.deb" | cut -d' ' -f1)
got=$(sha256sum < "$tmp/dl/pbp_0.0.1_amd64.deb" | cut -d' ' -f1)
[ "$got" = "$want" ] || fail "downloaded package SHA256 $got, want $want"
echo "PASS: apt-get update and download from the signed repository; SHA256 matches"

# A second run with the same packages changes no index file.
before=$(find "$tmp/repo/dists" -type f -name 'Packages*' | sort | xargs sha256sum)
"$repo_dir/packaging/apt-publish.sh" --local "$tmp/repo" --key-file "$tmp/key.asc" \
    "$tmp/pbp_0.0.1_amd64.deb" "$tmp/pbp_0.0.1_arm64.deb" >/dev/null
after=$(find "$tmp/repo/dists" -type f -name 'Packages*' | sort | xargs sha256sum)
[ "$before" = "$after" ] || fail "a rerun with the same packages changed the index"
echo "PASS: rerun is a no-op on the index"

# Tampered index files: every Packages file, plain, compressed and by hash.
cp -a "$tmp/repo" "$tmp/repo.orig"
find "$tmp/repo/dists/stable/main" -type f | while read -r f; do printf 'x' >> "$f"; done
rm -rf "$tmp/apt/state/lists"; mkdir -p "$tmp/apt/state/lists/partial"
if aptget update >"$tmp/tamper.log" 2>&1 && ! grep -q '^[WE]:' "$tmp/tamper.log"; then
    cat "$tmp/tamper.log"; fail "apt-get update accepted a tampered index"
fi
grep -qi "hash sum mismatch\|hashes of expected file" "$tmp/tamper.log" || { cat "$tmp/tamper.log"; fail "tampered index failed for another reason"; }
echo "PASS: tampered index rejected ($(grep -m1 "^[WE]:" "$tmp/tamper.log" | cut -c1-80))"

# Tampered InRelease: the signature no longer matches.
rm -rf "$tmp/repo"; cp -a "$tmp/repo.orig" "$tmp/repo"
sed -i 's/^Label: PBP$/Label: PBX/' "$tmp/repo/dists/stable/InRelease"
rm -f "$tmp/repo/dists/stable/Release.gpg" "$tmp/repo/dists/stable/Release"
rm -rf "$tmp/apt/state/lists"; mkdir -p "$tmp/apt/state/lists/partial"
if aptget update >"$tmp/tamper2.log" 2>&1 && ! grep -q '^[WE]:' "$tmp/tamper2.log"; then
    cat "$tmp/tamper2.log"; fail "apt-get update accepted a tampered InRelease"
fi
grep -qi "signature" "$tmp/tamper2.log" || { cat "$tmp/tamper2.log"; fail "tampered InRelease failed for another reason"; }
echo "PASS: tampered InRelease rejected ($(grep -m1 "^[WE]:" "$tmp/tamper2.log" | cut -c1-80))"

# A publish whose live index cannot be fetched stops before any upload
# (the real v0.10.0 release is downloaded and verified first).
GNUPGHOME="$tmp/pubhome"; export GNUPGHOME
mkdir -m 700 "$GNUPGHOME"
gpg --batch --quiet --import "$tmp/key.asc"
fpr=$(gpg --batch --with-colons --list-secret-keys | awk -F: '/^fpr/ {print $10; exit}')
gpg --batch --armor --export "$fpr" > "$tmp/pub.asc"
printf 'R2_ACCOUNT_ID=test\nR2_ACCESS_KEY_ID=id\nR2_SECRET_ACCESS_KEY=secret\nR2_BUCKET=bucket\n' > "$tmp/creds"
chmod 600 "$tmp/creds"
if PBP_APT_KEY=$fpr PBP_R2_CREDENTIALS="$tmp/creds" PBP_APT_KEYRING="$tmp/pub.asc" PBP_APT_URL=https://127.0.0.1:9 \
    "$repo_dir/packaging/apt-publish.sh" v0.10.0 >"$tmp/unreachable.log" 2>&1; then
    cat "$tmp/unreachable.log"; fail "a publish with an unreachable repository succeeded"
fi
grep -q "fetching the live InRelease failed" "$tmp/unreachable.log" || { cat "$tmp/unreachable.log"; fail "unreachable repository: wrong failure"; }
grep -q '^uploaded' "$tmp/unreachable.log" && fail "something was uploaded"
echo "PASS: unreachable live index stops the publish before any upload"
