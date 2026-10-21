// Package probe decides whether a booted restored guest is healthy by running
// commands inside it through an Executor.
package probe

import (
	"context"
	"errors"
)

// ExecResult is the outcome of one command run inside the guest.
type ExecResult struct {
	Stdout, Stderr string
	ExitCode       int
}

// Executor runs a command inside the guest. The error is non-nil only when the
// command could not be run at all; a non-zero exit is reported via ExitCode.
type Executor interface {
	Exec(ctx context.Context, argv []string) (ExecResult, error)
}

// ErrExecBlocked is returned by executors when the guest agent refuses exec.
var ErrExecBlocked = errors.New("agent cannot run commands")

// ErrNoAnswer is returned by executors when the transport never got the
// command to the guest (for example the SSH connection to the node died
// silently). It is a transport fault even when the command's own timeout
// ended the wait: the check did not time out, it never ran.
var ErrNoAnswer = errors.New("node did not answer")

// OSFamily is the guest operating system family.
type OSFamily string

const (
	Linux   OSFamily = "linux"
	Windows OSFamily = "windows"
	Unknown OSFamily = "unknown"
)
