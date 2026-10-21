package probe

import (
	"context"
	"slices"
	"strings"
)

// namespaceExit is systemd's exit status when it cannot set up a unit's
// sandbox (226/NAMESPACE).
const namespaceExit = "226"

// apiMounts are systemd's own mounts that an unprivileged container without
// nesting cannot set up.
var apiMounts = map[string]bool{
	"dev-mqueue.mount": true, "dev-hugepages.mount": true, "tmp.mount": true, "run-lock.mount": true,
	"sys-kernel-config.mount": true, "sys-kernel-debug.mount": true, "sys-kernel-tracing.mount": true,
	"sys-fs-fuse-connections.mount": true, "proc-sys-fs-binfmt_misc.mount": true,
}

// nestingFailures splits failed units into those that fail only because an
// unprivileged container without nesting cannot sandbox them, and the rest
// (issue #34). A nesting failure is a service that exited 226/NAMESPACE,
// one of systemd's API mounts, or a socket whose service is a nesting
// failure. A unit whose state cannot be read counts as a real failure.
func nestingFailures(ctx context.Context, ex Executor, failed []string) (nesting, other []string) {
	result := map[string]map[string]string{}
	for _, u := range failed {
		result[u] = showResult(ctx, ex, u)
	}
	isNesting := map[string]bool{}
	for _, u := range failed {
		r := result[u]
		isNesting[u] = apiMounts[u] ||
			strings.HasSuffix(u, ".service") && r["Result"] == "exit-code" && r["ExecMainStatus"] == namespaceExit
	}
	for _, u := range failed {
		if svc, ok := strings.CutSuffix(u, ".socket"); ok {
			// The service the socket starts: its Triggers property, which
			// may name a service other than the socket's own name (code
			// review 5.13), or else the same-named service.
			triggers := strings.Fields(result[u]["Triggers"])
			if len(triggers) == 0 {
				triggers = []string{svc + ".service"}
			}
			isNesting[u] = result[u]["Result"] == "service-start-limit-hit" && slices.ContainsFunc(triggers, func(s string) bool { return isNesting[s] })
		}
		if isNesting[u] {
			nesting = append(nesting, u)
		} else {
			other = append(other, u)
		}
	}
	return nesting, other
}

// showResult reads a unit's Result, ExecMainStatus and Triggers; nil if it
// cannot.
func showResult(ctx context.Context, ex Executor, unit string) map[string]string {
	res, err := ex.Exec(ctx, []string{"systemctl", "show", "-p", "Result", "-p", "ExecMainStatus", "-p", "Triggers", "--", unit})
	if err != nil || res.ExitCode != 0 {
		return nil
	}
	props := map[string]string{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), "="); ok {
			props[k] = strings.TrimSpace(v)
		}
	}
	return props
}
