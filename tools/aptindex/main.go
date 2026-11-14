// Command aptindex adds .deb files to a local copy of PBP's APT repository
// and rewrites the suite's index files and its unsigned Release (issue
// #130). packaging/apt-publish.sh runs it, then signs and uploads.
//
// usage: aptindex -root DIR -date "Mon, 02 Jan 2006 15:04:05 UTC" [-suite stable] DEB...
//
// It reads DIR/dists/<suite>/main/binary-<arch>/Packages where present,
// copies each new .deb to DIR/pool/main/<p>/<package>/, and prints
// "added <file>" or "present <file>" for each.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/aptrepo"
)

// archs are the architectures the repository carries.
var archs = []string{"amd64", "arm64"}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "aptindex:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("aptindex", flag.ContinueOnError)
	root := fs.String("root", "", "the local repository tree")
	date := fs.String("date", "", `the Release date, RFC 1123 in UTC ("Mon, 02 Jan 2006 15:04:05 UTC")`)
	suite := fs.String("suite", "stable", "the suite")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *root == "" {
		return errors.New("-root is required")
	}
	when, err := time.Parse(time.RFC1123, *date)
	if err != nil {
		return fmt.Errorf("-date: %w", err)
	}
	s := aptrepo.Suite{Name: *suite, Archs: archs, Date: when, Packages: map[string][]aptrepo.Stanza{}}
	for _, arch := range archs {
		p := filepath.Join(*root, "dists", *suite, "main", "binary-"+arch, "Packages")
		// #nosec G304 -- inside the repository tree the caller names
		b, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		st, err := aptrepo.ParsePackages(bytes.NewReader(b))
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		s.Packages[arch] = st
	}
	for _, path := range fs.Args() {
		d, err := aptrepo.ReadDeb(path)
		if err != nil {
			return err
		}
		arch, pkg := d.Control.Get("Architecture"), d.Control.Get("Package")
		if !slices.Contains(archs, arch) {
			return fmt.Errorf("%s: architecture %q is not one of %v", path, arch, archs)
		}
		if pkg == "" {
			return fmt.Errorf("%s: no Package field", path)
		}
		rel := filepath.ToSlash(filepath.Join("pool", "main", pkg[:1], pkg, filepath.Base(path)))
		ix := aptrepo.Index{Stanzas: s.Packages[arch]}
		added, err := ix.Add(d, rel)
		if err != nil {
			return err
		}
		s.Packages[arch] = ix.Stanzas
		if !added {
			fmt.Fprintln(stdout, "present", filepath.Base(path))
			continue
		}
		if err := copyFile(path, filepath.Join(*root, filepath.FromSlash(rel))); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "added", filepath.Base(path))
	}
	return aptrepo.WriteSuite(*root, s)
}

// copyFile copies src to dst, creating dst's directory.
func copyFile(src, dst string) error {
	// #nosec G304 -- the caller names the package file to add
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { // #nosec G301 -- a public repository tree
		return err
	}
	return os.WriteFile(dst, b, 0o644) // #nosec G306 -- a public repository tree
}
