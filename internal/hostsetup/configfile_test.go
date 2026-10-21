package hostsetup

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
)

func testAnswers() Answers {
	return Answers{
		Address: "pve1.example.lan", VMIDRange: [2]int{990000, 990009}, Bridge: "vmbr77",
		RestoreStorage: "local-lvm", PBSStorages: []string{"pbs-a", "pbs-b"},
		Schedule: "30 1 * * *", TZ: "Europe/Berlin",
	}
}

const fp = "11:22:33:44:55:66:77:88:99:00:AA:BB:CC:DD:EE:FF:11:22:33:44:55:66:77:88:99:00:AA:BB:CC:DD:EE:FF"

// finished loads the written config with the two pending values filled in.
func finished(t *testing.T, path string) config.Config {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, r := range [][2]string{{`node: ""`, "node: pve1"}, {`tls_fingerprint: ""`, `tls_fingerprint: "` + fp + `"`}} {
		if strings.Count(s, r[0]) != 1 {
			t.Fatalf("want one %q in:\n%s", r[0], s)
		}
		s = strings.Replace(s, r[0], r[1], 1)
	}
	p := filepath.Join(t.TempDir(), "done.yaml")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestWriteConfigLoads(t *testing.T) {
	debDir, owners := fakeOwners(t)
	d := Layout{Dir: t.TempDir()}
	if err := WriteConfig(d, testAnswers()); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(d.ConfigPath())
	text := string(raw)
	for _, want := range []string{"# cron, in the container's TZ", "# Units ignored when judging", "config.example.yaml"} {
		if !strings.Contains(text, want) {
			t.Errorf("written config lacks %q:\n%s", want, text)
		}
	}
	// The sample checks.guests entries are gone ("match:"); the empty
	// selection.exclude.guests list stays.
	for _, bad := range []string{"profiles:", "match:", "myapp", "ca_file:"} {
		if strings.Contains(text, bad) && !strings.Contains(text, "# "+bad) {
			t.Errorf("written config has %q:\n%s", bad, text)
		}
	}
	if !strings.Contains(text, "\n\nsources:") {
		t.Errorf("top-level sections are not separated by blank lines:\n%s", text)
	}
	c := finished(t, d.ConfigPath())
	tg := c.Target
	if tg.API.URL != "https://pve1.example.lan:8006" || tg.API.TokenID != "pbp@pve!proof" ||
		tg.API.TokenSecretFile != "/run/secrets/pve_token" || tg.API.CAFile != "" ||
		tg.SSH.Host != "pve1.example.lan" || tg.SSH.User != "pbp" || tg.SSH.Port != 22 ||
		tg.SSH.KeyFile != "/run/secrets/pve_ssh_key" || tg.SSH.KnownHostsFile != "/etc/pbp/known_hosts" ||
		tg.Bridge != "vmbr77" || tg.VMIDRange != [2]int{990000, 990009} {
		t.Errorf("target = %+v", tg)
	}
	if !reflect.DeepEqual(c.Sources.Storages, []string{"pbs-a", "pbs-b"}) || c.Restore.Storage != "local-lvm" ||
		c.Schedule != "30 1 * * *" || len(c.Checks.Profiles) != 0 || len(c.Checks.Guests) != 0 {
		t.Errorf("config = %+v", c)
	}
	if env, err := os.ReadFile(d.EnvPath()); err != nil || string(env) != "TZ=Europe/Berlin\n" {
		t.Errorf(".env = %q, %v", env, err)
	}
	for p, m := range map[string]os.FileMode{d.ConfigPath(): 0o444, d.EnvPath(): 0o444, d.LogsDir(): 0o750} {
		if got := mode(t, p); got != m {
			t.Errorf("mode of %s = %o, want %o", p, got, m)
		}
	}
	o := owners()
	for _, p := range []string{d.ConfigPath(), d.EnvPath(), d.LogsDir()} {
		if o[p] != (owned{65532, 65532}) {
			t.Errorf("owner of %s = %v", p, o[p])
		}
	}

	b := Layout{Deb: true}
	a := testAnswers()
	a.TZ = ""
	if err := WriteConfig(b, a); err != nil {
		t.Fatal(err)
	}
	cp := filepath.Join(debDir, "config.yaml")
	c = finished(t, cp)
	if c.Target.API.TokenSecretFile != "/etc/pbp/secrets/pve_token" || c.Target.SSH.KeyFile != "/etc/pbp/secrets/pve_ssh_key" ||
		c.Target.SSH.KnownHostsFile != "/etc/pbp/known_hosts" {
		t.Errorf("deb paths: %+v", c.Target)
	}
	if m := mode(t, cp); m != 0o640 {
		t.Errorf("deb config mode %o", m)
	}
	if o := owners()[cp]; o != (owned{0, 4242}) {
		t.Errorf("deb config owner %v", o)
	}
	for _, p := range []string{".env", "logs"} {
		if _, err := os.Lstat(filepath.Join(debDir, p)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("deb layout must not have %s: %v", p, err)
		}
	}
}

func TestWriteConfigIPv6(t *testing.T) {
	fakeOwners(t)
	d := Layout{Dir: t.TempDir()}
	a := testAnswers()
	a.Address = "fd00::5"
	if err := WriteConfig(d, a); err != nil {
		t.Fatal(err)
	}
	c := finished(t, d.ConfigPath())
	if c.Target.API.URL != "https://[fd00::5]:8006" || c.Target.SSH.Host != "fd00::5" {
		t.Fatalf("ipv6: %+v", c.Target)
	}
}

func TestLoadStatePending(t *testing.T) {
	fakeOwners(t)
	d := Layout{Dir: t.TempDir()}
	if _, _, err := LoadState(d); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing config: err = %v, want ErrNotExist", err)
	}
	if err := WriteConfig(d, testAnswers()); err != nil {
		t.Fatal(err)
	}
	c, pending, err := LoadState(d)
	if err != nil || !pending {
		t.Fatalf("LoadState: pending=%v err=%v", pending, err)
	}
	if c.Target.Node != "" || c.Target.API.TLSFingerprint != "" || c.Target.Bridge != "vmbr77" || c.Restore.Storage != "local-lvm" {
		t.Fatalf("cfg = %+v", c.Target)
	}
}

func TestLoadStatePendingInvalid(t *testing.T) {
	fakeOwners(t)
	d := Layout{Dir: t.TempDir()}
	if err := WriteConfig(d, testAnswers()); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(d.ConfigPath())
	_ = os.Chmod(d.ConfigPath(), 0o600)
	if err := os.WriteFile(d.ConfigPath(), []byte(strings.Replace(string(raw), "bridge: vmbr77", "bridge: ''", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, pending, err := LoadState(d)
	if err == nil || !strings.Contains(err.Error(), "target.bridge: required") || strings.Contains(err.Error(), "pending") {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
}

func TestConfigIsDirectory(t *testing.T) {
	d := Layout{Dir: t.TempDir()}
	if err := os.Mkdir(d.ConfigPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, err := LoadState(d)
	want := "config.yaml is a directory (Docker creates one when the file is missing); remove it and run pbp setup again"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v", err)
	}
}

// A complete 0.2-style install done by hand: nothing pending, no questions,
// and no --create-token since the token file exists.
func TestLoadStateManualInstall(t *testing.T) {
	fakeOwners(t)
	d := Layout{Dir: t.TempDir()}
	cfg := `schedule: "0 2 * * *"
target:
  node: pve1
  api:
    url: https://pve1:8006
    token_id: pbp@pve!proof
    token_secret_file: /run/secrets/pve_token
    tls_fingerprint: "` + fp + `"
  ssh: {host: pve1, key_file: /run/secrets/pve_ssh_key, known_hosts_file: /etc/pbp/known_hosts}
  bridge: vmbr99
  vmid_range: [990000, 990049]
sources: {storages: [pbs-main]}
restore: {storage: local-zfs}
`
	if err := os.WriteFile(d.ConfigPath(), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteSecretFile(d.TokenPath(), []byte("secret\n")); err != nil {
		t.Fatal(err)
	}
	c, pending, err := LoadState(d)
	if err != nil || pending || c.Target.Node != "pve1" {
		t.Fatalf("pending=%v err=%v node=%q", pending, err, c.Target.Node)
	}
	pub, _, err := EnsureKey(d)
	if err != nil {
		t.Fatal(err)
	}
	lines := NodeLines(c, pub, "0.3.0", d.HaveToken())
	if strings.Contains(lines, "--create-token") || !strings.Contains(lines, "--storages local-zfs,pbs-main") {
		t.Fatalf("lines:\n%s", lines)
	}
}

func TestLoadStateInvalidComplete(t *testing.T) {
	d := Layout{Dir: t.TempDir()}
	if err := os.WriteFile(d.ConfigPath(), []byte("target: {node: pve1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadState(d); err == nil || !strings.Contains(err.Error(), "invalid config") {
		t.Fatalf("err = %v", err)
	}
}
