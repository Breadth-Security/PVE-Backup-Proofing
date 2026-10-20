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

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/hostsetup"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
)

const (
	setupUsage     = "usage: pbp setup [--dir DIR] [--deb]\n"
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

	cfg, pending, err := hostsetup.LoadState(l)
	if errors.Is(err, fs.ErrNotExist) {
		if code := a.askAndWrite(ctx, in, l); code != exitOK {
			return code
		}
		cfg, pending, err = hostsetup.LoadState(l)
	}
	if err != nil {
		return fail(err)
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
		if a.setupCheck(ctx, l, cfg, &buf) == exitOK {
			fprintln(a.stdout, msgAlreadyComplete)
			return exitOK
		}
		fprintf(a.stdout, "The existing setup does not pass the check:\n%s\nRun the lines below on the node to repair it.\n", buf.String())
	}

	fprint(a.stdout, "\n"+hostsetup.NodeLines(cfg, pub, version, l.HaveToken())+"\n")
	rep, code := a.readReply(ctx, in)
	if code != exitOK {
		return code
	}
	changes := hostsetup.Changes(cfg, rep)
	needRotate, err := hostsetup.Finish(l, cfg, rep)
	if err != nil {
		return fail(err)
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
	if code := a.setupCheck(ctx, l, done, a.stdout); code != exitOK {
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
func (a *app) askAndWrite(ctx context.Context, in *bufio.Reader, l hostsetup.Layout) int {
	if !l.Deb {
		if fi, err := os.Stat(l.Dir); err != nil || !fi.IsDir() {
			fprintf(a.stderr, "pbp setup: %s is not a directory\n", l.Dir)
			return exitFail
		}
	}
	fprintf(a.stdout, "No %s yet. A few questions (Enter accepts the default in brackets):\n", l.ConfigPath())
	var ans hostsetup.Answers
	var askErr error
	if !a.interruptible(ctx, func() { ans, askErr = hostsetup.Ask(in, a.stdout, l, a.defaultTZ()) }) {
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
// on-disk files: the config names the paths the daemon sees.
func (a *app) setupCheck(ctx context.Context, l hostsetup.Layout, cfg config.Config, out io.Writer) int {
	c := cfg
	c.Target.SSH.KeyFile = l.KeyPath()
	c.Target.API.TokenSecretFile = l.TokenPath()
	c.Target.SSH.KnownHostsFile = l.KnownHostsPath()
	return a.checkSteps(ctx, c, nil, &checkOut{w: out, ok: true})
}

// exists reports whether p exists.
func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// hostTZ is the time zone offered by default: $TZ, else /etc/timezone, else
// the zone the /etc/localtime symlink points at, else Etc/UTC.
func hostTZ(getenv func(string) string, timezoneFile, localtime string) string {
	zone := func(s string) string {
		s = strings.TrimPrefix(strings.TrimSpace(s), ":")
		if i := strings.Index(s, "zoneinfo/"); i >= 0 {
			s = s[i+len("zoneinfo/"):]
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
