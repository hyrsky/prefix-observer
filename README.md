# prefix-observer

Watches for DHCPv6 Prefix Delegation changes and patches Kubernetes resources to assign globally routable (GUA) IPv6 addresses to LoadBalancer services.

Designed for clusters using [Cilium](https://cilium.io/) as the LoadBalancer implementation and [Gateway API](https://gateway-api.sigs.k8s.io/) for ingress.

## How it works

```
ISP ──/56──▶ Router ──DHCPv6-PD──▶ prefix-observer (receives /64)
                                         │
                                         ├─▶ patches CiliumLoadBalancerIPPool (GUA block)
                                         └─▶ patches Gateway (GUA address)
```

1. Sends a DHCPv6 Solicit/Request on the configured interface to obtain a delegated prefix
2. Computes a LoadBalancer CIDR and Gateway address from the delegated prefix
3. Patches the `CiliumLoadBalancerIPPool` to add/replace the GUA CIDR block (IPv4 and ULA blocks are preserved)
4. Patches the `Gateway` to add/replace the GUA address (IPv4 and ULA addresses are preserved)
5. Waits for lease renewal; if the prefix changes, repeats steps 2-4

## Configuration

All configuration is via environment variables.

| Variable | Required | Default | Description |
|---|---|---|---|
| `DHCPV6_INTERFACE` | Yes | | Network interface for DHCPv6-PD |
| `GATEWAY_HOST_ID` | Yes | | Host part for the Gateway GUA address (e.g. `::64`) |
| `IPPOOL_NAME` | Yes | | Name of the `CiliumLoadBalancerIPPool` to patch |
| `GATEWAY_NAME` | Yes | | Name of the `Gateway` resource to patch |
| `GATEWAY_NAMESPACE` | Yes | | Namespace of the `Gateway` resource |
| `LB_PREFIX_LENGTH` | No | `112` | Prefix length for the LB CIDR carved from the delegated prefix |
| `DRY_RUN` | No | `false` | Log changes without applying them |

## Deployment

The program is intended to run as a Kubernetes Deployment with `hostNetwork: true` on a node that has access to the DHCPv6-PD server (e.g. a node on the correct VLAN).

It requires the `NET_RAW` capability to send DHCPv6 packets.

### RBAC

The service account needs the following permissions:

```yaml
rules:
  - apiGroups: ["cilium.io"]
    resources: ["ciliumloadbalancerippools"]
    verbs: ["get", "list", "patch"]
  - apiGroups: ["gateway.networking.k8s.io"]
    resources: ["gateways"]
    verbs: ["get", "list", "patch"]
```

## Building

```sh
docker build -t prefix-observer .
```
