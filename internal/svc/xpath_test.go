package svc

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestXPathGet_Hostname(t *testing.T) {
	got, err := xpathGetWithLog(context.Background(), nil, []byte(sampleConfig), "//opnsense/system/hostname")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 match, got %d", len(got))
	}
	if !strings.Contains(got[0], "opnsense") {
		t.Fatalf("unexpected match: %q", got[0])
	}
}

func TestXPathGet_Multiple(t *testing.T) {
	got, err := xpathGetWithLog(context.Background(), nil, []byte(sampleConfig), "//opnsense/interfaces/*/if")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 matches (wan+lan if), got %d: %v", len(got), got)
	}
}

func TestXPathGet_NoMatch(t *testing.T) {
	got, err := xpathGetWithLog(context.Background(), nil, []byte(sampleConfig), "//opnsense/nonexistent/key")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 matches, got %d", len(got))
	}
}

func TestXPathGet_EmptyExpr(t *testing.T) {
	if _, err := xpathGetWithLog(context.Background(), nil, []byte(sampleConfig), ""); err == nil {
		t.Fatal("expected error on empty expr")
	}
}

func TestXPathSet_ChangesValue(t *testing.T) {
	out, n, err := xpathSetWithLog(context.Background(), nil, []byte(sampleConfig), "//opnsense/system/hostname", "newhost")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 change, got %d", n)
	}
	if !bytes.Contains(out, []byte("<hostname>newhost</hostname>")) {
		t.Fatalf("set did not apply:\n%s", out)
	}
}

func TestXPathSet_NoMatch(t *testing.T) {
	out, n, err := xpathSetWithLog(context.Background(), nil, []byte(sampleConfig), "//opnsense/nonexistent", "x")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected 0 changes, got %d", n)
	}
	if !bytes.Equal(out, []byte(sampleConfig)) {
		t.Fatal("no-op should return original bytes")
	}
}

func TestXPathDelete_RemovesNode(t *testing.T) {
	out, n, err := xpathDeleteWithLog(context.Background(), nil, []byte(sampleConfig), "//opnsense/interfaces/wan/gatewayv6")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 delete, got %d", n)
	}
	if bytes.Contains(out, []byte("gatewayv6")) {
		t.Fatalf("delete left gatewayv6 in output:\n%s", out)
	}
}

func TestXPathDelete_NoMatch(t *testing.T) {
	out, n, err := xpathDeleteWithLog(context.Background(), nil, []byte(sampleConfig), "//opnsense/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected 0 deletes, got %d", n)
	}
	if !bytes.Equal(out, []byte(sampleConfig)) {
		t.Fatal("no-op should return original bytes")
	}
}

func TestXPathSet_Then_XPathGet_RoundTrip(t *testing.T) {
	out, _, err := xpathSetWithLog(context.Background(), nil, []byte(sampleConfig), "//opnsense/system/hostname", "rt-host")
	if err != nil {
		t.Fatal(err)
	}
	got, err := xpathGetWithLog(context.Background(), nil, out, "//opnsense/system/hostname")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0], "rt-host") {
		t.Fatalf("round-trip mismatch: %v", got)
	}
}

func TestNodeToString_NotEmpty(t *testing.T) {
	got, err := xpathGetWithLog(context.Background(), nil, []byte(sampleConfig), "//opnsense/interfaces/wan")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 wan match, got %d", len(got))
	}
	if !strings.Contains(got[0], "<wan>") || !strings.Contains(got[0], "</wan>") {
		t.Fatalf("element serialization missing wrapping tags: %s", got[0])
	}
}
