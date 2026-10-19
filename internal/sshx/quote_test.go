package sshx

import "testing"

func TestQuote(t *testing.T) {
	got := Quote("pct", "exec", "101", "--", "sh", "-c", "echo 'hi'")
	want := `'pct' 'exec' '101' '--' 'sh' '-c' 'echo '\''hi'\'''`
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	if Quote() != "" {
		t.Fatal("empty args should give empty string")
	}
}
