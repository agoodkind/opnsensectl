package cli

import (
	"fmt"
	"os"
)

// Usage prints the top-level command help.
//
// The output lists service configuration requirements and supported verbs.
//
// For example:
//
//	Usage(os.Stdout)
func Usage(out *os.File) {
	fmt.Fprintln(out, "usage: opnsensectl <verb> [args...]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "With no verb, opnsensectl prints this help and does nothing else.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Long-running services, each started only with an explicit config file:")
	fmt.Fprintln(out, "  daemon serve --config PATH  the in-VM daemon on the OPNsense guest")
	fmt.Fprintln(out, "  host serve --config PATH    the host bridge on the Proxmox host")
	fmt.Fprintln(out, "  host drain --config PATH    the chardev drainer on the Proxmox host")
	fmt.Fprintln(out, "Run `opnsensectl <service> --help` for the environment each one requires.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Top-level verbs:")
	fmt.Fprintln(out, "  version                   print mwan-opnsense build identity")
	fmt.Fprintln(
		out,
		"  exec CMD [ARGS...]        run a command on the OPNsense guest via daemon Exec RPC",
	)
	fmt.Fprintln(out, "  selftest                  run the selftest payload against the daemon")
	fmt.Fprintln(
		out,
		"  daemon <verb>             in-VM daemon controls (serve|is-enabled|version|state|push|stage|restart|revert|gc)",
	)
	fmt.Fprintln(out, "  host <verb>               host-side services (serve|drain)")
	fmt.Fprintln(
		out,
		"  config <verb>             config.xml operations "+
			"(read|write|backup|import|xpath ...|strip-gateway-v6|inject-gateway-v6)",
	)
	fmt.Fprintln(
		out,
		"  file <verb>               file transfer to/from the OPNsense guest (push|pull)",
	)
	fmt.Fprintln(
		out,
		"  upgrade <phase>           upgrade orchestration (prepare|execute|validate|rollback|commit|run|reset)",
	)
	fmt.Fprintln(
		out,
		"  install                   write this platform's service files (rc.d on FreeBSD, systemd units on linux)",
	)
}
