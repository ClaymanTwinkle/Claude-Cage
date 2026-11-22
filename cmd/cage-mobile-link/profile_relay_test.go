package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sylvinus/agent-vm/internal/mobilelink"
)

func TestNewPairingUsesDesktopRelay(t *testing.T) {
	defer func(old string) { mobilelink.DefaultRelay = old }(mobilelink.DefaultRelay)
	mobilelink.DefaultRelay = "https://default-relay.example"
	dir := t.TempDir()
	config := filepath.Join(dir, "config.json")
	prefs := filepath.Join(dir, "workbench.json")
	for _, tc := range []struct{ name, json, want string }{
		{"new profile", "", mobilelink.DefaultRelay},
		{"existing profile", `{"keyboard":"mac"}`, mobilelink.DefaultRelay},
		{"custom relay", `{"mobile_relay":"https://relay.example:8443"}`, "https://relay.example:8443"},
		{"damaged preferences", "{", ""},
		{"insecure relay", `{"mobile_relay":"http://relay.example"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.json != "" {
				if err := os.WriteFile(prefs, []byte(tc.json), 0600); err != nil {
					t.Fatal(err)
				}
			}
			origin, err := profileRelay(config)
			if origin != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("origin = %q, error = %v", origin, err)
			}
		})
	}
}

func TestNewPairingWithoutDefaultRelay(t *testing.T) {
	defer func(old string) { mobilelink.DefaultRelay = old }(mobilelink.DefaultRelay)
	mobilelink.DefaultRelay = ""
	dir := t.TempDir()
	config := filepath.Join(dir, "config.json")
	if origin, err := profileRelay(config); err == nil {
		t.Fatalf("origin = %q without a configured or default relay", origin)
	}
	if err := os.WriteFile(filepath.Join(dir, "workbench.json"), []byte(`{"mobile_relay":"https://relay.example"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if origin, err := profileRelay(config); err != nil || origin != "https://relay.example" {
		t.Fatalf("origin = %q, error = %v", origin, err)
	}
}
