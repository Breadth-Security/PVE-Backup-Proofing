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
	"regexp"
	"slices"
	"time"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/aptrepo"
)

// archs are the architectures the repository carries.
var archs = []string{"amd64", "arm64"}

// rePackage is Debian policy's package name rule (5.6.1): lower-case
// letters, digits, '+', '-' and '.', at least two characters, starting
// with a letter or digit.
var rePackage = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`)

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
		if !rePackage.MatchString(pkg) {
			// It becomes a path in the pool (pool/main/<p>/<package>/).
			return fmt.Errorf("%s: %q is not a valid Debian package name", path, pkg)
		}
		rel := filepath.ToSlash(filepath.Join("pool", "main", pkg[:1], pkg, filepath.Base(path)))
		ix := aptrepo.Index{Stanzas: s.Packages[arch]}
		added, err := ix.Add(d, rel)
		if err != nil {
			return err
		}
		s.Packages[arch] = ix.Stanzas
		if !added {
			if _, err := fmt.Fprintln(stdout, "present", filepath.Base(path)); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(path, filepath.Join(*root, filepath.FromSlash(rel))); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(stdout, "added", filepath.Base(path)); err != nil {
			return err
		}
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
	// #nosec G306 G703 -- a public repository tree; dst is under -root, built from a checked package name and a file's base name
	return os.WriteFile(dst, b, 0o644)
}
