package svc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/beevik/etree"
)

// ConfigPath is the canonical OPNsense config.xml location. Override
// in tests via the Server's configPath field.
const ConfigPath = "/conf/config.xml"

// BackupDir is where snapshot files land. Created on demand. OPNsense
// itself uses /conf/backup so we land alongside its native backups.
const BackupDir = "/conf/backup"

func readConfigWithLog(ctx context.Context, log *slog.Logger, path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, logWrappedErrorContext(
			ctx,
			log,
			"opnsensesvc: read config failed",
			"readConfig: "+path,
			err,
			slog.String("path", path),
		)
	}
	return b, nil
}

func writeConfigWithLog(ctx context.Context, log *slog.Logger, path string, content []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config.xml.tmp.*")
	if err != nil {
		return logWrappedErrorContext(
			ctx,
			log,
			"opnsensesvc: create temp config failed",
			"writeConfig: tmp",
			err,
			slog.String("dir", dir),
		)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		if err := os.Remove(tmpName); err != nil && !errors.Is(err, os.ErrNotExist) {
			loggerOrDefault(log).WarnContext(ctx,
				"opnsensesvc: remove temp config failed",
				"path", tmpName,
				"err", err)
		}
	}

	if _, err := tmp.Write(content); err != nil {
		logCloseErrorContext(ctx, log, tmp, "opnsensesvc: close temp config failed")
		cleanup()
		return logWrappedErrorContext(ctx, log,
			"opnsensesvc: write temp config failed", "writeConfig: write", err)
	}
	if err := tmp.Sync(); err != nil {
		logCloseErrorContext(ctx, log, tmp, "opnsensesvc: close temp config failed")
		cleanup()
		return logWrappedErrorContext(ctx, log,
			"opnsensesvc: sync temp config failed", "writeConfig: sync", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return logWrappedErrorContext(ctx, log,
			"opnsensesvc: close temp config failed", "writeConfig: close", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		cleanup()
		return logWrappedErrorContext(ctx, log,
			"opnsensesvc: chmod temp config failed", "writeConfig: chmod", err,
			slog.String("path", tmpName))
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return logWrappedErrorContext(ctx, log,
			"opnsensesvc: rename temp config failed", "writeConfig: rename", err,
			slog.String("from", tmpName), slog.String("to", path))
	}
	return nil
}

func backupConfigWithLog(
	ctx context.Context,
	log *slog.Logger,
	candidateClock Clock,
	srcPath string,
	backupDir string,
	label string,
) (string, error) {
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return "", logWrappedErrorContext(ctx, log,
			"opnsensesvc: make backup dir failed", "backupConfig: mkdir", err,
			slog.String("backup_dir", backupDir))
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return "", logWrappedErrorContext(ctx, log,
			"opnsensesvc: open config backup source failed", "backupConfig: open src", err,
			slog.String("path", srcPath))
	}
	defer logCloseErrorContext(ctx, log, src,
		"opnsensesvc: close config backup source failed",
		slog.String("path", srcPath))

	stamp := clockOrReal(candidateClock).Now().UTC().Format("20060102-150405")
	name := stamp + ".xml"
	if label != "" {
		name = stamp + "-" + sanitizeLabel(label) + ".xml"
	}
	destPath := filepath.Join(backupDir, name)

	tmp, err := os.CreateTemp(backupDir, ".backup.tmp.*")
	if err != nil {
		return "", logWrappedErrorContext(ctx, log,
			"opnsensesvc: create temp backup failed", "backupConfig: tmp", err,
			slog.String("backup_dir", backupDir))
	}
	tmpName := tmp.Name()
	cleanup := func() {
		if err := os.Remove(tmpName); err != nil && !errors.Is(err, os.ErrNotExist) {
			loggerOrDefault(log).WarnContext(ctx,
				"opnsensesvc: remove temp backup failed",
				"path", tmpName,
				"err", err)
		}
	}

	if _, err := io.Copy(tmp, src); err != nil {
		logCloseErrorContext(ctx, log, tmp, "opnsensesvc: close temp backup failed")
		cleanup()
		return "", logWrappedErrorContext(ctx, log,
			"opnsensesvc: copy config backup failed", "backupConfig: copy", err)
	}
	if err := tmp.Sync(); err != nil {
		logCloseErrorContext(ctx, log, tmp, "opnsensesvc: close temp backup failed")
		cleanup()
		return "", logWrappedErrorContext(ctx, log,
			"opnsensesvc: sync temp backup failed", "backupConfig: sync", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", logWrappedErrorContext(ctx, log,
			"opnsensesvc: close temp backup failed", "backupConfig: close", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		cleanup()
		return "", logWrappedErrorContext(ctx, log,
			"opnsensesvc: chmod temp backup failed", "backupConfig: chmod", err,
			slog.String("path", tmpName))
	}
	if err := os.Rename(tmpName, destPath); err != nil {
		cleanup()
		return "", logWrappedErrorContext(ctx, log,
			"opnsensesvc: rename temp backup failed", "backupConfig: rename", err,
			slog.String("from", tmpName), slog.String("to", destPath))
	}
	return destPath, nil
}

func sanitizeLabel(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s) && i < 32; i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c)
		case c >= '0' && c <= '9':
			out = append(out, c)
		case c == '-' || c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "labeled"
	}
	return string(out)
}

func stripGatewayV6WithLog(ctx context.Context, log *slog.Logger, input []byte) ([]byte, bool, error) {
	doc := etree.NewDocument()
	if err := doc.ReadFromBytes(input); err != nil {
		return nil, false, logWrappedErrorContext(ctx, log,
			"opnsensesvc: strip gateway parse failed", "stripGatewayV6: parse", err)
	}
	wan := doc.FindElement("//opnsense/interfaces/wan")
	if wan == nil {
		return input, false, nil
	}
	gw := wan.FindElement("./gatewayv6")
	if gw == nil {
		return input, false, nil
	}
	wan.RemoveChild(gw)

	out, err := doc.WriteToBytes()
	if err != nil {
		return nil, false, logWrappedErrorContext(ctx, log,
			"opnsensesvc: strip gateway serialize failed", "stripGatewayV6: serialize", err)
	}
	return out, true, nil
}

func injectGatewayV6WithLog(
	ctx context.Context,
	log *slog.Logger,
	input []byte,
	gatewayName string,
) ([]byte, bool, error) {
	if gatewayName == "" {
		return nil, false, errors.New("injectGatewayV6: gatewayName required")
	}
	doc := etree.NewDocument()
	if err := doc.ReadFromBytes(input); err != nil {
		return nil, false, logWrappedErrorContext(ctx, log,
			"opnsensesvc: inject gateway parse failed", "injectGatewayV6: parse", err)
	}
	wan := doc.FindElement("//opnsense/interfaces/wan")
	if wan == nil {
		return input, false, nil
	}
	if wan.FindElement("./gatewayv6") != nil {
		return input, false, nil
	}
	gw := wan.CreateElement("gatewayv6")
	gw.SetText(gatewayName)

	out, err := doc.WriteToBytes()
	if err != nil {
		return nil, false, logWrappedErrorContext(ctx, log,
			"opnsensesvc: inject gateway serialize failed", "injectGatewayV6: serialize", err)
	}
	return out, true, nil
}
