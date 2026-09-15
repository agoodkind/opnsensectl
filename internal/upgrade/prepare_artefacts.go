package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
)

// Artefact filenames captured during the prepare phase. The names are
// part of the public contract so the validate phase, the operator, and
// the design doc all agree on where to look.
const (
	// ArtefactConfigXMLPre is the raw /conf/config.xml capture.
	ArtefactConfigXMLPre = "config.xml.pre"
	// ArtefactConfigXMLSHA256 is the sidecar with `<hex>  config.xml.pre`.
	ArtefactConfigXMLSHA256 = "config.xml.pre.sha256"
	// ArtefactVersion holds the running OPNsense version string.
	ArtefactVersion = "version.txt"
	// ArtefactInterfaces holds a JSON object with ifconfig and netstat output.
	ArtefactInterfaces = "interfaces.json"
	// ArtefactBGPStatus holds the FRR/vtysh BGP summary JSON.
	ArtefactBGPStatus = "bgp_status.json"
)

// interfacesArtefact is the on-disk shape for interfaces.json. Each
// field is the raw stdout of the matching guest command. Keeping the
// raw text means the validate phase can diff strings without re-running
// the guest commands.
type interfacesArtefact struct {
	IfconfigAV   string `json:"ifconfig_av"`
	NetstatV4    string `json:"netstat_rn_inet"`
	NetstatV6    string `json:"netstat_rn_inet6"`
	IfconfigErr  string `json:"ifconfig_err,omitempty"`
	NetstatV4Err string `json:"netstat_rn_inet_err,omitempty"`
	NetstatV6Err string `json:"netstat_rn_inet6_err,omitempty"`
}

// bgpStatusArtefact is the on-disk shape for bgp_status.json. When
// vtysh is present and returns data, Captured is true and Raw holds
// the verbatim vtysh output. When capture fails for any reason,
// Captured is false and Reason explains why. A zero-byte file is
// indistinguishable from a write error, so a sentinel document is
// always written even when no BGP data is available.
type bgpStatusArtefact struct {
	Captured bool   `json:"captured"`
	Raw      string `json:"raw,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// errCaptureExecMissing is returned when a capture is asked to run
// without a configured Executor.
var errCaptureExecMissing = errors.New("upgrade: capture requires deps.Exec")

// capturePreUpgradeArtefacts runs every documented capture under the
// per-deploy directory. Returns an error only when a required artefact
// (config.xml.pre or firmware.pre.json) cannot be written; best-effort
// artefacts log a warning and write an empty placeholder so the
// directory shape is always uniform.
func capturePreUpgradeArtefacts(ctx context.Context, deps Deps, opts Options, deployDir string) error {
	if err := captureConfigXML(ctx, deps, opts, deployDir); err != nil {
		return err
	}
	if err := captureFirmware(ctx, deps, opts, deployDir); err != nil {
		return err
	}
	captureVersion(ctx, deps, opts, deployDir)
	captureInterfaces(ctx, deps, opts, deployDir)
	captureBGPStatus(ctx, deps, opts, deployDir)
	return nil
}

// captureConfigXML reads /conf/config.xml from the guest, writes it as
// `config.xml.pre`, and writes the sha256 sidecar. This artefact is
// required: rollback and forensic recovery both depend on having the
// pre-upgrade config bytes, so a failure here aborts Prepare.
func captureConfigXML(ctx context.Context, deps Deps, opts Options, deployDir string) error {
	if deps.Exec == nil {
		slog.ErrorContext(ctx, "upgrade.Prepare: capture config.xml: deps.Exec missing",
			"err", errCaptureExecMissing)
		return errCaptureExecMissing
	}
	res, err := deps.Exec.GuestExec(ctx, opts.VMID, guestCat, "/conf/config.xml")
	if err != nil {
		slog.ErrorContext(ctx, "upgrade.Prepare: capture config.xml: GuestExec failed",
			"err", err, "vmid", opts.VMID)
		return fmt.Errorf("upgrade.Prepare: capture config.xml: %w", err)
	}
	if res.ExitCode != 0 {
		err := fmt.Errorf("cat /conf/config.xml exit=%d stderr=%s", res.ExitCode, res.Stderr)
		slog.ErrorContext(ctx, "upgrade.Prepare: capture config.xml: non-zero exit",
			"err", err, "vmid", opts.VMID, "exit", res.ExitCode)
		return fmt.Errorf("upgrade.Prepare: capture config.xml: %w", err)
	}
	body := []byte(res.Stdout)
	xmlPath := filepath.Join(deployDir, ArtefactConfigXMLPre)
	if err := WriteFileBytes(ctx, xmlPath, body); err != nil {
		return fmt.Errorf("upgrade.Prepare: write config.xml.pre: %w", err)
	}
	digest := sha256.Sum256(body)
	line := fmt.Sprintf("%s  %s\n", hex.EncodeToString(digest[:]), ArtefactConfigXMLPre)
	shaPath := filepath.Join(deployDir, ArtefactConfigXMLSHA256)
	if err := WriteFileBytes(ctx, shaPath, []byte(line)); err != nil {
		return fmt.Errorf("upgrade.Prepare: write config.xml.pre.sha256: %w", err)
	}
	slog.InfoContext(ctx, "upgrade.Prepare: captured config.xml",
		"vmid", opts.VMID, "bytes", len(body), "sha256", hex.EncodeToString(digest[:]))
	return nil
}

// captureVersion writes `version.txt`. Best-effort: a guest that does
// not respond to `opnsense-version` still produces a placeholder file
// so the deploy-dir shape is uniform.
func captureVersion(ctx context.Context, deps Deps, opts Options, deployDir string) {
	path := filepath.Join(deployDir, ArtefactVersion)
	if deps.Exec == nil {
		slog.WarnContext(ctx, "upgrade.Prepare: capture version: deps.Exec missing, writing empty placeholder")
		_ = WriteFileBytes(ctx, path, nil)
		return
	}
	res, err := deps.Exec.GuestExec(ctx, opts.VMID, guestVersion)
	if err != nil {
		slog.WarnContext(ctx, "upgrade.Prepare: capture version: GuestExec failed, writing empty placeholder",
			"err", err, "vmid", opts.VMID)
		_ = WriteFileBytes(ctx, path, nil)
		return
	}
	if res.ExitCode != 0 {
		slog.WarnContext(ctx, "upgrade.Prepare: capture version: non-zero exit, writing empty placeholder",
			"vmid", opts.VMID, "exit", res.ExitCode, "stderr", res.Stderr)
		_ = WriteFileBytes(ctx, path, nil)
		return
	}
	if err := WriteFileBytes(ctx, path, []byte(res.Stdout)); err != nil {
		slog.WarnContext(ctx, "upgrade.Prepare: capture version: write failed",
			"err", err, "path", path)
	}
}

// captureInterfaces runs `ifconfig -av` plus the IPv4 and IPv6 netstat
// route dumps and writes them as a JSON object. Best-effort: a partial
// failure still produces a JSON file, with the missing sections holding
// an empty string and the error captured in the per-section *_err
// field.
func captureInterfaces(ctx context.Context, deps Deps, opts Options, deployDir string) {
	path := filepath.Join(deployDir, ArtefactInterfaces)
	art := interfacesArtefact{
		IfconfigAV:   "",
		NetstatV4:    "",
		NetstatV6:    "",
		IfconfigErr:  "",
		NetstatV4Err: "",
		NetstatV6Err: "",
	}
	if deps.Exec == nil {
		slog.WarnContext(ctx, "upgrade.Prepare: capture interfaces: deps.Exec missing, writing empty JSON")
		writeInterfacesArtefact(ctx, path, art)
		return
	}
	art.IfconfigAV, art.IfconfigErr = runCaptureCommand(ctx, deps, opts.VMID, guestIfconfig, "-av")
	art.NetstatV4, art.NetstatV4Err = runCaptureCommand(ctx, deps, opts.VMID, guestNetstat, "-rn", "-f", "inet")
	art.NetstatV6, art.NetstatV6Err = runCaptureCommand(ctx, deps, opts.VMID, guestNetstat, "-rn", "-f", "inet6")
	writeInterfacesArtefact(ctx, path, art)
}

// captureBGPStatus runs `vtysh -c 'show bgp summary json'` and writes
// `bgp_status.json`. Best-effort: a guest without FRR or vtysh still
// produces a valid JSON sentinel so the validate phase can distinguish
// "BGP not captured" from "file missing entirely".
func captureBGPStatus(ctx context.Context, deps Deps, opts Options, deployDir string) {
	path := filepath.Join(deployDir, ArtefactBGPStatus)
	if deps.Exec == nil {
		slog.WarnContext(ctx, "upgrade.Prepare: capture bgp_status: deps.Exec missing")
		writeBGPStatusArtefact(ctx, path, bgpStatusArtefact{
			Captured: false,
			Raw:      "",
			Reason:   "deps.Exec not configured",
		})
		return
	}
	res, err := deps.Exec.GuestExec(ctx, opts.VMID, guestVtysh, "-c", "show bgp summary json")
	if err != nil {
		slog.WarnContext(ctx, "upgrade.Prepare: capture bgp_status: GuestExec failed",
			"err", err, "vmid", opts.VMID)
		writeBGPStatusArtefact(ctx, path, bgpStatusArtefact{
			Captured: false,
			Raw:      "",
			Reason:   "GuestExec error: " + err.Error(),
		})
		return
	}
	if res.ExitCode != 0 {
		slog.WarnContext(ctx, "upgrade.Prepare: capture bgp_status: non-zero exit",
			"vmid", opts.VMID, "exit", res.ExitCode, "stderr", res.Stderr)
		writeBGPStatusArtefact(ctx, path, bgpStatusArtefact{
			Captured: false,
			Raw:      "",
			Reason:   fmt.Sprintf("vtysh exit=%d stderr=%s", res.ExitCode, res.Stderr),
		})
		return
	}
	writeBGPStatusArtefact(ctx, path, bgpStatusArtefact{
		Captured: true,
		Raw:      res.Stdout,
		Reason:   "",
	})
}

// writeBGPStatusArtefact marshals the bgp status artefact and writes
// it. A marshal failure is unexpected (the type is concrete) but is
// logged without propagating because the surrounding capture is
// best-effort.
func writeBGPStatusArtefact(ctx context.Context, path string, art bgpStatusArtefact) {
	body, err := json.MarshalIndent(art, "", "  ")
	if err != nil {
		slog.WarnContext(ctx, "upgrade.Prepare: marshal bgp_status artefact failed",
			"err", err, "path", path)
		return
	}
	if err := WriteFileBytes(ctx, path, body); err != nil {
		slog.WarnContext(ctx, "upgrade.Prepare: write bgp_status artefact failed",
			"err", err, "path", path)
	}
}

// runCaptureCommand executes one guest command and returns its stdout
// plus a string-shaped error description for inclusion in the JSON
// artefact. Returning a string keeps the JSON serialisation simple and
// avoids embedding Go error types in operator-facing artefacts.
func runCaptureCommand(ctx context.Context, deps Deps, vmid string, args ...string) (string, string) {
	res, err := deps.Exec.GuestExec(ctx, vmid, args...)
	if err != nil {
		slog.WarnContext(ctx, "upgrade.Prepare: capture command failed",
			"err", err, "vmid", vmid, "argv", args)
		return "", err.Error()
	}
	if res.ExitCode != 0 {
		errMsg := fmt.Sprintf("exit=%d stderr=%s", res.ExitCode, res.Stderr)
		slog.WarnContext(ctx, "upgrade.Prepare: capture command non-zero exit",
			"vmid", vmid, "argv", args, "exit", res.ExitCode)
		return res.Stdout, errMsg
	}
	return res.Stdout, ""
}

// writeInterfacesArtefact marshals the interfaces artefact and writes
// it. A marshal failure is unexpected (the type is concrete) but is
// logged and not propagated because the surrounding capture is
// best-effort.
func writeInterfacesArtefact(ctx context.Context, path string, art interfacesArtefact) {
	body, err := json.MarshalIndent(art, "", "  ")
	if err != nil {
		slog.WarnContext(ctx, "upgrade.Prepare: marshal interfaces artefact failed",
			"err", err, "path", path)
		_ = WriteFileBytes(ctx, path, nil)
		return
	}
	if err := WriteFileBytes(ctx, path, body); err != nil {
		slog.WarnContext(ctx, "upgrade.Prepare: write interfaces artefact failed",
			"err", err, "path", path)
	}
}
