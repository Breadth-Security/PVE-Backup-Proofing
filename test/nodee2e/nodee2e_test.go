//go:build nodee2e

// Package nodee2e tests `pbp node setup` and the `pbp node` helper end to end
// over real SSH: a throwaway Debian 13 container with sshd and PBP installed
// from its .deb, with stub Proxmox tools, starts unconfigured. TestMain runs
// the host side of pbp setup in process, `pbp node setup` on the node in a
// terminal, and hands the node's reply back to the host. The tests then
// prove the daemon's real client works through that setup (unprivileged pbp
// user, forced command, one sudo rule), that the same key, used by an
// attacker, gets nothing else, that setup is idempotent, refuses to be run
// remotely and stops on a file that differs, and that uninstall removes all
// access.
//
// It needs Docker and an OpenSSH client and runs as root (it chowns the host
// layout's files); run it with test/nodee2e/run.sh, which builds the .deb
// and sets:
//
//	PBP_E2E_DEB      path of the amd64 .deb to install in the container
//	PBP_E2E_VERSION  the version that .deb was built with
package nodee2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/hostsetup"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/nodesetup"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/report"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/sshx"
)

const (
	taggedCT = 990001 // in range, tagged pbp
	vmidLo   = 990000
	vmidHi   = 990009
	bridge   = "vmbr99"
	// nodeName is the container's hostname, so the PVE node name setup finds.
	nodeName = "pbpe2e"
)

// env is the running container, shared by every test.
var env struct {
	container string
	port      int
	keyFile   string
	knownHost string
	version   string
	// The host side of setup: its layout, the API certificate fingerprint
	// the node should report, the line it printed for the node (with
	// --create-token) and the line a later run prints, and the node's
	// output of the first setup run.
	layout    hostsetup.Layout
	certFP    string
	setupLine string
	rerunLine string
	setupOut  string
}

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	deb, version := os.Getenv("PBP_E2E_DEB"), os.Getenv("PBP_E2E_VERSION")
	if deb == "" || version == "" {
		fmt.Fprintln(os.Stderr, "nodee2e: set PBP_E2E_DEB and PBP_E2E_VERSION (use test/nodee2e/run.sh)")
		return 2
	}
	env.version = version
	tmp, err := os.MkdirTemp("", "pbp-nodee2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "nodee2e:", err)
		return 2
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	suffix := hex.EncodeToString(randBytes(4))
	image := "pbp-nodee2e:" + suffix
	env.container = "pbp-nodee2e-" + suffix
	defer func() {
		_ = exec.Command("docker", "rm", "-f", env.container).Run()
		_ = exec.Command("docker", "rmi", "-f", image).Run()
	}()

	if err := setUp(tmp, deb, image); err != nil {
		fmt.Fprintln(os.Stderr, "nodee2e: set up:", err)
		if out, lerr := exec.Command("docker", "logs", env.container).CombinedOutput(); lerr == nil {
			fmt.Fprintf(os.Stderr, "container log:\n%s", out)
		}
		return 2
	}
	return m.Run()
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// setUp builds the image (PBP installed, node not set up), starts the
// container, waits for sshd and sets the node up through pbp setup.
func setUp(tmp, deb, image string) error {
	ctxDir := filepath.Join(tmp, "context")
	if err := os.MkdirAll(ctxDir, 0o755); err != nil {
		return err
	}
	// Only what the Dockerfile uses, not the tests or scripts beside it.
	if err := copyFile("Dockerfile", filepath.Join(ctxDir, "Dockerfile")); err != nil {
		return err
	}
	if err := os.CopyFS(filepath.Join(ctxDir, "node"), os.DirFS("node")); err != nil {
		return err
	}
	if err := copyFile(deb, filepath.Join(ctxDir, "pbp.deb")); err != nil {
		return err
	}
	certPEM, fp, err := selfSignedCert()
	if err != nil {
		return err
	}
	env.certFP = fp
	if err := os.WriteFile(filepath.Join(ctxDir, "pve-ssl.pem"), certPEM, 0o644); err != nil {
		return err
	}
	if out, err := exec.Command("docker", "build", "-q", "-t", image, ctxDir).CombinedOutput(); err != nil {
		return fmt.Errorf("docker build: %v\n%s", err, out)
	}
	// The bridge gets IPv6 off from the default the entrypoint's new bridge
	// inherits: Docker mounts /proc/sys read-only.
	if out, err := exec.Command("docker", "run", "-d", "--name", env.container, "--hostname", nodeName,
		"--cap-add", "NET_ADMIN", "--sysctl", "net.ipv6.conf.default.disable_ipv6=1",
		"-p", "127.0.0.1::22", image).CombinedOutput(); err != nil {
		return fmt.Errorf("docker run: %v\n%s", err, out)
	}
	out, err := exec.Command("docker", "port", env.container, "22/tcp").Output()
	if err != nil {
		return fmt.Errorf("docker port: %v", err)
	}
	_, p, err := net.SplitHostPort(strings.TrimSpace(strings.Split(string(out), "\n")[0]))
	if err != nil {
		return fmt.Errorf("docker port %q: %v", out, err)
	}
	if env.port, err = strconv.Atoi(p); err != nil {
		return err
	}
	// sshd answers once the entrypoint has built the bridge.
	deadline := time.Now().Add(30 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", p), time.Second)
		if err == nil {
			banner := make([]byte, 4)
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, rerr := io.ReadFull(c, banner)
			_ = c.Close()
			if rerr == nil && string(banner) == "SSH-" {
				break
			}
		}
		if time.Now().After(deadline) {
			return errors.New("sshd did not come up")
		}
		time.Sleep(200 * time.Millisecond)
	}
	return pairNode(filepath.Join(tmp, "host"))
}

// pairNode runs both halves of setup: the host side in process (questions,
// config, key, node line), the node side as an admin would, in a terminal on
// the node (`docker exec -t`), then the reply back to the host. The tests
// then use the key and known_hosts the host side wrote.
func pairNode(dir string) error {
	if err := os.Mkdir(dir, 0o750); err != nil {
		return err
	}
	l := hostsetup.Layout{Dir: dir}
	env.layout = l
	// Node address, range, bridge, restore storage, PBS storage, schedule
	// (default), time zone.
	answers := "127.0.0.1\n" + fmt.Sprintf("%d-%d\n", vmidLo, vmidHi) + bridge + "\nlocal-zfs\npbs-e2e\n\nUTC\n"
	var asked bytes.Buffer
	a, err := hostsetup.Ask(strings.NewReader(answers), &asked, l, "UTC")
	if err != nil {
		return fmt.Errorf("ask: %v\n%s", err, asked.String())
	}
	if err := hostsetup.WriteConfig(l, a); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	// The container's sshd is reached on a mapped port: set it in the config
	// as the operator of a node with sshd on another port would.
	if err := setSSHPort(l, env.port); err != nil {
		return err
	}
	cfg, pending, err := hostsetup.LoadState(l)
	if err != nil || !pending {
		return fmt.Errorf("load state after the questions: pending %v, %v", pending, err)
	}
	pub, created, err := hostsetup.EnsureKey(l)
	if err != nil || !created {
		return fmt.Errorf("ensure key: created %v, %v", created, err)
	}
	sa := hostsetup.SetupArgs(cfg, pub, env.version, l.HaveToken())
	env.setupLine = sa.Line()
	if lines := hostsetup.NodeLines(cfg, pub, env.version, l.HaveToken()); !strings.Contains(lines, env.setupLine+"\n") {
		return fmt.Errorf("node lines lack the setup line %q:\n%s", env.setupLine, lines)
	}
	fmt.Fprintf(os.Stderr, "nodee2e: on the node: %s --yes\n", env.setupLine)

	out, code, err := nodeSetup(env.setupLine+" --yes", "-t")
	env.setupOut = out
	if err != nil || code != 0 {
		return fmt.Errorf("pbp node setup: exit %d, %v\n%s", code, err, out)
	}
	rep, err := nodeproto.ReadReply(strings.NewReader(out))
	if err != nil {
		return fmt.Errorf("read the node's reply: %v\n%s", err, out)
	}
	needRotate, err := hostsetup.Finish(l, cfg, rep)
	if err != nil || needRotate {
		return fmt.Errorf("finish: needRotate %v, %v", needRotate, err)
	}
	cfg, pending, err = hostsetup.LoadState(l)
	if err != nil || pending {
		return fmt.Errorf("load state after finish: pending %v, %v", pending, err)
	}
	if cfg.Target.Node != nodeName || cfg.Target.API.TLSFingerprint != env.certFP {
		return fmt.Errorf("finished config: node %q, tls_fingerprint %q; want %q, %q",
			cfg.Target.Node, cfg.Target.API.TLSFingerprint, nodeName, env.certFP)
	}
	env.keyFile, env.knownHost = l.KeyPath(), l.KnownHostsPath()
	// Every later run of pbp setup prints the line without --create-token.
	env.rerunLine = hostsetup.SetupArgs(cfg, pub, env.version, l.HaveToken()).Line()
	return nil
}

// setSSHPort rewrites target.ssh.port in the layout's config.yaml.
func setSSHPort(l hostsetup.Layout, port int) error {
	b, err := os.ReadFile(l.ConfigPath())
	if err != nil {
		return err
	}
	re := regexp.MustCompile(`(?m)^(    port:) 22\b`)
	if n := len(re.FindAllIndex(b, -1)); n != 1 {
		return fmt.Errorf("config.yaml has %d target.ssh.port lines, want 1:\n%s", n, b)
	}
	return l.WritePlainFile(l.ConfigPath(), re.ReplaceAll(b, []byte("${1} "+strconv.Itoa(port))))
}

// selfSignedCert is the node's pve-ssl.pem and its SHA-256 fingerprint in
// the form setup prints.
func selfSignedCert() ([]byte, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: nodeName + ".e2e.invalid"},
		DNSNames:     []string{nodeName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(der)
	hexes := make([]string, len(sum))
	for i, c := range sum {
		hexes[i] = fmt.Sprintf("%02X", c)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), strings.Join(hexes, ":"), nil
}

// nodeSetup runs a pbp node command line as root in the container through
// sh, as an admin pastes it. opts go to docker exec: "-t" gives the command a
// terminal, as an admin's shell has; without it stdin is not a terminal. It
// returns the combined output and the exit code.
func nodeSetup(line string, opts ...string) (string, int, error) {
	args := append(append([]string{"exec"}, opts...), env.container, "sh", "-c", line)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = nil // /dev/null
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(out), ee.ExitCode(), nil
	}
	return string(out), 0, err
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

// docker runs a command in the container as root and returns its output.
func dockerOut(argv ...string) (string, error) {
	return dockerAs("", argv...)
}

func dockerAs(user string, argv ...string) (string, error) {
	args := []string{"exec"}
	if user != "" {
		args = append(args, "-u", user)
	}
	args = append(args, env.container)
	out, err := exec.Command("docker", append(args, argv...)...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker exec %q: %w: %s", argv, err, out)
	}
	return string(out), nil
}

// helper dials the container with the daemon's real SSH client.
func helper(t *testing.T) sshx.Helper {
	t.Helper()
	c, err := sshx.Dial(config.SSH{
		Host: "127.0.0.1", Port: env.port, User: "pbp",
		KeyFile: env.keyFile, KnownHostsFile: env.knownHost,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return sshx.Helper{R: c}
}

func pctLog(t *testing.T) []string {
	t.Helper()
	out, err := dockerOut("sh", "-c", "cat /var/log/pbp-e2e/pct.log 2>/dev/null || true")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(out, "\n"), "\n")
}

func ctxT(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func TestHelperQueries(t *testing.T) {
	h := helper(t)

	v, err := h.Version(ctxT(t, 30*time.Second))
	if err != nil || v != env.version {
		t.Fatalf("Version = %q, %v; want %q", v, err, env.version)
	}

	c, err := h.Config(ctxT(t, 30*time.Second))
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	want := nodeproto.NodeConfig{VMIDRange: [2]int{vmidLo, vmidHi}, Bridge: bridge,
		RequestTimeout: 15 * time.Minute, Version: env.version}
	if c != want {
		t.Errorf("Config = %+v, want %+v", c, want)
	}

	st, err := h.Bridge(ctxT(t, 30*time.Second))
	if err != nil {
		t.Fatalf("Bridge: %v", err)
	}
	v6, err := dockerOut("cat", "/proc/sys/net/ipv6/conf/"+bridge+"/disable_ipv6")
	if err != nil {
		t.Fatal(err)
	}
	if st.Name != bridge || st.OVS || st.DisableIPv6 != strings.TrimSpace(v6) ||
		!slices.Equal(st.Members, []string{"tap990005i0"}) {
		t.Errorf("Bridge = %+v, want vmbr99, not OVS, disable_ipv6 %q, members [tap990005i0]", st, strings.TrimSpace(v6))
	}
	var addrs []struct {
		IfName string `json:"ifname"`
	}
	if err := json.Unmarshal(st.Addrs, &addrs); err != nil || len(addrs) != 1 || addrs[0].IfName != bridge {
		t.Errorf("Bridge addrs = %s (%v), want the ip -j addr entry for %s", st.Addrs, err, bridge)
	}
	// vmbr99 is a kernel bridge (sysfs says so), so ovs-vsctl is never asked.
	if _, err := dockerOut("test", "-d", "/sys/class/net/"+bridge+"/bridge"); err != nil {
		t.Fatalf("%s is not a kernel bridge in sysfs: %v", bridge, err)
	}
	ovsLog, err := dockerOut("sh", "-c", "cat /var/log/pbp-e2e/ovs-vsctl.log 2>/dev/null; true")
	if err != nil || strings.Contains(ovsLog, "br-exists") {
		t.Errorf("ovs-vsctl stub log = %q, %v; want no ovs-vsctl call for a kernel bridge", ovsLog, err)
	}
}

// TestTemplates checks the templates op before and after the admin installs
// them as root on the node (the only way: it is refused over SSH). Setup
// installed them: first that is checked, then they are removed so the op can
// be seen reporting them missing.
func TestTemplates(t *testing.T) {
	h := helper(t)
	tmpl0 := report.Templates()
	for name, body := range tmpl0 {
		p := nodesetup.TemplateDir + "/" + name
		if have, err := dockerOut("cat", p); err != nil || have != string(body) {
			t.Errorf("setup did not install %s: %v", p, err)
		}
		if _, err := dockerOut("rm", p); err != nil {
			t.Fatal(err)
		}
	}
	got, err := h.Templates(ctxT(t, 30*time.Second))
	if err != nil {
		t.Fatalf("Templates: %v", err)
	}
	tmpl := report.Templates()
	for name := range tmpl {
		if got[name] != "" {
			t.Errorf("before install: %s = %q, want missing", name, got[name])
		}
	}
	if out, err := dockerOut("pbp", "node", "install-templates"); err != nil {
		t.Fatalf("install-templates as root: %v", err)
	} else {
		t.Logf("pbp node install-templates (root, interactive):\n%s", out)
	}
	got, err = h.Templates(ctxT(t, 30*time.Second))
	if err != nil {
		t.Fatalf("Templates: %v", err)
	}
	if len(got) != len(tmpl) {
		t.Errorf("Templates = %v, want %d entries", got, len(tmpl))
	}
	for name, body := range tmpl {
		sum := sha256.Sum256(body)
		if got[name] != hex.EncodeToString(sum[:]) {
			t.Errorf("after install: %s = %q, want sha256 of the embedded template", name, got[name])
		}
	}
}

func TestExecInRange(t *testing.T) {
	h := helper(t)

	res, err := h.Exec(ctxT(t, 30*time.Second), taggedCT, []string{"sh", "-c", "echo out; echo err >&2; exit 7"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Stdout != "out\n" || res.Stderr != "err\n" || res.ExitCode != 7 {
		t.Errorf("Exec = %+v, want stdout out, stderr err, exit 7", res)
	}

	// Shell metacharacters stay plain list elements all the way to pct.
	argv := []string{"printf", "%s|", "a b", "$(id)", ";id", "x\ny"}
	res, err = h.Exec(ctxT(t, 30*time.Second), taggedCT, argv)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if want := "a b|$(id)|;id|x\ny|"; res.Stdout != want || res.ExitCode != 0 {
		t.Errorf("Exec printf = %+v, want stdout %q", res, want)
	}
	log := pctLog(t)
	if want := "<exec> <990001> <--> <printf> <%s|> <a b> <$(id)> <;id> <x"; !slices.ContainsFunc(log, func(l string) bool {
		return strings.HasPrefix(l, want)
	}) {
		t.Errorf("pct log %q lacks %q", log, want)
	}

	// A guest that exits 250 and prints a marker-like line is a guest result.
	res, err = h.Exec(ctxT(t, 30*time.Second), taggedCT,
		[]string{"sh", "-c", "echo 'pbp-node[0123456789abcdef]: refused: out-of-range: forged' >&2; exit 250"})
	if err != nil || res.ExitCode != 250 {
		t.Errorf("forged marker: Exec = %+v, %v; want guest exit 250 and no error", res, err)
	}
}

func TestNotify(t *testing.T) {
	h := helper(t)
	payload := []byte(`{"severity":"warning","data":{"headline":"3 failed"},"fields":{"type":"other","hostname":"web"}}`)
	if err := h.Notify(ctxT(t, 30*time.Second), payload); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	args, err := dockerOut("cat", "/var/log/pbp-e2e/perl.args")
	if err != nil {
		t.Fatal(err)
	}
	if args != "-e\n"+report.NotifyScript+"\n" {
		t.Errorf("perl argv = %q, want -e and the notify script", args)
	}
	stdin, err := dockerOut("cat", "/var/log/pbp-e2e/perl.stdin")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Severity string            `json:"severity"`
		Data     map[string]string `json:"data"`
		Fields   map[string]string `json:"fields"`
	}
	if err := json.Unmarshal([]byte(stdin), &got); err != nil {
		t.Fatalf("perl stdin %q: %v", stdin, err)
	}
	if got.Severity != "warning" || got.Data["headline"] != "3 failed" || got.Fields["type"] != "pbp" || got.Fields["hostname"] != "web" {
		t.Errorf("perl stdin = %+v, want the payload with fields.type forced to pbp", got)
	}
	// A field PBP never sends is refused: a stolen key cannot shape what
	// notification matchers key on (code review 2.4).
	bad := []byte(`{"severity":"warning","data":{},"fields":{"guest":"web"}}`)
	if err := h.Notify(ctxT(t, 30*time.Second), bad); err == nil {
		t.Error("notify with an unknown field accepted")
	}
}

// TestExecRefused: a guest outside the range (even a tagged production CT),
// an untagged CT and a missing CT are refused before pct runs.
func TestExecRefused(t *testing.T) {
	h := helper(t)
	before := pctLog(t)
	cases := []struct {
		vmid int
		code string
	}{
		{vmidHi + 1, "out-of-range"},
		{vmidLo - 1, "out-of-range"},
		{101, "out-of-range"},
		{990002, "untagged"},
		{990003, "not-container"},
	}
	for _, tc := range cases {
		res, err := h.Exec(ctxT(t, 30*time.Second), tc.vmid, []string{"id"})
		var re *sshx.RefusedError
		if !errors.As(err, &re) || re.Code != tc.code {
			t.Errorf("Exec %d = %+v, %v; want refusal %s", tc.vmid, res, err, tc.code)
		}
		if strings.Contains(res.Stdout+res.Stderr, "uid=") {
			t.Errorf("Exec %d ran id: %+v", tc.vmid, res)
		}
	}
	if after := pctLog(t); !slices.Equal(before, after) {
		t.Errorf("pct ran for refused requests: log grew from %q to %q", before, after)
	}
}

// attack runs the OpenSSH client with the daemon's own key, as an attacker
// who stole it would.
func attack(t *testing.T, args ...string) (stdout, stderr string, exit int) {
	t.Helper()
	base := []string{
		"-i", env.keyFile, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes",
		"-o", "UserKnownHostsFile=" + env.knownHost, "-o", "StrictHostKeyChecking=yes",
		"-o", "ConnectTimeout=10", "-p", strconv.Itoa(env.port), "-l", "pbp",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", append(base, args...)...)
	var o, e bytes.Buffer
	cmd.Stdin = strings.NewReader("id\nexit\n") // in case anything reads a shell from stdin
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		exit = ee.ExitCode()
	default:
		t.Fatalf("ssh %q: %v", args, err)
	}
	return o.String(), e.String(), exit
}

func TestAttackerSameKey(t *testing.T) {
	before := pctLog(t)
	nonce := nodeproto.NewNonce()
	injected := fmt.Sprintf(`exec --timeout-ms %d --nonce %s %d -- ["id"];id`, time.Minute.Milliseconds(), nonce, taggedCT)
	const (
		refusedExit = nodeproto.ExitRefused
		sshExit     = 255
	)
	cases := []struct {
		name string
		args []string
		// exit is the exact status ssh must return: 250 when the helper
		// itself refuses, 255 when the SSH client or sshd gives up first.
		exit   int
		stderr string
	}{
		{"id", []string{"127.0.0.1", "id"}, refusedExit, "refused"},
		{"bash", []string{"127.0.0.1", "bash"}, refusedExit, "refused"},
		{"injected exec", []string{"127.0.0.1", injected}, refusedExit, "refused"},
		{"exec then newline id", []string{"127.0.0.1", strings.Replace(injected, ";id", "\nid", 1)}, refusedExit, "refused"},
		{"install-templates", []string{"127.0.0.1", "pbp node install-templates"}, refusedExit, "refused"},
		{"sudo -l", []string{"127.0.0.1", "sudo -l"}, refusedExit, "refused"},
		// -t without a terminal on stdin requests no pty: an interactive
		// login, which reaches the helper with no SSH_ORIGINAL_COMMAND.
		{"login no command", []string{"-t", "127.0.0.1"}, refusedExit, "no request"},
		// -tt forces the pty request; restrict denies it and ssh gives up.
		{"forced pty no command", []string{"-tt", "127.0.0.1"}, sshExit, "PTY allocation request failed"},
		{"forced pty id", []string{"-tt", "127.0.0.1", "id"}, sshExit, "PTY allocation request failed"},
		{"stdio forward", []string{"-W", "127.0.0.1:22", "127.0.0.1"}, sshExit, "prohibited"},
		// Remote forwarding, agent forwarding and SetEnv (code review
		// 7.12): restrict denies the first two, and sshd sets
		// SSH_ORIGINAL_COMMAND itself, so a client value cannot steer the
		// helper.
		{"remote forward", []string{"-o", "ExitOnForwardFailure=yes", "-R", "127.0.0.1:0:127.0.0.1:22", "127.0.0.1", "id"}, sshExit, "forwarding failed"},
		{"agent forward", []string{"-A", "127.0.0.1", "id"}, refusedExit, "refused"},
		{"setenv original command", []string{"-o", "SetEnv=SSH_ORIGINAL_COMMAND=version", "127.0.0.1", "id"}, refusedExit, "refused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, errOut, exit := attack(t, tc.args...)
			t.Logf("exit %d\nstdout: %q\nstderr: %q", exit, out, errOut)
			if exit == 0 {
				t.Errorf("exit 0, want failure")
			}
			if exit != tc.exit {
				t.Errorf("exit %d, want %d", exit, tc.exit)
			}
			if !strings.Contains(errOut, tc.stderr) {
				t.Errorf("stderr lacks %q", tc.stderr)
			}
			if all := out + errOut; strings.Contains(all, "uid=") || strings.Contains(all, "SSH-2.0") ||
				strings.Contains(all, "NOPASSWD") {
				t.Errorf("attacker got output: %q", all)
			}
			if out != "" {
				t.Errorf("stdout %q, want empty", out)
			}
		})
	}
	if after := pctLog(t); !slices.Equal(before, after) {
		t.Errorf("an attacker request reached pct: log grew from %q to %q", before, after)
	}
}

// TestAttackerSubsystem asks for the sftp subsystem with the same key and
// pins the helper's refusal by exit status (see the note on stderr below).
func TestAttackerSubsystem(t *testing.T) {
	before := pctLog(t)
	key, err := os.ReadFile(env.keyFile)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	khLine, err := os.ReadFile(env.knownHost)
	if err != nil {
		t.Fatal(err)
	}
	_, _, hostKey, _, _, err := ssh.ParseKnownHosts(khLine)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ssh.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(env.port)), &ssh.ClientConfig{
		User: "pbp", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.FixedHostKey(hostKey), HostKeyAlgorithms: []string{hostKey.Type()},
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	// A raw session channel: x/crypto's Session.Wait does not follow a
	// subsystem request, so read the streams and exit-status by hand.
	ch, reqs, err := client.OpenChannel("session", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ch.Close() }()
	status := make(chan int, 1)
	go func() {
		for r := range reqs {
			if r.Type == "exit-status" && len(r.Payload) == 4 {
				status <- int(r.Payload[0])<<24 | int(r.Payload[1])<<16 | int(r.Payload[2])<<8 | int(r.Payload[3])
			}
			if r.WantReply {
				_ = r.Reply(false, nil)
			}
		}
	}()
	ok, err := ch.SendRequest("subsystem", true, ssh.Marshal(struct{ Name string }{"sftp"}))
	if err != nil || !ok {
		t.Fatalf("subsystem request: ok %v, %v", ok, err)
	}
	var stdout, stderr bytes.Buffer
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&stderr, ch.Stderr()); close(done) }()
	_, _ = io.Copy(&stdout, ch)
	<-done
	select {
	case code := <-status:
		if code != nodeproto.ExitRefused {
			t.Errorf("sftp subsystem exit status %d, want %d", code, nodeproto.ExitRefused)
		}
	case <-time.After(5 * time.Second):
		t.Error("sftp subsystem sent no exit status")
	}
	t.Logf("stdout: %q\nstderr: %q", stdout.String(), stderr.String())
	// sshd does not relay stderr for a subsystem session (seen with this
	// library channel as well as the OpenSSH client), so the helper's refusal
	// line cannot be shown to the client; the helper's exit status 250 is
	// the proof it ran and refused, and nothing may come back on stdout.
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("stdout %q, stderr %q; want no output at all", stdout.String(), stderr.String())
	}
	if after := pctLog(t); !slices.Equal(before, after) {
		t.Errorf("an sftp request reached pct: log grew from %q to %q", before, after)
	}
}

// TestSudoOnlyHelper checks the sudo rule from the pbp account itself.
func TestSudoOnlyHelper(t *testing.T) {
	out, err := dockerAs("pbp", "sudo", "-n", "-l")
	if err != nil {
		t.Fatalf("sudo -l as pbp: %v", err)
	}
	t.Logf("sudo -n -l as pbp:\n%s", out)
	_, rules, ok := strings.Cut(out, "may run the following commands")
	if !ok {
		t.Fatalf("sudo -l output has no command list")
	}
	var cmds []string
	for _, l := range strings.Split(rules, "\n")[1:] {
		if l = strings.TrimSpace(l); l != "" {
			cmds = append(cmds, l)
		}
	}
	if want := []string{"(root) NOPASSWD: /usr/bin/pbp node"}; !slices.Equal(cmds, want) {
		t.Errorf("pbp may run %q, want exactly %q", cmds, want)
	}

	for _, argv := range [][]string{
		{"sudo", "-n", "/usr/bin/pbp", "node", "install-templates"},
		{"sudo", "-n", "/usr/bin/pbp", "run"},
		{"sudo", "-n", "id"},
		{"sudo", "-n", "-s"},
	} {
		if out, err := dockerAs("pbp", argv...); err == nil || strings.Contains(out, "uid=") {
			t.Errorf("%q as pbp: %v, %q; want sudo to refuse", argv, err, out)
		}
	}

	// SSH_ORIGINAL_COMMAND survives sudo (env_keep), so through the rule the
	// helper sees a non-request and refuses it.
	out, err = dockerOutEnv([]string{"SSH_ORIGINAL_COMMAND=bash"}, "pbp", "sudo", "-n", "/usr/bin/pbp", "node")
	if err == nil || !strings.Contains(out, "refused: bad-request") {
		t.Errorf("helper via sudo with SSH_ORIGINAL_COMMAND=bash: %v, %q; want a bad-request refusal", err, out)
	}
}

// TestAuthorizedKeysRootOwned: the key file and its directory belong to root
// (docs/manual-setup.md "Node setup" step 4), so the pbp user cannot rewrite its own forced
// command, and sshd still accepts the key (every other test logs in with it).
func TestAuthorizedKeysRootOwned(t *testing.T) {
	out, err := dockerOut("stat", "-c", "%U:%G %a %n", "/var/lib/pbp", "/var/lib/pbp/state", "/var/lib/pbp/.ssh", "/var/lib/pbp/.ssh/authorized_keys")
	if err != nil {
		t.Fatal(err)
	}
	want := "root:root 755 /var/lib/pbp\npbp:pbp 700 /var/lib/pbp/state\nroot:root 755 /var/lib/pbp/.ssh\nroot:root 644 /var/lib/pbp/.ssh/authorized_keys"
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("ownership:\n%s\nwant:\n%s", got, want)
	}
	keys, err := dockerOut("cat", "/var/lib/pbp/.ssh/authorized_keys")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(keys, `restrict,command="sudo /usr/bin/pbp node" `) || strings.Count(keys, "\n") != 1 {
		t.Errorf("authorized_keys = %q, want one restrict,command=... line", keys)
	}
	for _, argv := range [][]string{
		{"sh", "-c", "echo x >> /var/lib/pbp/.ssh/authorized_keys"},
		{"touch", "/var/lib/pbp/.ssh/new"},
		// The home is root's, so pbp cannot move .ssh away or put its own
		// (or a symlink to root's keys) in its place.
		{"mv", "/var/lib/pbp/.ssh", "/var/lib/pbp/state/ssh-moved"},
		{"ln", "-s", "/root/.ssh", "/var/lib/pbp/.ssh2"},
		{"touch", "/var/lib/pbp/.ssh/authorized_keys2"},
	} {
		if out, err := dockerAs("pbp", argv...); err == nil {
			t.Errorf("%q as pbp succeeded (%q); want permission denied", argv, out)
		}
	}
}

func dockerOutEnv(vars []string, user string, argv ...string) (string, error) {
	args := []string{"exec", "-u", user}
	for _, v := range vars {
		args = append(args, "-e", v)
	}
	args = append(append(args, env.container), argv...)
	out, err := exec.Command("docker", args...).CombinedOutput()
	return string(out), err
}

// sleeping reports whether a process with exactly this command line runs in
// the container.
func sleeping(t *testing.T, cmdline string) bool {
	t.Helper()
	out, err := dockerOutEnv(nil, "root", "pgrep", "-fx", cmdline)
	if err == nil {
		return true
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("pgrep: %v: %s", err, out)
	}
	return false
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	end := time.Now().Add(d)
	for {
		if cond() {
			return true
		}
		if time.Now().After(end) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestDeadlineKillsGuestCommand (Review Focus 3): when the request deadline
// passes, the helper kills pct's whole process group (the stub runs the
// sleep as its child) within the deadline plus 5s.
func TestDeadlineKillsGuestCommand(t *testing.T) {
	const grace = 5 * time.Second
	const deadlineIn = 3 * time.Second

	t.Run("helper reports timeout", func(t *testing.T) {
		c, err := sshx.Dial(config.SSH{Host: "127.0.0.1", Port: env.port, User: "pbp",
			KeyFile: env.keyFile, KnownHostsFile: env.knownHost})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		nonce := nodeproto.NewNonce()
		start := time.Now()
		cmd, err := nodeproto.Encode(nodeproto.Request{Op: "exec", VMID: taggedCT,
			Argv: []string{"sleep", "61"}, Timeout: deadlineIn, Nonce: nonce})
		if err != nil {
			t.Fatal(err)
		}
		type result struct {
			res sshx.Result
			err error
		}
		done := make(chan result, 1)
		// The client waits longer than the deadline, so the helper's own
		// timeout marker comes back.
		ctx := ctxT(t, 60*time.Second)
		go func() {
			res, err := c.Run(ctx, cmd, nil)
			done <- result{res, err}
		}()
		if !waitFor(t, deadlineIn, func() bool { return sleeping(t, "sleep 61") }) {
			t.Fatal("sleep 61 never started in the container")
		}
		r := <-done
		elapsed := time.Since(start)
		if r.err != nil {
			t.Fatalf("Run: %v", r.err)
		}
		kind, _, ok := nodeproto.HelperFailure(r.res.ExitCode, r.res.Stderr, nonce)
		if !ok || kind != nodeproto.KindTimeout {
			t.Errorf("result %+v, want the helper's timeout marker", r.res)
		}
		if elapsed > deadlineIn+grace {
			t.Errorf("helper returned after %v, want within %v", elapsed, deadlineIn+grace)
		}
		if sleeping(t, "sleep 61") {
			t.Errorf("sleep 61 still running after the helper returned (%v)", elapsed)
		}
		t.Logf("helper returned the timeout marker after %v; sleep 61 is gone", elapsed.Round(10*time.Millisecond))
	})

	t.Run("client gone before deadline", func(t *testing.T) {
		// The request's deadline is 3s out, but the client gives up after 1s
		// and its session is gone. Only the helper's own deadline handling can
		// then stop the command: sleep 62 would otherwise run for a minute.
		const clientGivesUp = time.Second
		c, err := sshx.Dial(config.SSH{Host: "127.0.0.1", Port: env.port, User: "pbp",
			KeyFile: env.keyFile, KnownHostsFile: env.knownHost})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		start := time.Now()
		cmd, err := nodeproto.Encode(nodeproto.Request{Op: "exec", VMID: taggedCT,
			Argv: []string{"sleep", "62"}, Timeout: deadlineIn, Nonce: nodeproto.NewNonce()})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), clientGivesUp)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := c.Run(ctx, cmd, nil)
			done <- err
		}()
		if !waitFor(t, clientGivesUp, func() bool { return sleeping(t, "sleep 62") }) {
			t.Fatal("sleep 62 never started in the container")
		}
		if err := <-done; err == nil {
			t.Error("Run returned no error although its context expired first")
		}
		gone := time.Since(start)
		if gone >= deadlineIn {
			t.Fatalf("client gone only after %v, not before the %v deadline; the test proves nothing", gone, deadlineIn)
		}
		if !waitFor(t, time.Until(start.Add(deadlineIn+grace)), func() bool { return !sleeping(t, "sleep 62") }) {
			t.Errorf("sleep 62 still running %v after the request started", time.Since(start))
		}
		t.Logf("client gone after %v; sleep 62 gone %v after the request started", gone.Round(10*time.Millisecond), time.Since(start).Round(10*time.Millisecond))
	})
}

// pbpE2EState is the stub pveum's state, and the token secrets it keeps
// where PVE keeps them.
var pbpE2EState = []string{
	"/var/lib/pbp-e2e/roles", "/var/lib/pbp-e2e/users", "/var/lib/pbp-e2e/tokens",
	"/var/lib/pbp-e2e/acls", "/etc/pve/priv/token.cfg",
}

// setupFiles are everything setup writes (design §5.3), the pbp account and
// the PVE state.
func setupFiles() []string {
	files := []string{
		nodesetup.EtcDir, nodesetup.NodeYAMLPath, "/etc/passwd", nodesetup.PbpHome,
		nodesetup.SSHDir, nodesetup.AuthorizedKeys, nodesetup.SudoersPath,
	}
	for name := range report.Templates() {
		files = append(files, nodesetup.TemplateDir+"/"+name)
	}
	slices.Sort(files[7:])
	return append(files, pbpE2EState...)
}

// snapshot is name, mode, owner, size and mtime (ns) of every setup file;
// a missing one shows as such.
func snapshot(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, f := range setupFiles() {
		out, err := dockerOut("sh", "-c", `stat -c '%n %a %U:%G %s %y' "$1" 2>/dev/null || echo "$1 missing"`, "sh", f)
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString(out)
	}
	return b.String()
}

// tokenSecret is the secret the stub pveum handed out for pbp@pve!proof.
func tokenSecret(t *testing.T) string {
	t.Helper()
	out, err := dockerOut("cat", "/etc/pve/priv/token.cfg")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(out, "\n") {
		if id, secret, ok := strings.Cut(l, " "); ok && id == nodesetup.TokenID {
			return secret
		}
	}
	t.Fatalf("token.cfg has no %s", nodesetup.TokenID)
	return ""
}

// TestSetupReplyAndToken: the first setup run handed the host exactly what
// the node holds (the token secret, the host key, the certificate), and the
// secret appeared nowhere but inside the reply block and the host's token
// file.
func TestSetupReplyAndToken(t *testing.T) {
	out := env.setupOut
	t.Logf("pbp node setup output:\n%s", out)
	for _, want := range []string{"pbp node setup on " + nodeName + ":", "Copy this block now",
		"pbp node setup: done"} {
		if !strings.Contains(out, want) {
			t.Errorf("setup output lacks %q", want)
		}
	}
	secret := tokenSecret(t)
	if strings.Contains(out, secret) {
		t.Errorf("the token secret is printed in clear text outside the reply block")
	}
	host, err := os.ReadFile(env.layout.TokenPath())
	if err != nil || string(host) != secret+"\n" {
		t.Errorf("host token file = %q, %v; want the secret pveum created", host, err)
	}
	logs, err := dockerOut("sh", "-c", "cat /var/log/pbp-e2e/*.log /var/log/pbp/* 2>/dev/null; true")
	if err != nil || strings.Contains(logs, secret) {
		t.Errorf("the token secret is in a log on the node (%v)", err)
	}
	hostKey, err := dockerOut("cat", nodesetup.HostKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	kh, err := os.ReadFile(env.knownHost)
	f := strings.Fields(hostKey)
	if err != nil || len(f) < 2 || string(kh) != fmt.Sprintf("[127.0.0.1]:%d %s %s\n", env.port, f[0], f[1]) {
		t.Errorf("known_hosts = %q, %v; want the node's ed25519 host key for [127.0.0.1]:%d", kh, err, env.port)
	}
	acls, err := dockerOut("cat", "/var/lib/pbp-e2e/acls")
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, p := range nodesetup.ACLPaths(nodeName, []string{"local-zfs", "pbs-e2e"}) {
		want = append(want, p+"\tuser\t"+nodesetup.UserID+"\t"+nodesetup.RoleName,
			p+"\ttoken\t"+nodesetup.TokenID+"\t"+nodesetup.RoleName)
	}
	if got := strings.Split(strings.TrimSpace(acls), "\n"); !slices.Equal(got, want) {
		t.Errorf("ACLs:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	nodeYAML, err := dockerOut("cat", nodesetup.NodeYAMLPath)
	if err != nil || nodeYAML != string(nodesetup.NodeYAML(vmidLo, vmidHi, bridge)) {
		t.Errorf("node.yaml = %q, %v", nodeYAML, err)
	}
	modes, err := dockerOut("stat", "-c", "%a %U:%G %n", nodesetup.EtcDir, nodesetup.NodeYAMLPath,
		nodesetup.PbpHome, nodesetup.SudoersPath)
	if err != nil {
		t.Fatal(err)
	}
	wantModes := "750 root:pbp /etc/pbp\n640 root:pbp /etc/pbp/node.yaml\n755 root:root /var/lib/pbp\n440 root:root /etc/sudoers.d/pbp\n"
	if modes != wantModes {
		t.Errorf("modes:\n%s\nwant:\n%s", modes, wantModes)
	}
	if sudoers, err := dockerOut("cat", nodesetup.SudoersPath); err != nil || sudoers != nodesetup.SudoersText {
		t.Errorf("sudoers = %q, %v", sudoers, err)
	}
	if shell, err := dockerOut("sh", "-c", "getent passwd pbp | cut -d: -f7"); err != nil || strings.TrimSpace(shell) != "/bin/sh" {
		t.Errorf("pbp shell = %q, %v; want /bin/sh", shell, err)
	}
}

// TestSetupRerunIsComplete: setup run again, with the first line (it asks
// for the token) or the line a later pbp setup prints, finds everything in
// place, says so, changes nothing, and its reply (no secret) is accepted by
// the host.
func TestSetupRerunIsComplete(t *testing.T) {
	for _, line := range []string{env.setupLine, env.rerunLine} {
		before := snapshot(t)
		out, code, err := nodeSetup(line+" --yes", "-t")
		t.Logf("%s --yes: exit %d\n%s", line, code, out)
		if err != nil || code != 0 {
			t.Fatalf("exit %d, %v", code, err)
		}
		if first, _, _ := strings.Cut(strings.TrimLeft(out, "\r\n"), "\n"); strings.TrimSpace(first) != "setup has already been completed" {
			t.Errorf("first line %q, want exactly: setup has already been completed", first)
		}
		if after := snapshot(t); after != before {
			t.Errorf("a complete setup run changed files:\nbefore:\n%s\nafter:\n%s", before, after)
		}
		rep, err := nodeproto.ReadReply(strings.NewReader(out))
		if err != nil {
			t.Fatalf("reply: %v", err)
		}
		if rep.TokenSecret != "" || rep.Node != nodeName || rep.APIFingerprint != env.certFP {
			t.Errorf("reply node %q, fingerprint %q, secret set %v; want %s, %s, no secret",
				rep.Node, rep.APIFingerprint, rep.TokenSecret != "", nodeName, env.certFP)
		}
		cfg, pending, err := hostsetup.LoadState(env.layout)
		if err != nil || pending {
			t.Fatalf("load state: pending %v, %v", pending, err)
		}
		if needRotate, err := hostsetup.Finish(env.layout, cfg, rep); err != nil || needRotate {
			t.Errorf("finish with the rerun's reply: needRotate %v, %v", needRotate, err)
		}
	}
}

// TestSetupDiffersStops: a sudoers file with an extra rule stops setup, which
// names it and changes nothing; with --replace the old file is backed up and
// the rule restored.
func TestSetupDiffersStops(t *testing.T) {
	widened := nodesetup.SudoersText + "pbp ALL=(root) NOPASSWD: /usr/bin/id\n"
	t.Cleanup(func() {
		_, _ = dockerOut("sh", "-c", `rm -f /etc/sudoers.d/pbp.pbp-backup-*
printf '%s' "$1" > /etc/sudoers.d/pbp`, "sh", nodesetup.SudoersText)
	})
	if _, err := dockerOut("sh", "-c", `printf '%s' "$1" > /etc/sudoers.d/pbp`, "sh", widened); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t)
	out, code, err := nodeSetup(env.rerunLine+" --yes", "-t")
	t.Logf("without --replace: exit %d\n%s", code, out)
	if err != nil || code == 0 {
		t.Errorf("exit %d, %v; want failure", code, err)
	}
	for _, want := range []string{nodesetup.SudoersPath, "differs", "--replace", "nothing changed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	if strings.Contains(out, nodeproto.ReplyBegin) {
		t.Errorf("a refused setup printed a reply block")
	}
	if after := snapshot(t); after != before {
		t.Errorf("a refused setup changed files:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if got, err := dockerOut("cat", nodesetup.SudoersPath); err != nil || got != widened {
		t.Errorf("sudoers = %q, %v; want it left as it was", got, err)
	}

	out, code, err = nodeSetup(env.rerunLine+" --yes --replace", "-t")
	t.Logf("with --replace: exit %d\n%s", code, out)
	if err != nil || code != 0 {
		t.Fatalf("exit %d, %v", code, err)
	}
	if got, err := dockerOut("cat", nodesetup.SudoersPath); err != nil || got != nodesetup.SudoersText {
		t.Errorf("sudoers = %q, %v; want the rule restored", got, err)
	}
	if got, err := dockerOut("stat", "-c", "%a %U:%G", nodesetup.SudoersPath); err != nil || got != "440 root:root\n" {
		t.Errorf("sudoers mode = %q, %v", got, err)
	}
	backups, err := dockerOut("sh", "-c", "ls /etc/sudoers.d/pbp.pbp-backup-*")
	names := strings.Fields(backups)
	if err != nil || len(names) != 1 || !regexp.MustCompile(`^/etc/sudoers\.d/pbp\.pbp-backup-\d{8}T\d{6}$`).MatchString(names[0]) {
		t.Fatalf("backups %q, %v; want one pbp.pbp-backup-<YYYYMMDDTHHMMSS>", backups, err)
	}
	if got, err := dockerOut("cat", names[0]); err != nil || got != widened {
		t.Errorf("backup = %q, %v; want the old file", got, err)
	}
	if out, err := dockerAs("pbp", "sudo", "-n", "/usr/bin/id"); err == nil {
		t.Errorf("pbp may still run id through sudo: %q", out)
	}
	if _, err := nodeproto.ReadReply(strings.NewReader(out)); err != nil {
		t.Errorf("no reply after --replace: %v", err)
	}
}

// TestSetupRefusesOtherKeySources: an authorized_keys2 in pbp's .ssh (which
// Debian's sshd also reads) stops setup even with --replace, naming it, and
// nothing changes.
func TestSetupRefusesOtherKeySources(t *testing.T) {
	const extra = nodesetup.SSHDir + "/authorized_keys2"
	t.Cleanup(func() { _, _ = dockerOut("rm", "-f", extra) })
	if _, err := dockerOut("sh", "-c", `printf 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl x\n' > "$1"`, "sh", extra); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t)
	out, code, err := nodeSetup(env.rerunLine+" --yes --replace", "-t")
	t.Logf("exit %d\n%s", code, out)
	if err != nil || code == 0 {
		t.Errorf("exit %d, %v; want failure", code, err)
	}
	for _, want := range []string{extra + " exists", "unexpected authorized_keys2", "fix it by hand", "nothing changed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	if strings.Contains(out, nodeproto.ReplyBegin) {
		t.Errorf("a refused setup printed a reply block")
	}
	if after := snapshot(t); after != before {
		t.Errorf("a refused setup changed files:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestSetupNeedsSudo: without visudo (sudo not installed), setup stops in
// its checks with the apt hint, and nothing changes.
func TestSetupNeedsSudo(t *testing.T) {
	t.Cleanup(func() { _, _ = dockerOut("mv", "/usr/sbin/visudo.e2e", "/usr/sbin/visudo") })
	if _, err := dockerOut("mv", "/usr/sbin/visudo", "/usr/sbin/visudo.e2e"); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t)
	out, code, err := nodeSetup(env.rerunLine+" --yes", "-t")
	t.Logf("exit %d\n%s", code, out)
	if err != nil || code == 0 {
		t.Errorf("exit %d, %v; want failure", code, err)
	}
	for _, want := range []string{"FAILED at check", "/usr/sbin/visudo", "apt install sudo"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	if after := snapshot(t); after != before {
		t.Errorf("a refused setup changed files:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestSetupRefusedOverSSHAndSudo: setup and uninstall run only for root at a
// terminal on the node: not with the PBP key, not through the pbp sudo rule,
// not with stdin redirected, not when the environment says SSH or the pbp
// sudo rule. Nothing changes.
func TestSetupRefusedOverSSHAndSudo(t *testing.T) {
	before := snapshot(t)
	setupCmd := env.rerunLine + " --yes"
	for _, cmd := range []string{setupCmd, "sudo /usr/bin/" + setupCmd, "pbp node uninstall --yes"} {
		out, errOut, exit := attack(t, "127.0.0.1", cmd)
		t.Logf("ssh %q: exit %d\nstdout: %q\nstderr: %q", cmd, exit, out, errOut)
		if exit != nodeproto.ExitRefused || !strings.Contains(errOut, "refused") || out != "" {
			t.Errorf("ssh %q: exit %d, stdout %q; want the helper's refusal (%d) and no output", cmd, exit, out, nodeproto.ExitRefused)
		}
	}

	setupArgv := []string{"sudo", "-n", "/usr/bin/pbp", "node", "setup", "--vmid-range",
		fmt.Sprintf("%d-%d", vmidLo, vmidHi), "--bridge", bridge, "--pubkey", "x", "--yes"}
	if out, err := dockerAs("pbp", setupArgv...); err == nil || strings.Contains(out, "pbp node setup on") {
		t.Errorf("pbp ran setup through sudo: %v, %q", err, out)
	}
	if out, err := dockerOutEnv([]string{"SSH_ORIGINAL_COMMAND=" + setupCmd}, "pbp", "sudo", "-n", "/usr/bin/pbp", "node"); err == nil ||
		!strings.Contains(out, "refused: bad-request") {
		t.Errorf("setup through the sudo rule: %v, %q; want a bad-request refusal", err, out)
	}

	for _, line := range []string{setupCmd, "pbp node uninstall --yes"} {
		cases := []struct {
			name string
			opts []string
			why  string
		}{
			{"no terminal", nil, "stdin is not a terminal"},
			{"ssh env", []string{"-t", "-e", "SSH_ORIGINAL_COMMAND=x"}, "SSH_ORIGINAL_COMMAND is set"},
			{"pbp sudo env", []string{"-t", "-e", "SUDO_USER=pbp"}, "started through the pbp account's sudo rule"},
			{"not root", []string{"-t", "-u", "pbp"}, "not root"},
		}
		for _, tc := range cases {
			out, code, err := nodeSetup(line, tc.opts...)
			t.Logf("%s (%s): exit %d\n%s", line, tc.name, code, out)
			if err != nil || code != 2 || !strings.Contains(out, "refused: run it interactively as root") ||
				!strings.Contains(out, tc.why) {
				t.Errorf("%s (%s): exit %d, %v; want exit 2 and a refusal naming %q", line, tc.name, code, err, tc.why)
			}
			if strings.Contains(out, nodeproto.ReplyBegin) || strings.Contains(out, " on "+nodeName+":") {
				t.Errorf("%s (%s): got past the gate", line, tc.name)
			}
		}
	}
	if after := snapshot(t); after != before {
		t.Errorf("refused runs changed files:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestUninstallRemovesAccess must stay last: it removes the setup. After
// `pbp node uninstall --yes` the PBP key gets nothing at all: every attacker
// case and a valid version request fail at SSH authentication, and the sudo
// rule is gone. --remove-token then removes the PVE token, ACLs, user and
// role.
func TestUninstallRemovesAccess(t *testing.T) {
	beforePct := pctLog(t)
	out, code, err := nodeSetup("pbp node uninstall --yes", "-t")
	t.Logf("pbp node uninstall --yes: exit %d\n%s", code, out)
	if err != nil || code != 0 || !strings.Contains(out, "pbp node uninstall: done") {
		t.Fatalf("exit %d, %v", code, err)
	}
	gone, err := dockerOut("sh", "-c", `for f in "$@"; do test -e "$f" && echo "$f"; done; true`, "sh",
		nodesetup.SudoersPath, nodesetup.AuthorizedKeys, nodesetup.SSHDir, nodesetup.NodeYAMLPath)
	if err != nil || gone != "" {
		t.Errorf("still present after uninstall: %q, %v", gone, err)
	}
	if shell, err := dockerOut("sh", "-c", "getent passwd pbp | cut -d: -f7"); err != nil || strings.TrimSpace(shell) != nodesetup.NoLogin {
		t.Errorf("pbp shell = %q, %v; want %s", shell, err, nodesetup.NoLogin)
	}
	if toks, err := dockerOut("cat", "/var/lib/pbp-e2e/tokens"); err != nil || !strings.HasPrefix(toks, "pbp@pve\tproof\t") {
		t.Errorf("tokens = %q, %v; want the token kept without --remove-token", toks, err)
	}

	for _, args := range [][]string{
		{"127.0.0.1", "id"},
		{"127.0.0.1", "bash"},
		{"127.0.0.1", "pbp node install-templates"},
		{"127.0.0.1", "sudo -l"},
		{"127.0.0.1", env.rerunLine + " --yes"},
		{"-t", "127.0.0.1"},
		{"-tt", "127.0.0.1", "id"},
		{"-W", "127.0.0.1:22", "127.0.0.1"},
	} {
		stdout, stderr, exit := attack(t, args...)
		t.Logf("ssh %q: exit %d\nstdout: %q\nstderr: %q", args, exit, stdout, stderr)
		if exit != 255 || stdout != "" || !strings.Contains(stderr, "Permission denied (publickey") {
			t.Errorf("ssh %q: exit %d, stdout %q; want 255, nothing, and the key refused", args, exit, stdout)
		}
	}
	c, err := sshx.Dial(config.SSH{Host: "127.0.0.1", Port: env.port, User: "pbp",
		KeyFile: env.keyFile, KnownHostsFile: env.knownHost})
	// Dial authenticates at once, so after uninstall it must fail there:
	// the key itself is refused, not merely the request.
	switch {
	case err == nil:
		v, verr := sshx.Helper{R: c}.Version(ctxT(t, 30*time.Second))
		_ = c.Close()
		t.Errorf("PBP's key still logs in after uninstall (version request: %q, %v)", v, verr)
	case !strings.Contains(err.Error(), "unable to authenticate"):
		t.Errorf("dial after uninstall: %v; want the key refused (unable to authenticate)", err)
	}
	if out, err := dockerOutEnv([]string{"SSH_ORIGINAL_COMMAND=version"}, "pbp", "sudo", "-n", "/usr/bin/pbp", "node"); err == nil {
		t.Errorf("the pbp sudo rule still works after uninstall: %q", out)
	}
	if after := pctLog(t); !slices.Equal(beforePct, after) {
		t.Errorf("a request after uninstall reached pct: log grew from %q to %q", beforePct, after)
	}

	out, code, err = nodeSetup("pbp node uninstall --remove-token --yes", "-t")
	t.Logf("pbp node uninstall --remove-token --yes: exit %d\n%s", code, out)
	if err != nil || code != 0 {
		t.Fatalf("exit %d, %v", code, err)
	}
	state, err := dockerOut(append([]string{"sh", "-c", `grep -H . "$@"; true`, "sh"}, pbpE2EState...)...)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{nodesetup.UserID, nodesetup.RoleName, nodesetup.TokenName} {
		if strings.Contains(state, id) {
			t.Errorf("PVE state still names %s after --remove-token:\n%s", id, state)
		}
	}
	out, code, err = nodeSetup("pbp node uninstall --remove-token --yes", "-t")
	if err != nil || code != 0 || !strings.Contains(out, "nothing to remove") {
		t.Errorf("second uninstall: exit %d, %v\n%s; want nothing to remove", code, err, out)
	}
}
