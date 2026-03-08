package k8s

import (
	"context"
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestIsGUA(t *testing.T) {
	tests := []struct {
		cidr string
		want bool
	}{
		{"2001:db8::/32", true},
		{"2a01:abcd::/48", true},
		{"3fff::/16", true},
		{"fd00::/8", false},       // ULA
		{"fe80::/10", false},      // link-local
		{"192.168.1.0/24", false}, // IPv4
		{"::1/128", false},        // loopback
		{"invalid", false},
	}

	for _, tt := range tests {
		t.Run(tt.cidr, func(t *testing.T) {
			if got := isGUA(tt.cidr); got != tt.want {
				t.Errorf("isGUA(%q) = %v, want %v", tt.cidr, got, tt.want)
			}
		})
	}
}

func TestIsGUAAddr(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"2001:db8::1", true},
		{"2a01:abcd::ff", true},
		{"fd12:3456::1", false},
		{"fe80::1", false},
		{"192.168.1.1", false},
		{"::1", false},
		{"invalid", false},
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			if got := isGUAAddr(tt.addr); got != tt.want {
				t.Errorf("isGUAAddr(%q) = %v, want %v", tt.addr, got, tt.want)
			}
		})
	}
}

func TestPatchIPPool(t *testing.T) {
	pool := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "cilium.io/v2",
			"kind":       "CiliumLoadBalancerIPPool",
			"metadata": map[string]interface{}{
				"name": "test-pool",
			},
			"spec": map[string]interface{}{
				"blocks": []interface{}{
					map[string]interface{}{"cidr": "10.0.0.0/24"},
					map[string]interface{}{"cidr": "fd00::/112"},
					map[string]interface{}{"cidr": "2001:db8:abcd::/112"},
				},
			},
		},
	}

	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			ippoolGVR: "CiliumLoadBalancerIPPoolList",
		},
		pool,
	)

	var patchPayload []byte
	client.PrependReactor("patch", "ciliumloadbalancerippools", func(action clienttesting.Action) (bool, runtime.Object, error) {
		patchAction := action.(clienttesting.PatchAction)
		patchPayload = patchAction.GetPatch()
		return true, pool, nil
	})

	p := &Patcher{client: client, dryRun: false}
	err := p.PatchIPPool(context.Background(), "test-pool", "2a01:new::/112")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var patch map[string]interface{}
	if err := json.Unmarshal(patchPayload, &patch); err != nil {
		t.Fatalf("failed to unmarshal patch: %v", err)
	}

	blocks := patch["spec"].(map[string]interface{})["blocks"].([]interface{})

	// Should have: IPv4 + ULA + new GUA = 3 blocks
	if len(blocks) != 3 {
		t.Fatalf("expected 3 blocks, got %d: %v", len(blocks), blocks)
	}

	// Old GUA should be gone, new one present
	var cidrs []string
	for _, b := range blocks {
		cidrs = append(cidrs, b.(map[string]interface{})["cidr"].(string))
	}

	wantCIDRs := []string{"10.0.0.0/24", "fd00::/112", "2a01:new::/112"}
	for i, want := range wantCIDRs {
		if cidrs[i] != want {
			t.Errorf("block[%d] cidr = %q, want %q", i, cidrs[i], want)
		}
	}
}

func TestPatchIPPoolDryRun(t *testing.T) {
	pool := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "cilium.io/v2",
			"kind":       "CiliumLoadBalancerIPPool",
			"metadata": map[string]interface{}{
				"name": "test-pool",
			},
			"spec": map[string]interface{}{
				"blocks": []interface{}{
					map[string]interface{}{"cidr": "10.0.0.0/24"},
				},
			},
		},
	}

	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			ippoolGVR: "CiliumLoadBalancerIPPoolList",
		},
		pool,
	)

	patched := false
	client.PrependReactor("patch", "ciliumloadbalancerippools", func(action clienttesting.Action) (bool, runtime.Object, error) {
		patched = true
		return true, pool, nil
	})

	p := &Patcher{client: client, dryRun: true}
	err := p.PatchIPPool(context.Background(), "test-pool", "2a01:new::/112")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if patched {
		t.Error("dry run should not have patched")
	}
}

func TestPatchGateway(t *testing.T) {
	gw := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "Gateway",
			"metadata": map[string]interface{}{
				"name":      "test-gw",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"addresses": []interface{}{
					map[string]interface{}{"type": "IPAddress", "value": "10.0.0.1"},
					map[string]interface{}{"type": "IPAddress", "value": "fd00::1"},
					map[string]interface{}{"type": "IPAddress", "value": "2001:db8::1"},
				},
			},
		},
	}

	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			gatewayGVR: "GatewayList",
		},
		gw,
	)

	client.PrependReactor("get", "gateways", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, gw, nil
	})

	var patchPayload []byte
	client.PrependReactor("patch", "gateways", func(action clienttesting.Action) (bool, runtime.Object, error) {
		patchAction := action.(clienttesting.PatchAction)
		patchPayload = patchAction.GetPatch()
		return true, gw, nil
	})

	p := &Patcher{client: client, dryRun: false}
	err := p.PatchGateway(context.Background(), "default", "test-gw", "2a01:new::64")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var patch map[string]interface{}
	if err := json.Unmarshal(patchPayload, &patch); err != nil {
		t.Fatalf("failed to unmarshal patch: %v", err)
	}

	addrs := patch["spec"].(map[string]interface{})["addresses"].([]interface{})

	// Should have: IPv4 + ULA + new GUA = 3
	if len(addrs) != 3 {
		t.Fatalf("expected 3 addresses, got %d: %v", len(addrs), addrs)
	}

	lastAddr := addrs[2].(map[string]interface{})
	if lastAddr["value"] != "2a01:new::64" {
		t.Errorf("last address value = %q, want %q", lastAddr["value"], "2a01:new::64")
	}
	if lastAddr["type"] != "IPAddress" {
		t.Errorf("last address type = %q, want %q", lastAddr["type"], "IPAddress")
	}
}

func TestPatchIPPoolNotFound(t *testing.T) {
	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			ippoolGVR: "CiliumLoadBalancerIPPoolList",
		},
	)

	p := &Patcher{client: client, dryRun: false}
	err := p.PatchIPPool(context.Background(), "nonexistent", "2a01::/112")
	if err == nil {
		t.Fatal("expected error for nonexistent pool")
	}
}

func TestPatchGatewayNotFound(t *testing.T) {
	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			gatewayGVR: "GatewayList",
		},
	)

	// Create an empty resource to ensure the fake client is set up, but don't add the gateway
	p := &Patcher{client: client, dryRun: false}
	err := p.PatchGateway(context.Background(), "default", "nonexistent", "2a01::64")
	if err == nil {
		t.Fatal("expected error for nonexistent gateway")
	}
}

