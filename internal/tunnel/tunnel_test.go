package tunnel

import (
	"context"
	"testing"
	"time"
)

func TestFindPhysicalLANIPs(t *testing.T) {
	addrs := findPhysicalLANIPs()
	t.Logf("Found %d physical LAN address(es): %v", len(addrs), addrs)
}

func TestResolveHost(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ip, err := resolveHost(ctx, "free.pinggy.io")
	if err != nil {
		t.Logf("resolveHost failed (might be offline): %v", err)
	} else {
		t.Logf("resolveHost successfully resolved free.pinggy.io to: %s", ip)
		if ip == "" {
			t.Errorf("expected non-empty IP")
		}
	}
}

func TestManagerSwitch(t *testing.T) {
	mgr := NewManager(Config{LocalPort: 9000, Mode: "auto"}, func(s, u, e, p string) {})
	if mgr.CurrentProvider() != "" {
		t.Errorf("expected empty current provider initially, got %s", mgr.CurrentProvider())
	}

	mgr.setCur("Pinggy")
	if mgr.CurrentProvider() != "Pinggy" {
		t.Errorf("expected Pinggy, got %s", mgr.CurrentProvider())
	}

	next, err := mgr.SwitchNext()
	if err != nil {
		t.Fatalf("SwitchNext error: %v", err)
	}
	if next != "cloudflare" {
		t.Errorf("expected cloudflare after Pinggy, got %s", next)
	}

	mgr.setCur("Cloudflare")
	next2, err := mgr.SwitchNext()
	if err != nil {
		t.Fatalf("SwitchNext error: %v", err)
	}
	if next2 != "pinggy" {
		t.Errorf("expected pinggy after Cloudflare, got %s", next2)
	}
}
