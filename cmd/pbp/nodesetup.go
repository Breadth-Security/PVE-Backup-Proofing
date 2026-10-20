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

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/node"
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
	why := a.nodeGateReason()
	if why == "" {
		return true
	}
	fprintf(a.stderr, "pbp node %s: refused: run it interactively as root, not over SSH (%s)\n%s", sub, why, nodeUsage)
	return false
}

// nodeGateReason says why node setup may not run here, "" when it may.
func (a *app) nodeGateReason() string {
	_, sshSet := os.LookupEnv("SSH_ORIGINAL_COMMAND")
	switch {
	case sshSet:
		return "SSH_ORIGINAL_COMMAND is set"
	case os.Getenv("SUDO_USER") == "pbp":
		return "started through the pbp account's sudo rule"
	case a.euid() != 0:
		return "not root"
	case !a.isTerminal():
		return "stdin is not a terminal"
	}
	return ""
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
	if !node.ValidBridge(*bridge) {
		return a.usageErr(sub, "--bridge must be a bridge name such as vmbr99, got %q", *bridge)
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

	res, code := a.nodeSetupRun(ctx, sa, nodeRun{in: a.stdin, dryRun: *dryRun, yes: *yes, replace: *replace})
	switch {
	case code != exitOK || res.dryRun:
		return code
	case res.complete:
		fprintln(a.stdout, msgAlreadyComplete)
		return a.printBlock(sub, res.reply, "")
	}
	if code := a.printBlock(sub, res.reply, res.reply.TokenSecret); code != exitOK {
		return code
	}
	fprintln(a.stdout, "pbp node setup: done; paste the block above into pbp setup on the PBP host")
	return exitOK
}

// nodeRun holds how nodeSetupRun runs: where the y/N answer comes from,
// and the --dry-run, --yes and --replace choices.
type nodeRun struct {
	in                   io.Reader
	dryRun, yes, replace bool
}

// nodeResult is what a node setup run ends with: the reply for the PBP host
// (with the token secret when one was created), and whether the node was
// already complete or the run was only a dry run.
type nodeResult struct {
	reply            nodeproto.SetupReply
	complete, dryRun bool
}

// nodeSetupRun is `pbp node setup` after its arguments: the checks, the
// plan, the confirmation, apply and self-test, and the reply. Failures are
// reported here and give a non-zero code. PVE shows a token secret only
// once, so a secret created before a later failure is printed here, outside
// any reply markers, before the failure is reported. The caller hands the
// reply over: printed for a separate PBP host, or straight to pbp setup when
// it runs on this node.
func (a *app) nodeSetupRun(ctx context.Context, sa nodeproto.SetupArgs, o nodeRun) (nodeResult, int) {
	const sub = "setup"
	env := a.nodeSetupEnv
	api := pve.NewLocal(pveshBin, nodesetup.PveshRun(env))
	nodeName, err := nodesetup.Check(ctx, env, api, a.nodeSys, sa, version)
	if err != nil {
		return nodeResult{}, a.stepFailed(sub, "check", err)
	}
	if w := nodesetup.DaemonWarning(ctx, env); w != "" {
		fprintf(a.stderr, "pbp node setup: warning: %s\n", w)
	}
	plan, err := nodesetup.BuildPlan(ctx, env, sa, nodeName)
	if err != nil {
		return nodeResult{}, a.stepFailed(sub, "plan", err)
	}
	for _, w := range plan.Warnings {
		fprintf(a.stderr, "pbp node setup: warning: %s\n", w)
	}
	// The reply needs the node's ed25519 host key and API certificate, which
	// setup does not create: prove they are readable before changing
	// anything, so a token secret is never minted with no reply to carry it.
	preflight, err := nodesetup.BuildReply(env, sa, nodeName, version, "")
	if err != nil {
		fprintf(a.stderr, "pbp node setup: FAILED at reply: %v\n%s\n", err, msgNothingChanged)
		return nodeResult{}, exitFail
	}
	if plan.Complete() && !sa.RotateToken {
		return nodeResult{reply: preflight, complete: true}, exitOK
	}

	opts := nodesetup.ApplyOpts{Replace: o.replace}
	fprintf(a.stdout, "pbp node setup on %s:\n", nodeName)
	printPlan(a.stdout, plan)
	if o.dryRun {
		fprintln(a.stdout, "dry run: nothing changed")
		return nodeResult{dryRun: true}, exitOK
	}
	// Apply would refuse this plan before any change: say so, do not ask.
	if err := plan.Refusal(opts); err != nil {
		fprintf(a.stderr, "pbp node setup: %v\n%s\n", err, msgNothingChanged)
		return nodeResult{}, exitFail
	}
	if !o.yes && !nodesetup.Confirm(o.in, a.stdout, msgApplyPrompt) {
		fprintln(a.stdout, msgNothingChanged)
		return nodeResult{}, exitFail
	}

	secret, applyErr := nodesetup.Apply(ctx, env, plan, sa, nodeName, opts)
	var selfErr error
	if applyErr == nil {
		selfErr = nodesetup.SelfTest(ctx, env, sa, nodeName)
	}
	var rep nodeproto.SetupReply
	var repErr error
	if secret != "" || (applyErr == nil && selfErr == nil) {
		rep, repErr = nodesetup.BuildReply(env, sa, nodeName, version, secret)
	}
	if applyErr != nil || selfErr != nil || repErr != nil {
		if secret != "" {
			a.handOverSecret(rep, repErr, secret)
		}
		switch {
		case applyErr != nil:
			return nodeResult{}, a.stepFailed(sub, "apply", applyErr)
		case selfErr != nil:
			return nodeResult{}, a.stepFailed(sub, "self-test", selfErr)
		}
		return nodeResult{}, a.stepFailed(sub, "reply", repErr)
	}
	return nodeResult{reply: rep}, exitOK
}

// handOverSecret prints a token secret created by a run that then failed:
// in a reply block when one could be built, else on its own, clearly
// labelled and outside any reply markers, so no partial reply can be pasted.
func (a *app) handOverSecret(rep nodeproto.SetupReply, repErr error, secret string) {
	if repErr == nil {
		_ = a.printBlock("setup", rep, secret)
		return
	}
	fprintf(a.stdout, "\nThe API token %s was created but the reply could not be built: %v.\n"+
		"Its secret, shown once; put it in the PBP host's token file by hand:\n%s\n",
		nodesetup.TokenID, repErr, secret)
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
