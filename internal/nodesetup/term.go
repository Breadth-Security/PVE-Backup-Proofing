package nodesetup

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"golang.org/x/sys/unix"
)

// IsTerminal reports whether fd is a terminal.
func IsTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	return err == nil
}

// Confirm writes "<question> [y/N] " to w and reads one line from r. Only
// "y" or "yes" (any case, surrounding spaces ignored) is a yes; anything
// else, an empty line or end of input is a no.
func Confirm(r io.Reader, w io.Writer, question string) bool {
	_, _ = fmt.Fprint(w, question+" [y/N] ")
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}
