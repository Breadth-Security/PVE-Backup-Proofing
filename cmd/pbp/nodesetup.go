package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/nodesetup"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/pve"
)

const (
	msgAlreadyComplete = "setup has already been completed"
	msgCopyReply       = "Copy this block now; the token secret is not shown again."
	msgApplyPrompt     = "Apply these changes?"
	msgNothingChanged  = "nothing changed"
	msgNothingToRemove = "nothing to remove"
	pveshBin           = "/usr/bin/pvesh"
	maxNodeVMID        = 999999999
)

// setupGate refuses node setup and uninstall unless an admin runs them
// interactively as root: not over SSH (SSH_ORIGINAL_COMMAND set, even
// empty), not through the helper's sudo rule (SUDO_USER pbp), not as
// another user, and not with stdin redirected. An admin's own sudo (any
// other SUDO_USER) is allowed. --yes never lifts it.
func (a *app) setupGate(sub string) bool {
	var why string
	_, sshSet := os.LookupEnv("SSH_ORIGINAL_COMMAND")
	switch {
	case sshSet:
		why = "SSH_ORIGINAL_COMMAND is set"
	case os.Getenv("SUDO_USER") == "pbp":
		why = "started through the pbp account's sudo rule"
	case a.euid() != 0:
		why = "not root"
	case !a.isTerminal():
		why = "stdin is not a terminal"
	default:
		return true
	}
	fprintf(a.stderr, "pbp node %s: refused: run it interactively as root, not over SSH (%s)\n%s", sub, why, nodeUsage)
	return false
}

// usageErr prints a usage error for `pbp node <sub>`.
func (a *app) usageErr(sub, format string, args ...any) int {
	fprintf(a.stderr, "pbp node %s: %s\n%s", sub, fmt.Sprintf(format, args...), nodeUsage)
	return exitUsage
}

// stepFailed reports a failed step loudly and returns exitFail.
func (a *app) stepFailed(sub, step string, err error) int {
	fprintf(a.stderr, "pbp node %s: FAILED at %s: %v\n", sub, step, err)
	return exitFail
}

// parseVMIDRange parses "lo-hi" with the same bounds node.yaml enforces.
func parseVMIDRange(s string) ([2]int, error) {
	los, his, ok := strings.Cut(s, "-")
	lo, lerr := strconv.Atoi(los)
	hi, herr := strconv.Atoi(his)
	if !ok || lerr != nil || herr != nil {
		return [2]int{}, fmt.Errorf("--vmid-range must be LO-HI, got %q", s)
	}
	if lo < 100 || hi < lo || hi >= maxNodeVMID {
		return [2]int{}, fmt.Errorf("--vmid-range needs 100 <= LO <= HI < %d, got %q", maxNodeVMID, s)
	}
	return [2]int{lo, hi}, nil
}

// confirm asks before changing anything, unless --yes was given.
func (a *app) confirm(yes bool) bool {
	if yes {
		return true
	}
	ok := nodesetup.Confirm(a.stdin, a.stdout, msgApplyPrompt)
	if !ok {
		fprintln(a.stdout, msgNothingChanged)
	}
	return ok
}

// printPlan prints each item with its status and, if any, its diff.
func printPlan(w io.Writer, p nodesetup.Plan) {
	for _, it := range p.Items {
		if strings.HasSuffix(it.Name, it.Path) {
			fprintf(w, "  [%s] %s\n", it.Status, it.Name)
		} else {
			fprintf(w, "  [%s] %s (%s)\n", it.Status, it.Name, it.Path)
		}
		if it.Diff != "" {
			for _, l := range strings.Split(strings.TrimRight(it.Diff, "\n"), "\n") {
				fprintf(w, "        %s\n", l)
			}
		}
	}
}

// nodeSetup runs `pbp node setup`.
func (a *app) nodeSetup(ctx context.Context, args []string) int {
	const sub = "setup"
	if !a.setupGate(sub) {
		return exitUsage
	}
	fs := flag.NewFlagSet("pbp node setup", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	vmidRange := fs.String("vmid-range", "", "VMID range LO-HI for PBP's guests")
	bridge := fs.String("bridge", "", "the isolated bridge")
	pubkey := fs.String("pubkey", "", "the PBP host's ssh-ed25519 public key")
	storages := fs.String("storages", "", "storages PBP reads backups from and restores to, comma separated")
	daemonVersion := fs.String("daemon-version", "", "the PBP host's version")
	createToken := fs.Bool("create-token", false, "create the API token")
	rotateToken := fs.Bool("rotate-token", false, "replace the API token with a new secret (implies --create-token)")
	dryRun := fs.Bool("dry-run", false, "show the plan, change nothing")
	yes := fs.Bool("yes", false, "do not ask before applying")
	replace := fs.Bool("replace", false, "replace items that differ, after a backup")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		return a.usageErr(sub, "unexpected arguments %q", fs.Args())
	}
	if *vmidRange == "" || *bridge == "" || *pubkey == "" {
		return a.usageErr(sub, "--vmid-range, --bridge and --pubkey are required (copy the setup line from pbp setup)")
	}
	rng, err := parseVMIDRange(*vmidRange)
	if err != nil {
		return a.usageErr(sub, "%v", err)
	}
	var sts []string
	if *storages != "" {
		for s := range strings.SplitSeq(*storages, ",") {
			if s = strings.TrimSpace(s); s == "" {
				return a.usageErr(sub, "--storages has an empty name: %q", *storages)
			}
			sts = append(sts, s)
		}
	}
	sa := nodeproto.SetupArgs{VMIDRange: rng, Bridge: *bridge, PubKey: *pubkey, DaemonVersion: *daemonVersion,
		Storages: sts, CreateToken: *createToken || *rotateToken, RotateToken: *rotateToken}

	env := a.nodeSetupEnv
	api := pve.NewLocal(pveshBin, nodesetup.PveshRun(env))
	nodeName, err := nodesetup.Check(ctx, env, api, a.nodeSys, sa, version)
	if err != nil {
		return a.stepFailed(sub, "check", err)
	}
	if w := nodesetup.DaemonWarning(ctx, env); w != "" {
		fprintf(a.stderr, "pbp node setup: warning: %s\n", w)
	}
	plan, err := nodesetup.BuildPlan(ctx, env, sa, nodeName)
	if err != nil {
		return a.stepFailed(sub, "plan", err)
	}
	// The reply needs the node's ed25519 host key and API certificate, which
	// setup does not create: prove they are readable before changing
	// anything, so a token secret is never minted with no reply to carry it.
	preflight, err := nodesetup.BuildReply(env, sa, nodeName, version, "")
	if err != nil {
		fprintf(a.stderr, "pbp node setup: FAILED at reply: %v\n%s\n", err, msgNothingChanged)
		return exitFail
	}
	if plan.Complete() && !sa.RotateToken {
		fprintln(a.stdout, msgAlreadyComplete)
		return a.printBlock(sub, preflight, "")
	}

	opts := nodesetup.ApplyOpts{Replace: *replace}
	fprintf(a.stdout, "pbp node setup on %s:\n", nodeName)
	printPlan(a.stdout, plan)
	if *dryRun {
		fprintln(a.stdout, "dry run: nothing changed")
		return exitOK
	}
	// Apply would refuse this plan before any change: say so, do not ask.
	if err := plan.Refusal(opts); err != nil {
		fprintf(a.stderr, "pbp node setup: %v\n%s\n", err, msgNothingChanged)
		return exitFail
	}
	if !a.confirm(*yes) {
		return exitFail
	}

	secret, applyErr := nodesetup.Apply(ctx, env, plan, sa, nodeName, opts)
	var selfErr error
	if applyErr == nil {
		selfErr = nodesetup.SelfTest(ctx, env, sa, nodeName)
	}
	code := exitOK
	// PVE shows a token secret only once: hand it over before any error.
	if secret != "" || (applyErr == nil && selfErr == nil) {
		code = a.printReply(sub, env, sa, nodeName, secret)
	}
	switch {
	case applyErr != nil:
		return a.stepFailed(sub, "apply", applyErr)
	case selfErr != nil:
		return a.stepFailed(sub, "self-test", selfErr)
	}
	if code == exitOK {
		fprintln(a.stdout, "pbp node setup: done; paste the block above into pbp setup on the PBP host")
	}
	return code
}

// printReply builds and prints the reply block for the PBP host. If the
// reply cannot be built (the host key or certificate vanished after the
// preflight) while a secret is in hand, the secret is printed on its own,
// clearly labelled and outside any reply markers, so it is never lost and
// no partial reply can be pasted; the failure is reported.
func (a *app) printReply(sub string, env nodesetup.Env, sa nodeproto.SetupArgs, nodeName, secret string) int {
	rep, err := nodesetup.BuildReply(env, sa, nodeName, version, secret)
	if err != nil {
		if secret != "" {
			fprintf(a.stdout, "\nThe API token %s was created but the reply could not be built: %v.\n"+
				"Its secret, shown once; put it in the PBP host's token file by hand:\n%s\n",
				nodesetup.TokenID, err, secret)
		}
		return a.stepFailed(sub, "reply", err)
	}
	return a.printBlock(sub, rep, secret)
}

// printBlock prints rep as a reply block, after the copy warning when it
// carries a secret.
func (a *app) printBlock(sub string, rep nodeproto.SetupReply, secret string) int {
	block, err := nodeproto.EncodeReply(rep)
	if err != nil {
		// Never quote rep: it may hold the secret.
		return a.stepFailed(sub, "reply", errors.New("cannot encode the reply"))
	}
	fprintln(a.stdout)
	if secret != "" {
		fprintln(a.stdout, msgCopyReply)
	}
	fprint(a.stdout, block)
	return exitOK
}

// nodeUninstall runs `pbp node uninstall`.
func (a *app) nodeUninstall(ctx context.Context, args []string) int {
	const sub = "uninstall"
	if !a.setupGate(sub) {
		return exitUsage
	}
	fs := flag.NewFlagSet("pbp node uninstall", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	removeToken := fs.Bool("remove-token", false, "also remove the API token, its ACLs, the pbp@pve user and the PBPProofer role")
	dryRun := fs.Bool("dry-run", false, "show the plan, change nothing")
	yes := fs.Bool("yes", false, "do not ask before removing")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		return a.usageErr(sub, "unexpected arguments %q", fs.Args())
	}

	env := a.nodeSetupEnv
	plan, err := nodesetup.BuildUninstallPlan(ctx, env, *removeToken)
	if err != nil {
		return a.stepFailed(sub, "plan", err)
	}
	if len(plan.Items) == 0 {
		fprintln(a.stdout, msgNothingToRemove)
		return exitOK
	}
	fprintln(a.stdout, "pbp node uninstall:")
	printPlan(a.stdout, plan)
	if *dryRun {
		fprintln(a.stdout, "dry run: nothing changed")
		return exitOK
	}
	if !a.confirm(*yes) {
		return exitFail
	}
	if err := nodesetup.Uninstall(ctx, env, plan, *removeToken); err != nil {
		return a.stepFailed(sub, "uninstall", err)
	}
	fprintln(a.stdout, "pbp node uninstall: done")
	return exitOK
}
