package main

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	sdbus "github.com/coreos/go-systemd/v22/dbus"

	"goodkind.io/opnsensectl/internal/daemoncfg"
)

// installed is one file a test expects install to have placed.
type installed struct {
	path    string
	content []byte
	mode    fs.FileMode
}

// routerWant is the router layout the ansible deploy produced, plus the
// daemon settings file install now owns.
func routerWant() []installed {
	return []installed{
		{path: "/usr/local/etc/rc.d/mwan_opnsense", content: rcdScript, mode: 0o755},
		{path: "/usr/local/libexec/mwan-opnsense-run", content: runShim, mode: 0o755},
		{path: "/boot/loader.conf.d/mwan_opnsense.conf", content: loaderEntry, mode: 0o644},
		{path: "/etc/rc.conf.d/mwan_opnsense", content: rcConfDefaults, mode: 0o644},
		{path: "/usr/local/etc/opnsensectl.conf", content: daemoncfg.DefaultFile, mode: 0o600},
	}
}

// newTestInstaller returns an installer rooted at a temp dir that already has
// dirs, owned by the test user so the install runs without root.
func newTestInstaller(t *testing.T, dirs ...string) (installer, *bytes.Buffer) {
	t.Helper()
	root := t.TempDir()
	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	out := &bytes.Buffer{}
	return installer{root: root, uid: os.Getuid(), gid: os.Getgid(), out: out}, out
}

func routerTestInstaller(t *testing.T) (installer, *bytes.Buffer) {
	t.Helper()
	return newTestInstaller(t,
		"/usr/local/etc/rc.d", "/usr/local/libexec", "/boot/loader.conf.d", "/etc/rc.conf.d")
}

func assertInstalled(t *testing.T, in installer, want []installed) {
	t.Helper()
	for _, w := range want {
		target := filepath.Join(in.root, w.path)
		info, err := os.Lstat(target)
		if err != nil {
			t.Errorf("%s: %v", w.path, err)
			continue
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != w.mode {
			t.Errorf("%s: mode = %v, want regular %#o", w.path, info.Mode(), w.mode)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != in.uid || int(stat.Gid) != in.gid {
			t.Errorf("%s: owner is not %d:%d", w.path, in.uid, in.gid)
		}
		got, err := os.ReadFile(target)
		if err != nil {
			t.Errorf("%s: %v", w.path, err)
			continue
		}
		if !bytes.Equal(got, w.content) {
			t.Errorf("%s: content differs from the embedded source\ngot:\n%s", w.path, got)
		}
	}
}

// ageFiles sets every wanted file's mtime to a fixed past time and returns it,
// so a later rewrite shows up as a changed mtime.
func ageFiles(t *testing.T, in installer, want []installed) time.Time {
	t.Helper()
	past := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, w := range want {
		if err := os.Chtimes(filepath.Join(in.root, w.path), past, past); err != nil {
			t.Fatalf("chtimes %s: %v", w.path, err)
		}
	}
	return past
}

func assertUntouched(t *testing.T, in installer, want []installed, past time.Time) {
	t.Helper()
	for _, w := range want {
		info, err := os.Stat(filepath.Join(in.root, w.path))
		if err != nil {
			t.Fatalf("stat %s: %v", w.path, err)
		}
		if !info.ModTime().Equal(past) {
			t.Errorf("%s was rewritten on a rerun: mtime %v", w.path, info.ModTime())
		}
	}
}

func TestInstallRouterWritesServiceFilesAndIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in, out := routerTestInstaller(t)

	changed, err := in.placeAll(ctx, routerInstallFiles())
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	if changed != len(routerWant()) {
		t.Errorf("first install changed %d files, want %d\n%s", changed, len(routerWant()), out)
	}
	assertInstalled(t, in, routerWant())

	past := ageFiles(t, in, routerWant())
	out.Reset()
	changed, err = in.placeAll(ctx, routerInstallFiles())
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if changed != 0 || out.Len() != 0 {
		t.Errorf("rerun changed %d files, want 0\n%s", changed, out)
	}
	assertUntouched(t, in, routerWant(), past)
}

// TestInstallRouterKeepsOperatorSettings covers the absent-only files: an
// operator's rc.conf.d and daemon settings survive a reinstall untouched.
func TestInstallRouterKeepsOperatorSettings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in, _ := routerTestInstaller(t)
	if err := os.MkdirAll(filepath.Join(in.root, "/usr/local/etc"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	operatorRC := []byte("mwan_opnsense_logfile=\"/var/log/custom.log\"\n")
	operatorConf := []byte("[daemon]\nbaud = 921600\n")
	kept := []installed{
		{path: "/etc/rc.conf.d/mwan_opnsense", content: operatorRC, mode: 0o600},
		{path: "/usr/local/etc/opnsensectl.conf", content: operatorConf, mode: 0o640},
	}
	for _, k := range kept {
		if err := os.WriteFile(filepath.Join(in.root, k.path), k.content, k.mode); err != nil {
			t.Fatalf("seed %s: %v", k.path, err)
		}
	}

	changed, err := in.placeAll(ctx, routerInstallFiles())
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if changed != 3 {
		t.Errorf("install changed %d files, want 3", changed)
	}
	assertInstalled(t, in, routerWant()[:3])
	assertInstalled(t, in, kept)
}

// TestInstallRouterRepairsDrift proves a rerun restores a managed file whose
// content or mode drifted, and rewrites only those files.
func TestInstallRouterRepairsDrift(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in, out := routerTestInstaller(t)
	if _, err := in.placeAll(ctx, routerInstallFiles()); err != nil {
		t.Fatalf("first install: %v", err)
	}

	shim := filepath.Join(in.root, "/usr/local/libexec/mwan-opnsense-run")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("corrupt shim: %v", err)
	}
	if err := os.Chmod(filepath.Join(in.root, "/usr/local/etc/rc.d/mwan_opnsense"), 0o644); err != nil {
		t.Fatalf("chmod rc.d: %v", err)
	}

	out.Reset()
	changed, err := in.placeAll(ctx, routerInstallFiles())
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	wantOut := "wrote /usr/local/etc/rc.d/mwan_opnsense\nwrote /usr/local/libexec/mwan-opnsense-run\n"
	if changed != 2 || out.String() != wantOut {
		t.Errorf("rerun changed %d, output:\n%s\nwant 2 and:\n%s", changed, out, wantOut)
	}
	assertInstalled(t, in, routerWant())
}

// fakeSystemBus stands in for the systemd D-Bus manager. It records every call
// and, like systemd, reports a symlink change only for a unit not yet enabled.
type fakeSystemBus struct {
	calls   []string
	enabled map[string]bool
}

func (f *fakeSystemBus) EnableUnitFilesContext(_ context.Context, files []string, runtimeOnly bool, force bool) (bool, []sdbus.EnableUnitFileChange, error) {
	f.calls = append(f.calls, fmt.Sprintf("enable %s runtime=%t force=%t", strings.Join(files, ","), runtimeOnly, force))
	var changes []sdbus.EnableUnitFileChange
	for _, name := range files {
		if f.enabled[name] {
			continue
		}
		f.enabled[name] = true
		changes = append(changes, sdbus.EnableUnitFileChange{
			Type:        "symlink",
			Filename:    "/etc/systemd/system/multi-user.target.wants/" + name,
			Destination: "/etc/systemd/system/" + name,
		})
	}
	return false, changes, nil
}

func (f *fakeSystemBus) ReloadContext(_ context.Context) error {
	f.calls = append(f.calls, "reload")
	return nil
}

func (f *fakeSystemBus) Close() {
	f.calls = append(f.calls, "close")
}

func TestInstallHostWritesAndEnablesUnits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in, out := newTestInstaller(t, "/etc/systemd/system")
	bus := &fakeSystemBus{calls: nil, enabled: map[string]bool{}}
	openBus := func(context.Context) (unitManager, error) { return bus, nil }

	changed, err := in.installHost(ctx, openBus)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if changed != 4 {
		t.Errorf("install changed %d, want 2 units written and 2 enabled\n%s", changed, out)
	}
	units := []installed{
		{path: "/etc/systemd/system/mwan-opnsense-host.service", content: hostServiceUnit, mode: 0o644},
		{path: "/etc/systemd/system/mwan-opnsense-drain.service", content: drainServiceUnit, mode: 0o644},
	}
	assertInstalled(t, in, units)

	wantExec := map[string]string{
		"/etc/systemd/system/mwan-opnsense-host.service":  "\nExecStart=/usr/local/bin/opnsensectl host serve\n",
		"/etc/systemd/system/mwan-opnsense-drain.service": "\nExecStart=/usr/local/bin/opnsensectl host drain\n",
	}
	for path, line := range wantExec {
		data, readErr := os.ReadFile(filepath.Join(in.root, path))
		if readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}
		if !strings.Contains(string(data), line) {
			t.Errorf("%s missing %q", path, strings.TrimSpace(line))
		}
		if strings.Contains(string(data), ".socket") {
			t.Errorf("%s still references a socket unit", path)
		}
	}
	entries, err := os.ReadDir(filepath.Join(in.root, "/etc/systemd/system"))
	if err != nil {
		t.Fatalf("read unit dir: %v", err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if want := []string{drainUnitName, hostUnitName}; !slices.Equal(names, want) {
		t.Errorf("unit dir holds %v, want only %v", names, want)
	}

	past := ageFiles(t, in, units)
	out.Reset()
	changed, err = in.installHost(ctx, openBus)
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if changed != 0 || out.Len() != 0 {
		t.Errorf("rerun changed %d, want 0\n%s", changed, out)
	}
	assertUntouched(t, in, units, past)

	enableCall := "enable mwan-opnsense-host.service,mwan-opnsense-drain.service runtime=false force=false"
	wantCalls := []string{enableCall, "reload", "close", enableCall, "reload", "close"}
	if !slices.Equal(bus.calls, wantCalls) {
		t.Errorf("systemd calls = %q\nwant %q", bus.calls, wantCalls)
	}
}
