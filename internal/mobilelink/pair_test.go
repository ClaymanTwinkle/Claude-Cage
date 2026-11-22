package mobilelink

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/relay"
	"github.com/sylvinus/agent-vm/internal/remotelink"
)

func TestScanEnrollsOnlyHolderOfOriginalCode(t *testing.T) {
	const admin = "private-test-credential-for-custom-relay"
	service, err := relay.New(relay.Config{AdminToken: admin})
	if err != nil {
		t.Fatal(err)
	}
	edge := httptest.NewServer(service)
	defer edge.Close()
	defer service.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	desktop, err := remotelink.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	phone, err := remotelink.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	// An empty selected relay is a phone with no relay setting: it follows the
	// relay carried by the optical code.
	for _, tc := range []struct {
		tampered bool
		selected string
	}{{true, edge.URL}, {false, edge.URL}, {false, ""}} {
		tampered := tc.tampered
		room, err := CreateRoom(ctx, edge.URL, admin)
		if err != nil {
			t.Fatal(err)
		}
		code, raw, err := NewPairCode(edge.URL, room, desktop.Pin)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		var enrolled string
		go func() {
			done <- AcceptPair(ctx, code, room.HostToken, desktop, func(pin string) (PairedHost, error) {
				enrolled = pin
				return PairedHost{1, edge.URL, desktop.Pin, strings.Repeat("A", 43)}, nil
			})
		}()
		if _, err := RedeemPair(ctx, raw, "https://other-relay.example", phone.Pin); err == nil {
			t.Fatal("accepted unexpected relay")
		}
		if tampered {
			changed := code
			changed.Seed = append([]byte(nil), code.Seed...)
			changed.Seed[0] ^= 1
			payload, _ := json.Marshal(changed)
			raw = "CAGE1:" + string(payload)
		}
		got, clientErr := RedeemPair(ctx, raw, tc.selected, phone.Pin)
		hostErr := <-done
		if tampered {
			if enrolled != "" || hostErr == nil || clientErr == nil {
				t.Fatal("modified optical secret enrolled a device")
			}
			continue
		}
		var saved PairedHost
		if hostErr != nil || clientErr != nil || enrolled != phone.Pin || json.Unmarshal([]byte(got), &saved) != nil || saved.HostPin != desktop.Pin {
			t.Fatal("valid optical pairing failed", hostErr, clientErr)
		}
		expired := code
		expired.Expires = time.Now().Add(-time.Second).Unix()
		if _, err = expired.identity(); err == nil {
			t.Fatal("expired optical pairing accepted")
		}
	}
}

func TestRelayResolution(t *testing.T) {
	defer func(old string) { DefaultRelay = old }(DefaultRelay)
	DefaultRelay = ""
	if _, err := ResolveRelay(""); err == nil {
		t.Fatal("resolved a relay without configuration or build default")
	}
	if got, err := ResolveRelay("https://relay.example"); err != nil || got != "https://relay.example" {
		t.Fatalf("configured relay = %q, %v", got, err)
	}
	if (PairCode{}).origin() != "" {
		t.Fatal("legacy code without relay resolved to a relay with no build default")
	}
	DefaultRelay = "https://default.example"
	if got, err := ResolveRelay(""); err != nil || got != DefaultRelay {
		t.Fatalf("default relay = %q, %v", got, err)
	}
	if (PairCode{}).origin() != DefaultRelay {
		t.Fatal("legacy code without relay did not use the build default")
	}
	code, _, err := NewPairCode(DefaultRelay, relay.Invitation{ID: "room", ClientToken: "token", ExpiresAt: time.Now().Add(time.Minute)}, "sha256:x")
	if err != nil || code.Relay != DefaultRelay {
		t.Fatalf("pairing code relay = %q, %v", code.Relay, err)
	}
	DefaultRelay = "http://not-https.example"
	if _, err := ResolveRelay(""); err == nil {
		t.Fatal("accepted an invalid build default")
	}
}
