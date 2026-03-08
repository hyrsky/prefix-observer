package dhcpv6

import (
	"net"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv6"
)

func TestParsePrefixFromReply(t *testing.T) {
	t.Run("parses valid reply with timers", func(t *testing.T) {
		msg := buildReply(t, &net.IPNet{
			IP:   net.ParseIP("2001:db8:abcd::"),
			Mask: net.CIDRMask(48, 128),
		}, 300*time.Second, 480*time.Second, 600*time.Second, 1200*time.Second)

		result, err := parsePrefixFromReply(msg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if result.Prefix.String() != "2001:db8:abcd::/48" {
			t.Errorf("prefix = %s, want 2001:db8:abcd::/48", result.Prefix)
		}
		if result.T1 != 300*time.Second {
			t.Errorf("T1 = %v, want 300s", result.T1)
		}
		if result.T2 != 480*time.Second {
			t.Errorf("T2 = %v, want 480s", result.T2)
		}
	})

	t.Run("enforces minimum T1 of 30s", func(t *testing.T) {
		msg := buildReply(t, &net.IPNet{
			IP:   net.ParseIP("2001:db8::"),
			Mask: net.CIDRMask(48, 128),
		}, 5*time.Second, 10*time.Second, 600*time.Second, 1200*time.Second)

		result, err := parsePrefixFromReply(msg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.T1 != 30*time.Second {
			t.Errorf("T1 = %v, want minimum 30s", result.T1)
		}
	})

	t.Run("enforces minimum T2 of 60s", func(t *testing.T) {
		msg := buildReply(t, &net.IPNet{
			IP:   net.ParseIP("2001:db8::"),
			Mask: net.CIDRMask(48, 128),
		}, 300*time.Second, 30*time.Second, 600*time.Second, 1200*time.Second)

		result, err := parsePrefixFromReply(msg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.T2 != 60*time.Second {
			t.Errorf("T2 = %v, want minimum 60s", result.T2)
		}
	})

	t.Run("derives T1/T2 from preferred lifetime when zero", func(t *testing.T) {
		msg := buildReply(t, &net.IPNet{
			IP:   net.ParseIP("2001:db8::"),
			Mask: net.CIDRMask(48, 128),
		}, 0, 0, 600*time.Second, 1200*time.Second)

		result, err := parsePrefixFromReply(msg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// T1 = preferred/2 = 300s, T2 = preferred*4/5 = 480s
		if result.T1 != 300*time.Second {
			t.Errorf("T1 = %v, want 300s (preferred/2)", result.T1)
		}
		if result.T2 != 480*time.Second {
			t.Errorf("T2 = %v, want 480s (preferred*4/5)", result.T2)
		}
	})

	t.Run("errors on missing IA_PD", func(t *testing.T) {
		msg, _ := dhcpv6.NewMessage()
		msg.MessageType = dhcpv6.MessageTypeReply

		_, err := parsePrefixFromReply(msg)
		if err == nil {
			t.Fatal("expected error for missing IA_PD")
		}
	})

	t.Run("errors on IA_PD with no prefixes", func(t *testing.T) {
		msg, _ := dhcpv6.NewMessage()
		msg.MessageType = dhcpv6.MessageTypeReply
		iapd := &dhcpv6.OptIAPD{
			IaId: [4]byte{0, 0, 0, 1},
			T1:   300 * time.Second,
			T2:   480 * time.Second,
		}
		msg.AddOption(iapd)

		_, err := parsePrefixFromReply(msg)
		if err == nil {
			t.Fatal("expected error for empty prefixes")
		}
	})

	t.Run("parses /56 prefix", func(t *testing.T) {
		msg := buildReply(t, &net.IPNet{
			IP:   net.ParseIP("2a01:1234:5600::"),
			Mask: net.CIDRMask(56, 128),
		}, 3600*time.Second, 5400*time.Second, 7200*time.Second, 14400*time.Second)

		result, err := parsePrefixFromReply(msg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.Prefix.Bits() != 56 {
			t.Errorf("prefix bits = %d, want 56", result.Prefix.Bits())
		}
		if result.Prefix.Addr().String() != "2a01:1234:5600::" {
			t.Errorf("prefix addr = %s, want 2a01:1234:5600::", result.Prefix.Addr())
		}
	})
}

// buildReply constructs a DHCPv6 reply message with an IA_PD containing the given prefix.
func buildReply(t *testing.T, prefix *net.IPNet, t1, t2, preferred, valid time.Duration) *dhcpv6.Message {
	t.Helper()

	msg, err := dhcpv6.NewMessage()
	if err != nil {
		t.Fatalf("failed to create message: %v", err)
	}
	msg.MessageType = dhcpv6.MessageTypeReply

	iaPrefix := &dhcpv6.OptIAPrefix{
		PreferredLifetime: preferred,
		ValidLifetime:     valid,
		Prefix:            prefix,
	}

	iapd := &dhcpv6.OptIAPD{
		IaId: [4]byte{0, 0, 0, 1},
		T1:   t1,
		T2:   t2,
	}
	iapd.Options.Add(iaPrefix)
	msg.AddOption(iapd)

	return msg
}
