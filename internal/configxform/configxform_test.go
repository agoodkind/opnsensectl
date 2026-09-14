package configxform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beevik/etree"
)

func loadFixture(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join("testdata", "minimal-config.xml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %q: %v", path, err)
	}
	return data
}

func mustParse(t *testing.T, data []byte) *etree.Document {
	t.Helper()
	doc := etree.NewDocument()
	if err := doc.ReadFromBytes(data); err != nil {
		t.Fatalf("parse XML: %v", err)
	}
	return doc
}

func defaultSubs() Substitutions {
	return Substitutions{
		DeviceNames: []DeviceNameMapping{
			{Name: "iavf0 to vtnet0", From: "iavf0", To: "vtnet0"},
		},
		XPathSets: []XPathSet{
			// Note: domain is intentionally absent here. The domain text_literal
			// below rewrites the prod "home.goodkind.io" value in the serialized
			// XML and avoids double-prefixing the testbed domain.
			{Name: "hostname", XPath: "//opnsense/system/hostname", NewValue: "router"},
			{Name: "wan ipaddr", XPath: "//opnsense/interfaces/wan/ipaddr", NewValue: "10.240.250.2"},
			{Name: "wan ipaddrv6", XPath: "//opnsense/interfaces/wan/ipaddrv6", NewValue: "3d06:bad:b01:2fe::2"},
			{Name: "vmnet v4", XPath: "//opnsense/interfaces/opt6/ipaddr", NewValue: "10.240.4.1"},
			{Name: "vmnet v6", XPath: "//opnsense/interfaces/opt6/ipaddrv6", NewValue: "3d06:bad:b01:210::1"},
		},
		RemoveElements: []ElementRemove{
			{Name: "wireguard peers", XPath: "//opnsense/OPNsense/wireguard/client/clients/client"},
			{Name: "management interface", XPath: "//opnsense/interfaces/opt9"},
		},
		TextLiterals: []TextLiteral{
			{Name: "nat source net", From: "10.250.0.0/24", To: "10.240.0.0/24"},
			{Name: "domain literal", From: "home.goodkind.io", To: "suburban.goodkind.io"},
		},
	}
}

func TestApplyRoundTripDeviceNames(t *testing.T) {
	input := loadFixture(t)
	out, err := Apply(input, defaultSubs())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	doc := mustParse(t, out)
	ifs := doc.FindElements("//opnsense//if")
	if len(ifs) == 0 {
		t.Fatal("expected at least one <if> element after transform")
	}
	for _, el := range ifs {
		got := strings.TrimSpace(el.Text())
		if got == "iavf0" {
			t.Errorf("found stale iavf0 reference at %s", el.GetPath())
		}
		if got != "vtnet0" {
			t.Errorf("unexpected <if> value %q at %s; want vtnet0", got, el.GetPath())
		}
	}
}

func TestApplyXPathHostnameAndDomain(t *testing.T) {
	out, err := Apply(loadFixture(t), defaultSubs())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	doc := mustParse(t, out)
	cases := []struct {
		xpath string
		want  string
	}{
		{"//opnsense/system/hostname", "router"},
		{"//opnsense/system/domain", "suburban.goodkind.io"},
		{"//opnsense/interfaces/wan/ipaddr", "10.240.250.2"},
		{"//opnsense/interfaces/wan/ipaddrv6", "3d06:bad:b01:2fe::2"},
		{"//opnsense/interfaces/opt6/ipaddr", "10.240.4.1"},
		{"//opnsense/interfaces/opt6/ipaddrv6", "3d06:bad:b01:210::1"},
	}
	for _, tc := range cases {
		t.Run(tc.xpath, func(t *testing.T) {
			el := doc.FindElement(tc.xpath)
			if el == nil {
				t.Fatalf("xpath %q matched no element", tc.xpath)
			}
			got := strings.TrimSpace(el.Text())
			if got != tc.want {
				t.Errorf("xpath %q: got %q, want %q", tc.xpath, got, tc.want)
			}
		})
	}
}

// TestApplyKeepsVMNETAndDropsManagement locks in which interface the testbed
// guests land on. MANAGEMENT ends in a terminal reject that would deny servers
// outbound HTTP, DNS, and apt, so the transform drops it and keeps VMNET, which
// carries server-appropriate rules. Swapping the two would leave the guests
// reachable but unable to reach anything.
func TestApplyKeepsVMNETAndDropsManagement(t *testing.T) {
	out, err := Apply(loadFixture(t), defaultSubs())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	doc := mustParse(t, out)

	if el := doc.FindElement("//opnsense/interfaces/opt9"); el != nil {
		t.Error("MANAGEMENT interface opt9 survives the transform")
	}

	vmnet := doc.FindElement("//opnsense/interfaces/opt6")
	if vmnet == nil {
		t.Fatal("VMNET interface opt6 is missing after the transform")
	}
	for _, tc := range []struct{ child, want string }{
		{"if", "vtnet0"},
		{"ipaddr", "10.240.4.1"},
		{"ipaddrv6", "3d06:bad:b01:210::1"},
		{"lock", "1"},
	} {
		el := vmnet.FindElement(tc.child)
		if el == nil {
			t.Errorf("opt6 has no %s child", tc.child)
			continue
		}
		if got := strings.TrimSpace(el.Text()); got != tc.want {
			t.Errorf("opt6/%s = %q, want %q", tc.child, got, tc.want)
		}
	}
}

func TestApplyTextLiteralReplace(t *testing.T) {
	out, err := Apply(loadFixture(t), defaultSubs())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if strings.Contains(string(out), "10.250.0.0/24") {
		t.Errorf("output still contains prod literal 10.250.0.0/24")
	}
	if !strings.Contains(string(out), "10.240.0.0/24") {
		t.Errorf("output missing testbed literal 10.240.0.0/24")
	}
}

func TestApplyRemovesWireguardPeers(t *testing.T) {
	out, err := Apply(loadFixture(t), defaultSubs())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	doc := mustParse(t, out)
	peers := doc.FindElements("//opnsense/OPNsense/wireguard/client/clients/client")
	if len(peers) != 0 {
		t.Errorf("expected zero wireguard peers after strip, got %d", len(peers))
	}
}

func TestApplyEmptyInputReturnsError(t *testing.T) {
	if _, err := Apply(nil, defaultSubs()); err == nil {
		t.Fatal("Apply(nil) returned no error")
	}
}

func TestApplyMalformedXMLReturnsError(t *testing.T) {
	bad := []byte("<opnsense><unclosed>")
	if _, err := Apply(bad, defaultSubs()); err == nil {
		t.Fatal("Apply on malformed XML returned no error")
	}
}

func TestDecodeMalformedYAMLReturnsError(t *testing.T) {
	bad := []byte("device_names: [unterminated\n")
	if _, err := Decode(bad); err == nil {
		t.Fatal("Decode on malformed YAML returned no error")
	}
}

func TestDecodeUnknownFieldReturnsError(t *testing.T) {
	bad := []byte("not_a_known_field: 1\n")
	if _, err := Decode(bad); err == nil {
		t.Fatal("Decode on unknown field returned no error")
	}
}

func TestDecodeEmptyInputReturnsError(t *testing.T) {
	if _, err := Decode(nil); err == nil {
		t.Fatal("Decode(nil) returned no error")
	}
}

func TestDecodeRoundTrip(t *testing.T) {
	yamlBytes := []byte(`device_names:
  - name: "iavf0 to vtnet0"
    from: "iavf0"
    to:   "vtnet0"
xpath_sets:
  - name: "hostname"
    xpath: "//opnsense/system/hostname"
    new_value: "router"
remove_elements:
  - name: "wg peers"
    xpath: "//opnsense/OPNsense/wireguard/client/clients/client"
insert_elements:
  - name: "ssh rule"
    parent_xpath: "//opnsense/filter"
    xml: "<rule/>"
text_literals:
  - name: "nat src"
    from: "10.250.0.0/24"
    to:   "10.240.0.0/24"
`)
	got, err := Decode(yamlBytes)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got.DeviceNames) != 1 || got.DeviceNames[0].From != "iavf0" || got.DeviceNames[0].To != "vtnet0" {
		t.Errorf("DeviceNames roundtrip mismatch: %+v", got.DeviceNames)
	}
	if len(got.XPathSets) != 1 || got.XPathSets[0].NewValue != "router" {
		t.Errorf("XPathSets roundtrip mismatch: %+v", got.XPathSets)
	}
	if len(got.RemoveElements) != 1 {
		t.Errorf("RemoveElements roundtrip mismatch: %+v", got.RemoveElements)
	}
	if len(got.InsertElements) != 1 || got.InsertElements[0].ParentXPath != "//opnsense/filter" {
		t.Errorf("InsertElements roundtrip mismatch: %+v", got.InsertElements)
	}
	if len(got.TextLiterals) != 1 || got.TextLiterals[0].To != "10.240.0.0/24" {
		t.Errorf("TextLiterals roundtrip mismatch: %+v", got.TextLiterals)
	}
}

func TestApplyInsertElementAddsRuleToFilter(t *testing.T) {
	// The fixture rule is a synthetic example exercising the engine, not a
	// real testbed substitution. It must not be interpreted as policy.
	const fixtureDescr = "configxform-engine-fixture-allow-icmp"
	subs := Substitutions{
		InsertElements: []ElementInsert{
			{
				Name:        "fixture: allow icmp on opt9 (engine-test only)",
				ParentXPath: "//opnsense/filter",
				XML: `<rule uuid="configxform-engine-fixture">` +
					`<type>pass</type>` +
					`<interface>opt9</interface>` +
					`<protocol>icmp</protocol>` +
					`<descr>` + fixtureDescr + `</descr>` +
					`<source><network>opt9</network></source>` +
					`<destination><network>opt9ip</network></destination>` +
					`</rule>`,
			},
		},
	}
	out, err := Apply(loadFixture(t), subs)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	doc := mustParse(t, out)
	rules := doc.FindElements(`//opnsense/filter/rule`)
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules after insert, got %d", len(rules))
	}
	inserted := doc.FindElement(`//opnsense/filter/rule[descr]`)
	var found bool
	for _, el := range rules {
		descr := el.FindElement("descr")
		if descr != nil && strings.Contains(descr.Text(), fixtureDescr) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("inserted rule with fixture descr not found; last rule descr: %v",
			inserted)
	}
}

func TestApplyInsertElementEmptyParentXPathReturnsError(t *testing.T) {
	subs := Substitutions{
		InsertElements: []ElementInsert{
			{Name: "broken", ParentXPath: "", XML: "<rule/>"},
		},
	}
	if _, err := Apply(loadFixture(t), subs); err == nil {
		t.Fatal("Apply with empty parent_xpath returned no error")
	}
}

func TestApplyInsertElementEmptyXMLReturnsError(t *testing.T) {
	subs := Substitutions{
		InsertElements: []ElementInsert{
			{Name: "broken", ParentXPath: "//opnsense/filter", XML: ""},
		},
	}
	if _, err := Apply(loadFixture(t), subs); err == nil {
		t.Fatal("Apply with empty xml returned no error")
	}
}

func TestApplyInsertElementMalformedXMLReturnsError(t *testing.T) {
	subs := Substitutions{
		InsertElements: []ElementInsert{
			{Name: "broken", ParentXPath: "//opnsense/filter", XML: "<rule><unclosed>"},
		},
	}
	if _, err := Apply(loadFixture(t), subs); err == nil {
		t.Fatal("Apply with malformed xml fragment returned no error")
	}
}

func TestApplyInsertElementNoMatchIsTolerated(t *testing.T) {
	subs := Substitutions{
		InsertElements: []ElementInsert{
			{Name: "no match", ParentXPath: "//opnsense/does-not-exist", XML: "<rule/>"},
		},
	}
	if _, err := Apply(loadFixture(t), subs); err != nil {
		t.Fatalf("Apply with no-match parent_xpath should not error, got: %v", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join("testdata", "does-not-exist.yaml")); err == nil {
		t.Fatal("Load on missing file returned no error")
	}
}

// TestApplyDomainLiteralNoDoublePrefix verifies that the domain literal rewrite
// only applies once.
func TestApplyDomainLiteralNoDoublePrefix(t *testing.T) {
	out, err := Apply(loadFixture(t), defaultSubs())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	doc := mustParse(t, out)

	// <domain> must be rewritten exactly once by the text_literal.
	domainEl := doc.FindElement("//opnsense/system/domain")
	if domainEl == nil {
		t.Fatal("xpath //opnsense/system/domain matched no element")
	}
	gotDomain := strings.TrimSpace(domainEl.Text())
	if gotDomain != "suburban.goodkind.io" {
		t.Errorf("<domain> got %q, want %q", gotDomain, "suburban.goodkind.io")
	}

	// The dnssearchdomain embedded copy must also be rewritten exactly once.
	dsEl := doc.FindElement("//opnsense/system/dnssearchdomain")
	if dsEl == nil {
		t.Fatal("xpath //opnsense/system/dnssearchdomain matched no element")
	}
	gotDS := strings.TrimSpace(dsEl.Text())
	if gotDS != "router.suburban.goodkind.io" {
		t.Errorf("<dnssearchdomain> got %q, want %q", gotDS, "router.suburban.goodkind.io")
	}

	// No doubled prefix anywhere in the output.
	if strings.Contains(string(out), "suburban.suburban.goodkind.io") {
		t.Error("output contains doubled suburban domain")
	}
}

func TestApplyDeviceNameRejectsEmptyMapping(t *testing.T) {
	subs := Substitutions{
		DeviceNames: []DeviceNameMapping{{Name: "broken", From: "", To: "vtnet0"}},
	}
	if _, err := Apply(loadFixture(t), subs); err == nil {
		t.Fatal("Apply with empty From mapping returned no error")
	}
}

func TestApplyXPathSetRejectsEmptyXPath(t *testing.T) {
	subs := Substitutions{
		XPathSets: []XPathSet{{Name: "broken", XPath: "", NewValue: "x"}},
	}
	if _, err := Apply(loadFixture(t), subs); err == nil {
		t.Fatal("Apply with empty XPath returned no error")
	}
}

func TestApplyXPathSetMissingMatchIsTolerated(t *testing.T) {
	subs := Substitutions{
		XPathSets: []XPathSet{
			{Name: "no match", XPath: "//opnsense/system/this-element-does-not-exist", NewValue: "value"},
		},
	}
	if _, err := Apply(loadFixture(t), subs); err != nil {
		t.Fatalf("Apply with no-match XPath should not error, got: %v", err)
	}
}
