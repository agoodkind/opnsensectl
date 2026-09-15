package main

import (
	"fmt"
	"os"
)

// opnsenseVerb is the typed enum of top-level `mwan opnsense <verb>`
// subcommands. Every verb is a positional token; no flags are accepted
// at the top level, only inside the leaf verb when it owns inherent
// arguments (binary path, xpath expression, etc).
type opnsenseVerb string

const (
	opnsenseVerbVersion  opnsenseVerb = "version"
	opnsenseVerbExec     opnsenseVerb = "exec"
	opnsenseVerbSelftest opnsenseVerb = "selftest"
	opnsenseVerbDaemon   opnsenseVerb = "daemon"
	opnsenseVerbHost     opnsenseVerb = "host"
	opnsenseVerbConfig   opnsenseVerb = "config"
	opnsenseVerbFile     opnsenseVerb = "file"
	opnsenseVerbUpgrade  opnsenseVerb = "upgrade"
	opnsenseVerbHelpH    opnsenseVerb = "-h"
	opnsenseVerbHelpL    opnsenseVerb = "--help"
	opnsenseVerbHelp     opnsenseVerb = "help"
)

func opnsenseUsage(out *os.File) {
	fmt.Fprintln(out, "usage: mwan opnsense <verb> [args...]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Top-level verbs:")
	fmt.Fprintln(out, "  version                   print mwan-opnsense build identity")
	fmt.Fprintln(out, "  exec CMD [ARGS...]        run a command on the OPNsense guest via daemon Exec RPC")
	fmt.Fprintln(out, "  selftest                  run the selftest payload against the daemon")
	fmt.Fprintln(out, "  daemon <verb>             in-VM daemon controls (serve|is-enabled|version|state|push|stage|restart|revert|gc)")
	fmt.Fprintln(out, "  host <verb>               host-side bridge controls (serve)")
	fmt.Fprintln(out, "  config <verb>             config.xml operations (read|write|backup|import|xpath ...|strip-gateway-v6|inject-gateway-v6)")
	fmt.Fprintln(out, "  file <verb>               file transfer to/from the OPNsense guest (push|pull)")
	fmt.Fprintln(out, "  upgrade <phase>           upgrade orchestration (prepare|execute|validate|rollback|commit|run|reset)")
}

// runOPNsense is the entry point for `mwan opnsense ...`. It dispatches
// on the first positional verb and hands the remaining args to the
// leaf-specific runner. Returns a process exit code.
func runOPNsense(args []string) int {
	if len(args) < 1 {
		opnsenseUsage(os.Stderr)
		return 2
	}
	verb := opnsenseVerb(args[0])
	rest := args[1:]

	switch verb {
	case opnsenseVerbHelpH, opnsenseVerbHelpL, opnsenseVerbHelp:
		opnsenseUsage(os.Stdout)
		return 0
	case opnsenseVerbVersion:
		return runOPNsenseVersion(rest)
	case opnsenseVerbExec:
		return runOPNsenseExec(rest)
	case opnsenseVerbSelftest:
		return runOPNsenseSelftest(rest)
	case opnsenseVerbDaemon:
		return runOPNsenseDaemon(rest)
	case opnsenseVerbHost:
		return runOPNsenseHost(rest)
	case opnsenseVerbConfig:
		return runOPNsenseConfig(rest)
	case opnsenseVerbFile:
		return runOPNsenseFile(rest)
	case opnsenseVerbUpgrade:
		return runOPNsenseUpgradeCmd(rest)
	default:
		fmt.Fprintf(os.Stderr, "mwan opnsense: unknown verb %q\n", string(verb))
		opnsenseUsage(os.Stderr)
		return 2
	}
}
