package svc

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleConfig = `<?xml version="1.0"?>
<opnsense>
  <interfaces>
    <wan>
      <if>vtnet1</if>
      <ipaddr>dhcp</ipaddr>
      <ipaddrv6>dhcp6</ipaddrv6>
      <gatewayv6>WAN_GW6</gatewayv6>
    </wan>
    <lan>
      <if>vtnet0</if>
      <ipaddr>10.250.0.1</ipaddr>
    </lan>
  </interfaces>
  <system>
    <hostname>opnsense</hostname>
  </system>
</opnsense>
`

func TestStripGatewayV6_Removes(t *testing.T) {
	out, changed, err := stripGatewayV6WithLog(context.Background(), nil, []byte(sampleConfig))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}
	if bytes.Contains(out, []byte("gatewayv6")) {
		t.Fatalf("output still contains gatewayv6:\n%s", out)
	}
	// LAN block must remain
	if !bytes.Contains(out, []byte("<if>vtnet0</if>")) {
		t.Fatalf("output dropped lan section:\n%s", out)
	}
}

func TestStripGatewayV6_Idempotent(t *testing.T) {
	first, _, err := stripGatewayV6WithLog(context.Background(), nil, []byte(sampleConfig))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	out, changed, err := stripGatewayV6WithLog(context.Background(), nil, first)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if changed {
		t.Fatal("expected changed=false on second call")
	}
	if !bytes.Equal(out, first) {
		t.Fatal("idempotent call returned different bytes")
	}
}

func TestStripGatewayV6_NoWan(t *testing.T) {
	noWan := `<?xml version="1.0"?><opnsense><system><hostname>x</hostname></system></opnsense>`
	out, changed, err := stripGatewayV6WithLog(context.Background(), nil, []byte(noWan))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if changed {
		t.Fatal("expected changed=false when no <wan>")
	}
	if !bytes.Equal(out, []byte(noWan)) {
		t.Fatal("no-op returned modified bytes")
	}
}

func TestInjectGatewayV6_Inserts(t *testing.T) {
	stripped, _, err := stripGatewayV6WithLog(context.Background(), nil, []byte(sampleConfig))
	if err != nil {
		t.Fatalf("strip: %v", err)
	}
	out, changed, err := injectGatewayV6WithLog(context.Background(), nil, stripped, "WAN_GW6")
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}
	if !bytes.Contains(out, []byte("<gatewayv6>WAN_GW6</gatewayv6>")) {
		t.Fatalf("did not insert gatewayv6:\n%s", out)
	}
}

func TestInjectGatewayV6_AlreadyPresent(t *testing.T) {
	out, changed, err := injectGatewayV6WithLog(context.Background(), nil, []byte(sampleConfig), "WAN_GW6")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if changed {
		t.Fatal("expected changed=false when gatewayv6 already present")
	}
	if !bytes.Equal(out, []byte(sampleConfig)) {
		t.Fatal("no-op returned modified bytes")
	}
}

func TestInjectGatewayV6_RequiresName(t *testing.T) {
	if _, _, err := injectGatewayV6WithLog(context.Background(), nil, []byte(sampleConfig), ""); err == nil {
		t.Fatal("expected error on empty gateway name")
	}
}

func TestStripThenInjectRoundTrip(t *testing.T) {
	stripped, _, err := stripGatewayV6WithLog(context.Background(), nil, []byte(sampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	restored, _, err := injectGatewayV6WithLog(context.Background(), nil, stripped, "WAN_GW6")
	if err != nil {
		t.Fatal(err)
	}
	// We don't promise byte-for-byte equality (etree may normalize
	// whitespace), but the structural content must match: gatewayv6
	// is back, system is intact.
	if !bytes.Contains(restored, []byte("<gatewayv6>WAN_GW6</gatewayv6>")) {
		t.Fatal("round-trip lost gatewayv6")
	}
	if !bytes.Contains(restored, []byte("<hostname>opnsense</hostname>")) {
		t.Fatal("round-trip lost system section")
	}
}

func TestReadWriteConfig_AtomicAndExactBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.xml")
	if err := os.WriteFile(path, []byte(sampleConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readConfigWithLog(context.Background(), nil, path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte(sampleConfig)) {
		t.Fatal("readConfigWithLog altered bytes")
	}
	updated := bytes.ReplaceAll(got, []byte("opnsense"), []byte("opnsense2"))
	if err := writeConfigWithLog(context.Background(), nil, path, updated); err != nil {
		t.Fatal(err)
	}
	got2, err := readConfigWithLog(context.Background(), nil, path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got2, updated) {
		t.Fatal("writeConfigWithLog did not persist updated bytes")
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("expected mode 0644, got %v", st.Mode().Perm())
	}
}

func TestBackupConfig_LandsInDirAndPreservesContent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "config.xml")
	if err := os.WriteFile(src, []byte(sampleConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(dir, "backup")
	dest, err := backupConfigWithLog(context.Background(), nil, nil, src, backupDir, "before-strip")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dest, "before-strip") {
		t.Fatalf("backup name missing label: %s", dest)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte(sampleConfig)) {
		t.Fatal("backup content differs from source")
	}
}

func TestSanitizeLabel(t *testing.T) {
	cases := map[string]string{
		"hello":           "hello",
		"with space":      "with_space",
		"path/like/value": "path_like_value",
		"":                "labeled",
		"01234567890123456789012345678901234567890": "01234567890123456789012345678901",
	}
	for in, want := range cases {
		if got := sanitizeLabel(in); got != want {
			t.Errorf("sanitizeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
