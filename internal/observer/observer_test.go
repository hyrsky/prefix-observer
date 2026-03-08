package observer

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/hyrsky/prefix-observer/internal/dhcpv6"
)

// --- test doubles ---

type stubPatcher struct {
	mu          sync.Mutex
	ipPoolCalls []ipPoolCall
	gatewayCalls []gatewayCall
	ipPoolErr   error
	gatewayErr  error
}

type ipPoolCall struct {
	Name    string
	GUACIDR string
}

type gatewayCall struct {
	Namespace string
	Name      string
	GUAAddr   string
}

func (s *stubPatcher) PatchIPPool(_ context.Context, name, guaCIDR string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ipPoolCalls = append(s.ipPoolCalls, ipPoolCall{name, guaCIDR})
	return s.ipPoolErr
}

func (s *stubPatcher) PatchGateway(_ context.Context, namespace, name, guaAddr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gatewayCalls = append(s.gatewayCalls, gatewayCall{namespace, name, guaAddr})
	return s.gatewayErr
}

type stubClient struct {
	mu            sync.Mutex
	requestResult *dhcpv6.PrefixResult
	requestErr    error
	requestCalls  int
	renewResult   *dhcpv6.PrefixResult
	renewErr      error
	renewCalls    int
}

func (s *stubClient) RequestPrefix(_ context.Context) (*dhcpv6.PrefixResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requestCalls++
	return s.requestResult, s.requestErr
}

func (s *stubClient) RenewPrefix(_ context.Context, _ *dhcpv6.PrefixResult) (*dhcpv6.PrefixResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renewCalls++
	return s.renewResult, s.renewErr
}

func noWait(_ context.Context, _ string) error { return nil }

func testConfig() Config {
	return Config{
		DHCPv6Interface:  "eth0",
		LBPrefixLength:   112,
		GatewayHostID:    "::64",
		IPPoolName:       "pool",
		GatewayName:      "gw",
		GatewayNamespace: "ns",
	}
}

// --- tests ---

func TestApplyPrefix(t *testing.T) {
	patcher := &stubPatcher{}
	client := &stubClient{}
	obs := New(testConfig(), patcher, client, noWait)

	prefix := netip.MustParsePrefix("2001:db8:abcd::/48")
	err := obs.applyPrefix(context.Background(), prefix)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(patcher.ipPoolCalls) != 1 {
		t.Fatalf("expected 1 PatchIPPool call, got %d", len(patcher.ipPoolCalls))
	}
	if patcher.ipPoolCalls[0].GUACIDR != "2001:db8:abcd::/112" {
		t.Errorf("PatchIPPool CIDR = %q, want %q", patcher.ipPoolCalls[0].GUACIDR, "2001:db8:abcd::/112")
	}
	if patcher.ipPoolCalls[0].Name != "pool" {
		t.Errorf("PatchIPPool name = %q, want %q", patcher.ipPoolCalls[0].Name, "pool")
	}

	if len(patcher.gatewayCalls) != 1 {
		t.Fatalf("expected 1 PatchGateway call, got %d", len(patcher.gatewayCalls))
	}
	if patcher.gatewayCalls[0].GUAAddr != "2001:db8:abcd::64" {
		t.Errorf("PatchGateway addr = %q, want %q", patcher.gatewayCalls[0].GUAAddr, "2001:db8:abcd::64")
	}
	if patcher.gatewayCalls[0].Namespace != "ns" {
		t.Errorf("PatchGateway namespace = %q, want %q", patcher.gatewayCalls[0].Namespace, "ns")
	}

	if obs.current != prefix {
		t.Errorf("current prefix = %s, want %s", obs.current, prefix)
	}
}

func TestApplyPrefixIPPoolError(t *testing.T) {
	patcher := &stubPatcher{ipPoolErr: errors.New("api error")}
	client := &stubClient{}
	obs := New(testConfig(), patcher, client, noWait)

	err := obs.applyPrefix(context.Background(), netip.MustParsePrefix("2001:db8::/48"))
	if err == nil {
		t.Fatal("expected error")
	}
	// Gateway should not be called if IP pool fails
	if len(patcher.gatewayCalls) != 0 {
		t.Error("PatchGateway should not be called when PatchIPPool fails")
	}
}

func TestApplyPrefixGatewayError(t *testing.T) {
	patcher := &stubPatcher{gatewayErr: errors.New("api error")}
	client := &stubClient{}
	obs := New(testConfig(), patcher, client, noWait)

	err := obs.applyPrefix(context.Background(), netip.MustParsePrefix("2001:db8::/48"))
	if err == nil {
		t.Fatal("expected error")
	}
	// IP pool should have been called
	if len(patcher.ipPoolCalls) != 1 {
		t.Error("PatchIPPool should be called before PatchGateway fails")
	}
}

func TestRequestWithRetry(t *testing.T) {
	t.Run("succeeds on first attempt", func(t *testing.T) {
		result := &dhcpv6.PrefixResult{
			Prefix: netip.MustParsePrefix("2001:db8::/48"),
			T1:     300 * time.Second,
		}
		client := &stubClient{requestResult: result}
		obs := New(testConfig(), &stubPatcher{}, client, noWait)

		got, err := obs.requestWithRetry(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Prefix != result.Prefix {
			t.Errorf("prefix = %s, want %s", got.Prefix, result.Prefix)
		}
		if client.requestCalls != 1 {
			t.Errorf("expected 1 call, got %d", client.requestCalls)
		}
	})

	t.Run("fails after all retries", func(t *testing.T) {
		client := &stubClient{requestErr: errors.New("timeout")}
		obs := New(testConfig(), &stubPatcher{}, client, noWait)

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		_, err := obs.requestWithRetry(ctx)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestRenewWithRetry(t *testing.T) {
	t.Run("succeeds on first attempt", func(t *testing.T) {
		result := &dhcpv6.PrefixResult{
			Prefix: netip.MustParsePrefix("2001:db8::/48"),
			T1:     300 * time.Second,
		}
		client := &stubClient{renewResult: result}
		obs := New(testConfig(), &stubPatcher{}, client, noWait)

		current := &dhcpv6.PrefixResult{Prefix: netip.MustParsePrefix("2001:db8::/48")}
		got, err := obs.renewWithRetry(context.Background(), current)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Prefix != result.Prefix {
			t.Errorf("prefix = %s, want %s", got.Prefix, result.Prefix)
		}
		if client.renewCalls != 1 {
			t.Errorf("expected 1 call, got %d", client.renewCalls)
		}
	})

	t.Run("fails after all retries", func(t *testing.T) {
		client := &stubClient{renewErr: errors.New("timeout")}
		obs := New(testConfig(), &stubPatcher{}, client, noWait)

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		_, err := obs.renewWithRetry(ctx, &dhcpv6.PrefixResult{})
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestRunInterfaceWaitError(t *testing.T) {
	waitErr := func(_ context.Context, _ string) error {
		return errors.New("no interface")
	}
	obs := New(testConfig(), &stubPatcher{}, &stubClient{}, waitErr)

	err := obs.Run(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRunAppliesInitialPrefix(t *testing.T) {
	prefix := netip.MustParsePrefix("2001:db8:abcd::/48")
	client := &stubClient{
		requestResult: &dhcpv6.PrefixResult{
			Prefix: prefix,
			T1:     time.Hour, // long T1 so renewal doesn't trigger
		},
	}
	patcher := &stubPatcher{}

	obs := New(testConfig(), patcher, client, noWait)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- obs.Run(ctx) }()

	// Give Run time to apply the initial prefix, then cancel
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if len(patcher.ipPoolCalls) != 1 {
		t.Fatalf("expected 1 PatchIPPool call, got %d", len(patcher.ipPoolCalls))
	}
	if patcher.ipPoolCalls[0].GUACIDR != "2001:db8:abcd::/112" {
		t.Errorf("CIDR = %q, want %q", patcher.ipPoolCalls[0].GUACIDR, "2001:db8:abcd::/112")
	}
	if patcher.gatewayCalls[0].GUAAddr != "2001:db8:abcd::64" {
		t.Errorf("GUA addr = %q, want %q", patcher.gatewayCalls[0].GUAAddr, "2001:db8:abcd::64")
	}
}

// --- pure function tests ---

func TestComputeGatewayAddr(t *testing.T) {
	tests := []struct {
		name    string
		prefix  string
		hostID  string
		want    string
		wantErr bool
	}{
		{
			name:   "basic prefix with ::64 host ID",
			prefix: "2001:db8:abcd::/48",
			hostID: "::64",
			want:   "2001:db8:abcd::64",
		},
		{
			name:   "prefix with larger host ID",
			prefix: "2a01:1234:5678::/48",
			hostID: "::1:0:0:1",
			want:   "2a01:1234:5678:0:1::1",
		},
		{
			name:   "/56 prefix with host ID",
			prefix: "2001:db8:ab00::/56",
			hostID: "::ff",
			want:   "2001:db8:ab00::ff",
		},
		{
			name:    "invalid host ID",
			prefix:  "2001:db8::/32",
			hostID:  "not-an-address",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prefix := netip.MustParsePrefix(tt.prefix)
			got, err := computeGatewayAddr(prefix, tt.hostID)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.String() != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestEnvOr(t *testing.T) {
	t.Run("returns env value when set", func(t *testing.T) {
		t.Setenv("TEST_ENV_OR", "custom")
		if got := envOr("TEST_ENV_OR", "default"); got != "custom" {
			t.Errorf("got %q, want %q", got, "custom")
		}
	})

	t.Run("returns default when unset", func(t *testing.T) {
		t.Setenv("TEST_ENV_OR_MISSING", "")
		if got := envOr("TEST_ENV_OR_MISSING", "default"); got != "default" {
			t.Errorf("got %q, want %q", got, "default")
		}
	})
}

func TestEnvOrInt(t *testing.T) {
	t.Run("returns parsed int", func(t *testing.T) {
		t.Setenv("TEST_ENV_INT", "42")
		if got := envOrInt("TEST_ENV_INT", 10); got != 42 {
			t.Errorf("got %d, want 42", got)
		}
	})

	t.Run("returns default for unset", func(t *testing.T) {
		t.Setenv("TEST_ENV_INT_MISSING", "")
		if got := envOrInt("TEST_ENV_INT_MISSING", 10); got != 10 {
			t.Errorf("got %d, want 10", got)
		}
	})

	t.Run("returns default for invalid", func(t *testing.T) {
		t.Setenv("TEST_ENV_INT_BAD", "abc")
		if got := envOrInt("TEST_ENV_INT_BAD", 10); got != 10 {
			t.Errorf("got %d, want 10", got)
		}
	})

	t.Run("trims whitespace", func(t *testing.T) {
		t.Setenv("TEST_ENV_INT_WS", "  99  ")
		if got := envOrInt("TEST_ENV_INT_WS", 10); got != 99 {
			t.Errorf("got %d, want 99", got)
		}
	})
}

func TestConfigFromEnv(t *testing.T) {
	t.Run("returns config with all vars set", func(t *testing.T) {
		t.Setenv("DHCPV6_INTERFACE", "eth0")
		t.Setenv("GATEWAY_HOST_ID", "::64")
		t.Setenv("IPPOOL_NAME", "my-pool")
		t.Setenv("GATEWAY_NAME", "my-gw")
		t.Setenv("GATEWAY_NAMESPACE", "default")
		t.Setenv("LB_PREFIX_LENGTH", "112")
		t.Setenv("DRY_RUN", "true")

		cfg, err := ConfigFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.DHCPv6Interface != "eth0" {
			t.Errorf("DHCPv6Interface = %q, want %q", cfg.DHCPv6Interface, "eth0")
		}
		if cfg.GatewayHostID != "::64" {
			t.Errorf("GatewayHostID = %q, want %q", cfg.GatewayHostID, "::64")
		}
		if cfg.LBPrefixLength != 112 {
			t.Errorf("LBPrefixLength = %d, want 112", cfg.LBPrefixLength)
		}
		if !cfg.DryRun {
			t.Error("DryRun = false, want true")
		}
	})

	t.Run("uses defaults for optional vars", func(t *testing.T) {
		t.Setenv("DHCPV6_INTERFACE", "eth0")
		t.Setenv("GATEWAY_HOST_ID", "::64")
		t.Setenv("IPPOOL_NAME", "my-pool")
		t.Setenv("GATEWAY_NAME", "my-gw")
		t.Setenv("GATEWAY_NAMESPACE", "default")
		t.Setenv("LB_PREFIX_LENGTH", "")
		t.Setenv("DRY_RUN", "")

		cfg, err := ConfigFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.LBPrefixLength != 112 {
			t.Errorf("LBPrefixLength = %d, want default 112", cfg.LBPrefixLength)
		}
		if cfg.DryRun {
			t.Error("DryRun = true, want default false")
		}
	})

	t.Run("errors on missing required vars", func(t *testing.T) {
		t.Setenv("DHCPV6_INTERFACE", "")
		t.Setenv("GATEWAY_HOST_ID", "")
		t.Setenv("IPPOOL_NAME", "")
		t.Setenv("GATEWAY_NAME", "")
		t.Setenv("GATEWAY_NAMESPACE", "")

		_, err := ConfigFromEnv()
		if err == nil {
			t.Fatal("expected error for missing vars, got nil")
		}
	})
}
