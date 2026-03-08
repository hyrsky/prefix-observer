package dhcpv6

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/insomniacslk/dhcp/dhcpv6/nclient6"
)

// PrefixResult holds a delegated prefix and its lease timers.
type PrefixResult struct {
	Prefix netip.Prefix
	T1     time.Duration // Renewal time
	T2     time.Duration // Rebind time
}

// Client performs DHCPv6 Prefix Delegation.
type Client struct {
	iface string
}

// NewClient creates a DHCPv6-PD client on the given interface.
func NewClient(iface string) *Client {
	return &Client{iface: iface}
}

func (c *Client) pdModifier(hint *net.IPNet) dhcpv6.Modifier {
	if hint != nil {
		return dhcpv6.WithIAPD([4]byte{0, 0, 0, 1}, &dhcpv6.OptIAPrefix{
			Prefix: hint,
		})
	}
	return dhcpv6.WithIAPD([4]byte{0, 0, 0, 1})
}

// RequestPrefix sends a DHCPv6 Solicit+Request for a prefix delegation.
func (c *Client) RequestPrefix(ctx context.Context) (*PrefixResult, error) {
	client, err := c.newClient()
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()

	mod := c.pdModifier(nil)

	adv, err := client.Solicit(ctx, mod)
	if err != nil {
		return nil, fmt.Errorf("DHCPv6 solicit: %w", err)
	}

	reply, err := client.Request(ctx, adv, mod)
	if err != nil {
		return nil, fmt.Errorf("DHCPv6 request: %w", err)
	}

	return parsePrefixFromReply(reply)
}

// RenewPrefix renews an existing prefix delegation.
func (c *Client) RenewPrefix(ctx context.Context, current *PrefixResult) (*PrefixResult, error) {
	client, err := c.newClient()
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()

	// Hint with current prefix
	prefixIP := current.Prefix.Addr().As16()
	hint := &net.IPNet{
		IP:   net.IP(prefixIP[:]),
		Mask: net.CIDRMask(current.Prefix.Bits(), 128),
	}
	mod := c.pdModifier(hint)

	adv, err := client.Solicit(ctx, mod)
	if err != nil {
		return nil, fmt.Errorf("DHCPv6 solicit (renew): %w", err)
	}

	reply, err := client.Request(ctx, adv, mod)
	if err != nil {
		return nil, fmt.Errorf("DHCPv6 request (renew): %w", err)
	}

	return parsePrefixFromReply(reply)
}

func (c *Client) newClient() (*nclient6.Client, error) {
	client, err := nclient6.New(c.iface)
	if err != nil {
		return nil, fmt.Errorf("creating DHCPv6 client on %s: %w", c.iface, err)
	}
	return client, nil
}

func parsePrefixFromReply(msg *dhcpv6.Message) (*PrefixResult, error) {
	iapd := msg.Options.OneIAPD()
	if iapd == nil {
		return nil, fmt.Errorf("no IA_PD in DHCPv6 reply: %s", msg.Summary())
	}

	t1 := iapd.T1
	t2 := iapd.T2

	prefixes := iapd.Options.Prefixes()
	if len(prefixes) == 0 {
		return nil, fmt.Errorf("no IA_Prefix in IA_PD: %s", msg.Summary())
	}

	iaprefix := prefixes[0]
	if iaprefix.Prefix == nil {
		return nil, fmt.Errorf("nil prefix in IA_Prefix: %s", msg.Summary())
	}

	ones, _ := iaprefix.Prefix.Mask.Size()
	addr, ok := netip.AddrFromSlice(iaprefix.Prefix.IP.To16())
	if !ok {
		return nil, fmt.Errorf("invalid prefix IP: %s", iaprefix.Prefix.IP)
	}
	prefix := netip.PrefixFrom(addr, ones)

	slog.Info("received delegated prefix",
		"prefix", prefix,
		"t1", t1,
		"t2", t2,
		"preferred_lifetime", iaprefix.PreferredLifetime,
		"valid_lifetime", iaprefix.ValidLifetime,
	)

	// Use minimum sane timer values
	if t1 == 0 {
		t1 = iaprefix.PreferredLifetime / 2
	}
	if t2 == 0 {
		t2 = iaprefix.PreferredLifetime * 4 / 5
	}
	if t1 < 30*time.Second {
		t1 = 30 * time.Second
	}
	if t2 < 60*time.Second {
		t2 = 60 * time.Second
	}

	return &PrefixResult{
		Prefix: prefix,
		T1:     t1,
		T2:     t2,
	}, nil
}

// WaitForInterface blocks until the interface exists and has a link-local address.
func WaitForInterface(ctx context.Context, name string) error {
	for {
		iface, err := net.InterfaceByName(name)
		if err == nil {
			addrs, _ := iface.Addrs()
			for _, addr := range addrs {
				if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.IsLinkLocalUnicast() {
					slog.Info("interface ready", "interface", name, "link_local", ipnet.IP)
					return nil
				}
			}
		}

		slog.Info("waiting for interface", "interface", name)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}
