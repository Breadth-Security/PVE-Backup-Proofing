// Package aptrepo builds the index files of PBP's APT repository (issue
// #130): it reads .deb control data and keeps the Packages stanzas, and
// writes a suite with by-hash index files and an unsigned Release. Signing
// and uploading are packaging/apt-publish.sh's.
package aptrepo

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Field is one field of a stanza. A multi-line value keeps its continuation
// lines, each still starting with a space, joined by "\n".
type Field struct{ Name, Value string }

// Stanza is one paragraph of a Debian control-style file, in file order.
type Stanza []Field

// Get returns the value of the field name, or "".
func (s Stanza) Get(name string) string {
	for _, f := range s {
		if f.Name == name {
			return f.Value
		}
	}
	return ""
}

// ParsePackages reads the stanzas of a Packages (or control) file.
func ParsePackages(r io.Reader) ([]Stanza, error) {
	var out []Stanza
	var cur Stanza
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		ln := sc.Text()
		switch {
		case ln == "":
			if len(cur) > 0 {
				out = append(out, cur)
				cur = nil
			}
		case ln[0] == ' ' || ln[0] == '\t':
			if len(cur) == 0 {
				return nil, fmt.Errorf("line %d: continuation line without a field", n)
			}
			cur[len(cur)-1].Value += "\n" + ln
		default:
			name, value, ok := strings.Cut(ln, ":")
			if !ok || name == "" {
				return nil, fmt.Errorf("line %d: not a field: %q", n, ln)
			}
			cur = append(cur, Field{Name: name, Value: strings.TrimPrefix(value, " ")})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out, nil
}

// FormatPackages writes stanzas as a Packages file: fields in order,
// stanzas separated by a blank line.
func FormatPackages(st []Stanza) []byte {
	var b bytes.Buffer
	for i, s := range st {
		if i > 0 {
			b.WriteByte('\n')
		}
		for _, f := range s {
			b.WriteString(f.Name + ":")
			if f.Value != "" && !strings.HasPrefix(f.Value, "\n") {
				b.WriteByte(' ')
			}
			b.WriteString(f.Value + "\n")
		}
	}
	return b.Bytes()
}

// Index is the Packages stanzas of one architecture, in the order added.
type Index struct{ Stanzas []Stanza }

// Add appends d's stanza, with filename (its path in the repository), Size
// and SHA256. A package already present with the same SHA256 is a no-op
// (added false); with another SHA256 it is an error, since a published
// file never changes. A pre-release version (with "~") is refused.
func (ix *Index) Add(d Deb, filename string) (added bool, err error) {
	pkg, ver, arch := d.Control.Get("Package"), d.Control.Get("Version"), d.Control.Get("Architecture")
	if pkg == "" || ver == "" || arch == "" {
		return false, errors.New("control data lacks Package, Version or Architecture")
	}
	if strings.Contains(ver, "~") {
		return false, fmt.Errorf("%s %s is a pre-release: the repository carries final releases only", pkg, ver)
	}
	for _, s := range ix.Stanzas {
		if s.Get("Package") != pkg || s.Get("Version") != ver || s.Get("Architecture") != arch {
			continue
		}
		if s.Get("SHA256") == d.SHA256 {
			return false, nil
		}
		return false, fmt.Errorf("%s %s %s is already published with a different SHA256 (%s, now %s): a published file never changes",
			pkg, ver, arch, s.Get("SHA256"), d.SHA256)
	}
	s := append(Stanza{}, d.Control...)
	s = append(s, Field{"Filename", filename}, Field{"Size", strconv.FormatInt(d.Size, 10)}, Field{"SHA256", d.SHA256})
	ix.Stanzas = append(ix.Stanzas, s)
	return true, nil
}
