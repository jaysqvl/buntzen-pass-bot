// Package origin normalizes HTTP authorities and origins for configuration,
// browser requests, and the booking credential boundary.
package origin

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Host validates an exact HTTP Host authority. Ports are retained
// because the browser's same-origin boundary includes them.
func Host(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, ` /\\@`) {
		return "", errors.New("invalid host")
	}
	parsed, err := url.Parse("http://" + value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("invalid host")
	}
	hostname := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if hostname == "" || strings.Contains(hostname, "*") {
		return "", errors.New("invalid host")
	}
	port := parsed.Port()
	if port != "" {
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil || value == 0 {
			return "", errors.New("invalid host port")
		}
		return net.JoinHostPort(hostname, port), nil
	}
	if strings.Contains(hostname, ":") {
		return "[" + hostname + "]", nil
	}
	return hostname, nil
}

// Canonical validates an HTTP(S) origin and returns its browser-equivalent
// serialization. It accepts an explicit default port but stores it omitted.
func Canonical(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("invalid origin")
	}
	return canonicalURL(parsed)
}

// FromURL returns the origin of an absolute HTTP(S) URL, allowing a path, query,
// and fragment but rejecting embedded credentials.
func FromURL(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
		return "", errors.New("invalid HTTP(S) URL")
	}
	return canonicalURL(parsed)
}

func canonicalURL(parsed *url.URL) (string, error) {
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("invalid origin scheme")
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "" || strings.Contains(host, "*") {
		return "", errors.New("invalid origin host")
	}
	port := parsed.Port()
	if port != "" {
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil || value == 0 {
			return "", errors.New("invalid origin port")
		}
		if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
			port = ""
		}
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host, nil
}
