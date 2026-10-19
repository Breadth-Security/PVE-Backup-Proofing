package sshx

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// OpenSSH does not pass a cancelled context on to the remote command, so a
// command run with a deadline is bounded on the node by coreutils timeout:
// the remaining time rounded up to whole seconds (at least 1s). Without a
// deadline the command is sent unchanged.
func TestBounded(t *testing.T) {
	cmd := "pct exec 990002 -- " + Quote("sh", "-c", "echo 'hi'")
	if got := bounded(context.Background(), cmd); got != cmd {
		t.Errorf("no deadline: got %q", got)
	}
	long, cancel := context.WithTimeout(context.Background(), 90*time.Second+500*time.Millisecond)
	defer cancel()
	if got, want := bounded(long, cmd), "timeout --kill-after=5s 91s sh -c "+Quote(cmd); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	short, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	if got, want := bounded(short, "true"), "timeout --kill-after=5s 1s sh -c 'true'"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	past, cancel3 := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel3()
	if got, want := bounded(past, "true"), "timeout --kill-after=5s 1s sh -c 'true'"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The wrapped command runs exactly as the original: quotes, shell operators
// and exit codes pass through the wrapper unchanged.
func TestBoundedRunsTheSameCommand(t *testing.T) {
	for _, tool := range []string{"sh", "timeout"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available: %v", tool, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := "printf '%s|' " + Quote("a b", `it's`, `"q"`, "$HOME", "`x`", ";", "\\") + "; test -e /nonexistent || exit 3"
	run := func(c string) (string, int) {
		out, err := exec.CommandContext(ctx, "sh", "-c", c).Output() // #nosec G204 -- test-built command
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return string(out), code
	}
	wantOut, wantCode := run(cmd)
	if wantCode != 3 || wantOut != `a b|it's|"q"|$HOME|`+"`x`"+`|;|\|` {
		t.Fatalf("plain command: %q exit %d", wantOut, wantCode)
	}
	if out, code := run(bounded(ctx, cmd)); out != wantOut || code != wantCode {
		t.Errorf("bounded: %q exit %d, want %q exit %d", out, code, wantOut, wantCode)
	}
}

// Client.Run sends the bounded command when its context has a deadline.
func TestRunSendsBoundedCommand(t *testing.T) {
	s := newServer(t, true)
	c, err := Dial(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if res, err := c.Run(context.Background(), "hello", nil); err != nil || res.Stdout != "ok:hello" {
		t.Errorf("no deadline: %+v, %v", res, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := c.Run(ctx, "hello", nil)
	if want := "ok:timeout --kill-after=5s 30s sh -c 'hello'"; err != nil || res.Stdout != want {
		t.Errorf("deadline: %+v, %v; want stdout %q", res, err, want)
	}
}
