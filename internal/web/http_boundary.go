package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/jaysqvl/lake-pass-bot/internal/origin"
)

const clientIPContextKey contextKey = "validated-client-ip"

func socketIP(r *http.Request) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, errors.New("invalid socket peer")
	}
	address, err := netip.ParseAddr(host)
	if err != nil || address.Zone() != "" {
		return netip.Addr{}, errors.New("invalid socket peer")
	}
	return address.Unmap(), nil
}

func healthRequest(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Path == "/healthz"
}

func (s *Server) enforceHTTPBoundary(r *http.Request) (*http.Request, error) {
	if s.config.PublicOrigin == "" {
		// Disabling the private hostname allowlist does not permit malformed
		// authorities or change the public HTTPS boundary below.
		if _, err := origin.Host(r.Host); err != nil {
			return r, errors.New("invalid Host header")
		}
		if s.config.HostCheckEnabled && !s.hostAllowed(r.Host) {
			return r, errors.New("invalid Host header")
		}
		return r, nil
	}
	peer, err := socketIP(r)
	if err != nil {
		return r, err
	}
	requestOrigin, err := origin.Canonical("https://" + r.Host)
	if healthRequest(r) {
		// Health returns no credentials or cookies. Keep existing deployment
		// probes usable without granting their HTTP authority access to the UI.
		if err == nil && requestOrigin == s.config.PublicOrigin {
			return r, nil
		}
		host, hostErr := origin.Host(r.Host)
		if hostErr != nil {
			return r, errors.New("invalid health authority")
		}
		for _, allowed := range s.config.AllowedHosts {
			if host == allowed {
				return r, nil
			}
		}
		if peer.IsLoopback() && s.hostAllowed(r.Host) {
			return r, nil
		}
		return r, errors.New("invalid health authority")
	}
	if err != nil || requestOrigin != s.config.PublicOrigin {
		return r, errors.New("public requests require the configured hostname")
	}
	trusted := false
	for _, prefix := range s.config.TrustedProxies {
		if prefix.Contains(peer) {
			trusted = true
			break
		}
	}
	clientIP := peer
	if trusted {
		if proto, ok := singleHeader(r, "X-Forwarded-Proto"); !ok || proto != "https" {
			return r, errors.New("public requests require HTTPS")
		}
		rawIP, ok := singleHeader(r, "CF-Connecting-IP")
		clientIP, err = netip.ParseAddr(rawIP)
		if !ok || err != nil || clientIP.Zone() != "" || !clientIP.IsGlobalUnicast() || clientIP.IsLoopback() {
			return r, errors.New("trusted proxy must supply one valid visitor IP")
		}
		clientIP = clientIP.Unmap()
	} else if r.TLS == nil {
		return r, errors.New("public requests require HTTPS from a trusted connector")
	}
	return r.WithContext(context.WithValue(r.Context(), clientIPContextKey, clientIP.String())), nil
}

func singleHeader(r *http.Request, name string) (string, bool) {
	values := r.Header.Values(name)
	if len(values) != 1 {
		return "", false
	}
	value := strings.TrimSpace(values[0])
	return value, value != "" && !strings.Contains(value, ",")
}
