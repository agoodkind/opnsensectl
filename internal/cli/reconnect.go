package cli

import (
	"fmt"
	"log/slog"

	"goodkind.io/opnsensectl/internal/opnsense"
	"goodkind.io/opnsensectl/internal/upgrade"
)

// Redial returns a function that reconnects the OPNsense RPC client.
//
// GRPCExecutor calls this function after rollback closes the client session.
//
// For example:
//
//	executor.Redial = Redial(target)
func Redial(target string) func() (upgrade.OPNsenseRPCClient, error) {
	return func() (upgrade.OPNsenseRPCClient, error) {
		client, err := opnsense.Dial(target)
		if err != nil {
			slog.Error("opnsense upgrade: redial", "err", err, "target", target)

			return nil, fmt.Errorf("dial %s: %w", target, err)
		}

		return client.RPC(), nil
	}
}
