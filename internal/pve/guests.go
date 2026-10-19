package pve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
)

// GuestRef identifies a guest on the cluster.
type GuestRef struct {
	Key        model.GuestKey
	Node, Name string
	Status     string
	Tags       []string
}

// OSInfo is the guest OS as reported by the QEMU guest agent.
type OSInfo struct{ ID, Name string }

// AgentExecStatus is the state of a command started with AgentExec.
type AgentExecStatus struct {
	Exited           bool
	ExitCode         int
	OutData, ErrData string
}

func guestPath(node string, kind model.GuestKind, vmid int) string {
	return "/nodes/" + url.PathEscape(node) + "/" + url.PathEscape(string(kind)) + "/" + strconv.Itoa(vmid)
}

// Guests lists every qemu and lxc guest in the cluster.
func (c *Client) Guests(ctx context.Context) ([]GuestRef, error) {
	var entries []struct {
		Type   string  `json:"type"`
		VMID   flexInt `json:"vmid"`
		Node   string  `json:"node"`
		Name   string  `json:"name"`
		Status string  `json:"status"`
		Tags   string  `json:"tags"`
	}
	if err := c.do(ctx, http.MethodGet, "/cluster/resources", url.Values{"type": {"vm"}}, nil, &entries); err != nil {
		return nil, err
	}
	out := make([]GuestRef, 0, len(entries))
	for _, e := range entries {
		kind := model.GuestKind(e.Type)
		if kind != model.KindQEMU && kind != model.KindLXC {
			continue
		}
		var tags []string
		for _, t := range strings.Split(e.Tags, ";") {
			if t = strings.TrimSpace(t); t != "" {
				tags = append(tags, t)
			}
		}
		out = append(out, GuestRef{Key: model.GuestKey{Kind: kind, VMID: int(e.VMID)}, Node: e.Node, Name: e.Name, Status: e.Status, Tags: tags})
	}
	return out, nil
}

// Restore starts restoring a backup into a new guest and returns the task UPID.
func (c *Client) Restore(ctx context.Context, node string, kind model.GuestKind, vmid int, volid, storage string) (string, error) {
	form := url.Values{"vmid": {strconv.Itoa(vmid)}, "storage": {storage}, "unique": {"1"}}
	if kind == model.KindLXC {
		form.Set("ostemplate", volid)
		form.Set("restore", "1")
	} else {
		form.Set("archive", volid)
	}
	return c.upid(ctx, http.MethodPost, "/nodes/"+url.PathEscape(node)+"/"+url.PathEscape(string(kind)), nil, form)
}

func (c *Client) upid(ctx context.Context, method, path string, query, form url.Values) (string, error) {
	var upid string
	if err := c.do(ctx, method, path, query, form, &upid); err != nil {
		return "", err
	}
	return upid, nil
}

// GuestConfig returns the current config with every value stringified. The
// digest is dropped; the raw "lxc" array becomes "key: value" lines.
func (c *Client) GuestConfig(ctx context.Context, node string, kind model.GuestKind, vmid int) (map[string]string, error) {
	var raw map[string]json.RawMessage
	if err := c.do(ctx, http.MethodGet, guestPath(node, kind, vmid)+"/config", nil, nil, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if k == "digest" {
			continue
		}
		v = bytes.TrimSpace(v)
		switch {
		case len(v) == 0 || string(v) == "null":
			continue
		case v[0] == '"':
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return nil, fmt.Errorf("pve api config key %s: %w", k, err)
			}
			out[k] = s
		case v[0] == '[':
			var pairs [][]string
			if err := json.Unmarshal(v, &pairs); err != nil {
				return nil, fmt.Errorf("pve api config key %s: %w", k, err)
			}
			lines := make([]string, 0, len(pairs))
			for _, p := range pairs {
				if len(p) != 2 {
					return nil, fmt.Errorf("pve api config key %s: entry %q is not a [key, value] pair", k, p)
				}
				lines = append(lines, p[0]+": "+p[1])
			}
			out[k] = strings.Join(lines, "\n")
		default:
			out[k] = string(v)
		}
	}
	return out, nil
}

// SetGuestConfig sets and/or deletes config keys with the synchronous PUT,
// so a refused change is returned as an error. (qemu's POST variant runs as
// a task and returns before the change is applied or refused.)
func (c *Client) SetGuestConfig(ctx context.Context, node string, kind model.GuestKind, vmid int, set map[string]string, del []string) error {
	form := url.Values{}
	for k, v := range set {
		form.Set(k, v)
	}
	if len(del) > 0 {
		form.Set("delete", strings.Join(del, ","))
	}
	return c.do(ctx, http.MethodPut, guestPath(node, kind, vmid)+"/config", nil, form, nil)
}

// OriginalConfig returns the raw guest config stored inside a backup.
func (c *Client) OriginalConfig(ctx context.Context, node, volid string) (string, error) {
	var s string
	err := c.do(ctx, http.MethodGet, "/nodes/"+url.PathEscape(node)+"/vzdump/extractconfig", url.Values{"volume": {volid}}, nil, &s)
	return s, err
}

func (c *Client) Start(ctx context.Context, node string, kind model.GuestKind, vmid int) (string, error) {
	return c.upid(ctx, http.MethodPost, guestPath(node, kind, vmid)+"/status/start", nil, url.Values{})
}

func (c *Client) Shutdown(ctx context.Context, node string, kind model.GuestKind, vmid int) (string, error) {
	return c.upid(ctx, http.MethodPost, guestPath(node, kind, vmid)+"/status/shutdown", nil, url.Values{"timeout": {"60"}})
}

func (c *Client) Stop(ctx context.Context, node string, kind model.GuestKind, vmid int) (string, error) {
	return c.upid(ctx, http.MethodPost, guestPath(node, kind, vmid)+"/status/stop", nil, url.Values{})
}

// Destroy removes a guest along with its disks.
func (c *Client) Destroy(ctx context.Context, node string, kind model.GuestKind, vmid int) (string, error) {
	q := url.Values{"purge": {"1"}, "destroy-unreferenced-disks": {"1"}}
	return c.upid(ctx, http.MethodDelete, guestPath(node, kind, vmid), q, nil)
}

// GuestStatus returns the current power status, e.g. "running" or "stopped".
func (c *Client) GuestStatus(ctx context.Context, node string, kind model.GuestKind, vmid int) (string, error) {
	var r struct {
		Status string `json:"status"`
	}
	if err := c.do(ctx, http.MethodGet, guestPath(node, kind, vmid)+"/status/current", nil, nil, &r); err != nil {
		return "", err
	}
	return r.Status, nil
}

func (c *Client) AgentPing(ctx context.Context, node string, vmid int) error {
	return c.do(ctx, http.MethodPost, guestPath(node, model.KindQEMU, vmid)+"/agent/ping", nil, url.Values{}, nil)
}

func (c *Client) AgentOSInfo(ctx context.Context, node string, vmid int) (OSInfo, error) {
	var r struct {
		Result struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"result"`
	}
	if err := c.do(ctx, http.MethodGet, guestPath(node, model.KindQEMU, vmid)+"/agent/get-osinfo", nil, nil, &r); err != nil {
		return OSInfo{}, err
	}
	return OSInfo{ID: r.Result.ID, Name: r.Result.Name}, nil
}

// AgentExec starts a command in the guest and returns its pid.
func (c *Client) AgentExec(ctx context.Context, node string, vmid int, argv []string) (int, error) {
	var r struct {
		PID flexInt `json:"pid"`
	}
	form := url.Values{"command": argv}
	if err := c.do(ctx, http.MethodPost, guestPath(node, model.KindQEMU, vmid)+"/agent/exec", nil, form, &r); err != nil {
		return 0, err
	}
	return int(r.PID), nil
}

func (c *Client) AgentExecStatus(ctx context.Context, node string, vmid, pid int) (AgentExecStatus, error) {
	var r struct {
		Exited   flexBool `json:"exited"`
		ExitCode flexInt  `json:"exitcode"`
		OutData  string   `json:"out-data"`
		ErrData  string   `json:"err-data"`
	}
	q := url.Values{"pid": {strconv.Itoa(pid)}}
	if err := c.do(ctx, http.MethodGet, guestPath(node, model.KindQEMU, vmid)+"/agent/exec-status", q, nil, &r); err != nil {
		return AgentExecStatus{}, err
	}
	return AgentExecStatus{Exited: bool(r.Exited), ExitCode: int(r.ExitCode), OutData: r.OutData, ErrData: r.ErrData}, nil
}
