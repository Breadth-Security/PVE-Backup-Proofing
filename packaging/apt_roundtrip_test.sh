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
cmp -s "$tmp/repo/index.html" "$repo_dir/packaging/apt/index.html" || fail "--local did not add the landing page"
echo "PASS: --local adds the landing page"

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

# The live repository, served over HTTP on localhost (PBP_APT_PROTO lets
# the script fetch it without TLS, for tests only), by a stand-in for R2
# behind Cloudflare: the tree in $tmp/live is public at the root (/ serves
# index.html, as the Cloudflare rewrite does), and under /bucket it takes
# signed PUT, GET and ListObjectsV2 requests, as R2 does
# (PBP_R2_ENDPOINT points the script at it, for tests only). Marker files
# in the tree make it misbehave: .public-404 hides the public side,
# .inject adds a script before </body> of HTML as Cloudflare's bot
# detection does, .corrupt-byhash spoils by-hash uploads, and .list-fail
# fails bucket listings.
cat > "$tmp/fake_r2.py" <<'EOF'
import http.server, os, sys, urllib.parse
root, port = sys.argv[1], int(sys.argv[2])
BUCKET = "/bucket"

def marker(name):
    return os.path.exists(os.path.join(root, name))

class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def send(self, code, body=b"", ctype="application/octet-stream"):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def file(self, rel):
        base = os.path.realpath(root)
        full = os.path.realpath(os.path.join(base, urllib.parse.unquote(rel)))
        if not full.startswith(base + os.sep):
            return None
        return full

    def signed(self):
        return self.headers.get("Authorization", "").startswith("AWS4-HMAC-SHA256 ")

    def do_GET(self):
        u = urllib.parse.urlsplit(self.path)
        if u.path in (BUCKET, BUCKET + "/"):
            q = urllib.parse.parse_qs(u.query)
            if not self.signed() or q.get("list-type") != ["2"]:
                return self.send(403)
            if marker(".list-fail"):
                return self.send(500)
            prefix, limit = q.get("prefix", [""])[0], int(q.get("max-keys", ["1000"])[0])
            n = 0
            for d, _, files in os.walk(root):
                for f in files:
                    rel = os.path.relpath(os.path.join(d, f), root)
                    if not rel.startswith(".") and rel.startswith(prefix):
                        n += 1
            body = "<?xml version='1.0' encoding='UTF-8'?><ListBucketResult><KeyCount>%d</KeyCount></ListBucketResult>" % min(n, limit)
            return self.send(200, body.encode(), "application/xml")
        if u.path.startswith(BUCKET + "/"):
            if not self.signed():
                return self.send(403)
            rel, public = u.path[len(BUCKET) + 1:], False
        else:
            if marker(".public-404"):
                return self.send(404)
            rel, public = (u.path.lstrip("/") or "index.html"), True
        full = self.file(rel)
        if full is None or not os.path.isfile(full):
            return self.send(404)
        with open(full, "rb") as f:
            body = f.read()
        if public and rel.endswith(".html") and marker(".inject"):
            body = body.replace(b"\n</body>\n", b"\n<script>(function(){cdn()})();</script></body>\n")
        self.send(200, body, "text/html" if rel.endswith(".html") else "application/octet-stream")

    def do_PUT(self):
        u = urllib.parse.urlsplit(self.path)
        if not u.path.startswith(BUCKET + "/") or not self.signed():
            return self.send(403)
        data = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        rel = u.path[len(BUCKET) + 1:]
        if "/by-hash/" in rel and marker(".corrupt-byhash"):
            data += b"x"
        full = self.file(rel)
        if full is None:
            return self.send(400)
        os.makedirs(os.path.dirname(full), exist_ok=True)
        with open(full, "wb") as f:
            f.write(data)
        self.send(200)

http.server.ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever()
EOF
port=$((20000 + $$ % 20000))
mkdir -p "$tmp/live"
python3 -I "$tmp/fake_r2.py" "$tmp/live" "$port" >"$tmp/fake_r2.log" 2>&1 &
server=$!
trap 'kill "$server" 2>/dev/null; rm -rf "$tmp"' EXIT
i=0
until curl -s -o /dev/null "http://127.0.0.1:$port/"; do
    i=$((i + 1))
    [ "$i" -lt 50 ] || { cat "$tmp/fake_r2.log"; fail "the stand-in for R2 did not start on port $port"; }
    sleep 0.1
done
# The landing page with the throwaway key's fingerprint, as the script
# requires the page to name the key it publishes with.
sed "s/\(id=\"fingerprint\">\)[^<]*/\1$fpr/" "$repo_dir/packaging/apt/index.html" > "$tmp/page.html"
live_page="$tmp/page.html"
live() {
    PBP_APT_PROTO='=http' PBP_APT_URL="http://127.0.0.1:$port" PBP_APT_KEY=$fpr PBP_APT_KEYRING="$tmp/pub.asc" \
        PBP_APT_PAGE="$live_page" "$repo_dir/packaging/apt-publish.sh" "$@"
}
# kept_rm removes the work directory a dry run kept, named in log $1.
kept_rm() { d=$(sed -n 's/^work directory kept: //p' "$1"); [ -z "$d" ] || rm -rf "$d"; }

# An empty repository without --first is an error, not a first publish.
if live --dry-run v0.10.0 >"$tmp/empty.log" 2>&1; then cat "$tmp/empty.log"; fail "an empty repository was taken as new without --first"; fi
grep -q "not found (HTTP 404): pass --first" "$tmp/empty.log" || { cat "$tmp/empty.log"; fail "empty repository: wrong failure"; }
kept_rm "$tmp/empty.log"
echo "PASS: a missing InRelease without --first stops the publish"

# An upload interrupted after Packages and before InRelease: the live
# Packages is newer than the live InRelease names. A rerun still works,
# because it reads the index by hash, which never changes. A dry run needs
# no R2 credentials and leaves none on disk.
rm -rf "$tmp/live"; cp -a "$tmp/repo.orig" "$tmp/live"
cp "$live_page" "$tmp/live/index.html"
printf 'Package: pbp\nVersion: 9.9.9\n' >> "$tmp/live/dists/stable/main/binary-amd64/Packages"
if ! live --dry-run v0.10.0 >"$tmp/resume.log" 2>&1; then cat "$tmp/resume.log"; fail "a publish after an interrupted upload failed"; fi
grep -q "dry run: nothing uploaded" "$tmp/resume.log" || { cat "$tmp/resume.log"; fail "dry run did not finish"; }
kept=$(sed -n 's/^work directory kept: //p' "$tmp/resume.log")
[ -d "$kept" ] || fail "dry run did not keep its work directory"
[ ! -e "$kept/curl.cfg" ] || fail "dry run left R2 credentials on disk"
grep -q '^Version: 0.0.1$' "$kept/repo/dists/stable/main/binary-amd64/Packages" || fail "the rebuilt index lost the published version"
grep -q '^Version: 9.9.9$' "$kept/repo/dists/stable/main/binary-amd64/Packages" && fail "the rebuilt index took the half-uploaded Packages"
rm -rf "$kept"
echo "PASS: a publish after an interrupted upload reads the index by hash; a dry run keeps no credentials"

# The landing page: a publish compares it with the live one, and --page
# handles it alone. Dry runs only: an upload needs R2.
grep -q "^landing page is current$" "$tmp/resume.log" || { cat "$tmp/resume.log"; fail "publish: the live landing page was not found current"; }
grep -q "warning" "$tmp/resume.log" && { cat "$tmp/resume.log"; fail "publish: the root serves the page, yet it warned"; }
rm "$tmp/live/index.html"
if ! live --page --dry-run >"$tmp/page.log" 2>&1; then cat "$tmp/page.log"; fail "--page --dry-run failed"; fi
grep -q "^landing page differs from the live one: would upload$" "$tmp/page.log" || { cat "$tmp/page.log"; fail "--page: a missing live page was not reported"; }
grep -q '^uploaded' "$tmp/page.log" && fail "--page --dry-run uploaded something"
kept_rm "$tmp/page.log"
printf 'stale page\n' > "$tmp/live/index.html"
live --page --dry-run >"$tmp/page.log" 2>&1 || { cat "$tmp/page.log"; fail "--page --dry-run on a stale page failed"; }
grep -q "^landing page differs from the live one: would upload$" "$tmp/page.log" || { cat "$tmp/page.log"; fail "--page: a stale live page was not reported"; }
kept_rm "$tmp/page.log"
echo "PASS: the landing page is compared with the live one; --page works alone"

# A CDN's markup right before </body> (Cloudflare's bot detection adds a
# script there) still counts as the page, at /index.html and at the root;
# markup anywhere else does not.
sed 's#^</body>$#<script>(function(){cdn()})();</script></body>#' "$live_page" > "$tmp/live/index.html"
live --page --dry-run >"$tmp/page.log" 2>&1 || { cat "$tmp/page.log"; fail "--page --dry-run on a page with CDN markup failed"; }
grep -q "^landing page is current$" "$tmp/page.log" || { cat "$tmp/page.log"; fail "--page: CDN markup before </body> made the page differ"; }
grep -q "warning" "$tmp/page.log" && { cat "$tmp/page.log"; fail "--page: CDN markup before </body> made the root warn"; }
kept_rm "$tmp/page.log"
sed 's#^<body>$#<body><script>cdn()</script>#' "$live_page" > "$tmp/live/index.html"
live --page --dry-run >"$tmp/page.log" 2>&1 || { cat "$tmp/page.log"; fail "--page --dry-run on a page with markup after <body> failed"; }
grep -q "^landing page differs from the live one: would upload$" "$tmp/page.log" || { cat "$tmp/page.log"; fail "--page: markup elsewhere than before </body> was accepted"; }
kept_rm "$tmp/page.log"
differs() { # $1: what the served page is, for the message
    live --page --dry-run >"$tmp/page.log" 2>&1 || { cat "$tmp/page.log"; fail "--page --dry-run on $1 failed"; }
    grep -q "^landing page differs from the live one: would upload$" "$tmp/page.log" || { cat "$tmp/page.log"; fail "--page: $1 was taken as current"; }
    kept_rm "$tmp/page.log"
}
{ cat "$live_page"; printf '<p>extra</p>\n'; } > "$tmp/live/index.html"
differs "a page with markup after </html>"
# An older page with one more line before </body> (a footer since removed).
sed 's#^</body>$#<p>old footer</p>\n<script>cdn()</script></body>#' "$live_page" > "$tmp/live/index.html"
differs "an older page with an extra line before </body>"
sed 's#^</body>$#<p>old footer</p></body>#' "$live_page" > "$tmp/live/index.html"
differs "a page with non-script markup before </body>"
sed 's#^</body>$#<script>cdn()</script></body><p>after</p>#' "$live_page" > "$tmp/live/index.html"
differs "a page with markup after </body>"
grep -v '^</body>$' "$live_page" > "$tmp/live/index.html"
differs "a page without </body>"
: > "$tmp/live/index.html"
differs "an empty page"
echo "PASS: CDN markup right before </body> is tolerated, nowhere else"

# --page refuses every other mode, before doing any of their work.
for combo in "v0.10.0" "--init" "--first" "--local $tmp/x --key-file $tmp/key.asc" "--key-file $tmp/key.asc"; do
    rc=0
    # shellcheck disable=SC2086 # the options, split on purpose
    live --page $combo >"$tmp/combo.log" 2>&1 || rc=$?
    [ "$rc" -eq 2 ] || { cat "$tmp/combo.log"; fail "--page $combo: exit $rc, want 2 (usage)"; }
done
[ ! -e "$tmp/x" ] || fail "--page --local built a repository"
echo "PASS: --page with another mode is a usage error"

# A page that does not name the publishing key is never uploaded, and a
# publish says that only the page failed.
printf 'stale page\n' > "$tmp/live/index.html"
live_page="$repo_dir/packaging/apt/index.html"
if live --page --dry-run >"$tmp/badpage.log" 2>&1; then cat "$tmp/badpage.log"; fail "--page accepted a page with another key's fingerprint"; fi
grep -q "fingerprint" "$tmp/badpage.log" || { cat "$tmp/badpage.log"; fail "--page: wrong failure for a wrong fingerprint"; }
kept_rm "$tmp/badpage.log"
if live --dry-run v0.10.0 >"$tmp/badpub.log" 2>&1; then cat "$tmp/badpub.log"; fail "a publish with a wrong page succeeded"; fi
grep -q "only the landing page failed" "$tmp/badpub.log" || { cat "$tmp/badpub.log"; fail "publish: a page failure was not told apart"; }
kept_rm "$tmp/badpub.log"
live_page="$tmp/page.html"
echo "PASS: a page with the wrong fingerprint is refused; a publish says only the page failed"

# A failure of the index tool stops the publish before anything is signed:
# the live index already lists pbp 0.10.0 amd64 with other content.
d="$tmp/pkg-conflict"
mkdir -p "$d/DEBIAN"
printf 'Package: pbp\nVersion: 0.10.0\nArchitecture: amd64\nMaintainer: Test <test@example.invalid>\nDescription: PBP conflicting test\n' > "$d/DEBIAN/control"
dpkg-deb -Zgzip --root-owner-group --build "$d" "$tmp/pbp_0.10.0_amd64.deb" >/dev/null
rm -rf "$tmp/live"
"$repo_dir/packaging/apt-publish.sh" --local "$tmp/live" --key-file "$tmp/key.asc" "$tmp/pbp_0.10.0_amd64.deb" >/dev/null
if live --dry-run v0.10.0 >"$tmp/conflict.log" 2>&1; then cat "$tmp/conflict.log"; fail "a publish whose index tool failed succeeded"; fi
grep -q "the index tool failed" "$tmp/conflict.log" || { cat "$tmp/conflict.log"; fail "an index tool failure was not reported as such"; }
kept_rm "$tmp/conflict.log"
echo "PASS: an index tool failure stops the publish before signing"

# r2 runs the script against the stand-in for R2.
r2() { PBP_R2_CREDENTIALS="$tmp/creds" PBP_R2_ENDPOINT="http://127.0.0.1:$port/bucket" live "$@"; }

# --first checks the bucket itself: an empty one is a first publish, but a
# public 404 in front of a bucket that holds an index is not.
rm -rf "$tmp/live"; mkdir -p "$tmp/live"
r2 --first --dry-run v0.10.0 >"$tmp/first.log" 2>&1 || { cat "$tmp/first.log"; fail "--first on an empty bucket failed"; }
grep -q "dry run: nothing uploaded" "$tmp/first.log" || { cat "$tmp/first.log"; fail "--first on an empty bucket did not finish"; }
kept=$(sed -n 's/^work directory kept: //p' "$tmp/first.log")
[ ! -e "$kept/curl.cfg" ] || fail "--first --dry-run left R2 credentials on disk"
kept_rm "$tmp/first.log"
# A listing that fails is reported as such, not as a full bucket.
touch "$tmp/live/.list-fail"
if r2 --first --dry-run v0.10.0 >"$tmp/first.log" 2>&1; then cat "$tmp/first.log"; fail "--first went ahead although the listing failed"; fi
grep -q "listing dists/ in R2 failed" "$tmp/first.log" || { cat "$tmp/first.log"; fail "--first: a failed listing was not reported as such"; }
grep -q "already holds" "$tmp/first.log" && { cat "$tmp/first.log"; fail "--first: a failed listing was reported as a full bucket"; }
kept_rm "$tmp/first.log"
rm "$tmp/live/.list-fail"
# A real first publish into the empty bucket.
r2 --first v0.10.0 >"$tmp/first.log" 2>&1 || { cat "$tmp/first.log"; fail "a first publish failed"; }
grep -q "^published and verified: v0.10.0$" "$tmp/first.log" || { cat "$tmp/first.log"; fail "a first publish did not verify"; }
rm -rf "$tmp/live"; mkdir -p "$tmp/live"
cp -a "$tmp/repo.orig/." "$tmp/live/"
touch "$tmp/live/.public-404"
if r2 --first --dry-run v0.10.0 >"$tmp/first.log" 2>&1; then cat "$tmp/first.log"; fail "--first went ahead although the bucket holds an index"; fi
grep -q "the bucket already holds" "$tmp/first.log" || { cat "$tmp/first.log"; fail "--first: wrong failure for a non-empty bucket"; }
kept_rm "$tmp/first.log"
rm "$tmp/live/.public-404"
echo "PASS: --first trusts the bucket, not the public 404"

# PBP_R2_ENDPOINT, a test hook, never sends anything to another host.
if PBP_R2_CREDENTIALS="$tmp/creds" PBP_R2_ENDPOINT="https://r2.example.invalid/bucket" live --page >"$tmp/endpoint.log" 2>&1; then
    cat "$tmp/endpoint.log"; fail "a non-local PBP_R2_ENDPOINT was used"
fi
grep -q "PBP_R2_ENDPOINT" "$tmp/endpoint.log" || { cat "$tmp/endpoint.log"; fail "a non-local PBP_R2_ENDPOINT failed for another reason"; }
echo "PASS: PBP_R2_ENDPOINT is refused unless it is local"

# A real publish and page upload against the stand-in: everything is read
# back, the page from the bucket byte for byte and through the domain with
# the injected script.
rm -rf "$tmp/live"; cp -a "$tmp/repo.orig" "$tmp/live"; cp "$live_page" "$tmp/live/index.html"
r2 v0.10.0 >"$tmp/pub.log" 2>&1 || { cat "$tmp/pub.log"; fail "a publish to the stand-in failed"; }
grep -q "^published and verified: v0.10.0$" "$tmp/pub.log" || { cat "$tmp/pub.log"; fail "publish: no verified line"; }
[ -f "$tmp/live/pool/main/p/pbp/pbp_0.10.0_amd64.deb" ] || fail "publish: the package is not in the bucket"
grep -q "^landing page is current$" "$tmp/pub.log" || { cat "$tmp/pub.log"; fail "publish: the page was not current"; }
rm "$tmp/live/index.html"; touch "$tmp/live/.inject"
r2 --page >"$tmp/pageup.log" 2>&1 || { cat "$tmp/pageup.log"; fail "--page against the stand-in failed"; }
grep -q "^uploaded index.html$" "$tmp/pageup.log" || { cat "$tmp/pageup.log"; fail "--page did not upload"; }
grep -q "warning" "$tmp/pageup.log" && { cat "$tmp/pageup.log"; fail "--page warned about the root"; }
cmp -s "$tmp/live/index.html" "$live_page" || fail "--page stored another page"
rm "$tmp/live/.inject"
echo "PASS: a publish and a page upload are read back"

# A by-hash file that arrives spoiled fails the read-back.
touch "$tmp/live/.corrupt-byhash"
if r2 v0.10.0 >"$tmp/byhash.log" 2>&1; then cat "$tmp/byhash.log"; fail "a spoiled by-hash upload passed the read-back"; fi
grep -q "by-hash" "$tmp/byhash.log" || { cat "$tmp/byhash.log"; fail "a spoiled by-hash upload failed for another reason"; }
rm "$tmp/live/.corrupt-byhash"
echo "PASS: the read-back checks the by-hash files"

# --init, from a copy of the script beside its own allowed_signers (a
# throwaway release key): a dry run writes no key file and uploads
# nothing; a real run uploads the keyring and its signature and writes it.
mkdir -p "$tmp/initrepo/packaging"
cp "$repo_dir/packaging/apt-publish.sh" "$tmp/initrepo/packaging/"
ssh-keygen -q -t ed25519 -N '' -C test -f "$tmp/sshkey"
printf 'pbp-release namespaces="pbp-release" %s\n' "$(cut -d' ' -f1,2 "$tmp/sshkey.pub")" > "$tmp/initrepo/allowed_signers"
init() {
    PBP_APT_KEY=$fpr PBP_SIGNING_KEY="$tmp/sshkey" PBP_R2_CREDENTIALS="$tmp/creds" \
        PBP_R2_ENDPOINT="http://127.0.0.1:$port/bucket" PBP_APT_PROTO='=http' "$tmp/initrepo/packaging/apt-publish.sh" "$@"
}
rm -rf "$tmp/live"; mkdir -p "$tmp/live"
init --init --dry-run >"$tmp/init.log" 2>&1 || { cat "$tmp/init.log"; fail "--init --dry-run failed"; }
[ ! -e "$tmp/initrepo/packaging/apt/pbp-archive-keyring.asc" ] || fail "--init --dry-run wrote the key file"
grep -q '^uploaded' "$tmp/init.log" && fail "--init --dry-run uploaded something"
kept_rm "$tmp/init.log"
init --init >"$tmp/init.log" 2>&1 || { cat "$tmp/init.log"; fail "--init failed"; }
[ -s "$tmp/initrepo/packaging/apt/pbp-archive-keyring.asc" ] || fail "--init wrote no key file"
if [ ! -s "$tmp/live/pbp-archive-keyring.gpg" ] || [ ! -s "$tmp/live/pbp-archive-keyring.gpg.sig" ]; then fail "--init uploaded no keyring or signature"; fi
echo "PASS: --init --dry-run changes nothing; --init uploads and writes the key"
