package configxform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixturePath resolves a file in the capture fixture. The fixture is a frozen
// copy of the redacted prod capture and the substitution table that
// agoodkind/configs commits under testbed/opnsense, taken at configs 247e6b88
// with every authorizedkeys value blanked. It does not follow later changes to
// the live capture, so these tests guard the transform against that fixed
// shape and no longer detect drift between the live capture and the table.
func fixturePath(name string) string {
	return filepath.Join("testdata", "capture", name)
}

// transformRealCapture applies the committed substitution table to the
// committed prod capture and returns the candidate bytes.
func transformRealCapture(t *testing.T) []byte {
	t.Helper()
	capturePath := fixturePath("prod-config-redacted.xml")
	input, err := os.ReadFile(capturePath) //nolint:gosec // fixed package-relative path
	if err != nil {
		t.Fatalf("read capture %q: %v", capturePath, err)
	}

	subsPath := fixturePath("substitutions.yaml")
	subs, err := Load(subsPath)
	if err != nil {
		t.Fatalf("load substitutions %q: %v", subsPath, err)
	}

	out, err := Apply(input, subs)
	if err != nil {
		t.Fatalf("Apply on real capture: %v", err)
	}
	return out
}

// TestApplyRealCaptureWithCommittedSubstitutions runs the committed
// substitution table against the committed prod capture. Every other test in
// this package uses a small hand-written fixture, so nothing else notices when
// the capture and the substitutions drift apart. A refreshed capture whose
// shape no longer matches the table fails here rather than at import time on
// the testbed router.
func TestApplyRealCaptureWithCommittedSubstitutions(t *testing.T) {
	out := transformRealCapture(t)
	doc := mustParse(t, out)

	for _, tc := range []struct{ xpath, want string }{
		{"//opnsense/system/hostname", "router"},
		{"//opnsense/system/domain", "suburban.goodkind.io"},
	} {
		el := doc.FindElement(tc.xpath)
		if el == nil {
			t.Errorf("%s matched no element", tc.xpath)
			continue
		}
		if got := strings.TrimSpace(el.Text()); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.xpath, got, tc.want)
		}
	}
	if strings.Contains(string(out), "test.home.goodkind.io") {
		t.Error("transformed capture retains the retired test.home.goodkind.io domain")
	}

	for _, el := range doc.FindElements("//opnsense//if") {
		if strings.TrimSpace(el.Text()) == "iavf0" {
			t.Errorf("stale prod device iavf0 survives at %s", el.GetPath())
		}
	}

	for _, xpath := range []string{
		"//opnsense/interfaces/opt9",
		"//opnsense/cert",
		"//opnsense/OPNsense/wireguard/client/clients/client",
		"//opnsense/OPNsense/wireguard/server/servers/server",
	} {
		if n := len(doc.FindElements(xpath)); n != 0 {
			t.Errorf("%s: %d elements survive, want 0", xpath, n)
		}
	}

	// The capture must carry the post-26.7 shape. Router advertisements moved
	// out of the per-interface dhcpdv6 model into OPNsense/radvd, so a ramode
	// element means the capture predates the migration the live router ran.
	if n := len(doc.FindElements("//ramode")); n != 0 {
		t.Errorf("capture carries %d pre-26.7 ramode elements", n)
	}
}

// TestApplyRealCapturePlacesGuestsOnVMNET checks the renumber end to end on the
// real capture: the guest segment lands on VMNET inside the translated /60, the
// MANAGEMENT segment is gone along with everything that referenced it, and the
// transit, IPv6-only, and NAT64 addresses are left alone.
func TestApplyRealCapturePlacesGuestsOnVMNET(t *testing.T) {
	out := transformRealCapture(t)
	doc := mustParse(t, out)

	for _, tc := range []struct{ xpath, want string }{
		{"//opnsense/interfaces/opt6/if", "vtnet0"},
		{"//opnsense/interfaces/opt6/ipaddr", "10.240.4.1"},
		{"//opnsense/interfaces/opt6/ipaddrv6", "3d06:bad:b01:210::1"},
		{"//opnsense/interfaces/lan/ipaddrv6", "3d06:bad:b01:211::1"},
		{"//opnsense/interfaces/opt4/ipaddrv6", "3d06:bad:b01:212::1"},
		{"//opnsense/interfaces/wan/ipaddr", "10.240.240.2"},
		{"//opnsense/interfaces/wan/ipaddrv6", "3d06:bad:b01:201::2"},
		{"//opnsense/interfaces/opt8/ipaddrv6", "3d06:bad:b01:264::1"},
		{"//opnsense/OPNsense/tayga/general/v6prefix", "3d06:bad:b01:2664::/96"},
	} {
		el := doc.FindElement(tc.xpath)
		if el == nil {
			t.Errorf("%s matched no element", tc.xpath)
			continue
		}
		if got := strings.TrimSpace(el.Text()); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.xpath, got, tc.want)
		}
	}

	// Every VLAN must hang off the device the testbed VM actually has.
	for _, el := range doc.FindElements("//opnsense/vlans/vlan/if") {
		if got := strings.TrimSpace(el.Text()); got != "vtnet0" {
			t.Errorf("VLAN parent = %q, want vtnet0", got)
		}
	}

	// Removing MANAGEMENT has to take its references with it, or the candidate
	// carries rules, a virtual IP, and a group member pointing at an interface
	// that no longer exists.
	if strings.Contains(string(out), "opt9") {
		t.Error("candidate still references the removed MANAGEMENT interface")
	}

	// Any surviving prod literal means a shift was missed.
	for _, stale := range []string{
		"3d06:bad:b01:21::", "3d06:bad:b01:22::", "3d06:bad:b01:23::",
		"3d06:bad:b01:204::", "3d06:bad:b01:200::",
		"10.250.250.",
		"10.250.0.", "10.250.1.", "10.250.2.", "10.250.3.", "10.250.4.",
	} {
		if n := strings.Count(string(out), stale); n != 0 {
			t.Errorf("stale literal %q appears %d times in the candidate", stale, n)
		}
	}
}
