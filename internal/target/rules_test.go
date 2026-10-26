package target

import (
	"slices"
	"testing"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/nodeproto"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/pve"
)

func TestForeignInRange(t *testing.T) {
	g := func(v int, tags ...string) pve.GuestRef {
		return pve.GuestRef{Key: model.GuestKey{Kind: model.KindQEMU, VMID: v}, Tags: tags}
	}
	guests := []pve.GuestRef{g(990001, OwnerTag), g(990002), g(105), g(990003, "other")}
	got := ForeignInRange(guests, 990000, 990009)
	want := []string{"qemu/990002", "qemu/990003"}
	if !slices.Equal(got, want) {
		t.Errorf("ForeignInRange = %v, want %v", got, want)
	}
}

func TestLiveIsolationProblem(t *testing.T) {
	const noAddrs = `[{"addr_info":[]}]`
	ok := func() nodeproto.BridgeState {
		return nodeproto.BridgeState{Name: "vmbr9", DisableIPv6: "1", Addrs: []byte(noAddrs), Members: []string{"tap990003i0"}}
	}
	tests := []struct {
		name       string
		mut        func(*nodeproto.BridgeState)
		apiOVS     bool
		problem    string
		unverified bool
	}{
		{name: "clean", mut: func(*nodeproto.BridgeState) {}},
		{name: "ipv6", mut: func(s *nodeproto.BridgeState) { s.DisableIPv6 = "0" },
			problem: `has IPv6 enabled (disable_ipv6 = "0"); it gets a link-local address the node can be reached on`},
		{name: "address", mut: func(s *nodeproto.BridgeState) {
			s.Addrs = []byte(`[{"addr_info":[{"local":"10.0.0.1","prefixlen":24}]}]`)
		}, problem: "has address 10.0.0.1/24 on the node"},
		{name: "bad addrs", mut: func(s *nodeproto.BridgeState) { s.Addrs = []byte(`{`) }, unverified: true,
			problem: "cannot be verified: parse ip addr output: unexpected end of JSON input"},
		{name: "foreign member", mut: func(s *nodeproto.BridgeState) { s.Members = []string{"tap990003i0", "tap105i0", "eth0"} },
			problem: "has members that are not proof-guest interfaces: tap105i0, eth0"},
		{name: "wrong bridge", mut: func(s *nodeproto.BridgeState) { s.Name = "vmbr0" }, unverified: true,
			problem: `cannot be verified: the node helper checks bridge "vmbr0" instead`},
		{name: "ovs mismatch", mut: func(s *nodeproto.BridgeState) { s.OVS = true }, unverified: true,
			problem: "cannot be verified: the API says OVS bridge = false, the node says OVS bridge = true"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := ok()
			tc.mut(&st)
			p, u := LiveIsolationProblem(st, "vmbr9", tc.apiOVS, 990000, 990009)
			if p != tc.problem || u != tc.unverified {
				t.Errorf("got (%q, %v), want (%q, %v)", p, u, tc.problem, tc.unverified)
			}
		})
	}
}
