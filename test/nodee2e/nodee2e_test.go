//go:build nodee2e

// Package nodee2e tests the `pbp node` helper end to end over real SSH: a
// throwaway Debian 13 container with sshd, PBP installed from its .deb and
// the node set up per README "Node setup" (unprivileged pbp user, forced
// command, one sudo rule), with stub Proxmox tools. It proves the daemon's
// real client works through that setup and that the same key, used by an
// attacker, gets nothing else.
//
// It needs Docker and an OpenSSH client; run it with test/nodee2e/run.sh,
// which builds the .deb and sets:
//
//	PBP_E2E_DEB      path of the amd64 .deb to install in the container
//	PBP_E2E_VERSION  the version that .deb was built with
package nodee2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/report"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/sshx"
)

const (
	taggedCT = 990001 // in range, tagged pbp
	vmidLo   = 990000
	vmidHi   = 990009
	bridge   = "vmbr99"
)

// env is the running container, shared by every test.
var env struct {
	container string
	port      int
	keyFile   string
	knownHost string
	version   string
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
	defer os.RemoveAll(tmp)

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

// setUp builds the image with a fresh key, starts the container and records
// its host key.
func setUp(tmp, deb, image string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	block, err := ssh.MarshalPrivateKey(priv, "pbp-nodee2e")
	if err != nil {
		return err
	}
	env.keyFile = filepath.Join(tmp, "id_ed25519")
	if err := os.WriteFile(env.keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		return err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return err
	}
	pubLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " pbp-nodee2e"

	ctxDir := filepath.Join(tmp, "context")
	if err := os.MkdirAll(ctxDir, 0o755); err != nil {
		return err
	}
	if err := os.CopyFS(ctxDir, os.DirFS(".")); err != nil {
		return err
	}
	if err := copyFile(deb, filepath.Join(ctxDir, "pbp.deb")); err != nil {
		return err
	}
	if out, err := exec.Command("docker", "build", "-q", "-t", image,
		"--build-arg", "PBP_PUBKEY="+pubLine, ctxDir).CombinedOutput(); err != nil {
		return fmt.Errorf("docker build: %v\n%s", err, out)
	}
	if out, err := exec.Command("docker", "run", "-d", "--name", env.container,
		"--cap-add", "NET_ADMIN", "-p", "127.0.0.1::22", image).CombinedOutput(); err != nil {
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
	hostKey, err := dockerOut("cat", "/etc/ssh/ssh_host_ed25519_key.pub")
	if err != nil {
		return err
	}
	f := strings.Fields(hostKey)
	if len(f) < 2 {
		return fmt.Errorf("host key %q", hostKey)
	}
	env.knownHost = filepath.Join(tmp, "known_hosts")
	line := fmt.Sprintf("[127.0.0.1]:%d %s %s\n", env.port, f[0], f[1])
	if err := os.WriteFile(env.knownHost, []byte(line), 0o600); err != nil {
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
				return nil
			}
		}
		if time.Now().After(deadline) {
			return errors.New("sshd did not come up")
		}
		time.Sleep(200 * time.Millisecond)
	}
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
		RequestTimeout: 2 * time.Minute, Version: env.version}
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
		!slices.Equal(st.Members, []string{"pbpe2e0"}) {
		t.Errorf("Bridge = %+v, want vmbr99, not OVS, disable_ipv6 %q, members [pbpe2e0]", st, strings.TrimSpace(v6))
	}
	var addrs []struct {
		IfName string `json:"ifname"`
	}
	if err := json.Unmarshal(st.Addrs, &addrs); err != nil || len(addrs) != 1 || addrs[0].IfName != bridge {
		t.Errorf("Bridge addrs = %s (%v), want the ip -j addr entry for %s", st.Addrs, err, bridge)
	}
	ovsLog, err := dockerOut("cat", "/var/log/pbp-e2e/ovs-vsctl.log")
	if err != nil || !strings.Contains(ovsLog, "<br-exists> <vmbr99>") {
		t.Errorf("ovs-vsctl stub log = %q, %v; want a br-exists vmbr99 call", ovsLog, err)
	}
}

// TestTemplates checks the templates op before and after the admin installs
// them as root on the node (the only way: it is refused over SSH).
func TestTemplates(t *testing.T) {
	h := helper(t)
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
	payload := []byte(`{"severity":"warning","data":{"total":3},"fields":{"type":"other","guest":"web"}}`)
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
		Data     map[string]int    `json:"data"`
		Fields   map[string]string `json:"fields"`
	}
	if err := json.Unmarshal([]byte(stdin), &got); err != nil {
		t.Fatalf("perl stdin %q: %v", stdin, err)
	}
	if got.Severity != "warning" || got.Data["total"] != 3 || got.Fields["type"] != "pbp" || got.Fields["guest"] != "web" {
		t.Errorf("perl stdin = %+v, want the payload with fields.type forced to pbp", got)
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
	dl := time.Now().Add(time.Minute).UnixMilli()
	injected := fmt.Sprintf(`exec --deadline %d --nonce %s %d -- ["id"];id`, dl, nonce, taggedCT)
	cases := []struct {
		name string
		args []string
		// refused: the helper itself refuses with exit 250.
		refused bool
		stderr  string
	}{
		{"id", []string{"127.0.0.1", "id"}, true, "refused"},
		{"bash", []string{"127.0.0.1", "bash"}, true, "refused"},
		{"injected exec", []string{"127.0.0.1", injected}, true, "refused"},
		{"exec then newline id", []string{"127.0.0.1", strings.Replace(injected, ";id", "\nid", 1)}, true, "refused"},
		{"install-templates", []string{"127.0.0.1", "pbp node install-templates"}, true, "refused"},
		{"sudo -l", []string{"127.0.0.1", "sudo -l"}, true, "refused"},
		// -t without a terminal on stdin requests no pty: an interactive
		// login, which reaches the helper with no SSH_ORIGINAL_COMMAND.
		{"login no command", []string{"-t", "127.0.0.1"}, true, "no request"},
		// -tt forces the pty request; no-pty denies it and ssh gives up.
		{"forced pty no command", []string{"-tt", "127.0.0.1"}, false, "PTY allocation request failed"},
		{"forced pty id", []string{"-tt", "127.0.0.1", "id"}, false, "PTY allocation request failed"},
		// The OpenSSH client printed no stderr for this subsystem session in
		// testing; exit 250 (the helper's refusal code) is the check.
		{"sftp subsystem", []string{"-s", "127.0.0.1", "sftp"}, true, ""},
		{"stdio forward", []string{"-W", "127.0.0.1:22", "127.0.0.1"}, false, "prohibited"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, errOut, exit := attack(t, tc.args...)
			t.Logf("exit %d\nstdout: %q\nstderr: %q", exit, out, errOut)
			if exit == 0 {
				t.Errorf("exit 0, want failure")
			}
			if tc.refused && exit != nodeproto.ExitRefused {
				t.Errorf("exit %d, want %d", exit, nodeproto.ExitRefused)
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

	// env_reset drops PBP_NODE_CONFIG; the root helper reads node.yaml only.
	out, err = dockerOutEnv([]string{"SSH_ORIGINAL_COMMAND=bash", "PBP_NODE_CONFIG=/nonexistent"},
		"pbp", "sudo", "-n", "/usr/bin/pbp", "node")
	if err == nil || !strings.Contains(out, "refused: bad-request") {
		t.Errorf("helper via sudo with PBP_NODE_CONFIG set: %v, %q; want a bad-request refusal from node.yaml settings", err, out)
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
		defer c.Close()
		nonce := nodeproto.NewNonce()
		start := time.Now()
		cmd, err := nodeproto.Encode(nodeproto.Request{Op: "exec", VMID: taggedCT,
			Argv: []string{"sleep", "61"}, Deadline: start.Add(deadlineIn), Nonce: nonce})
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

	t.Run("client gone at deadline", func(t *testing.T) {
		h := helper(t)
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), deadlineIn)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := h.Exec(ctx, taggedCT, []string{"sleep", "62"})
			done <- err
		}()
		if !waitFor(t, deadlineIn, func() bool { return sleeping(t, "sleep 62") }) {
			t.Fatal("sleep 62 never started in the container")
		}
		if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Exec err = %v, want context.DeadlineExceeded", err)
		}
		// The daemon's session is gone; the helper must still kill the
		// command on its own.
		if !waitFor(t, time.Until(start.Add(deadlineIn+grace)), func() bool { return !sleeping(t, "sleep 62") }) {
			t.Errorf("sleep 62 still running %v after the request started", time.Since(start))
		}
		t.Logf("sleep 62 gone %v after the request started", time.Since(start).Round(10*time.Millisecond))
	})
}
