package probe

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
)

// globMatch reports whether name matches pattern; a malformed pattern never matches.
func globMatch(pattern, name string) bool {
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}

func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if globMatch(p, name) {
			return true
		}
	}
	return false
}

func guestMatches(m config.GuestMatch, g GuestInfo) bool {
	if m.VMID != 0 && m.VMID == g.VMID {
		return true
	}
	if m.Name != "" && m.Name == g.Name {
		return true
	}
	if m.Tag != "" {
		for _, t := range g.Tags {
			if t == m.Tag {
				return true
			}
		}
	}
	return false
}

// checker runs check sets through an Executor and accumulates results. A
// command that times out fails its own check and the others still run;
// any other Exec error aborts the checks.
type checker struct {
	ex       Executor
	results  []model.CheckResult
	ports    map[int]bool
	portsOK  bool
	portsErr error // the listing timed out: port checks fail
}

func (c *checker) record(name string, ok bool, detail string) {
	c.results = append(c.results, model.CheckResult{Name: name, OK: ok, Detail: detail})
}

func (c *checker) service(ctx context.Context, prefix, unit string) error {
	name := prefix + ":services:" + unit
	r, err := c.ex.Exec(ctx, []string{"systemctl", "is-active", unit})
	if errors.Is(err, errCommandTimeout) {
		c.record(name, false, timedOutDetail)
		return nil
	}
	if err != nil {
		return err
	}
	state := strings.TrimSpace(r.Stdout)
	c.record(name, state == "active", state)
	return nil
}

func (c *checker) listening(ctx context.Context) (map[int]bool, error) {
	if c.portsOK {
		return c.ports, c.portsErr
	}
	r, err := c.ex.Exec(ctx, []string{"ss", "-ltnH"})
	if errors.Is(err, errCommandTimeout) {
		c.portsOK, c.portsErr = true, err
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	if r.ExitCode == 0 {
		c.ports = parseListeningPorts(r.Stdout)
		c.portsOK = true
		return c.ports, nil
	}
	// No ss (or it failed): the kernel's own tables (code review I8). cat
	// exits non-zero when tcp6 is absent but still prints tcp.
	r, err = c.ex.Exec(ctx, []string{"cat", "/proc/net/tcp", "/proc/net/tcp6"})
	if errors.Is(err, errCommandTimeout) {
		c.portsOK, c.portsErr = true, err
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(r.Stdout) == "" {
		c.portsOK, c.portsErr = true, errNoPortSource
		return nil, errNoPortSource
	}
	c.ports = parseProcNetTCP(r.Stdout)
	c.portsOK = true
	return c.ports, nil
}

// errNoPortSource: neither ss nor /proc/net/tcp could be read, so port
// checks cannot be judged (ERROR, never FAILED).
var errNoPortSource = errors.New("cannot list listening ports: neither ss nor /proc/net/tcp is readable")

// parseProcNetTCP reads /proc/net/tcp and tcp6: a socket listens when its
// state (column 4) is 0A, and its port is the hex after the local address's
// ':' (column 2).
func parseProcNetTCP(out string) map[int]bool {
	ports := map[int]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[3] != "0A" {
			continue
		}
		i := strings.LastIndex(f[1], ":")
		if i < 0 {
			continue
		}
		if p, err := strconv.ParseUint(f[1][i+1:], 16, 16); err == nil {
			ports[int(p)] = true
		}
	}
	return ports
}

// parseListeningPorts reads `ss -ltnH` output; the local address is column 4
// and the port follows the last ':'.
func parseListeningPorts(out string) map[int]bool {
	ports := map[int]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		addr := f[3]
		i := strings.LastIndex(addr, ":")
		if i < 0 {
			continue
		}
		if p, err := strconv.Atoi(addr[i+1:]); err == nil {
			ports[p] = true
		}
	}
	return ports
}

func (c *checker) command(ctx context.Context, prefix, cmd string) error {
	name := prefix + ":commands:" + cmd
	r, err := c.ex.Exec(ctx, []string{"sh", "-c", cmd})
	if errors.Is(err, errCommandTimeout) {
		c.record(name, false, timedOutDetail)
		return nil
	}
	if err != nil {
		return err
	}
	detail := ""
	if r.ExitCode != 0 {
		detail = fmt.Sprintf("exit %d", r.ExitCode)
		if s := strings.TrimSpace(r.Stderr); s != "" {
			detail += ": " + s
		}
	}
	c.record(name, r.ExitCode == 0, detail)
	return nil
}

// run executes a CheckSet. anyPort selects profile semantics (one result,
// any listed port suffices) versus guest semantics (one result per port).
func (c *checker) run(ctx context.Context, prefix string, cs config.CheckSet, anyPort bool) error {
	for _, u := range cs.Services {
		if err := c.service(ctx, prefix, u); err != nil {
			return err
		}
	}
	if len(cs.Ports) > 0 {
		listening, err := c.listening(ctx)
		timedOut := errors.Is(err, errCommandTimeout)
		if err != nil && !timedOut {
			return err
		}
		if anyPort {
			strs := make([]string, len(cs.Ports))
			ok := false
			for i, p := range cs.Ports {
				strs[i] = strconv.Itoa(p)
				ok = ok || listening[p]
			}
			name := strings.Join(strs, ",")
			detail := ""
			switch {
			case timedOut:
				detail = timedOutDetail
			case !ok:
				detail = "none listening"
			}
			c.record(prefix+":ports:"+name, ok, detail)
		} else {
			for _, p := range cs.Ports {
				detail := ""
				switch {
				case timedOut:
					detail = timedOutDetail
				case !listening[p]:
					detail = "not listening"
				}
				c.record(prefix+":ports:"+strconv.Itoa(p), listening[p], detail)
			}
		}
	}
	for _, cmd := range cs.Commands {
		if err := c.command(ctx, prefix, cmd); err != nil {
			return err
		}
	}
	return nil
}

func emptySet(cs config.CheckSet) bool {
	return len(cs.Services) == 0 && len(cs.Ports) == 0 && len(cs.Commands) == 0
}

// runProfiles runs the CheckSet of every effective profile whose unit glob
// matches an enabled unit, then the matching per-guest checks.
func (c *checker) runProfiles(ctx context.Context, in Input, enabled []string) error {
	for _, p := range in.Checks.EffectiveProfiles() {
		var matched []string
		for _, u := range enabled {
			if globMatch(p.Unit, u) {
				matched = append(matched, u)
			}
		}
		if len(matched) == 0 {
			continue
		}
		if emptySet(p.CheckSet) {
			for _, u := range matched {
				if err := c.service(ctx, p.Name, u); err != nil {
					return err
				}
			}
			continue
		}
		if err := c.run(ctx, p.Name, p.CheckSet, true); err != nil {
			return err
		}
	}
	for _, g := range in.Checks.Guests {
		if guestMatches(g.Match, in.Guest) {
			if err := c.run(ctx, "guest", g.CheckSet, false); err != nil {
				return err
			}
		}
	}
	return nil
}
