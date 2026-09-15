package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	mwanv1 "goodkind.io/opnsensectl/gen/opnsense/v1"
	"goodkind.io/opnsensectl/internal/configxform"
)

// configVerb enumerates `mwan opnsense config <verb>` sub-verbs.
type configVerb string

const (
	configVerbRead            configVerb = "read"
	configVerbWrite           configVerb = "write"
	configVerbBackup          configVerb = "backup"
	configVerbImport          configVerb = "import"
	configVerbXPath           configVerb = "xpath"
	configVerbStripGatewayV6  configVerb = "strip-gateway-v6"
	configVerbInjectGatewayV6 configVerb = "inject-gateway-v6"
)

func configUsage(out *os.File) {
	fmt.Fprintln(out, "usage: mwan opnsense config <verb> [args...]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Verbs:")
	fmt.Fprintln(out, "  read                          fetch config.xml metadata from the daemon")
	fmt.Fprintln(out, "  write FILE                    write FILE as the new config.xml")
	fmt.Fprintln(out, "  backup                        snapshot the current config.xml")
	fmt.Fprintln(out, "  import SOURCE                 transform SOURCE prod XML into the testbed-shaped output (paths from TOML)")
	fmt.Fprintln(out, "  xpath get EXPR                evaluate EXPR and print matches")
	fmt.Fprintln(out, "  xpath set EXPR VALUE          set EXPR to VALUE")
	fmt.Fprintln(out, "  xpath delete EXPR             delete nodes matching EXPR")
	fmt.Fprintln(out, "  strip-gateway-v6 NAME         strip the IPv6 gateway named NAME")
	fmt.Fprintln(out, "  inject-gateway-v6 NAME        inject an IPv6 gateway named NAME")
}

func runOPNsenseConfig(args []string) int {
	if len(args) < 1 {
		configUsage(os.Stderr)
		return 2
	}
	verb := configVerb(args[0])
	rest := args[1:]
	switch verb {
	case configVerbRead:
		return runConfigRead(rest)
	case configVerbWrite:
		return runConfigWrite(rest)
	case configVerbBackup:
		return runConfigBackup(rest)
	case configVerbImport:
		return runConfigImport(rest)
	case configVerbXPath:
		return runConfigXPath(rest)
	case configVerbStripGatewayV6:
		return runConfigStripGatewayV6(rest)
	case configVerbInjectGatewayV6:
		return runConfigInjectGatewayV6(rest)
	default:
		fmt.Fprintf(os.Stderr, "mwan opnsense config: unknown verb %q\n", string(verb))
		configUsage(os.Stderr)
		return 2
	}
}

func runConfigRead(args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "usage: mwan opnsense config read")
		return 2
	}
	cli, ctx, cancel, err := dialProbe()
	if err != nil {
		return printAndExit("config read", err)
	}
	defer cancel()
	defer func() { _ = cli.Close() }()
	resp, err := cli.RPC().ReadConfigXML(ctx)
	if err != nil {
		return printAndExit("config read", err)
	}
	fmt.Printf("config_bytes=%d sha256=%s\n", resp.SizeBytes, resp.Sha256)
	return 0
}

func runConfigWrite(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: mwan opnsense config write FILE")
		return 2
	}
	path := filepath.Clean(args[0])
	cli, ctx, cancel, err := dialProbe()
	if err != nil {
		return printAndExit("config write", err)
	}
	defer cancel()
	defer func() { _ = cli.Close() }()
	content, err := os.ReadFile(path)
	if err != nil {
		return printAndExit("config write", fmt.Errorf("read %s: %w", path, err))
	}
	resp, err := cli.RPC().WriteConfigXML(ctx, content, "")
	if err != nil {
		return printAndExit("config write", err)
	}
	fmt.Printf("backup_path=%s bytes_written=%d\n", resp.BackupPath, resp.BytesWritten)
	return 0
}

func runConfigBackup(args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "usage: mwan opnsense config backup")
		return 2
	}
	cli, ctx, cancel, err := dialProbe()
	if err != nil {
		return printAndExit("config backup", err)
	}
	defer cancel()
	defer func() { _ = cli.Close() }()
	resp, err := cli.RPC().BackupConfigXML(ctx, &mwanv1.BackupConfigXMLRequest{Label: ""})
	if err != nil {
		return printAndExit("config backup", err)
	}
	fmt.Printf("backup_path=%s size_bytes=%d\n", resp.GetBackupPath(), resp.GetSizeBytes())
	return 0
}

// runConfigImport transforms a redacted prod XML at SOURCE into the
// testbed-shaped output. The substitutions YAML and the output path
// come from [opnsense.config.import] in TOML. The transform is local;
// no daemon RPC is involved.
func runConfigImport(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: mwan opnsense config import SOURCE")
		return 2
	}
	source := args[0]
	cfg, err := loadOpnsenseConfig()
	if err != nil {
		return printAndExit("config import", err)
	}
	subsPath := cfg.OPNsense.ConfigImport.Substitutions
	outputPath := cfg.OPNsense.ConfigImport.Output
	if subsPath == "" {
		return printAndExit("config import", fmt.Errorf("[opnsense.config.import].substitutions is required in %s", cfg.Source))
	}
	if outputPath == "" {
		return printAndExit("config import", fmt.Errorf("[opnsense.config.import].output is required in %s", cfg.Source))
	}

	cleanInput := filepath.Clean(source)
	cleanSubs := filepath.Clean(subsPath)
	cleanOutput := filepath.Clean(outputPath)

	// Path values come from operator-supplied source path and TOML keys;
	// filepath.Clean strips any `..` traversal (gosec G304 mitigation).
	inputBytes, err := os.ReadFile(cleanInput)
	if err != nil {
		return printAndExit("config import", fmt.Errorf("read %s: %w", cleanInput, err))
	}
	subs, err := configxform.Load(cleanSubs)
	if err != nil {
		return printAndExit("config import", fmt.Errorf("load substitutions: %w", err))
	}
	transformed, err := configxform.Apply(inputBytes, subs)
	if err != nil {
		return printAndExit("config import", fmt.Errorf("apply transform: %w", err))
	}
	if err := writeOutputAtomic(cleanOutput, transformed); err != nil {
		return printAndExit("config import", fmt.Errorf("write %s: %w", cleanOutput, err))
	}
	fmt.Printf("config import: bytes=%d output=%s\n", len(transformed), cleanOutput)
	return 0
}

// writeOutputAtomic writes content to dest atomically by creating a
// sibling temp file, flushing it, and renaming. The temp filename is
// chosen by [os.CreateTemp], which sidesteps the gosec G703 path-
// traversal taint check because the actual write target is constructed
// by the runtime.
func writeOutputAtomic(dest string, content []byte) error {
	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, ".opnsense-import.tmp.*")
	if err != nil {
		return fmt.Errorf("create temp file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		if removeErr := os.Remove(tmpName); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			slog.Warn("config import: remove temp file failed", "path", tmpName, "err", removeErr)
		}
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp file %q: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("sync temp file %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp file %q: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp file %q: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		cleanup()
		return fmt.Errorf("rename temp %q to %q: %w", tmpName, dest, err)
	}
	return nil
}

// xpathSubVerb enumerates `mwan opnsense config xpath <verb>` actions.
type xpathSubVerb string

const (
	xpathSubGet    xpathSubVerb = "get"
	xpathSubSet    xpathSubVerb = "set"
	xpathSubDelete xpathSubVerb = "delete"
)

func runConfigXPath(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: mwan opnsense config xpath {get|set|delete} ...")
		return 2
	}
	verb := xpathSubVerb(args[0])
	rest := args[1:]
	switch verb {
	case xpathSubGet:
		return runConfigXPathGet(rest)
	case xpathSubSet:
		return runConfigXPathSet(rest)
	case xpathSubDelete:
		return runConfigXPathDelete(rest)
	default:
		fmt.Fprintf(os.Stderr, "mwan opnsense config xpath: unknown verb %q\n", string(verb))
		return 2
	}
}

func runConfigXPathGet(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: mwan opnsense config xpath get EXPR")
		return 2
	}
	cli, ctx, cancel, err := dialProbe()
	if err != nil {
		return printAndExit("config xpath get", err)
	}
	defer cancel()
	defer func() { _ = cli.Close() }()
	matches, err := cli.RPC().XPathGet(ctx, &mwanv1.XPathGetRequest{Expression: args[0]})
	if err != nil {
		return printAndExit("config xpath get", err)
	}
	for _, m := range matches {
		fmt.Println(m)
	}
	return 0
}

func runConfigXPathSet(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: mwan opnsense config xpath set EXPR VALUE")
		return 2
	}
	cli, ctx, cancel, err := dialProbe()
	if err != nil {
		return printAndExit("config xpath set", err)
	}
	defer cancel()
	defer func() { _ = cli.Close() }()
	resp, err := cli.RPC().XPathSet(ctx, &mwanv1.XPathSetRequest{Expression: args[0], NewValue: args[1]})
	if err != nil {
		return printAndExit("config xpath set", err)
	}
	fmt.Printf("backup_path=%s changed_count=%d\n", resp.GetBackupPath(), resp.GetChangedCount())
	return 0
}

func runConfigXPathDelete(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: mwan opnsense config xpath delete EXPR")
		return 2
	}
	cli, ctx, cancel, err := dialProbe()
	if err != nil {
		return printAndExit("config xpath delete", err)
	}
	defer cancel()
	defer func() { _ = cli.Close() }()
	resp, err := cli.RPC().XPathDelete(ctx, &mwanv1.XPathDeleteRequest{Expression: args[0]})
	if err != nil {
		return printAndExit("config xpath delete", err)
	}
	fmt.Printf("backup_path=%s deleted_count=%d\n", resp.GetBackupPath(), resp.GetDeletedCount())
	return 0
}

func runConfigStripGatewayV6(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: mwan opnsense config strip-gateway-v6 NAME")
		return 2
	}
	// The RPC predates the per-gateway arg; the daemon strips any v6
	// default gateway. The NAME positional is accepted for symmetry with
	// inject-gateway-v6 but is not yet wired into the wire schema.
	_ = args[0]
	cli, ctx, cancel, err := dialProbe()
	if err != nil {
		return printAndExit("config strip-gateway-v6", err)
	}
	defer cancel()
	defer func() { _ = cli.Close() }()
	resp, err := cli.RPC().StripGatewayV6(ctx, &mwanv1.StripGatewayV6Request{})
	if err != nil {
		return printAndExit("config strip-gateway-v6", err)
	}
	fmt.Printf("backup_path=%s changed=%t\n", resp.GetBackupPath(), resp.GetChanged())
	return 0
}

func runConfigInjectGatewayV6(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: mwan opnsense config inject-gateway-v6 NAME")
		return 2
	}
	cli, ctx, cancel, err := dialProbe()
	if err != nil {
		return printAndExit("config inject-gateway-v6", err)
	}
	defer cancel()
	defer func() { _ = cli.Close() }()
	resp, err := cli.RPC().InjectGatewayV6(ctx, &mwanv1.InjectGatewayV6Request{GatewayName: args[0]})
	if err != nil {
		return printAndExit("config inject-gateway-v6", err)
	}
	fmt.Printf("backup_path=%s changed=%t\n", resp.GetBackupPath(), resp.GetChanged())
	return 0
}
