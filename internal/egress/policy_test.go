package egress

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

type lookupFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f lookupFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return f(ctx, network, host)
}

func testEndpoint(t *testing.T, rule Rule) endpoint {
	t.Helper()
	p, err := NewPolicy([]Rule{rule})
	if err != nil {
		t.Fatal(err)
	}
	e, err := p.selected(rule.Origin)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestPolicyRejectsUnapprovedAndAmbiguousOrigins(t *testing.T) {
	p, err := NewPolicy([]Rule{{Origin: "https://example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"https://other.example", "http://example.test", "https://example.test:444", "https://example.test?", "https://user@example.test", "https://example.test/path", "https://example.test:", "https://[fe80::1%25lo0]", "https://example.test/%2f", "https://example.test\\evil", "https://*.example"} {
		if _, err := p.NewClient(raw, time.Second); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	if _, err := p.NewClient("https://EXAMPLE.test.:443/", time.Second); err != nil {
		t.Fatal("canonical equivalent rejected", err)
	}
	var absent *Policy
	if _, err := absent.NewClient("https://example.test", time.Second); err == nil {
		t.Fatal("nil policy admitted provider")
	}
	for _, rule := range []Rule{{Origin: "http://private.example"}, {Origin: "http://private.example", Networks: []string{"127.0.0.1/8"}}, {Origin: "http://private.example", Networks: []string{"169.254.169.254/32"}}, {Origin: "https://private.example", Networks: []string{"::ffff:127.0.0.1/128"}}} {
		if _, err := NewPolicy([]Rule{rule}); err == nil {
			t.Errorf("accepted unsafe rule %+v", rule)
		}
	}
}

func TestDNSValidationPrecedesEveryDialAndRejectsMixedAnswers(t *testing.T) {
	e := testEndpoint(t, Rule{Origin: "https://example.test"})
	for _, raw := range []string{"127.0.0.1", "::ffff:127.0.0.1", netip.AddrFrom4([4]byte{10, 0, 0, 1}).String(), "169.254.169.254", "100.100.100.200", "192.0.2.1", "198.18.0.1", "224.0.0.1", "240.0.0.1", "::1", "fc00::1", "fe80::1", "64:ff9b::a9fe:a9fe", "2002:7f00:1::", "2001:db8::1", "3fff::1"} {
		t.Run(raw, func(t *testing.T) {
			var dials int
			lookup := lookupFunc(func(context.Context, string, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr(raw)}, nil
			})
			_, err := e.dial(context.Background(), "tcp", "example.test:443", lookup, func(context.Context, string, string) (net.Conn, error) {
				dials++
				return nil, errors.New("unexpected dial")
			})
			if err == nil || dials != 0 {
				t.Fatalf("unsafe mixed DNS triggered %d dials: %v", dials, err)
			}
		})
	}
	var lookups int
	var dialed []string
	lookup := lookupFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		lookups++
		if lookups == 1 {
			return []netip.Addr{netip.MustParseAddr("::ffff:8.8.8.8")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	dial := func(_ context.Context, _ string, address string) (net.Conn, error) {
		dialed = append(dialed, address)
		a, b := net.Pipe()
		b.Close()
		return a, nil
	}
	connection, err := e.dial(context.Background(), "tcp", "example.test:443", lookup, dial)
	if err != nil {
		t.Fatal(err)
	}
	connection.Close()
	if _, err = e.dial(context.Background(), "tcp", "example.test:443", lookup, dial); err == nil {
		t.Fatal("DNS rebind admitted")
	}
	if lookups != 2 || !reflect.DeepEqual(dialed, []string{"8.8.8.8:443"}) {
		t.Fatalf("validation-to-dial mismatch: %d %v", lookups, dialed)
	}
}

func TestPinsRestrictRatherThanExtendPublicDestinations(t *testing.T) {
	e := testEndpoint(t, Rule{Origin: "https://private.example", Networks: []string{"127.0.0.1/32"}})
	for _, raw := range []string{"127.0.0.2", "8.8.8.8", "::1"} {
		if e.allows(netip.MustParseAddr(raw)) {
			t.Errorf("unpinned address accepted %s", raw)
		}
	}
	if !e.allows(netip.MustParseAddr("::ffff:127.0.0.1")) {
		t.Fatal("mapped pinned control rejected")
	}
}

func TestUnreachableFirstAddressLeavesTimeForFallback(t *testing.T) {
	e := testEndpoint(t, Rule{Origin: "https://example.test"})
	lookup := lookupFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("2001:4860:4860::8888"), netip.MustParseAddr("8.8.8.8")}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	var calls int
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		calls++
		if calls == 1 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		a, b := net.Pipe()
		b.Close()
		return a, nil
	}
	connection, err := e.dial(ctx, "tcp", "example.test:443", lookup, dial)
	if err != nil {
		t.Fatalf("unreachable IPv6 prevented valid IPv4 fallback: %v", err)
	}
	connection.Close()
	if calls != 2 || ctx.Err() != nil {
		t.Fatal("fallback exceeded total request budget")
	}
}

func TestPublicTLSRetainsHostAndSNIAndRejectsProxyAndCrossOriginReuse(t *testing.T) {
	var requests, proxyRequests, dials atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyRequests.Add(1)
		http.Error(w, "unexpected proxy", 500)
	}))
	defer proxy.Close()
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Host != "example.com" || r.TLS.ServerName != "example.com" {
			t.Errorf("host or TLS identity changed: %q %q", r.Host, r.TLS.ServerName)
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()
	e := testEndpoint(t, Rule{Origin: "https://example.com"})
	lookup := lookupFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	})
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		if address != "8.8.8.8:443" {
			t.Errorf("dial did not use validated literal: %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	client := e.newClient(time.Second, lookup, dial)
	defer client.CloseIdleConnections()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	client.Transport.(*originTransport).transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	for i := 0; i < 2; i++ {
		response, err := client.Get("https://example.com/ping")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
	}
	for _, test := range []struct{ raw, host, opaque string }{{raw: "https://other.example/ping"}, {raw: "https://example.com:444/ping"}, {raw: "http://example.com/ping"}, {raw: "https://example.com/ping", host: "other.example"}, {raw: "https://example.com/ping", opaque: "//other.example/ping"}} {
		r, _ := http.NewRequest(http.MethodGet, test.raw, nil)
		r.Host = test.host
		r.URL.Opaque = test.opaque
		if response, err := client.Do(r); err == nil {
			response.Body.Close()
			t.Fatal("alternate authority accepted")
		}
	}
	if requests.Load() != 2 || proxyRequests.Load() != 0 || dials.Load() != 1 {
		t.Fatalf("requests=%d proxy=%d dials=%d", requests.Load(), proxyRequests.Load(), dials.Load())
	}
}

func TestPinnedLANClientAndRedirectRejection(t *testing.T) {
	var foreign atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreign.Add(1) }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) }))
	defer server.Close()
	p, err := NewPolicy([]Rule{{Origin: server.URL, Networks: []string{"127.0.0.1/32"}}})
	if err != nil {
		t.Fatal(err)
	}
	client, err := p.NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 302 || foreign.Load() != 0 {
		t.Fatal("redirect followed")
	}
	parsed, _ := url.Parse(server.URL)
	if parsed.Hostname() != "127.0.0.1" {
		t.Fatal("fixture requires pinned loopback")
	}
}
