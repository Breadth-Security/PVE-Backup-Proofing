package hostsetup

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/node"
)

// Answers are the values setup asks for (spec §4.2).
type Answers struct {
	Address        string // node SSH/API address: hostname or IP, no scheme or port
	VMIDRange      [2]int
	Bridge         string
	RestoreStorage string
	PBSStorages    []string
	Schedule       string
	TZ             string // Docker only; "" for the deb layout
}

// Defaults offered in brackets.
const (
	defaultRange    = "990000-990049"
	defaultBridge   = "vmbr99"
	defaultRestore  = "local-zfs"
	defaultSchedule = "0 2 * * *"
)

var (
	reStorage  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]*$`)
	reHostname = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*\.?$`)
)

// Ask asks the setup questions on out, reading answers from in. Enter takes
// the default in brackets; an invalid answer is re-asked with the rule's
// message. The time zone is asked only for the Docker layout, offering
// defaultTZ. End of input is an error.
func Ask(in io.Reader, out io.Writer, l Layout, defaultTZ string) (Answers, error) {
	q := asker{r: bufio.NewReader(in), w: out}
	var a Answers
	steps := []func() error{
		func() error {
			return q.ask("Node address (hostname or IP, as the host reaches its SSH and API)", "", func(s string) error {
				if err := validAddress(s); err != nil {
					return err
				}
				a.Address = s
				return nil
			})
		},
		func() error {
			return q.ask("VMID range for proof guests (lo-hi)", defaultRange, func(s string) (err error) {
				a.VMIDRange, err = parseRange(s)
				return err
			})
		},
		func() error {
			return q.ask("Isolated bridge on the node", defaultBridge, func(s string) error {
				if !node.ValidBridge(s) {
					return errors.New("not a valid bridge name (up to 15 letters, digits, '_', '.', '-'; not starting with '.' or '-')")
				}
				a.Bridge = s
				return nil
			})
		},
		func() error {
			return q.ask("Restore storage on the node", defaultRestore, func(s string) error {
				if err := validStorage(s); err != nil {
					return err
				}
				a.RestoreStorage = s
				return nil
			})
		},
		func() error {
			return q.ask("PBS storage(s) to prove, comma-separated", "", func(s string) (err error) {
				a.PBSStorages, err = parseStorages(s)
				return err
			})
		},
		func() error {
			return q.ask("Schedule (cron)", defaultSchedule, func(s string) error {
				s = strings.Trim(s, `"'`)
				if _, err := cron.ParseStandard(s); err != nil {
					return fmt.Errorf("invalid cron expression %q: %v", s, err)
				}
				a.Schedule = s
				return nil
			})
		},
	}
	if !l.Deb {
		steps = append(steps, func() error {
			return q.ask("Time zone for the schedule", defaultTZ, func(s string) error {
				if s == "Local" {
					return errors.New(`unknown time zone "Local": name a zone such as Europe/London or UTC`)
				}
				if _, err := time.LoadLocation(s); err != nil {
					return fmt.Errorf("unknown time zone %q: name a zone such as Europe/London or UTC", s)
				}
				a.TZ = s
				return nil
			})
		})
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return Answers{}, err
		}
	}
	return a, nil
}

type asker struct {
	r *bufio.Reader
	w io.Writer
}

// ask prompts until check accepts the answer (or the default on Enter).
func (q asker) ask(prompt, def string, check func(string) error) error {
	for {
		if def != "" {
			_, _ = fmt.Fprintf(q.w, "%s [%s]: ", prompt, def)
		} else {
			_, _ = fmt.Fprintf(q.w, "%s: ", prompt)
		}
		line, err := q.r.ReadString('\n')
		if err != nil && (!errors.Is(err, io.EOF) || line == "") {
			if errors.Is(err, io.EOF) {
				_, _ = fmt.Fprintln(q.w)
				return fmt.Errorf("no answer to %q: end of input", prompt)
			}
			return fmt.Errorf("read answer: %w", err)
		}
		s := strings.TrimSpace(line)
		if s == "" {
			s = def
		}
		if s == "" {
			_, _ = fmt.Fprintln(q.w, "  an answer is required")
			continue
		}
		if err := check(s); err != nil {
			_, _ = fmt.Fprintf(q.w, "  %v\n", err)
			continue
		}
		return nil
	}
}

// validAddress accepts a hostname or an IP address, nothing else: no scheme,
// port, path or brackets.
func validAddress(s string) error {
	if ip := net.ParseIP(s); ip != nil {
		return nil
	}
	if strings.Contains(s, "://") || strings.ContainsAny(s, ":/[]@ ") || len(s) > 253 || !reHostname.MatchString(s) {
		return errors.New("enter a hostname or IP address without a scheme, port or path (e.g. pve-node.example.lan)")
	}
	return nil
}

// parseRange parses "lo-hi" and applies config's vmid_range rule.
func parseRange(s string) ([2]int, error) {
	los, his, ok := strings.Cut(s, "-")
	lo, err1 := strconv.Atoi(strings.TrimSpace(los))
	hi, err2 := strconv.Atoi(strings.TrimSpace(his))
	if !ok || err1 != nil || err2 != nil {
		return [2]int{}, fmt.Errorf("enter the range as lo-hi, e.g. %s", defaultRange)
	}
	if err := config.ValidateVMIDRange(lo, hi); err != nil {
		return [2]int{}, err
	}
	return [2]int{lo, hi}, nil
}

func validStorage(s string) error {
	if !reStorage.MatchString(s) {
		return fmt.Errorf("%q is not a valid storage id (a letter, then letters, digits, '_', '.', '-')", s)
	}
	return nil
}

// parseStorages splits a comma-separated list, dropping repeats.
func parseStorages(s string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if err := validStorage(part); err != nil {
			return nil, err
		}
		if !slices.Contains(out, part) {
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("at least one storage is required")
	}
	return out, nil
}
