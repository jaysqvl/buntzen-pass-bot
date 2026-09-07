// Package egress restricts credential-bearing provider HTTP to operator-approved
// origins and addresses. Policy is separate from stored member configuration.
package egress

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/origin"
)

var ErrDenied = errors.New("provider destination is not approved by the operator")

// Rule permits public HTTPS by default. Networks restrict the origin to those
// exact prefixes, and explicitly permit private/loopback addresses and LAN HTTP.
type Rule struct {
	Origin   string   `json:"origin"`
	Networks []string `json:"networks,omitempty"`
}

type endpoint struct {
	origin, scheme, host, port string
	pins                       []netip.Prefix
}

// Policy has no mutators. A nil or empty policy denies all destinations.
type Policy struct{ endpoints map[string]endpoint }

func NewPolicy(rules []Rule) (*Policy, error) {
	if len(rules) > 16 {
		return nil, errors.New("provider policy allows at most 16 origins")
	}
	p := &Policy{endpoints: make(map[string]endpoint, len(rules))}
	for _, rule := range rules {
		canonical, err := CanonicalOrigin(rule.Origin)
		if err != nil {
			return nil, ErrDenied
		}
		if _, exists := p.endpoints[canonical]; exists {
			return nil, errors.New("duplicate provider policy origin")
		}
		u, _ := url.Parse(canonical)
		e := endpoint{origin: canonical, scheme: u.Scheme, host: u.Hostname(), port: u.Port()}
		if e.port == "" {
			if e.scheme == "https" {
				e.port = "443"
			} else {
				e.port = "80"
			}
		}
		if len(rule.Networks) > 16 {
			return nil, errors.New("provider origin allows at most 16 network pins")
		}
		for _, raw := range rule.Networks {
			prefix, err := netip.ParsePrefix(raw)
			if err != nil || prefix.Addr().Is4In6() || prefix.Addr().Zone() != "" {
				return nil, errors.New("invalid provider network pin")
			}
			minimum := 64
			if prefix.Addr().Is4() {
				minimum = 24
			}
			if prefix.Bits() < minimum {
				return nil, errors.New("provider network pins must be IPv4 /24 or IPv6 /64 or narrower")
			}
			prefix = prefix.Masked()
			if !permittedAddress(prefix.Addr()) || (e.scheme == "http" && !localAddress(prefix.Addr())) {
				return nil, errors.New("HTTP provider pins must identify private or loopback addresses; special-use addresses are prohibited")
			}
			e.pins = append(e.pins, prefix)
		}
		if e.scheme == "http" && len(e.pins) == 0 {
			return nil, errors.New("HTTP providers require explicit private network pins")
		}
		if literal, err := netip.ParseAddr(e.host); err == nil && !e.allows(literal) {
			return nil, ErrDenied
		}
		p.endpoints[canonical] = e
	}
	return p, nil
}

// CanonicalOrigin accepts a server root, rejecting parser ambiguities before
// canonicalization. It folds default ports, DNS case, trailing dots and mapped IPs.
func CanonicalOrigin(raw string) (string, error) {
	if len(raw) > 2048 {
		return "", ErrDenied
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Opaque != "" || u.User != nil || u.ForceQuery || u.RawQuery != "" || u.Fragment != "" || u.RawFragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return "", ErrDenied
	}
	if strings.HasSuffix(u.Host, ":") {
		return "", ErrDenied
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" {
			return "", ErrDenied
		}
		host = ip.Unmap().String()
	} else {
		if len(host) == 0 || len(host) > 253 {
			return "", ErrDenied
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return "", ErrDenied
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
					return "", ErrDenied
				}
			}
		}
	}
	if u.Port() != "" {
		host = net.JoinHostPort(host, u.Port())
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return origin.Canonical(u.Scheme + "://" + host)
}

func (p *Policy) selected(raw string) (endpoint, error) {
	canonical, err := CanonicalOrigin(raw)
	if err != nil || p == nil {
		return endpoint{}, ErrDenied
	}
	e, ok := p.endpoints[canonical]
	if !ok {
		return endpoint{}, ErrDenied
	}
	return e, nil
}

func (e endpoint) allows(ip netip.Addr) bool {
	if ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if !permittedAddress(ip) {
		return false
	}
	if len(e.pins) == 0 {
		return e.scheme == "https" && publicAddress(ip)
	}
	if e.scheme == "http" && !localAddress(ip) {
		return false
	}
	for _, pin := range e.pins {
		if pin.Contains(ip) {
			return true
		}
	}
	return false
}

func localAddress(ip netip.Addr) bool { return ip.IsPrivate() || ip.IsLoopback() }
func permittedAddress(ip netip.Addr) bool {
	return ip.IsValid() && (localAddress(ip) || publicAddress(ip))
}

// Conservative exclusions from IANA's special-purpose registries. IPv6 is
// restricted to allocated global unicast space, excluding transition mechanisms.
var excluded = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}
var globalIPv6 = netip.MustParsePrefix("2000::/3")

func publicAddress(ip netip.Addr) bool {
	if !ip.IsGlobalUnicast() || localAddress(ip) || ip.IsLinkLocalUnicast() || (ip.Is6() && !globalIPv6.Contains(ip)) {
		return false
	}
	for _, block := range excluded {
		if block.Contains(ip) {
			return false
		}
	}
	return true
}

type resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}
type dialContext func(context.Context, string, string) (net.Conn, error)

// NewClient restricts both each request and each new socket. DNS answers are
// checked together, then dialed as literals without another resolution. The URL
// retains its hostname for Host, TLS SNI and certificate verification.
func (p *Policy) NewClient(raw string, timeout time.Duration) (*http.Client, error) {
	e, err := p.selected(raw)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 || timeout > time.Minute {
		return nil, errors.New("invalid provider request timeout")
	}
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	return e.newClient(timeout, net.DefaultResolver, dialer.DialContext), nil
}

func (e endpoint) dial(ctx context.Context, network, address string, lookup resolver, dial dialContext) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host != e.host || port != e.port || network != "tcp" {
		return nil, ErrDenied
	}
	var addresses []netip.Addr
	if literal, err := netip.ParseAddr(host); err == nil {
		addresses = []netip.Addr{literal}
	} else {
		addresses, err = lookup.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, ErrDenied
		}
	}
	if len(addresses) == 0 || len(addresses) > 64 {
		return nil, ErrDenied
	}
	for _, address := range addresses {
		if !e.allows(address) {
			return nil, ErrDenied
		}
	}
	for index, address := range addresses {
		attemptCtx := ctx
		cancel := func() {}
		if deadline, ok := ctx.Deadline(); ok {
			// Reserve time for the remaining approved answers, so a blackholed
			// first IPv6 address cannot consume the entire IPv4 fallback budget.
			attemptCtx, cancel = context.WithTimeout(ctx, time.Until(deadline)/time.Duration(len(addresses)-index))
		}
		connection, err := dial(attemptCtx, network, net.JoinHostPort(address.Unmap().String(), port))
		cancel()
		if err == nil {
			return connection, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, errors.New("provider connection failed")
}

type originTransport struct {
	endpoint  endpoint
	transport *http.Transport
}

func (t *originTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL == nil || r.URL.Opaque != "" || r.URL.User != nil || r.URL.Fragment != "" {
		return nil, ErrDenied
	}
	canonical, err := CanonicalOrigin(r.URL.Scheme + "://" + r.URL.Host)
	if err != nil || canonical != t.endpoint.origin {
		return nil, ErrDenied
	}
	if r.Host != "" {
		hostOrigin, err := CanonicalOrigin(r.URL.Scheme + "://" + r.Host)
		if err != nil || hostOrigin != canonical {
			return nil, ErrDenied
		}
	}
	// Use the policy's canonical authority for socket selection; equivalent DNS
	// case/default-port spellings must not create a validation-to-dial mismatch.
	r = r.Clone(r.Context())
	canonicalURL, _ := url.Parse(canonical)
	r.URL.Scheme, r.URL.Host = canonicalURL.Scheme, canonicalURL.Host
	return t.transport.RoundTrip(r)
}
func (t *originTransport) CloseIdleConnections() { t.transport.CloseIdleConnections() }

func (e endpoint) newClient(timeout time.Duration, lookup resolver, dial dialContext) *http.Client {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			return e.dial(ctx, network, address, lookup, dial)
		},
		ForceAttemptHTTP2: true, MaxIdleConns: 4, MaxConnsPerHost: 4,
		IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: timeout,
		ResponseHeaderTimeout: timeout, ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Timeout: timeout, Transport: &originTransport{endpoint: e, transport: transport}, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}
