package pve

import (
	"context"
	"net/http"
	"net/url"
)

type StorageStatus struct {
	Active, Enabled bool
	Avail, Total    int64
	Content         string
}

type NodeMemory struct{ Total, Available int64 }

type NetIface struct {
	Iface, Type, BridgePorts, OVSPorts, Address, CIDR, Address6, CIDR6 string
}

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
