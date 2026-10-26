package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/hostsetup"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/nodesetup"
)

const (
	setupUsage     = "usage: pbp setup [--dir DIR] [--deb [--replace]]\n"
	msgPasteReply  = "Paste the reply printed by pbp node setup, then press Enter:"
	msgInterrupted = "pbp setup: interrupted; everything written so far stays; run pbp setup again to continue"
)

// setupMain runs `pbp setup` on the PBP host (spec §4): the questions (only
// when config.yaml is missing), the SSH key, the lines for the node, the
// node's reply, and a final check. It runs as root and never opens the
// state store or takes the run lock.
func (a *app) setupMain(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("pbp setup", flag.ContinueOnError)
	flags.SetOutput(a.stderr)
	dir := flags.String("dir", ".", "the Docker checkout directory to set up")
	deb := flags.Bool("deb", false, "set up a .deb install (/etc/pbp) instead of a Docker directory")
	replace := flags.Bool("replace", false, "when setting up this Proxmox VE node too: back up and replace node files that differ, as pbp node setup --replace")
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if flags.NArg() > 0 {
		fprintf(a.stderr, "pbp setup: unexpected arguments %q\n%s", flags.Args(), setupUsage)
		return exitUsage
	}
	dirSet := false
	flags.Visit(func(f *flag.Flag) { dirSet = dirSet || f.Name == "dir" })
	if *deb && dirSet {
		fprintf(a.stderr, "pbp setup: --deb and --dir cannot be used together\n%s", setupUsage)
		return exitUsage
	}
	l := hostsetup.Layout{Dir: *dir, Deb: *deb}
	in := bufio.NewReader(a.stdin)
	fail := func(err error) int {
		fprintf(a.stderr, "pbp setup: %v\n", err)
		return exitFail
	}

	// On a Proxmox VE node, a .deb host can set the node up in this same
	// run: no second shell, no paste, and the token secret never shown.
	onNode := l.Deb && a.onPVENode != nil && a.onPVENode()
	cfg, pending, err := hostsetup.LoadState(l)
	if errors.Is(err, fs.ErrNotExist) {
		if code := a.askAndWrite(ctx, in, l, onNode); code != exitOK {
			return code
		}
		cfg, pending, err = hostsetup.LoadState(l)
	}
	if err != nil {
		return fail(err)
	}
	if err := l.CheckPaths(cfg); err != nil {
		return fail(err)
	}
	fixed, err := l.SecureSecrets()
	if err != nil {
		return fail(err)
	}
	for _, f := range fixed {
		fprintf(a.stdout, "Secured %s\n", f)
	}
	pub, created, err := hostsetup.EnsureKey(l)
	if err != nil {
		return fail(err)
	}
	if created {
		fprintf(a.stdout, "Created the SSH key %s\n", l.KeyPath())
	}

	if !pending && l.HaveToken() && exists(l.KnownHostsPath()) {
		var buf bytes.Buffer
		code, rejected := a.setupCheck(ctx, l, cfg, &buf)
		if code == exitOK {
			fprintln(a.stdout, msgAlreadyComplete)
			return exitOK
		}
		fprintf(a.stdout, "The existing setup does not pass the check:\n%s", buf.String())
		if rejected {
			a.rotateHint(cfg, pub, l)
			return exitFail
		}
		fprintln(a.stdout, "\nRun the lines below on the node to repair it.")
	}

	var rep nodeproto.SetupReply
	if local, code := a.offerLocalNode(onNode, in); code != exitOK {
		return code
	} else if local {
		var code int
		if rep, code = a.setUpLocalNode(ctx, in, l, cfg, pub, *replace); code != exitOK {
			return code
		}
	} else {
		fprint(a.stdout, "\n"+hostsetup.NodeLines(cfg, pub, version, l.HaveToken())+"\n")
		var code int
		if rep, code = a.readReply(ctx, in); code != exitOK {
			return code
		}
	}
	changes := hostsetup.Changes(cfg, rep)
	needRotate, warning, err := hostsetup.FinishChecked(ctx, l, cfg, rep, a.probeHostKey)
	if err != nil {
		return fail(err)
	}
	if warning != "" {
		fprintf(a.stderr, "pbp setup: warning: %s\n", warning)
	}
	if needRotate {
		sa := hostsetup.SetupArgs(cfg, pub, version, false)
		sa.RotateToken = true
		fprintf(a.stdout, "\nThe token %s already exists on the node and its secret cannot be read back, "+
			"and this host has no token secret. Nothing was written.\n"+
			"Run this on the node instead. It replaces the token with a new secret: any other daemon "+
			"using that token loses access until it gets the new secret.\n\n%s\n\nThen run pbp setup again.\n",
			hostsetup.TokenID, sa.Line())
		return exitFail
	}
	for _, c := range changes {
		fprintf(a.stdout, "Changed %s\n", c)
	}
	if rep.TokenSecret != "" {
		fprintf(a.stdout, "Wrote the token secret %s\n", l.TokenPath())
	}
	fprintf(a.stdout, "Wrote %s and %s\n\nChecking the setup:\n", l.KnownHostsPath(), l.ConfigPath())
	done, _, err := hostsetup.LoadState(l)
	if err != nil {
		return fail(err)
	}
	if code, rejected := a.setupCheck(ctx, l, done, a.stdout); code != exitOK {
		if rejected {
			a.rotateHint(done, pub, l)
		}
		return code
	}
	if l.Deb {
		fprintln(a.stdout, "\nSetup is complete. Start PBP: sudo systemctl start pbp")
	} else {
		fprintln(a.stdout, "\nSetup is complete. Start PBP: docker compose up -d")
	}
	return exitOK
}

// askAndWrite asks the questions and writes config.yaml (and, for Docker,
// logs/ and .env).
func (a *app) askAndWrite(ctx context.Context, in *bufio.Reader, l hostsetup.Layout, onNode bool) int {
	if !l.Deb {
		if fi, err := os.Stat(l.Dir); err != nil || !fi.IsDir() {
			fprintf(a.stderr, "pbp setup: %s is not a directory\n", l.Dir)
			return exitFail
		}
	}
	fprintf(a.stdout, "No %s yet. A few questions (Enter accepts the default in brackets):\n", l.ConfigPath())
	var ans hostsetup.Answers
	var askErr error
	// On the node itself, 127.0.0.1 always reaches it, whatever its name
	// resolves to.
	defaultAddr := ""
	if onNode {
		defaultAddr = "127.0.0.1"
	}
	if !a.interruptible(ctx, func() { ans, askErr = hostsetup.AskWith(in, a.stdout, l, a.defaultTZ(), defaultAddr) }) {
		fprintln(a.stderr, "\n"+msgInterrupted)
		return exitFail
	}
	if askErr != nil {
		fprintf(a.stderr, "pbp setup: %v\n", askErr)
		return exitFail
	}
	if err := hostsetup.WriteConfig(l, ans); err != nil {
		fprintf(a.stderr, "pbp setup: %v\n", err)
		return exitFail
	}
	fprintf(a.stdout, "Wrote %s\n", l.ConfigPath())
	return exitOK
}

// readReply prompts for the node's reply and reads it, with echo off when
// stdin is a terminal. Echo is restored on every path, including an
// interrupt (SIGINT or SIGTERM cancel ctx while the read blocks). The reply
// is never printed.
func (a *app) readReply(ctx context.Context, in *bufio.Reader) (nodeproto.SetupReply, int) {
	fprintln(a.stdout, msgPasteReply)
	if a.isTerminal() {
		restore, err := a.echoOff()
		if err != nil {
			fprintf(a.stderr, "pbp setup: %v\n", err)
			return nodeproto.SetupReply{}, exitFail
		}
		defer func() {
			restore()
			fprintln(a.stdout) // the Enter that ended the paste was not echoed
		}()
	}
	var rep nodeproto.SetupReply
	var err error
	if !a.interruptible(ctx, func() { rep, err = nodeproto.ReadReply(in) }) {
		fprintln(a.stderr, "\n"+msgInterrupted)
		return nodeproto.SetupReply{}, exitFail
	}
	if err != nil {
		fprintf(a.stderr, "pbp setup: %v\n", err)
		return nodeproto.SetupReply{}, exitFail
	}
	return rep, exitOK
}

// interruptible runs fn, a blocking read of stdin, and reports false if ctx
// ends first. fn is then abandoned blocked on its read; the caller exits.
func (a *app) interruptible(ctx context.Context, fn func()) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// setupCheck runs pbp check's steps (not the state store, not the lock) on
// a copy of cfg whose key, token and known_hosts paths are the layout's
// on-disk files: the config names the paths the daemon sees. It also
// reports whether the node rejected this host's token secret (HTTP 401).
func (a *app) setupCheck(ctx context.Context, l hostsetup.Layout, cfg config.Config, out io.Writer) (int, bool) {
	c := cfg
	c.Target.SSH.KeyFile = l.KeyPath()
	c.Target.API.TokenSecretFile = l.TokenPath()
	c.Target.SSH.KnownHostsFile = l.KnownHostsPath()
	co := &checkOut{w: out, ok: true}
	code := a.checkSteps(ctx, c, nil, co)
	return code, co.tokenRejected
}

// rotateHint tells the admin to replace a token whose secret this host
// has wrong: the node rejected it, so no reply without a new secret can
// fix the setup (code review 3.6).
func (a *app) rotateHint(cfg config.Config, pub string, l hostsetup.Layout) {
	sa := hostsetup.SetupArgs(cfg, pub, version, false)
	sa.RotateToken = true
	fprintf(a.stdout, "\nThe node rejects this host's token secret (%s): it was probably replaced elsewhere, or the file is wrong.\n"+
		"Run this on the node. It replaces the token with a new secret: any other daemon using that token loses access "+
		"until it gets the new secret.\n\n%s\n\nThen run pbp setup again and paste its reply.\n", l.TokenPath(), sa.Line())
}

// exists reports whether p exists.
func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// hostTZ is the time zone offered by default: $TZ, else /etc/timezone, else
// the zone the /etc/localtime symlink points at, else Etc/UTC. A source
// whose zone does not load (a POSIX rule such as EST5EDT,M3.2.0,M11.1.0 in
// $TZ, say) is skipped, since the question would reject it as its default.
func hostTZ(getenv func(string) string, timezoneFile, localtime string) string {
	zone := func(s string) string {
		s = strings.TrimPrefix(strings.TrimSpace(s), ":")
		if i := strings.Index(s, "zoneinfo/"); i >= 0 {
			s = s[i+len("zoneinfo/"):]
		}
		if s == "" {
			return ""
		}
		if _, err := time.LoadLocation(s); err != nil {
			return ""
		}
		return s
	}
	if z := zone(getenv("TZ")); z != "" {
		return z
	}
	// #nosec G304 -- a fixed system file
	if b, err := os.ReadFile(timezoneFile); err == nil {
		if z := zone(string(b)); z != "" {
			return z
		}
	}
	if t, err := os.Readlink(localtime); err == nil && strings.Contains(t, "zoneinfo/") {
		if z := zone(t); z != "" {
			return z
		}
	}
	return "Etc/UTC"
}

// offerLocalNode asks, on a Proxmox VE node, whether to set the node up in
// this run. It is offered only where pbp node setup itself may run (root,
// a terminal, not over SSH); otherwise the two-shell flow follows.
func (a *app) offerLocalNode(onNode bool, in *bufio.Reader) (bool, int) {
	if !onNode {
		return false, exitOK
	}
	if why := a.nodeGateReason(); why != "" {
		fprintf(a.stdout, "\nThis host is a Proxmox VE node, but it cannot be set up in this run (%s); run the lines below in a root shell on it.\n", why)
		return false, exitOK
	}
	fprintln(a.stdout, "\nThis host is a Proxmox VE node. pbp setup can set it up for PBP in this run as well:\n"+
		"the same checks, plan and confirmation as pbp node setup, with no second shell and no reply to paste.")
	return nodesetup.Confirm(in, a.stdout, "Set up this node here?"), exitOK
}

// setUpLocalNode runs pbp node setup's steps on this node and returns its
// reply, as if it had been pasted. When the API token already exists and
// this host has no secret for it, it offers to replace the token (PVE
// cannot show an existing secret again).
func (a *app) setUpLocalNode(ctx context.Context, in *bufio.Reader, l hostsetup.Layout, cfg config.Config, pub string, replace bool) (nodeproto.SetupReply, int) {
	sa := hostsetup.SetupArgs(cfg, pub, version, l.HaveToken())
	save := func(secret string) (string, error) {
		return l.TokenPath(), l.WriteSecretFile(l.TokenPath(), []byte(secret+"\n"))
	}
	res, code := a.nodeSetupRun(ctx, sa, nodeRun{in: in, replace: replace, local: true, saveSecret: save})
	if code != exitOK {
		return nodeproto.SetupReply{}, code
	}
	if res.complete {
		fprintln(a.stdout, "The node is already set up.")
	}
	if res.reply.TokenSecret != "" || l.HaveToken() {
		return res.reply, exitOK
	}
	fprintf(a.stdout, "\nThe API token %s already exists on this node, and PVE cannot show its secret again.\n"+
		"Replacing it makes a new secret; any other PBP daemon using the token loses access until it gets the new one.\n",
		hostsetup.TokenID)
	if !nodesetup.Confirm(in, a.stdout, "Replace the token?") {
		fprintln(a.stdout, msgNothingChanged+" on the token; pbp setup needs a token secret to finish")
		return nodeproto.SetupReply{}, exitFail
	}
	sa.CreateToken, sa.RotateToken = true, true
	if res, code = a.nodeSetupRun(ctx, sa, nodeRun{in: in, yes: true, replace: replace, local: true, quiet: true, saveSecret: save}); code != exitOK {
		return nodeproto.SetupReply{}, code
	}
	return res.reply, exitOK
}
