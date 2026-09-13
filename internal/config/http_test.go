package config

import "testing"

func TestPublicHTTPBoundaryConfiguration(t *testing.T) {
	isolateEnvironment(t)
	for _, tc := range []struct {
		origin, proxies string
		valid           bool
	}{
		{"", "", true},
		{"https://EXAMPLE.test:443", "127.0.0.1/32,::1/128", true},
		{"http://example.test", "127.0.0.1/32", false},
		{"https://example.test/path", "127.0.0.1/32", false},
		{"https://example.test", "", false},
		{"", "127.0.0.1/32", false},
		{"https://example.test", "0.0.0.0/0", false},
		{"https://example.test", "::/0", false},
		{"https://example.test", "127.0.0.1/32,", false},
	} {
		t.Setenv("LAKE_PASS_PUBLIC_ORIGIN", tc.origin)
		t.Setenv("LAKE_PASS_TRUSTED_PROXIES", tc.proxies)
		cfg, err := Load()
		if (err == nil) != tc.valid {
			t.Errorf("origin=%q proxies=%q: %v", tc.origin, tc.proxies, err)
		}
		if tc.valid && tc.origin != "" && cfg.PublicOrigin != "https://example.test" {
			t.Errorf("uncanonical public origin %q", cfg.PublicOrigin)
		}
	}
}
