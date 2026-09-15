package main

import "testing"

func TestInvokedAsOPNsenseDaemon(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		argv string
		want bool
	}{
		{name: "symlink", argv: "/usr/local/sbin/mwan-opnsense", want: true},
		{name: "current", argv: "/usr/local/sbin/mwan-opnsense.current", want: true},
		{name: "host bridge", argv: "/usr/local/bin/mwan-opnsense-host", want: false},
		{name: "monolith", argv: "/usr/local/bin/mwan", want: false},
		{name: "cli", argv: "/usr/local/bin/opnsensectl", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := invokedAsOPNsenseDaemon(tt.argv); got != tt.want {
				t.Fatalf("invokedAsOPNsenseDaemon(%q) = %v, want %v", tt.argv, got, tt.want)
			}
		})
	}
}
