package nodesetup

import (
	"os"
	"strings"
	"testing"
)

// TestDocsMatchState keeps README "Node setup" and the planner's desired
// state identical. README shows the sudoers lines indented inside a list
// item's code block, so leading indentation is stripped before comparing.
func TestDocsMatchState(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(b)
	lines := strings.Split(readme, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimLeft(l, " \t")
	}
	dedented := strings.Join(lines, "\n")

	if !strings.Contains(dedented, SudoersText) {
		t.Errorf("README does not contain the sudoers text:\n%s", SudoersText)
	}
	if prefix := `restrict,command="sudo /usr/bin/pbp node"`; !strings.Contains(readme, prefix) {
		t.Errorf("README does not contain %s", prefix)
	}
	if !strings.HasPrefix(AuthorizedKeysLine("K"), `restrict,command="sudo /usr/bin/pbp node" `) {
		t.Errorf("AuthorizedKeysLine prefix = %q", AuthorizedKeysLine("K"))
	}
	if privs := strings.Join(RolePrivs, ","); !strings.Contains(readme, privs) {
		t.Errorf("README does not contain the privilege list %s", privs)
	}
	for _, s := range []string{"install -d -m 0750 -o root -g pbp /etc/pbp", "chmod 0640 /etc/pbp/node.yaml",
		"install -d -m 0755 -o root -g root /var/lib/pbp/.ssh", "chmod 0644 /var/lib/pbp/.ssh/authorized_keys",
		"install -m 0440 -o root -g root", "usermod -s /bin/sh pbp", "--privsep 1"} {
		if !strings.Contains(readme, s) {
			t.Errorf("README does not contain %q", s)
		}
	}
}
