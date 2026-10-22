package pve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
)

// StorageStatus is the state of one storage on a node.
type StorageStatus struct {
	Active, Enabled bool
	Avail, Total    int64
	Content         string
}

// NodeMemory is a node's memory in bytes.
type NodeMemory struct{ Total, Available int64 }

// NetIface describes one network interface of a node; fields the interface
// does not have are empty.
type NetIface struct {
	Iface, Type, BridgePorts, OVSPorts, Address, CIDR, Address6, CIDR6 string
}

// StorageStatus returns the status of a storage on a node.
func (c *Client) StorageStatus(ctx context.Context, node, storage string) (StorageStatus, error) {
	var r struct {
		Active  flexBool `json:"active"`
		Enabled flexBool `json:"enabled"`
		Avail   flexInt  `json:"avail"`
		Total   flexInt  `json:"total"`
		Content string   `json:"content"`
	}
	path := "/nodes/" + url.PathEscape(node) + "/storage/" + url.PathEscape(storage) + "/status"
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &r); err != nil {
		return StorageStatus{}, err
	}
	return StorageStatus{Active: bool(r.Active), Enabled: bool(r.Enabled), Avail: int64(r.Avail), Total: int64(r.Total), Content: r.Content}, nil
}

// NodeMemory returns the node's total and available memory.
func (c *Client) NodeMemory(ctx context.Context, node string) (NodeMemory, error) {
	var r struct {
		Memory struct {
			Total     flexInt `json:"total"`
			Available flexInt `json:"available"`
		} `json:"memory"`
	}
	if err := c.do(ctx, http.MethodGet, "/nodes/"+url.PathEscape(node)+"/status", nil, nil, &r); err != nil {
		return NodeMemory{}, err
	}
	return NodeMemory{Total: int64(r.Memory.Total), Available: int64(r.Memory.Available)}, nil
}

// NetIface returns the named network interface of a node.
func (c *Client) NetIface(ctx context.Context, node, iface string) (NetIface, error) {
	var r struct {
		Iface       string `json:"iface"`
		Type        string `json:"type"`
		BridgePorts string `json:"bridge_ports"`
		OVSPorts    string `json:"ovs_ports"`
		Address     string `json:"address"`
		CIDR        string `json:"cidr"`
		Address6    string `json:"address6"`
		CIDR6       string `json:"cidr6"`
	}
	path := "/nodes/" + url.PathEscape(node) + "/network/" + url.PathEscape(iface)
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &r); err != nil {
		return NetIface{}, err
	}
	return NetIface(r), nil
}

// Nodes returns the names of the cluster's nodes.
func (c *Client) Nodes(ctx context.Context) ([]string, error) {
	var r []struct {
		Node string `json:"node"`
	}
	if err := c.do(ctx, http.MethodGet, "/nodes", nil, nil, &r); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(r))
	for _, n := range r {
		names = append(names, n.Node)
	}
	return names, nil
}

// Privileges returns the calling token's effective privileges on path,
// from GET /access/permissions?path=… ({"<path>": {"<priv>": 1, …}}).
func (c *Client) Privileges(ctx context.Context, path string) (map[string]bool, error) {
	var r map[string]map[string]json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/access/permissions", url.Values{"path": {path}}, nil, &r); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for p, privs := range r {
		if p != path {
			continue
		}
		// The value is PVE's propagate flag, not whether it is granted:
		// a key present means the privilege is held.
		for name := range privs {
			out[name] = true
		}
	}
	return out, nil
}
