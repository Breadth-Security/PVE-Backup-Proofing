package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain lets a test re-run this binary as the real `pbp` (main()), so the
// signal handling of a whole process can be observed from outside.
func TestMain(m *testing.M) {
	if os.Getenv("PBP_TEST_RUN_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// pbpProcess returns `pbp node` as a child process running main().
func pbpProcess(t *testing.T, request string) *exec.Cmd {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "node.yaml")
	if err := os.WriteFile(cfgPath, []byte("vmid_range: [990000, 990049]\nbridge: vmbr-pbp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// #nosec G204 -- re-runs this test binary.
	cmd := exec.Command(os.Args[0], "node")
	cmd.Env = append(os.Environ(), "PBP_TEST_RUN_MAIN=1", "PBP_NODE_CONFIG="+cfgPath,
		"SSH_ORIGINAL_COMMAND="+request)
	return cmd
}

// exitedNormally fails the test if err says the process died from a signal.
func exitedNormally(t *testing.T, err error) int {
	t.Helper()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatalf("wait: %v", err)
	}
	if ee != nil {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			t.Fatalf("pbp node was killed by %v; it must survive it and finish", ws.Signal())
		}
		return ee.ExitCode()
	}
	return 0
}

// TestNodeSurvivesClosedOutput: when the SSH client is gone, writes to the
// helper's stdout and stderr fail with EPIPE. The helper must not die from
// SIGPIPE before it has cleaned up (killed the guest command's group).
func TestNodeSurvivesClosedOutput(t *testing.T) {
	req, _ := nodeRequest(t, "version", time.Minute)
	cmd := pbpProcess(t, req)
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// Close the read ends first: every write the helper makes (the version on
	// stdout, or a refusal on stderr when running as root ignores
	// PBP_NODE_CONFIG) hits a broken pipe.
	_ = outR.Close()
	_ = errR.Close()
	cmd.Stdout, cmd.Stderr = outW, errW
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = outW.Close()
	_ = errW.Close()
	exitedNormally(t, cmd.Wait())
}

// sigMask reports whether the given mask line (SigIgn, SigCgt) of pid's
// /proc status contains sig.
func sigMask(pid, field string, sig syscall.Signal) (bool, error) {
	b, err := os.ReadFile("/proc/" + pid + "/status")
	if err != nil {
		return false, err
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, field+":"); ok {
			mask, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
			if err != nil {
				return false, err
			}
			return mask&(1<<(uint(sig)-1)) != 0, nil
		}
	}
	return false, errors.New("no " + field + " line")
}

// TestNodeSignalsNotInherited: the helper catches SIGPIPE and SIGHUP rather
// than ignoring them, because an ignored disposition survives exec — pct,
// lxc-attach and the guest's check commands would start with them ignored,
// changing how pipelines like "producer | head" behave in a guest check.
// Caught signals are reset to the default in an exec'd child.
func TestNodeSignalsNotInherited(t *testing.T) {
	if _, err := os.Stat("/proc/self/status"); err != nil {
		t.Skip("needs /proc")
	}
	stop := catchNodeSignals()
	t.Cleanup(stop)
	for _, sig := range []syscall.Signal{syscall.SIGPIPE, syscall.SIGHUP} {
		if ign, err := sigMask("self", "SigIgn", sig); err != nil || ign {
			t.Fatalf("helper ignores %v (err %v); it must catch it instead", sig, err)
		}
		if cgt, err := sigMask("self", "SigCgt", sig); err != nil || !cgt {
			t.Fatalf("helper does not catch %v (err %v)", sig, err)
		}
	}
	// A child exec'd now must see neither signal ignored.
	out, err := exec.Command("/bin/sh", "-c", "grep '^SigIgn:' /proc/self/status").Output()
	if err != nil {
		t.Fatal(err)
	}
	mask, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "SigIgn:")), 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	for _, sig := range []syscall.Signal{syscall.SIGPIPE, syscall.SIGHUP} {
		if mask&(1<<(uint(sig)-1)) != 0 {
			t.Errorf("child inherited %v as ignored (SigIgn %x)", sig, mask)
		}
	}
}

// TestNodeSurvivesHangup: a hangup (the SSH session going away) must not
// kill the helper either; its own deadline handling ends the request.
func TestNodeSurvivesHangup(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root PBP_NODE_CONFIG is ignored and the helper exits at once")
	}
	if _, err := os.Stat("/proc/self/status"); err != nil {
		t.Skip("needs /proc")
	}
	// notify reads its payload from stdin, so the helper waits there.
	req, _ := nodeRequest(t, "notify", time.Minute)
	cmd := pbpProcess(t, req)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	ready := false
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		pid := strconv.Itoa(cmd.Process.Pid)
		hup, err1 := sigMask(pid, "SigCgt", syscall.SIGHUP)
		pipe, err2 := sigMask(pid, "SigCgt", syscall.SIGPIPE)
		if err1 == nil && err2 == nil && hup && pipe {
			ready = true
			break
		}
	}
	if !ready {
		t.Fatal("pbp node never caught SIGHUP and SIGPIPE")
	}
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close() // empty payload: refused as bad-payload
	if code := exitedNormally(t, cmd.Wait()); code != 250 {
		t.Errorf("exit %d, want 250 (bad-payload refusal)", code)
	}
}
