package nodesetup

import (
	"os"
	"strings"
	"testing"
)

// TestDocsMatchState keeps docs/manual-setup.md "Node setup" (and the
// bridge stanza in docs/setup.md step 2) identical to the planner's desired
// state. The manual steps show the sudoers lines indented inside a list
// item's code block, so leading indentation is stripped before comparing.
func TestDocsMatchState(t *testing.T) {
	read := func(p string) string {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	manual := read("../../docs/manual-setup.md")
	setup := read("../../docs/setup.md")
	lines := strings.Split(manual, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimLeft(l, " \t")
	}
	dedented := strings.Join(lines, "\n")

	if !strings.Contains(dedented, SudoersText) {
		t.Errorf("docs/manual-setup.md does not contain the sudoers text:\n%s", SudoersText)
	}
	if prefix := `restrict,command="sudo /usr/bin/pbp node"`; !strings.Contains(manual, prefix) {
		t.Errorf("docs/manual-setup.md does not contain %s", prefix)
	}
	if !strings.HasPrefix(AuthorizedKeysLine("K"), `restrict,command="sudo /usr/bin/pbp node" `) {
		t.Errorf("AuthorizedKeysLine prefix = %q", AuthorizedKeysLine("K"))
	}
	if !strings.Contains(setup, BridgeStanza("vmbr99")) {
		t.Errorf("docs/setup.md does not contain the bridge stanza:\n%s", BridgeStanza("vmbr99"))
	}
	if privs := strings.Join(RolePrivs, ","); !strings.Contains(manual, privs) {
		t.Errorf("docs/manual-setup.md does not contain the privilege list %s", privs)
	}
	for _, s := range []string{"install -d -m 0750 -o root -g pbp /etc/pbp", "chmod 0640 /etc/pbp/node.yaml",
		"install -d -m 0755 -o root -g root /var/lib/pbp/.ssh", "chmod 0644 /var/lib/pbp/.ssh/authorized_keys",
		"install -m 0440 -o root -g root", "usermod -s /bin/sh pbp", "--privsep 1"} {
		if !strings.Contains(manual, s) {
			t.Errorf("docs/manual-setup.md does not contain %q", s)
		}
	}
}

// postrm removes the sudo rule on purge only when it is exactly setup's: its
// shell copy must match SudoersText (code review 3.8).
func TestPostrmKnowsTheSudoRule(t *testing.T) {
	b, err := os.ReadFile("../../packaging/scripts/postrm")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSuffix(SudoersText, "\n"), "\n") {
		if !strings.Contains(string(b), "'"+line+"'") {
			t.Errorf("postrm does not have the sudoers line %q", line)
		}
	}
}
