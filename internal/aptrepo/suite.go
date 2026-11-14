package aptrepo

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Suite is one suite of the repository, with a single component "main".
type Suite struct {
	Name  string
	Archs []string
	Date  time.Time
	// Packages holds each architecture's stanzas, keyed by architecture.
	Packages map[string][]Stanza
}

// WriteSuite writes s under root/dists/<Name>/: each architecture's
// Packages and Packages.gz, a copy of each under by-hash/SHA256/<hash>, and
// the unsigned Release listing them. By-hash files from earlier runs are
// left in place, so a client holding the previous Release still finds the
// files it names while an upload is under way. The output depends only on
// s: Packages.gz carries no name or timestamp.
func WriteSuite(root string, s Suite) error {
	base := filepath.Join(root, "dists", s.Name)
	type entry struct {
		path string // relative to base
		sum  string
		size int
	}
	var entries []entry
	for _, arch := range s.Archs {
		dir := "main/binary-" + arch
		plain := FormatPackages(s.Packages[arch])
		gz, err := gzipBytes(plain)
		if err != nil {
			return err
		}
		for _, f := range []struct {
			name string
			data []byte
		}{{"Packages", plain}, {"Packages.gz", gz}} {
			sum := sha256.Sum256(f.data)
			hexSum := hex.EncodeToString(sum[:])
			if err := writeFile(filepath.Join(base, dir, "by-hash", "SHA256", hexSum), f.data); err != nil {
				return err
			}
			if err := writeFile(filepath.Join(base, dir, f.name), f.data); err != nil {
				return err
			}
			entries = append(entries, entry{dir + "/" + f.name, hexSum, len(f.data)})
		}
	}
	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.path, b.path) })
	var rel bytes.Buffer
	fmt.Fprintf(&rel, "Origin: PBP\nLabel: PBP\nSuite: %s\nCodename: %s\nDate: %s\nArchitectures: %s\nComponents: main\nAcquire-By-Hash: yes\nSHA256:\n",
		s.Name, s.Name, s.Date.UTC().Format(time.RFC1123), strings.Join(s.Archs, " "))
	for _, e := range entries {
		fmt.Fprintf(&rel, " %s %d %s\n", e.sum, e.size, e.path)
	}
	return writeFile(filepath.Join(base, "Release"), rel.Bytes())
}

// gzipBytes compresses b with a fixed level and an empty header (no name,
// zero mtime), so the same input always gives the same bytes.
func gzipBytes(b []byte) ([]byte, error) {
	var out bytes.Buffer
	zw, err := gzip.NewWriterLevel(&out, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// writeFile writes data to p through a temporary file and a rename.
func writeFile(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil { // #nosec G301 -- a public repository tree
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil { // #nosec G306 -- a public repository tree
		return err
	}
	return os.Rename(tmp, p)
}
