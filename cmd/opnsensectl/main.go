// Command opnsensectl operates the OPNsense guest: the in-VM daemon, the
// Proxmox-host bridge and drainer, config.xml and file transfer over the
// daemon's RPC, and the upgrade orchestration.
package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"goodkind.io/opnsensectl/internal/version"
)

func main() {
	// When invoked via the in-VM symlink (mwan-opnsense or
	// mwan-opnsense.<sha>), the binary fast-paths directly into the
	// daemon serve loop so rc.d can keep its existing ExecStart.
	if invokedAsOPNsenseDaemon(os.Args[0]) {
		os.Exit(runOPNsenseDaemonServe(os.Args[1:]))
	}
	if len(os.Args) < 2 {
		opnsenseUsage(os.Stderr)
		os.Exit(2)
	}

	// Boundary log: every invocation is recorded with build identity and the
	// chosen verb, before verb-specific logger setup.
	slog.Info("opnsensectl boundary",
		"build", version.BuildVersionString(),
		"subcommand", os.Args[1])

	os.Exit(runOPNsense(os.Args[1:]))
}

func invokedAsOPNsenseDaemon(argv0 string) bool {
	binaryName := filepath.Base(argv0)
	return binaryName == "mwan-opnsense" || strings.HasPrefix(binaryName, "mwan-opnsense.")
}
