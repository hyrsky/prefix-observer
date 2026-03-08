package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

var (
	ippoolGVR = schema.GroupVersionResource{
		Group:    "cilium.io",
		Version:  "v2",
		Resource: "ciliumloadbalancerippools",
	}
	gatewayGVR = schema.GroupVersionResource{
		Group:    "gateway.networking.k8s.io",
		Version:  "v1",
		Resource: "gateways",
	}
)

// Patcher patches Kubernetes resources for prefix changes.
type Patcher struct {
	client dynamic.Interface
	dryRun bool
}

// NewPatcher creates a Patcher using in-cluster config.
func NewPatcher(dryRun bool) (*Patcher, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}

	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("creating dynamic client: %w", err)
	}

	return &Patcher{client: client, dryRun: dryRun}, nil
}

// PatchIPPool updates the CiliumLoadBalancerIPPool to include the GUA CIDR.
// It preserves existing IPv4 and ULA blocks, and adds/replaces the GUA block.
func (p *Patcher) PatchIPPool(ctx context.Context, name, guaCIDR string) error {
	slog.Info("patching CiliumLoadBalancerIPPool", "name", name, "gua_cidr", guaCIDR)

	pool, err := p.client.Resource(ippoolGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting IP pool %s: %w", name, err)
	}

	blocks, found, err := unstructured.NestedSlice(pool.Object, "spec", "blocks")
	if err != nil || !found {
		return fmt.Errorf("reading spec.blocks from IP pool: found=%v, err=%v", found, err)
	}

	// Filter out existing GUA blocks, keep IPv4 and ULA
	var newBlocks []interface{}
	for _, b := range blocks {
		block, ok := b.(map[string]interface{})
		if !ok {
			continue
		}
		cidr, ok := block["cidr"].(string)
		if !ok {
			continue
		}
		if isGUA(cidr) {
			slog.Info("removing old GUA block", "cidr", cidr)
			continue
		}
		newBlocks = append(newBlocks, b)
	}

	// Add the new GUA block
	newBlocks = append(newBlocks, map[string]interface{}{
		"cidr": guaCIDR,
	})

	// Build merge patch
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"blocks": newBlocks,
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshaling patch: %w", err)
	}

	if p.dryRun {
		slog.Info("DRY RUN: would patch IP pool", "name", name, "patch", string(patchBytes))
		return nil
	}

	_, err = p.client.Resource(ippoolGVR).Patch(ctx, name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patching IP pool %s: %w", name, err)
	}

	slog.Info("successfully patched IP pool", "name", name)
	return nil
}

// PatchGateway updates the Gateway to include the GUA address.
// It preserves existing IPv4 and ULA addresses, and adds/replaces the GUA address.
func (p *Patcher) PatchGateway(ctx context.Context, namespace, name, guaAddr string) error {
	slog.Info("patching Gateway", "namespace", namespace, "name", name, "gua_addr", guaAddr)

	gw, err := p.client.Resource(gatewayGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting gateway %s/%s: %w", namespace, name, err)
	}

	addresses, found, err := unstructured.NestedSlice(gw.Object, "spec", "addresses")
	if err != nil || !found {
		return fmt.Errorf("reading spec.addresses from gateway: found=%v, err=%v", found, err)
	}

	// Filter out existing GUA addresses, keep IPv4 and ULA
	var newAddresses []interface{}
	for _, a := range addresses {
		addr, ok := a.(map[string]interface{})
		if !ok {
			continue
		}
		value, ok := addr["value"].(string)
		if !ok {
			continue
		}
		if isGUAAddr(value) {
			slog.Info("removing old GUA address", "value", value)
			continue
		}
		newAddresses = append(newAddresses, a)
	}

	// Add the new GUA address
	newAddresses = append(newAddresses, map[string]interface{}{
		"type":  "IPAddress",
		"value": guaAddr,
	})

	// Build merge patch
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"addresses": newAddresses,
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshaling patch: %w", err)
	}

	if p.dryRun {
		slog.Info("DRY RUN: would patch gateway", "namespace", namespace, "name", name, "patch", string(patchBytes))
		return nil
	}

	_, err = p.client.Resource(gatewayGVR).Namespace(namespace).Patch(ctx, name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patching gateway %s/%s: %w", namespace, name, err)
	}

	slog.Info("successfully patched gateway", "namespace", namespace, "name", name)
	return nil
}

// isGUA checks if a CIDR string is a Global Unicast Address (2000::/3).
func isGUA(cidr string) bool {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return false
	}
	return prefix.Addr().Is6() && isGlobalUnicast(prefix.Addr())
}

// isGUAAddr checks if an address string is a Global Unicast Address.
func isGUAAddr(addr string) bool {
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return false
	}
	return a.Is6() && isGlobalUnicast(a)
}

// isGlobalUnicast checks if an IPv6 address is in the 2000::/3 range.
// This excludes ULA (fc00::/7) and link-local (fe80::/10).
func isGlobalUnicast(addr netip.Addr) bool {
	b := addr.As16()
	// 2000::/3 means first 3 bits are 001, so first byte is 0x20-0x3f
	return b[0] >= 0x20 && b[0] <= 0x3f
}
