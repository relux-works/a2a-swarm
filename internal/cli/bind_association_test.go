package cli

import (
	"os"
	"strings"
	"testing"
)

func TestAssociationUsesBindingMachineLabel(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"Synthetic-MacBook-Pro.local", "Synthetic-MacBook-Pro.local"},
		{"  Work\tStation\r\n.local  ", "Work Station .local"},
		{"Desk\x00\x1bTop.local", "DeskTop.local"},
		{"Рабочий-Mac.local", "Рабочий-Mac.local"},
		{strings.Repeat("H", 80) + ".local", strings.Repeat("H", 80) + ".local"},
		{"\x00\n\t", "unknown"},
		{"", "unknown"},
	} {
		if got := machineLabelFromHostname(tc.raw); got != tc.want {
			t.Fatalf("machine label: got %q want %q", got, tc.want)
		}
	}
	h, _ := os.Hostname()
	if got := machineLabel(); got != machineLabelFromHostname(h) {
		t.Fatal("binding machine label semantics diverged")
	}
}

func TestAssociationInvocationBoundary(t *testing.T) {
	for _, tc := range []struct {
		args        []string
		association bool
	}{
		{[]string{"--profile", "bind", "association", "--json"}, false},
		{[]string{"post", "bind", "association"}, false},
		{[]string{"--", "bind", "association"}, false},
		{[]string{"--as=synthetic-own", "bind", "association", "--json"}, true},
		{[]string{"bind", "--profile=one", "association", "--profile=two", "--json"}, true},
		{[]string{"bind", "association"}, true},
	} {
		if got := associationInvocation(tc.args) != nil; got != tc.association {
			t.Fatalf("%q: got %v", tc.args, got)
		}
	}
}
