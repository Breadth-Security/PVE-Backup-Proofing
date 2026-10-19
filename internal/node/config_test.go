package node

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "node.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigValid(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, "vmid_range: [990000, 990009]\nbridge: vmbr99\nrequest_timeout: 20m\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{VMIDRange: [2]int{990000, 990009}, Bridge: "vmbr99", RequestTimeout: 20 * time.Minute}
	if c != want {
		t.Errorf("got %+v, want %+v", c, want)
	}
}

func TestLoadConfigDefaultTimeout(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, "vmid_range: [990000, 990009]\nbridge: vmbr99\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.RequestTimeout != 15*time.Minute {
		t.Errorf("default timeout = %v, want 15m", c.RequestTimeout)
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestLoadConfigInvalid(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"empty", "", "empty"},
		{"range lo", "vmid_range: [99, 990009]\nbridge: vmbr99\n", "vmid_range"},
		{"range reversed", "vmid_range: [990009, 990000]\nbridge: vmbr99\n", "vmid_range"},
		{"range too high", "vmid_range: [990000, 1000000000]\nbridge: vmbr99\n", "vmid_range"},
		{"range one element", "vmid_range: [990000]\nbridge: vmbr99\n", "vmid_range"},
		{"range three elements", "vmid_range: [990000, 990001, 990002]\nbridge: vmbr99\n", "vmid_range"},
		{"range missing", "bridge: vmbr99\n", "vmid_range"},
		{"empty bridge", "vmid_range: [990000, 990009]\nbridge: \"\"\n", "bridge"},
		{"missing bridge", "vmid_range: [990000, 990009]\n", "bridge"},
		{"bridge with slash", "vmid_range: [990000, 990009]\nbridge: ../all\n", "bridge"},
		{"bridge leading dash", "vmid_range: [990000, 990009]\nbridge: -vmbr99\n", "bridge"},
		{"bridge too long", "vmid_range: [990000, 990009]\nbridge: vmbr0123456789ab\n", "bridge"},
		{"timeout too short", "vmid_range: [990000, 990009]\nbridge: vmbr99\nrequest_timeout: 5s\n", "request_timeout"},
		{"timeout unitless", "vmid_range: [990000, 990009]\nbridge: vmbr99\nrequest_timeout: 900\n", "request_timeout"},
		{"timeout garbage", "vmid_range: [990000, 990009]\nbridge: vmbr99\nrequest_timeout: soon\n", "request_timeout"},
		{"unknown field", "vmid_range: [990000, 990009]\nbridge: vmbr99\ntag: other\n", "tag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(writeConfig(t, tc.body))
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}
