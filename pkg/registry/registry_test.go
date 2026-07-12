package registry

import (
	"net/http"
	"testing"
)

// ─── parseDestination ────────────────────────────────────────────────────────

func TestParseDestination_Valid(t *testing.T) {
	host, name, tag, err := parseDestination("10.96.0.1:5000/bwa:v0.7.17")
	if err != nil {
		t.Fatalf("parseDestination: %v", err)
	}
	if host != "10.96.0.1:5000" {
		t.Errorf("host: got %q", host)
	}
	if name != "bwa" {
		t.Errorf("name: got %q", name)
	}
	if tag != "v0.7.17" {
		t.Errorf("tag: got %q", tag)
	}
}

func TestParseDestination_NoSlash(t *testing.T) {
	_, _, _, err := parseDestination("nodestination")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestParseDestination_NoColon(t *testing.T) {
	_, _, _, err := parseDestination("host/imagewithoutcolontag")
	if err == nil {
		t.Fatal("expected error")
	}
}

// ─── NewClient ───────────────────────────────────────────────────────────────

func TestNewClient_UsesDefaultHTTPClient(t *testing.T) {
	c := NewClient()
	if c.http != http.DefaultClient {
		t.Error("NewClient should use http.DefaultClient")
	}
}
