package nodesetup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPasswdLookup(t *testing.T) {
	p := filepath.Join(t.TempDir(), "passwd")
	mustWrite(t, p, "root:x:0:0:root:/root:/bin/bash\n"+
		"# comment\n"+
		"broken line\n"+
		"bad:x:notanumber:0::/x:/bin/sh\n"+
		"pbp:x:998:997:PBP:/var/lib/pbp:/usr/sbin/nologin\n"+
		"noshell:x:1001:1001::/home/n:\n", 0o644)
	look := passwdLookup(p)

	uid, gid, home, shell, err := look("pbp")
	if err != nil || uid != 998 || gid != 997 || home != "/var/lib/pbp" || shell != "/usr/sbin/nologin" {
		t.Errorf("pbp = %d %d %q %q %v", uid, gid, home, shell, err)
	}
	uid, gid, home, shell, err = look("root")
	if err != nil || uid != 0 || gid != 0 || home != "/root" || shell != "/bin/bash" {
		t.Errorf("root = %d %d %q %q %v", uid, gid, home, shell, err)
	}
	if _, _, _, shell, err = look("noshell"); err != nil || shell != "" {
		t.Errorf("noshell = %q %v", shell, err)
	}
	for _, u := range []string{"nobody", "bad", "", "root:x"} {
		if _, _, _, _, err := look(u); !errors.Is(err, ErrNoSuchUser) {
			t.Errorf("%q: err %v, want ErrNoSuchUser", u, err)
		}
	}

	_, _, _, _, err = passwdLookup(filepath.Join(t.TempDir(), "missing"))("pbp")
	if err == nil || errors.Is(err, ErrNoSuchUser) {
		t.Errorf("unreadable passwd: err %v, want a read error, not ErrNoSuchUser", err)
	}
}

func TestNewEnv(t *testing.T) {
	e := NewEnv()
	if e.Root != "/" || e.Run == nil || e.Chown == nil || e.Lookup == nil || e.Now == nil {
		t.Fatalf("NewEnv = %+v", e)
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	// Run captures stdout and stderr separately and reports the exit code;
	// it runs with the helper's minimal environment, not the caller's.
	t.Setenv("PBP_LEAK_CHECK", "leaked")
	out, errOut, code, err := e.Run(t.Context(), "/bin/sh", []string{"-c", "echo out; echo err >&2; echo \"$PBP_LEAK_CHECK\"; cat; exit 3"}, []byte("in\n"))
	if err != nil || code != 3 || out != "out\n\nin\n" || errOut != "err\n" {
		t.Errorf("Run = %q %q %d %v", out, errOut, code, err)
	}
}
