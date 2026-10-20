package hostsetup

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type owned struct{ uid, gid int }

// fakeOwners replaces the ownership seams and the deb root for one test and
// returns the recorded owner of every chown'd path.
func fakeOwners(t *testing.T) (debDir string, owners func() map[string]owned) {
	t.Helper()
	var mu sync.Mutex
	got := map[string]owned{}
	oldChown, oldGroup, oldRoot := chown, lookupGroup, debRoot
	chown = func(p string, uid, gid int) error {
		mu.Lock()
		defer mu.Unlock()
		got[finalName(p)] = owned{uid, gid}
		return nil
	}
	lookupGroup = func(name string) (int, error) {
		if name != "pbp" {
			t.Errorf("lookupGroup(%q), want pbp", name)
		}
		return 4242, nil
	}
	debRoot = t.TempDir()
	t.Cleanup(func() { chown, lookupGroup, debRoot = oldChown, oldGroup, oldRoot })
	return debRoot, func() map[string]owned {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]owned{}
		for k, v := range got {
			out[k] = v
		}
		return out
	}
}

// finalName maps writeFile's temp name (".<base>.tmp<rand>", chown'd before
// the rename) to the path it is renamed to.
func finalName(p string) string {
	dir, base := filepath.Split(p)
	if i := strings.LastIndex(base, ".tmp"); strings.HasPrefix(base, ".") && i > 0 {
		return filepath.Join(dir, base[1:i])
	}
	return p
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestLayoutPaths(t *testing.T) {
	d := Layout{Dir: "/srv/pbp"}
	for got, want := range map[string]string{
		d.ConfigPath():             "/srv/pbp/config.yaml",
		d.SecretsDir():             "/srv/pbp/secrets",
		d.KeyPath():                "/srv/pbp/secrets/pve_ssh_key",
		d.PubKeyPath():             "/srv/pbp/secrets/pve_ssh_key.pub",
		d.TokenPath():              "/srv/pbp/secrets/pve_token",
		d.KnownHostsPath():         "/srv/pbp/known_hosts",
		d.LogsDir():                "/srv/pbp/logs",
		d.EnvPath():                "/srv/pbp/.env",
		d.InConfigKeyPath():        "/run/secrets/pve_ssh_key",
		d.InConfigTokenPath():      "/run/secrets/pve_token",
		d.InConfigKnownHostsPath(): "/etc/pbp/known_hosts",
	} {
		if got != want {
			t.Errorf("docker: got %q, want %q", got, want)
		}
	}
	b := Layout{Dir: "/ignored", Deb: true}
	for got, want := range map[string]string{
		b.ConfigPath():             "/etc/pbp/config.yaml",
		b.SecretsDir():             "/etc/pbp/secrets",
		b.KeyPath():                "/etc/pbp/secrets/pve_ssh_key",
		b.PubKeyPath():             "/etc/pbp/secrets/pve_ssh_key.pub",
		b.TokenPath():              "/etc/pbp/secrets/pve_token",
		b.KnownHostsPath():         "/etc/pbp/known_hosts",
		b.InConfigKeyPath():        "/etc/pbp/secrets/pve_ssh_key",
		b.InConfigTokenPath():      "/etc/pbp/secrets/pve_token",
		b.InConfigKnownHostsPath(): "/etc/pbp/known_hosts",
	} {
		if got != want {
			t.Errorf("deb: got %q, want %q", got, want)
		}
	}
	if b.LogsDir() != "" || b.EnvPath() != "" {
		t.Errorf("deb has no logs dir or .env: %q %q", b.LogsDir(), b.EnvPath())
	}
}

func TestWriteSecretAndPlainFiles(t *testing.T) {
	debDir, owners := fakeOwners(t)
	d := Layout{Dir: t.TempDir()}
	if err := d.WriteSecretFile(d.TokenPath(), []byte("s3cret\n")); err != nil {
		t.Fatal(err)
	}
	if err := d.WritePlainFile(d.KnownHostsPath(), []byte("h k\n")); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, d.SecretsDir()); m != 0o700 {
		t.Errorf("docker secrets dir mode %o", m)
	}
	if m := mode(t, d.TokenPath()); m != 0o400 {
		t.Errorf("docker token mode %o", m)
	}
	if m := mode(t, d.KnownHostsPath()); m != 0o444 {
		t.Errorf("docker known_hosts mode %o", m)
	}
	if !d.HaveToken() {
		t.Error("HaveToken false after writing a token")
	}
	b := Layout{Deb: true}
	if err := b.WriteSecretFile(b.TokenPath(), []byte("s3cret\n")); err != nil {
		t.Fatal(err)
	}
	if err := b.WritePlainFile(b.KnownHostsPath(), []byte("h k\n")); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, filepath.Join(debDir, "secrets")); m != 0o750 {
		t.Errorf("deb secrets dir mode %o", m)
	}
	if m := mode(t, filepath.Join(debDir, "secrets", "pve_token")); m != 0o640 {
		t.Errorf("deb token mode %o", m)
	}
	if m := mode(t, filepath.Join(debDir, "known_hosts")); m != 0o640 {
		t.Errorf("deb known_hosts mode %o", m)
	}
	o := owners()
	for _, p := range []string{d.SecretsDir(), d.TokenPath(), d.KnownHostsPath()} {
		if o[p] != (owned{65532, 65532}) {
			t.Errorf("docker owner of %s = %v", p, o[p])
		}
	}
	for _, p := range []string{"secrets", "secrets/pve_token", "known_hosts"} {
		if o[filepath.Join(debDir, p)] != (owned{0, 4242}) {
			t.Errorf("deb owner of %s = %v", p, o[filepath.Join(debDir, p)])
		}
	}
	if ents, _ := os.ReadDir(d.SecretsDir()); len(ents) != 1 {
		t.Errorf("temp files left behind: %v", ents)
	}
}

func TestHaveTokenEmpty(t *testing.T) {
	fakeOwners(t)
	d := Layout{Dir: t.TempDir()}
	if d.HaveToken() {
		t.Fatal("no token file")
	}
	if err := d.WriteSecretFile(d.TokenPath(), nil); err != nil {
		t.Fatal(err)
	}
	if d.HaveToken() {
		t.Fatal("empty token file is no token")
	}
}
