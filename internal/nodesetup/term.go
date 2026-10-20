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

// EchoOff turns off echo on the terminal fd, so a pasted secret is not shown,
// and returns a function that restores the terminal's previous settings.
func EchoOff(fd int) (restore func(), err error) {
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, fmt.Errorf("read terminal settings: %w", err)
	}
	quiet := *old
	quiet.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &quiet); err != nil {
		return nil, fmt.Errorf("turn off terminal echo: %w", err)
	}
	return func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, old) }, nil
}
