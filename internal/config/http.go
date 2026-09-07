package config

import (
	"errors"
	"net/netip"
	"os"
	"strings"

	"github.com/jaysqvl/buntzen-pass-bot/internal/origin"
)

func (c *Config) loadHTTPBoundary() error {
	c.PublicOrigin = strings.TrimSpace(os.Getenv("BUNTZEN_PUBLIC_ORIGIN"))
	if c.PublicOrigin != "" {
		canonical, err := origin.Canonical(c.PublicOrigin)
		if err != nil {
			return errors.New("BUNTZEN_PUBLIC_ORIGIN must be an exact HTTPS origin without a path")
		}
		c.PublicOrigin = canonical
	}
	if raw := strings.TrimSpace(os.Getenv("BUNTZEN_TRUSTED_PROXIES")); raw != "" {
		for _, value := range strings.Split(raw, ",") {
			prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
			if err != nil {
				return errors.New("BUNTZEN_TRUSTED_PROXIES must contain connector IP CIDRs")
			}
			c.TrustedProxies = append(c.TrustedProxies, prefix.Masked())
		}
	}
	return c.ValidateHTTPBoundary()
}

// ValidateHTTPBoundary also covers callers that construct Config directly.
// Proxy authority comes only from deployment configuration, never HTTP headers.
func (c Config) ValidateHTTPBoundary() error {
	if c.PublicOrigin == "" {
		if len(c.TrustedProxies) != 0 {
			return errors.New("trusted proxies require BUNTZEN_PUBLIC_ORIGIN")
		}
		return nil
	}
	canonical, err := origin.Canonical(c.PublicOrigin)
	if err != nil || canonical != c.PublicOrigin || !strings.HasPrefix(canonical, "https://") {
		return errors.New("public origin must be a canonical HTTPS origin without a path")
	}
	if len(c.TrustedProxies) == 0 || len(c.TrustedProxies) > 16 {
		return errors.New("public mode requires 1 to 16 trusted connector IP CIDRs")
	}
	for _, prefix := range c.TrustedProxies {
		minimum := 64
		if prefix.Addr().Is4() {
			minimum = 24
		}
		if !prefix.IsValid() || prefix.Addr().Is4In6() || prefix.Bits() < minimum || prefix.Addr().IsUnspecified() || prefix.Addr().IsMulticast() {
			return errors.New("trusted connector CIDRs must be specific IPv4 /24-or-narrower or IPv6 /64-or-narrower networks")
		}
	}
	return nil
}
