package aptrepo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Deb is a .deb's control data and the file's size and SHA256.
type Deb struct {
	Control Stanza
	Size    int64
	SHA256  string
}

const arMagic = "!<arch>\n"

// ReadDeb reads the control file of the .deb at path. It reads the ar
// container and a control.tar.gz (or plain control.tar) member itself, so
// it needs no Debian tools.
func ReadDeb(path string) (Deb, error) {
	// #nosec G304 -- the caller names the package file to index
	b, err := os.ReadFile(path)
	if err != nil {
		return Deb{}, err
	}
	sum := sha256.Sum256(b)
	ctl, err := controlFile(b)
	if err != nil {
		return Deb{}, fmt.Errorf("%s: %w", path, err)
	}
	st, err := ParsePackages(bytes.NewReader(ctl))
	if err != nil {
		return Deb{}, fmt.Errorf("%s: control: %w", path, err)
	}
	if len(st) != 1 {
		return Deb{}, fmt.Errorf("%s: control has %d stanzas, want 1", path, len(st))
	}
	return Deb{Control: st[0], Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:])}, nil
}

// controlFile returns the control file from a .deb's bytes.
func controlFile(b []byte) ([]byte, error) {
	if !bytes.HasPrefix(b, []byte(arMagic)) {
		return nil, errors.New("not a .deb (no ar header)")
	}
	for off := len(arMagic); off+60 <= len(b); {
		h := b[off : off+60]
		name := strings.TrimSuffix(strings.TrimRight(string(h[0:16]), " "), "/")
		size, err := strconv.ParseInt(strings.TrimSpace(string(h[48:58])), 10, 64)
		if err != nil || size < 0 || string(h[58:60]) != "`\n" {
			return nil, fmt.Errorf("bad ar header at offset %d", off)
		}
		start := off + 60
		end := start + int(size)
		if end > len(b) {
			return nil, fmt.Errorf("ar member %s is truncated", name)
		}
		data := b[start:end]
		switch {
		case name == "control.tar.gz":
			zr, err := gzip.NewReader(bytes.NewReader(data))
			if err != nil {
				return nil, fmt.Errorf("control.tar.gz: %w", err)
			}
			return fromTar(zr)
		case name == "control.tar":
			return fromTar(bytes.NewReader(data))
		case strings.HasPrefix(name, "control.tar."):
			return nil, fmt.Errorf("unsupported compression of the control member %s (want control.tar.gz or control.tar)", name)
		}
		off = end + int(size%2)
	}
	return nil, errors.New("no control.tar member")
}

// fromTar returns ./control from a control tar.
func fromTar(r io.Reader) ([]byte, error) {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("control.tar has no control file")
		}
		if err != nil {
			return nil, fmt.Errorf("control.tar: %w", err)
		}
		if h.Name == "./control" || h.Name == "control" {
			return io.ReadAll(io.LimitReader(tr, 1<<20))
		}
	}
}
