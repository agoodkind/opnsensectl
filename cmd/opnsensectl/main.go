// Command opnsensectl operates the OPNsense guest: the in-VM daemon, the
// Proxmox-host bridge and drainer, config.xml and file transfer over the
// daemon's RPC, and the upgrade orchestration.
package main

import (
	"log/slog"
	"os"

	"goodkind.io/opnsensectl/internal/version"
)

func main() {
	// The binary acts only on an explicit command. Its file name selects
	// nothing, so the router's mwan-opnsense names behave like opnsensectl.
	if len(os.Args) < 2 {
		opnsenseUsage(os.Stdout)
		os.Exit(0)
	}

	// Boundary log: every invocation is recorded with build identity and the
	// chosen verb, before verb-specific logger setup.
	slog.Info("opnsensectl boundary",
		"build", version.BuildVersionString(),
		"subcommand", os.Args[1])

	os.Exit(runOPNsense(os.Args[1:]))
}
