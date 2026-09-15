package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	sdbus "github.com/coreos/go-systemd/v22/dbus"

	"goodkind.io/opnsensectl/internal/daemoncfg"
	"goodkind.io/opnsensectl/internal/svc"
)

// The router service files and the host units opnsensectl install writes. The
// rcd and shim tests read these same embedded bytes.
var (
	//go:embed opnsense-src/etc/rc.d/mwan_opnsense
	rcdScript []byte
	//go:embed opnsense-src/usr/local/libexec/mwan-opnsense-run
	runShim []byte
	//go:embed opnsense-src/boot/loader.conf.d/mwan_opnsense.conf
	loaderEntry []byte
	//go:embed opnsense-src/etc/rc.conf.d/mwan_opnsense.sample
	rcConfDefaults []byte
	//go:embed mwan-opnsense-host.service
	hostServiceUnit []byte
	//go:embed mwan-opnsense-drain.service
	drainServiceUnit []byte
)

const (
	// installRoot is the filesystem root install writes under.
	installRoot = "/"
	// rootUID and rootGID own every installed file: root:wheel on FreeBSD
	// and root:root on linux are both uid 0 and gid 0.
	rootUID = 0
	rootGID = 0

	hostUnitName  = "mwan-opnsense-host.service"
	drainUnitName = "mwan-opnsense-drain.service"
)

// installPlatform is the [runtime.GOOS] value install acts on.
type installPlatform string

const (
	// installPlatformRouter is the OPNsense guest.
	installPlatformRouter installPlatform = "freebsd"
	// installPlatformHost is the Proxmox host.
	installPlatformHost installPlatform = "linux"
)

// installFile is one file install places. absentOnly marks an operator-owned
// settings file: install writes its defaults when the file is missing and
// never touches an existing copy.
type installFile struct {
	path       string
	content    []byte
	mode       fs.FileMode
	absentOnly bool
}

// routerInstallFiles lists what install writes on the OPNsense guest. The
// binary, its symlink, and the rc.conf enable line stay with the deploy.
func routerInstallFiles() []installFile {
	return []installFile{
		{path: "/usr/local/etc/rc.d/mwan_opnsense", content: rcdScript, mode: 0o755, absentOnly: false},
		{path: "/usr/local/libexec/mwan-opnsense-run", content: runShim, mode: 0o755, absentOnly: false},
		{path: "/boot/loader.conf.d/mwan_opnsense.conf", content: loaderEntry, mode: 0o644, absentOnly: false},
		{path: "/etc/rc.conf.d/mwan_opnsense", content: rcConfDefaults, mode: 0o644, absentOnly: true},
		{path: daemoncfg.DefaultPath, content: daemoncfg.DefaultFile, mode: 0o600, absentOnly: true},
	}
}

// hostInstallFiles lists the systemd units install writes on the Proxmox host.
// The drainer binds its relay socket itself, so there is no socket unit.
func hostInstallFiles() []installFile {
	return []installFile{
		{path: "/etc/systemd/system/" + hostUnitName, content: hostServiceUnit, mode: 0o644, absentOnly: false},
		{path: "/etc/systemd/system/" + drainUnitName, content: drainServiceUnit, mode: 0o644, absentOnly: false},
	}
}

// unitManager is the part of the systemd D-Bus API install calls, so a test
// can stand in for the system bus.
type unitManager interface {
	EnableUnitFilesContext(ctx context.Context, files []string, runtimeOnly bool, force bool) (bool, []sdbus.EnableUnitFileChange, error)
	ReloadContext(ctx context.Context) error
	Close()
}

func openSystemBus(ctx context.Context) (unitManager, error) {
	conn, err := sdbus.NewSystemConnectionContext(ctx)
	if err != nil {
		return nil, wrapErr(ctx, "install: connect to systemd", err)
	}
	return conn, nil
}

// installer writes files under root, owned by uid and gid, and prints one line
// to out for every change it makes.
type installer struct {
	root string
	uid  int
	gid  int
	out  io.Writer
}

// runOPNsenseInstall writes this platform's service files: the rc.d service on
// FreeBSD, and the enabled systemd units on linux. A rerun changes nothing
// that is already in place.
func runOPNsenseInstall(args []string) int {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			fmt.Fprintln(os.Stdout, "usage: opnsensectl install")
			fmt.Fprintln(os.Stdout, "")
			fmt.Fprintln(os.Stdout, "On FreeBSD, write the rc.d script, the run shim, the loader entry, and the")
			fmt.Fprintln(os.Stdout, "settings defaults. On linux, write and enable the bridge and drain units.")
			return 0
		}
	}
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "opnsensectl install: unexpected arguments: %v\n", args)
		return 2
	}

	ctx := context.Background()
	in := installer{root: installRoot, uid: rootUID, gid: rootGID, out: os.Stdout}
	var changed int
	var err error
	switch installPlatform(runtime.GOOS) {
	case installPlatformRouter:
		changed, err = in.placeAll(ctx, routerInstallFiles())
	case installPlatformHost:
		changed, err = in.installHost(ctx, openSystemBus)
	default:
		err = wrapErr(ctx, "install", fmt.Errorf("unsupported platform %s", runtime.GOOS))
	}
	if err != nil {
		return printAndExit("install", err)
	}
	if changed == 0 {
		fmt.Fprintln(os.Stdout, "install: no change")
		return 0
	}
	fmt.Fprintf(os.Stdout, "install: %d changes\n", changed)
	return 0
}

// installHost writes the host units, enables them, and reloads systemd. The
// reload runs on every call, not only after a change, so a rerun after a run
// that wrote a unit and then failed still leaves systemd on the units on disk.
func (in installer) installHost(ctx context.Context, openBus func(context.Context) (unitManager, error)) (int, error) {
	changed, err := in.placeAll(ctx, hostInstallFiles())
	if err != nil {
		return changed, err
	}
	bus, err := openBus(ctx)
	if err != nil {
		return changed, err
	}
	defer bus.Close()

	_, enableChanges, err := bus.EnableUnitFilesContext(ctx, []string{hostUnitName, drainUnitName}, false, false)
	if err != nil {
		return changed, wrapErr(ctx, "install: enable units", err)
	}
	for _, change := range enableChanges {
		fmt.Fprintf(in.out, "%s %s -> %s\n", change.Type, change.Filename, change.Destination)
	}
	if err := bus.ReloadContext(ctx); err != nil {
		return changed + len(enableChanges), wrapErr(ctx, "install: daemon-reload", err)
	}
	return changed + len(enableChanges), nil
}

// placeAll places every file in order and returns how many it wrote.
func (in installer) placeAll(ctx context.Context, files []installFile) (int, error) {
	changed := 0
	for _, file := range files {
		wrote, err := in.place(ctx, file)
		if err != nil {
			return changed, err
		}
		if wrote {
			changed++
			fmt.Fprintf(in.out, "wrote %s\n", file.path)
		}
	}
	return changed, nil
}

// place writes file unless the target already holds its content, mode, and
// owner, or unless the file is absentOnly and the target exists. It reports
// whether it wrote. The parent directory must already exist.
func (in installer) place(ctx context.Context, file installFile) (bool, error) {
	target := filepath.Join(in.root, file.path)
	info, err := os.Lstat(target)
	switch {
	case err == nil && file.absentOnly:
		return false, nil
	case err == nil:
		current, matchErr := in.inPlace(ctx, target, info, file)
		if matchErr != nil || current {
			return false, matchErr
		}
	case !errors.Is(err, fs.ErrNotExist):
		return false, wrapErr(ctx, "install: stat "+target, err)
	}

	if err := svc.AtomicWriteFile(ctx, target, file.content, file.mode); err != nil {
		return false, wrapErr(ctx, "install: write "+target, err)
	}
	if err := os.Lchown(target, in.uid, in.gid); err != nil {
		return false, wrapErr(ctx, "install: chown "+target, err)
	}
	return true, nil
}

// inPlace reports whether target is a regular file that already holds file's
// content and mode and is owned by the installer's uid and gid.
func (in installer) inPlace(ctx context.Context, target string, info fs.FileInfo, file installFile) (bool, error) {
	if !info.Mode().IsRegular() || info.Mode().Perm() != file.mode {
		return false, nil
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != in.uid || int(stat.Gid) != in.gid {
		return false, nil
	}
	current, err := os.ReadFile(filepath.Clean(target))
	if err != nil {
		return false, wrapErr(ctx, "install: read "+target, err)
	}
	return bytes.Equal(current, file.content), nil
}
