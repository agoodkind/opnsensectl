package configxform

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"gopkg.in/yaml.v3"
)

// Substitutions is the operator-supplied table that drives the transform.
// The YAML structure is:
//
//	device_names:
//	  - name: "iavf0 -> vtnet0"
//	    from: "iavf0"
//	    to:   "vtnet0"
//	xpath_sets:
//	  - name: "WAN ipv4"
//	    xpath: "//opnsense/interfaces/wan/ipaddr"
//	    new_value: "10.240.250.2"
//	remove_elements:
//	  - name: "wireguard peers"
//	    xpath: "//opnsense/OPNsense/wireguard/client/clients/client"
//	insert_elements:
//	  - name: "SSH pass rule on MANAGEMENT"
//	    parent_xpath: "//opnsense/filter"
//	    xml: "<rule>...</rule>"
//	text_literals:
//	  - name: "v6 prefix shift 3d06:bad:b01:1:: -> 3d06:bad:b01:201::"
//	    from: "3d06:bad:b01:1::"
//	    to:   "3d06:bad:b01:201::"
type Substitutions struct {
	DeviceNames    []DeviceNameMapping `yaml:"device_names"`
	XPathSets      []XPathSet          `yaml:"xpath_sets"`
	RemoveElements []ElementRemove     `yaml:"remove_elements"`
	InsertElements []ElementInsert     `yaml:"insert_elements"`
	TextLiterals   []TextLiteral       `yaml:"text_literals"`
}

// DeviceNameMapping rewrites the text of every <if> element whose value
// equals From to the value To. This covers spec section 2 (iavf0 -> vtnet0
// across MANAGEMENT and the four VLAN parents).
type DeviceNameMapping struct {
	Name string `yaml:"name"`
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

// XPathSet writes NewValue as the text of every element selected by XPath.
// Example: //opnsense/interfaces/wan/ipaddr to set the WAN IPv4 address.
type XPathSet struct {
	Name     string `yaml:"name"`
	XPath    string `yaml:"xpath"`
	NewValue string `yaml:"new_value"`
}

// ElementRemove deletes every element selected by XPath. Used to strip prod
// only blocks like WireGuard peers (spec section 3.5).
type ElementRemove struct {
	Name  string `yaml:"name"`
	XPath string `yaml:"xpath"`
}

// ElementInsert appends a new XML child element to every element selected by
// ParentXPath. XML must be a single well-formed XML element fragment (no XML
// declaration). If ParentXPath matches more than one element, the fragment is
// appended to each match. Used to inject testbed-only rules that should not
// exist in the prod config (e.g. an SSH pass rule on MANAGEMENT).
type ElementInsert struct {
	Name        string `yaml:"name"`
	ParentXPath string `yaml:"parent_xpath"`
	XML         string `yaml:"xml"`
}

// TextLiteral is a byte-level substitution applied to the serialized XML.
// Used for values that may appear in many unrelated contexts (IP literals
// embedded in source_networks, hostnames in certificate altNames, captive
// portal hostnames).
type TextLiteral struct {
	Name string `yaml:"name"`
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

// Load reads a YAML file at path and decodes it into Substitutions.
// It returns a clear error on missing path, malformed YAML, or unknown fields.
func Load(path string) (Substitutions, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Error("configxform: load substitutions read failed", "path", path, "err", err)
		return Substitutions{}, fmt.Errorf("configxform.Load: read %q: %w", path, err)
	}
	subs, err := Decode(data)
	if err != nil {
		slog.Error("configxform: load substitutions decode failed", "path", path, "err", err)
		return Substitutions{}, err
	}
	return subs, nil
}

// Decode parses YAML bytes into Substitutions. Strict mode rejects unknown
// fields so a typo in the YAML key surfaces as an error instead of silently
// being dropped.
func Decode(data []byte) (Substitutions, error) {
	if len(data) == 0 {
		err := errors.New("configxform.Decode: empty YAML input")
		slog.Error("configxform: decode rejected empty input", "err", err)
		return Substitutions{}, err
	}
	var subs Substitutions
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&subs); err != nil {
		slog.Error("configxform: decode parse YAML failed", "err", err)
		return Substitutions{}, fmt.Errorf("configxform.Decode: parse YAML: %w", err)
	}
	return subs, nil
}
