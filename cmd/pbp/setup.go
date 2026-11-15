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
	setupUsage = "usage: pbp setup [--dir DIR] [--deb [--replace]]\n" +
		"       pbp setup [--dir DIR | --deb] --unattended [--address A] [--vmid-range LO-HI] [--bridge B] [--restore-storage S]\n" +
		"                 [--storages S1,S2] [--schedule CRON] [--tz ZONE] [--node-reply FILE|-] [--rotate-token] [--replace]\n"
	msgPasteReply  = "Paste the reply printed by pbp node setup, then press Enter:"
	msgInterrupted = "pbp setup: interrupted; everything written so far stays; run pbp setup again to continue"
	// msgFixStorages follows a check whose only failure is a storage
	// problem, which the node lines would not change (#119).
	msgFixStorages = "The node lines do not change this. Fix the storage named above on the node (pvesm status lists them), " +
		"or correct sources.storages in the config, then run pbp setup again."
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
	unattended := flags.Bool("unattended", false, "never prompt or read stdin: take the answers from the flags below")
	var ff hostsetup.Flags
	flags.StringVar(&ff.Address, "address", "", "the node's hostname or IP (default 127.0.0.1 on the node itself)")
	flags.StringVar(&ff.VMIDRange, "vmid-range", "", "VMID range LO-HI for proof guests (default 990000-990049)")
	flags.StringVar(&ff.Bridge, "bridge", "", "the isolated bridge (default vmbr99)")
	flags.StringVar(&ff.RestoreStorage, "restore-storage", "", "the restore storage (default local-zfs)")
	flags.StringVar(&ff.Storages, "storages", "", "the PBS storages to prove, comma-separated (required)")
	flags.StringVar(&ff.Schedule, "schedule", "", `cron schedule (default "0 2 * * *")`)
	flags.StringVar(&ff.TZ, "tz", "", "time zone for the schedule, Docker only (default the host's)")
	nodeReply := flags.String("node-reply", "", "read the node's reply from this file (- for stdin) instead of a paste")
	rotate := flags.Bool("rotate-token", false, "on the node itself: replace an existing API token whose secret this host lacks")
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
	if !*unattended && (ff != (hostsetup.Flags{}) || *nodeReply != "" || *rotate) {
		fprintf(a.stderr, "pbp setup: the answer flags, --node-reply and --rotate-token need --unattended\n%s", setupUsage)
		return exitUsage
	}
	l := hostsetup.Layout{Dir: *dir, Deb: *deb}
	// On a Proxmox VE node, a .deb host can set the node up in this same
	// run: no second shell, no paste, and the token secret never shown.
	onNode := l.Deb && a.onPVENode != nil && a.onPVENode()
	// --rotate-token acts only when this run sets the node up itself; a
	// separate host refuses it rather than ignore it (issue #134).
	if *rotate && (!onNode || a.nodeGateReason(true) != "") {
		fprintf(a.stderr, "pbp setup: --rotate-token applies only when pbp setup sets up the Proxmox VE node it runs on, as root; "+
			"for a separate PBP host, add --rotate-token to the pbp node setup line on the node\n%s", setupUsage)
		return exitUsage
	}
	in := bufio.NewReader(a.stdin)
	var flagAnswers *hostsetup.Flags
	lineOut := a.stdout
	if *unattended {
		// Nothing is read, and stdout carries only the node line, for
		// automation: every other message goes to stderr.
		in = bufio.NewReader(unattendedInput{})
		flagAnswers = &ff
		a.stdout = a.stderr
		defer func() { a.stdout = lineOut }()
	}
	fail := func(err error) int {
		fprintf(a.stderr, "pbp setup: %v\n", err)
		return exitFail
	}

	cfg, pending, err := hostsetup.LoadState(l)
	if errors.Is(err, fs.ErrNotExist) {
		if code := a.askAndWrite(ctx, in, l, onNode, flagAnswers); code != exitOK {
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
	if *unattended {
		if diffs := hostsetup.Conflicts(cfg, ff); len(diffs) > 0 {
			for _, d := range diffs {
				fprintf(a.stderr, "pbp setup: %s\n", d)
			}
			fprintln(a.stderr, "pbp setup: an unattended run does not change an existing config.yaml: edit it, or pass flags that match")
			return exitFail
		}
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

	// rejected: the node rejects this host's token secret. Only a new
	// secret can finish the setup, and this run can still take one: the
	// reply of the --rotate-token line, or the rotation on this node
	// (issue #134; it used to stop here, so every rerun stopped again).
	rejected := false
	if !pending && l.HaveToken() && exists(l.KnownHostsPath()) {
		var buf bytes.Buffer
		code, rej, storageOnly := a.setupCheck(ctx, l, cfg, &buf)
		if code == exitOK {
			fprintln(a.stdout, msgAlreadyComplete)
			return exitOK
		}
		fprintf(a.stdout, "The existing setup does not pass the check:\n%s", buf.String())
		switch {
		case rej:
			rejected = true
		case storageOnly:
			fprintln(a.stdout, "\n"+msgFixStorages)
			return exitFail
		default:
			fprintln(a.stdout, "\nRun the lines below on the node to repair it.")
		}
	}
	haveToken := l.HaveToken() && !rejected

	var rep nodeproto.SetupReply
	if local, code := a.offerLocalNode(onNode, in, *unattended); code != exitOK {
		return code
	} else if local {
		var code int
		if rep, code = a.setUpLocalNode(ctx, in, l, cfg, pub, *replace, *unattended, *rotate, rejected); code != exitOK {
			return code
		}
	} else if *unattended {
		if *nodeReply == "" {
			if rejected {
				a.rotateHint(pub, l, cfg, lineOut, true, "Then run pbp setup again with --node-reply FILE (on the node, add --yes --unattended --reply-file FILE).")
				return exitNeedRotate
			}
			fprintln(lineOut, hostsetup.SetupArgs(cfg, pub, version, haveToken).Line())
			fprintln(a.stderr, "pbp setup: run the line above on the node as root (add --yes --unattended --reply-file FILE), "+
				"then run pbp setup again with --node-reply FILE")
			return exitWaitReply
		}
		var err error
		if rep, err = a.readReplyFrom(*nodeReply); err != nil {
			return fail(err)
		}
	} else {
		if rejected {
			a.rotateHint(pub, l, cfg, lineOut, false, "Then paste its reply below.")
		} else {
			fprint(a.stdout, "\n"+hostsetup.NodeLines(cfg, pub, version, haveToken)+"\n")
		}
		var code int
		if rep, code = a.readReply(ctx, in); code != exitOK {
			return code
		}
	}
	if rejected && rep.TokenSecret == "" {
		line := rotateLine(cfg, pub)
		fprintf(a.stdout, "\nThe reply carries no new token secret, and the node rejects this host's. Nothing was written.\n"+
			"Run this on the node, then use its reply:\n\n%s\n", line)
		if *unattended {
			fprintln(lineOut, line)
		}
		return exitNeedRotate
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
		line := rotateLine(cfg, pub)
		fprintf(a.stdout, "\nThe token %s already exists on the node and its secret cannot be read back, "+
			"and this host has no token secret. Nothing was written.\n"+
			"Run this on the node instead. It replaces the token with a new secret: any other daemon "+
			"using that token loses access until it gets the new secret.\n\n%s\n\nThen run pbp setup again.\n",
			hostsetup.TokenID, line)
		if *unattended {
			fprintln(lineOut, line)
		}
		return exitNeedRotate
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
	if code, rejected, storageOnly := a.setupCheck(ctx, l, done, a.stdout); code != exitOK {
		switch {
		case rejected:
			next := "Then run pbp setup again and paste its reply."
			if *unattended {
				next = "Then run pbp setup again with --node-reply FILE."
			}
			a.rotateHint(pub, l, done, lineOut, *unattended, next)
			return exitNeedRotate
		case storageOnly:
			fprintln(a.stdout, "\n"+msgFixStorages)
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
// With ff (an unattended run), the answers come from the flags instead.
func (a *app) askAndWrite(ctx context.Context, in *bufio.Reader, l hostsetup.Layout, onNode bool, ff *hostsetup.Flags) int {
	if !l.Deb {
		if fi, err := os.Stat(l.Dir); err != nil || !fi.IsDir() {
			fprintf(a.stderr, "pbp setup: %s is not a directory\n", l.Dir)
			return exitFail
		}
	}
	var ans hostsetup.Answers
	var askErr error
	// On the node itself, 127.0.0.1 always reaches it, whatever its name
	// resolves to.
	defaultAddr := ""
	if onNode {
		defaultAddr = "127.0.0.1"
	}
	if ff != nil {
		ans, askErr = hostsetup.FromFlags(*ff, l, a.defaultTZ(), defaultAddr)
	} else {
		fprintf(a.stdout, "No %s yet. A few questions (Enter accepts the default in brackets):\n", l.ConfigPath())
		if !a.interruptible(ctx, func() { ans, askErr = hostsetup.AskWith(in, a.stdout, l, a.defaultTZ(), defaultAddr) }) {
			fprintln(a.stderr, "\n"+msgInterrupted)
			return exitFail
		}
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
// reports whether the node rejected this host's token secret (HTTP 401), and
// whether the only failure is one no node line fixes (checkOut.storageOnly).
func (a *app) setupCheck(ctx context.Context, l hostsetup.Layout, cfg config.Config, out io.Writer) (code int, rejected, storageOnly bool) {
	c := cfg
	c.Target.SSH.KeyFile = l.KeyPath()
	c.Target.API.TokenSecretFile = l.TokenPath()
	c.Target.SSH.KnownHostsFile = l.KnownHostsPath()
	co := &checkOut{w: out, ok: true}
	code = a.checkSteps(ctx, c, nil, co)
	return code, co.tokenRejected, co.storageOnly()
}

// rotateLine is the pbp node setup line that replaces the API token.
func rotateLine(cfg config.Config, pub string) string {
	sa := hostsetup.SetupArgs(cfg, pub, version, false)
	sa.RotateToken = true
	return sa.Line()
}

// rotateHint tells the admin to replace a token whose secret this host
// has wrong: the node rejected it, so only a reply with a new secret can
// fix the setup (code review 3.6). next says what to do after running the
// line. Unattended, the line alone also goes to lineOut (stdout), for
// automation.
func (a *app) rotateHint(pub string, l hostsetup.Layout, cfg config.Config, lineOut io.Writer, unattended bool, next string) {
	line := rotateLine(cfg, pub)
	if unattended {
		defer fprintln(lineOut, line)
	}
	fprintf(a.stdout, "\nThe node rejects this host's token secret (%s): it was probably replaced elsewhere, or the file is wrong.\n"+
		"Run this on the node. It replaces the token with a new secret: any other daemon using that token loses access "+
		"until it gets the new secret.\n\n%s\n\n%s\n", l.TokenPath(), line, next)
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
// An unattended run (issue #131) needs no terminal and takes yes.
func (a *app) offerLocalNode(onNode bool, in *bufio.Reader, unattended bool) (bool, int) {
	if !onNode {
		return false, exitOK
	}
	if why := a.nodeGateReason(unattended); why != "" {
		fprintf(a.stdout, "\nThis host is a Proxmox VE node, but it cannot be set up in this run (%s); run the lines below in a root shell on it.\n", why)
		return false, exitOK
	}
	fprintln(a.stdout, "\nThis host is a Proxmox VE node. pbp setup can set it up for PBP in this run as well:\n"+
		"the same checks, plan and confirmation as pbp node setup, with no second shell and no reply to paste.")
	if unattended {
		fprintln(a.stdout, "Setting this node up in this run (--unattended).")
		return true, exitOK
	}
	return nodesetup.Confirm(in, a.stdout, "Set up this node here?"), exitOK
}

// setUpLocalNode runs pbp node setup's steps on this node and returns its
// reply, as if it had been pasted. When the API token already exists and
// this host has no secret for it, it offers to replace the token (PVE
// cannot show an existing secret again).
// Unattended, the plan is applied without asking, and the token is
// replaced only with --rotate-token (rotate). rejected: the node rejects
// this host's secret, which then counts as none (issue #134).
func (a *app) setUpLocalNode(ctx context.Context, in *bufio.Reader, l hostsetup.Layout, cfg config.Config, pub string, replace, unattended, rotate, rejected bool) (nodeproto.SetupReply, int) {
	haveToken := l.HaveToken() && !rejected
	sa := hostsetup.SetupArgs(cfg, pub, version, haveToken)
	save := func(secret string) (string, error) {
		return l.TokenPath(), l.WriteSecretFile(l.TokenPath(), []byte(secret+"\n"))
	}
	res, code := a.nodeSetupRun(ctx, sa, nodeRun{in: in, yes: unattended, replace: replace, local: true, saveSecret: save})
	if code != exitOK {
		return nodeproto.SetupReply{}, code
	}
	if res.complete {
		fprintln(a.stdout, "The node is already set up.")
	}
	if res.reply.TokenSecret != "" || haveToken {
		return res.reply, exitOK
	}
	if rejected {
		fprintf(a.stdout, "\nThis node rejects this host's secret for the API token %s, and PVE cannot show the token's secret again.\n", hostsetup.TokenID)
	} else {
		fprintf(a.stdout, "\nThe API token %s already exists on this node, and PVE cannot show its secret again.\n", hostsetup.TokenID)
	}
	fprintln(a.stdout, "Replacing it makes a new secret; any other PBP daemon using the token loses access until it gets the new one.")
	switch {
	case unattended && !rotate:
		fprintln(a.stdout, msgNothingChanged+" on the token; pbp setup needs a token secret to finish: pass --rotate-token to replace it")
		return nodeproto.SetupReply{}, exitNeedRotate
	case !unattended && !nodesetup.Confirm(in, a.stdout, "Replace the token?"):
		fprintln(a.stdout, msgNothingChanged+" on the token; pbp setup needs a token secret to finish")
		return nodeproto.SetupReply{}, exitFail
	}
	sa.CreateToken, sa.RotateToken = true, true
	if res, code = a.nodeSetupRun(ctx, sa, nodeRun{in: in, yes: true, replace: replace, local: true, quiet: true, saveSecret: save}); code != exitOK {
		return nodeproto.SetupReply{}, code
	}
	return res.reply, exitOK
}

// unattendedInput is stdin for an unattended run: any read fails, so a
// prompt that was missed declines instead of waiting for input.
type unattendedInput struct{}

func (unattendedInput) Read([]byte) (int, error) {
	return 0, errors.New("an unattended run reads no input")
}

// readReplyFrom reads the node's reply from the file path, or from stdin
// for "-" (issue #131).
func (a *app) readReplyFrom(path string) (nodeproto.SetupReply, error) {
	if path == "-" {
		return nodeproto.ReadReply(a.stdin)
	}
	// #nosec G304 -- the admin names the reply file
	f, err := os.Open(path)
	if err != nil {
		return nodeproto.SetupReply{}, err
	}
	defer func() { _ = f.Close() }()
	return nodeproto.ReadReply(f)
}
