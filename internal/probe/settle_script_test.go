package probe

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// stubBin writes executable sh scripts into a temp dir and returns it.
func stubBin(t *testing.T, scripts map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// runSettle runs openRCSettleScript under /bin/sh with PATH limited to the
// stubs and the system's basic tools, and returns its exit code and run
// time. The script's 1 s sleep makes each loop take a second.
func runSettle(t *testing.T, bin string) (int, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", openRCSettleScript)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "STATE=" + bin}
	start := time.Now()
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatal("settle script never finished")
	}
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, time.Since(start)
}

// counter is a stub fragment: n is how many times this stub has run.
const counter = `f="$STATE/.n-$(basename "$0")-$1"; n=$(( $(cat "$f" 2>/dev/null || echo 0) + 1 )); echo $n > "$f"
`

// The OpenRC settle script decides when rc-status may be trusted: past the
// boot runlevel, no openrc process changing runlevel, and no service
// starting or stopping. It runs here under a real sh against stubs (code
// review I21; the bug fixed in 4f6b70d lived in this logic).
func TestOpenRCSettleScript(t *testing.T) {
	t.Run("no rc-status exits 127", func(t *testing.T) {
		if code, _ := runSettle(t, stubBin(t, nil)); code != 127 {
			t.Errorf("exit %d, want 127", code)
		}
	})
	t.Run("settled at once", func(t *testing.T) {
		bin := stubBin(t, map[string]string{
			"rc-status": `case "$1" in -r) echo default;; -a) echo " sshd   [  started  ]";; esac`,
			"pidof":     "exit 1",
		})
		if code, el := runSettle(t, bin); code != 0 || el > 900*time.Millisecond {
			t.Errorf("exit %d after %v, want 0 at once", code, el)
		}
	})
	for _, tc := range []struct {
		name  string
		stubs map[string]string
	}{
		{"waits for the boot runlevel to end", map[string]string{
			"rc-status": counter + `case "$1" in -r) if [ $n -lt 3 ]; then echo boot; else echo default; fi;; -a) echo " sshd [ started ]";; esac`,
			"pidof":     "exit 1",
		}},
		{"waits for openrc to exit", map[string]string{
			"rc-status": `case "$1" in -r) echo default;; -a) echo " sshd [ started ]";; esac`,
			"pidof":     counter + `[ $n -lt 3 ] && { echo 42; exit 0; }; exit 1`,
		}},
		{"waits while a service is starting", map[string]string{
			"rc-status": counter + `case "$1" in -r) echo default;; -a) if [ $n -lt 3 ]; then echo " nginx  [  starting  ]"; else echo " nginx [ started ]"; fi;; esac`,
			"pidof":     "exit 1",
		}},
		{"waits while a service is stopping", map[string]string{
			"rc-status": counter + `case "$1" in -r) echo default;; -a) if [ $n -lt 3 ]; then echo " cron [stopping]"; else echo " cron [ stopped ]"; fi;; esac`,
			"pidof":     "exit 1",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, el := runSettle(t, stubBin(t, tc.stubs))
			if code != 0 || el < 1500*time.Millisecond {
				t.Errorf("exit %d after %v, want 0 after two loops", code, el)
			}
		})
	}
	t.Run("an empty runlevel is not settled", func(t *testing.T) {
		bin := stubBin(t, map[string]string{
			"rc-status": counter + `case "$1" in -r) if [ $n -lt 2 ]; then echo; else echo default; fi;; -a) :;; esac`,
			"pidof":     "exit 1",
		})
		if code, el := runSettle(t, bin); code != 0 || el < 900*time.Millisecond {
			t.Errorf("exit %d after %v", code, el)
		}
	})
}
