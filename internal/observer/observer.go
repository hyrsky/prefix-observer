package observer

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hyrsky/prefix-observer/internal/dhcpv6"
)

// PrefixClient requests and renews DHCPv6 prefix delegations.
type PrefixClient interface {
	RequestPrefix(ctx context.Context) (*dhcpv6.PrefixResult, error)
	RenewPrefix(ctx context.Context, current *dhcpv6.PrefixResult) (*dhcpv6.PrefixResult, error)
}

// ResourcePatcher patches Kubernetes resources with prefix information.
type ResourcePatcher interface {
	PatchIPPool(ctx context.Context, name, guaCIDR string) error
	PatchGateway(ctx context.Context, namespace, name, guaAddr string) error
}

// InterfaceWaiter waits for a network interface to become ready.
type InterfaceWaiter func(ctx context.Context, name string) error

// Config holds the observer configuration.
type Config struct {
	DHCPv6Interface  string
	LBPrefixLength   int
	GatewayHostID    string
	IPPoolName       string
	GatewayName      string
	GatewayNamespace string
	DryRun           bool
}

// ConfigFromEnv reads configuration from environment variables.
// Returns an error if required variables are missing.
func ConfigFromEnv() (Config, error) {
	var missing []string
	requireEnv := func(key string) string {
		v := os.Getenv(key)
		if v == "" {
			missing = append(missing, key)
		}
		return v
	}

	cfg := Config{
		DHCPv6Interface:  requireEnv("DHCPV6_INTERFACE"),
		LBPrefixLength:   envOrInt("LB_PREFIX_LENGTH", 112),
		GatewayHostID:    requireEnv("GATEWAY_HOST_ID"),
		IPPoolName:       requireEnv("IPPOOL_NAME"),
		GatewayName:      requireEnv("GATEWAY_NAME"),
		GatewayNamespace: requireEnv("GATEWAY_NAMESPACE"),
		DryRun:           envOr("DRY_RUN", "false") == "true",
	}

	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	return cfg, nil
}

// Observer watches for DHCPv6-PD prefix changes and patches K8s resources.
type Observer struct {
	cfg           Config
	patcher       ResourcePatcher
	client        PrefixClient
	waitForIface  InterfaceWaiter
	current       netip.Prefix
}

// New creates a new Observer.
func New(cfg Config, patcher ResourcePatcher, client PrefixClient, waitForIface InterfaceWaiter) *Observer {
	return &Observer{
		cfg:          cfg,
		patcher:      patcher,
		client:       client,
		waitForIface: waitForIface,
	}
}

// Run starts the observer loop.
func (o *Observer) Run(ctx context.Context) error {
	if err := o.waitForIface(ctx, o.cfg.DHCPv6Interface); err != nil {
		return fmt.Errorf("waiting for interface: %w", err)
	}

	// Initial prefix request
	result, err := o.requestWithRetry(ctx)
	if err != nil {
		return err
	}

	if err := o.applyPrefix(ctx, result.Prefix); err != nil {
		slog.Error("failed to apply prefix", "error", err)
		// Continue — we'll retry on next renewal
	}

	// Renewal loop
	for {
		renewIn := result.T1
		slog.Info("waiting for renewal", "duration", renewIn, "current_prefix", o.current)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(renewIn):
		}

		newResult, err := o.renewWithRetry(ctx, result)
		if err != nil {
			slog.Error("renewal failed, will retry", "error", err)
			// On failure, retry after a shorter interval
			result.T1 = 30 * time.Second
			continue
		}

		if newResult.Prefix != result.Prefix {
			slog.Info("prefix changed", "old", result.Prefix, "new", newResult.Prefix)
			if err := o.applyPrefix(ctx, newResult.Prefix); err != nil {
				slog.Error("failed to apply new prefix", "error", err)
			}
		} else {
			slog.Info("prefix unchanged after renewal", "prefix", newResult.Prefix)
		}

		result = newResult
	}
}

func (o *Observer) requestWithRetry(ctx context.Context) (*dhcpv6.PrefixResult, error) {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<attempt) * time.Second
			slog.Info("retrying prefix request", "attempt", attempt+1, "backoff", backoff)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		result, err := o.client.RequestPrefix(ctx)
		if err != nil {
			lastErr = err
			slog.Warn("prefix request failed", "attempt", attempt+1, "error", err)
			continue
		}
		return result, nil
	}
	return nil, fmt.Errorf("prefix request failed after retries: %w", lastErr)
}

func (o *Observer) renewWithRetry(ctx context.Context, current *dhcpv6.PrefixResult) (*dhcpv6.PrefixResult, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<attempt) * time.Second
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		result, err := o.client.RenewPrefix(ctx, current)
		if err != nil {
			lastErr = err
			slog.Warn("renewal failed", "attempt", attempt+1, "error", err)
			continue
		}
		return result, nil
	}
	return nil, fmt.Errorf("renewal failed after retries: %w", lastErr)
}

func (o *Observer) applyPrefix(ctx context.Context, prefix netip.Prefix) error {
	// Compute LB CIDR: take the prefix network address and apply our LB prefix length
	lbCIDR := netip.PrefixFrom(prefix.Addr(), o.cfg.LBPrefixLength)
	slog.Info("computed LB CIDR", "cidr", lbCIDR)

	// Compute Gateway address: prefix base + host ID
	gwAddr, err := computeGatewayAddr(prefix, o.cfg.GatewayHostID)
	if err != nil {
		return fmt.Errorf("computing gateway address: %w", err)
	}
	slog.Info("computed Gateway address", "address", gwAddr)

	// Patch CiliumLoadBalancerIPPool
	if err := o.patcher.PatchIPPool(ctx, o.cfg.IPPoolName, lbCIDR.String()); err != nil {
		return fmt.Errorf("patching IP pool: %w", err)
	}

	// Patch Gateway
	if err := o.patcher.PatchGateway(ctx, o.cfg.GatewayNamespace, o.cfg.GatewayName, gwAddr.String()); err != nil {
		return fmt.Errorf("patching gateway: %w", err)
	}

	o.current = prefix
	slog.Info("successfully applied prefix", "prefix", prefix, "lb_cidr", lbCIDR, "gateway_addr", gwAddr)
	return nil
}

// computeGatewayAddr combines a prefix base address with a host ID like "::64".
func computeGatewayAddr(prefix netip.Prefix, hostID string) (netip.Addr, error) {
	// Parse host ID — pad with prefix to make it a valid address for parsing
	// e.g., "::64" is already valid IPv6
	hostAddr, err := netip.ParseAddr(hostID)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("parsing host ID %q: %w", hostID, err)
	}

	// OR the prefix base with the host ID
	prefixBytes := prefix.Addr().As16()
	hostBytes := hostAddr.As16()
	var result [16]byte
	for i := range result {
		result[i] = prefixBytes[i] | hostBytes[i]
	}

	return netip.AddrFrom16(result), nil
}

func envOr(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}

func envOrInt(key string, defaultValue int) int {
	v := os.Getenv(key)
	if v == "" {
		return defaultValue
	}
	v = strings.TrimSpace(v)
	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("invalid integer env var, using default", "key", key, "value", v, "default", defaultValue)
		return defaultValue
	}
	return n
}
