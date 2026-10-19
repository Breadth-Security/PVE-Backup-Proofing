// Command pbp is the Proxmox Backup Proofing daemon and CLI.
package main

import (
	"fmt"
	"io"
	"os"
)

const usage = "usage: pbp daemon|run [--guest VMID]|check|health [--config PATH]\n"

func main() { os.Exit(run(os.Args[1:], os.Stderr)) }

func run(args []string, errw io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(errw, usage)
		return 2
	}
	switch args[0] {
	case "daemon", "run", "check", "health":
		fmt.Fprintf(errw, "pbp %s: not implemented\n", args[0])
		return 1
	}
	fmt.Fprintf(errw, "pbp: unknown subcommand %q\n%s", args[0], usage)
	return 2
}
