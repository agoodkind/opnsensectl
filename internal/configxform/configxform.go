// Package configxform implements the prod-to-testbed transform for OPNsense
// config.xml. The transform consumes a redacted prod config.xml, an operator
// supplied substitution table, and produces a testbed-shaped config.xml.
//
// Structural rewrites use an XML-aware walker where element identity matters
// (interfaces, VLANs, hostname, domain, peers, NAT64 prefix). Textual literal
// substitutions cover values that may appear in many places, such as IP
// addresses, hostnames embedded in certificate names, and captive portal
// references.
//
// This package is intentionally small. The substitution table is loaded from
// YAML at runtime by Load. Apply takes a parsed Substitutions value plus the
// raw XML bytes and returns the transformed bytes.
package configxform

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/beevik/etree"
)

// Apply transforms the provided OPNsense config.xml bytes using the supplied
// substitutions. It returns the transformed bytes. It does not mutate the
// caller's input slice.
//
// Apply parses the XML once, applies every structural rewrite using element
// path matching, then walks the serialized output and applies textual literal
// substitutions. The order matters: textual substitutions run last so they can
// catch any embedded copies of the prod literals that the structural rewrites
// did not visit.
func Apply(input []byte, subs Substitutions) ([]byte, error) {
	if len(input) == 0 {
		err := errors.New("configxform.Apply: empty input")
		slog.Error("configxform: apply rejected empty input", "err", err)
		return nil, err
	}

	doc := etree.NewDocument()
	if err := doc.ReadFromBytes(input); err != nil {
		slog.Error("configxform: parse input XML failed", "err", err)
		return nil, fmt.Errorf("configxform.Apply: parse input XML: %w", err)
	}

	if err := applyDeviceNames(doc, subs.DeviceNames); err != nil {
		slog.Error("configxform: apply device names failed", "err", err)
		return nil, fmt.Errorf("configxform.Apply: device names: %w", err)
	}
	if err := applyXPathSets(doc, subs.XPathSets); err != nil {
		slog.Error("configxform: apply xpath sets failed", "err", err)
		return nil, fmt.Errorf("configxform.Apply: xpath sets: %w", err)
	}
	if err := applyElementRemoves(doc, subs.RemoveElements); err != nil {
		slog.Error("configxform: apply element removes failed", "err", err)
		return nil, fmt.Errorf("configxform.Apply: remove elements: %w", err)
	}
	if err := applyInsertElements(doc, subs.InsertElements); err != nil {
		slog.Error("configxform: apply insert elements failed", "err", err)
		return nil, fmt.Errorf("configxform.Apply: insert elements: %w", err)
	}

	out, err := doc.WriteToBytes()
	if err != nil {
		slog.Error("configxform: serialize XML failed", "err", err)
		return nil, fmt.Errorf("configxform.Apply: serialize: %w", err)
	}

	out = applyTextLiterals(out, subs.TextLiterals)
	return out, nil
}

// applyDeviceNames rewrites the <if> child of every interface that currently
// references the prod device name to the testbed device name. Section 2 of
// the spec enumerates exactly five locations in the redacted prod artifact
// where iavf0 appears. The rule walks every <if> element under <interfaces>
// and <vlans> so any future addition is caught automatically.
func applyDeviceNames(doc *etree.Document, mappings []DeviceNameMapping) error {
	if len(mappings) == 0 {
		return nil
	}
	root := doc.FindElement("//opnsense")
	if root == nil {
		return errors.New("applyDeviceNames: no <opnsense> root")
	}
	ifElems := root.FindElements(".//if")
	for _, mapping := range mappings {
		from := strings.TrimSpace(mapping.From)
		to := strings.TrimSpace(mapping.To)
		if from == "" || to == "" {
			return fmt.Errorf("applyDeviceNames: empty from or to in mapping %+v", mapping)
		}
		for _, el := range ifElems {
			if strings.TrimSpace(el.Text()) == from {
				el.SetText(to)
			}
		}
	}
	return nil
}

// applyXPathSets writes a new text value to the element selected by each
// XPath expression. The XPath syntax is the subset that beevik/etree
// accepts. If the selector matches more than one element, every match is
// updated. If the selector matches no element, that mapping is skipped
// silently because the prod config may not contain every possible element.
func applyXPathSets(doc *etree.Document, sets []XPathSet) error {
	for _, set := range sets {
		if set.XPath == "" {
			return fmt.Errorf("applyXPathSets: empty xpath for entry %q", set.Name)
		}
		matches := doc.FindElements(set.XPath)
		for _, el := range matches {
			el.SetText(set.NewValue)
		}
	}
	return nil
}

// applyElementRemoves removes every element matched by the given XPath
// expressions. Useful for stripping prod-only blocks like WireGuard peers
// per spec section 3.5.
func applyElementRemoves(doc *etree.Document, removes []ElementRemove) error {
	for _, remove := range removes {
		if remove.XPath == "" {
			return fmt.Errorf("applyElementRemoves: empty xpath for entry %q", remove.Name)
		}
		matches := doc.FindElements(remove.XPath)
		for _, el := range matches {
			parent := el.Parent()
			if parent == nil {
				continue
			}
			parent.RemoveChild(el)
		}
	}
	return nil
}

// applyInsertElements appends a new XML child to every element selected by
// ParentXPath. The XML field must be a single well-formed element fragment
// with no XML declaration. If ParentXPath matches zero elements the entry is
// skipped silently, matching the silent-skip behaviour of applyXPathSets. If
// the fragment fails to parse, an error is returned immediately.
func applyInsertElements(doc *etree.Document, inserts []ElementInsert) error {
	for _, ins := range inserts {
		if ins.ParentXPath == "" {
			err := fmt.Errorf("applyInsertElements: empty parent_xpath for entry %q", ins.Name)
			slog.Error("configxform: insert element rejected empty parent_xpath", "name", ins.Name, "err", err)
			return err
		}
		if strings.TrimSpace(ins.XML) == "" {
			err := fmt.Errorf("applyInsertElements: empty xml for entry %q", ins.Name)
			slog.Error("configxform: insert element rejected empty xml", "name", ins.Name, "err", err)
			return err
		}
		parents := doc.FindElements(ins.ParentXPath)
		for _, parent := range parents {
			frag := etree.NewDocument()
			if err := frag.ReadFromString(ins.XML); err != nil {
				slog.Error("configxform: insert element xml fragment parse failed", "name", ins.Name, "err", err)
				return fmt.Errorf("applyInsertElements: parse xml fragment for %q: %w", ins.Name, err)
			}
			root := frag.Root()
			if root == nil {
				err := fmt.Errorf("applyInsertElements: xml fragment for %q has no root element", ins.Name)
				slog.Error("configxform: insert element xml fragment has no root", "name", ins.Name, "err", err)
				return err
			}
			parent.AddChild(root.Copy())
		}
	}
	return nil
}

// applyTextLiterals performs a byte-level replace for each entry. This is the
// fallback layer for prod literals that appear in many places (IP addresses
// embedded in <source_networks>, hostnames in certificate altNames, captive
// portal hostnames). The substitutions table lists these in spec sections 3.1
// through 3.7.
//
// Order matters within the slice: longer or more specific values must come
// before shorter ones so a substring rewrite does not accidentally split a
// longer literal into a corrupted value.
func applyTextLiterals(input []byte, literals []TextLiteral) []byte {
	if len(literals) == 0 {
		return input
	}
	out := input
	for _, literal := range literals {
		if literal.From == "" {
			continue
		}
		out = bytes.ReplaceAll(out, []byte(literal.From), []byte(literal.To))
	}
	return out
}
