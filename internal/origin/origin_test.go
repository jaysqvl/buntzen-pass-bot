package origin

import "testing"

func TestURLAndOriginBoundaries(t *testing.T) {
	for _, test := range []struct {
		input    string
		want     string
		isOrigin bool
	}{
		{"https://Example.Test.:443", "https://example.test", true},
		{"http://example.test:80", "http://example.test", true},
		{"https://[::1]:8443", "https://[::1]:8443", true},
		{"https://example.test/path?query=1#fragment", "https://example.test", false},
		{"http://example.test/", "http://example.test", false},
	} {
		t.Run(test.input, func(t *testing.T) {
			got, err := FromURL(test.input)
			if err != nil || got != test.want {
				t.Fatalf("FromURL = %q, %v; want %q", got, err, test.want)
			}
			got, err = Canonical(test.input)
			if test.isOrigin && (err != nil || got != test.want) {
				t.Fatalf("Canonical = %q, %v; want %q", got, err, test.want)
			}
			if !test.isOrigin && err == nil {
				t.Fatal("accepted a URL with path, query, or fragment as an origin")
			}
		})
	}
	for _, input := range []string{"/relative", "https://user:password@example.test", "https://*.example.test", "ftp://example.test", "https://example.test:65536"} {
		if _, err := FromURL(input); err == nil {
			t.Errorf("FromURL accepted %q", input)
		}
	}
}

func TestHostRetainsExplicitPort(t *testing.T) {
	for input, want := range map[string]string{
		"Example.Test.:443": "example.test:443",
		"[::1]:8080":        "[::1]:8080",
		"Example.Test":      "example.test",
	} {
		if got, err := Host(input); err != nil || got != want {
			t.Errorf("Host(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
}
