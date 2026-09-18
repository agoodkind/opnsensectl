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
	// Under a router daemon name (mwan-opnsense or mwan-opnsense.<suffix>),
	// the binary runs the daemon serve loop, because the run shim that rc.d
	// supervises execs /usr/local/sbin/mwan-opnsense with no arguments. A
	// first argument of exactly install is the one exception: it falls
	// through to the normal install verb, because the router has the binary
	// only under the daemon names, so the deploy runs `mwan-opnsense install`
	// to write the startup files. No startup path passes install.
	if invokedAsOPNsenseDaemon(os.Args[0]) && !requestsInstall(os.Args[1:]) {
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

func requestsInstall(args []string) bool {
	return len(args) > 0 && opnsenseVerb(args[0]) == opnsenseVerbInstall
}
