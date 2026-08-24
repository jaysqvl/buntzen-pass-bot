// Package httpguard supplies the deliberately restricted HTTP behavior shared
// by read-only OTP adapters.
package httpguard

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

const absoluteMaxResponseBytes int64 = 4 << 20

// Error is safe to render or log: it never retains a request URL, response
// body, authorization header, or underlying network error string.
type Error struct {
	Provider string
	Op       string
	Status   int
	Reason   string
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s %s: %s (status %d)", e.Provider, e.Op, e.Reason, e.Status)
	}
	return fmt.Sprintf("%s %s: %s", e.Provider, e.Op, e.Reason)
}

// NewClient disables environment proxies and redirects. Those restrictions
// prevent a credential-bearing query URL from being forwarded elsewhere.
func NewClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          10,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   timeout,
			ResponseHeaderTimeout: timeout,
			ExpectContinueTimeout: time.Second,
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Do reads a successful response through a hard byte cap. It deliberately
// discards server and transport details on errors because either may echo an
// authenticated URL or message content.
func Do(ctx context.Context, client *http.Client, request *http.Request, provider, op string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 || maxBytes > absoluteMaxResponseBytes {
		return nil, &Error{Provider: provider, Op: op, Reason: "invalid response limit"}
	}
	response, err := client.Do(request.WithContext(ctx))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, &Error{Provider: provider, Op: op, Reason: "request failed"}
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, &Error{Provider: provider, Op: op, Status: response.StatusCode, Reason: "unexpected HTTP response"}
	}
	if response.ContentLength > maxBytes {
		return nil, &Error{Provider: provider, Op: op, Reason: "response exceeded size limit"}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return nil, &Error{Provider: provider, Op: op, Reason: "could not read response"}
	}
	if int64(len(body)) > maxBytes {
		return nil, &Error{Provider: provider, Op: op, Reason: "response exceeded size limit"}
	}
	return body, nil
}
