// Package twilio implements inbound-only polling of Twilio's Messages REST
// resource. It intentionally exposes no message-create or alert capability.
package twilio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/otp"
	"github.com/jaysqvl/buntzen-pass-bot/internal/otp/internal/httpguard"
)

const (
	Kind = "twilio"

	defaultBaseURL         = "https://api.twilio.com"
	defaultPollInterval    = 2 * time.Second
	defaultRequestTimeout  = 10 * time.Second
	defaultMaxResponseSize = int64(1 << 20)
	defaultPageLimit       = 20
	defaultMaxPages        = 3
	defaultFreshness       = 5 * time.Minute

	maxPageLimit = 50
	maxPages     = 5
)

var _ otp.Provider = (*Provider)(nil)

// Config contains decrypted Twilio credentials and the receiving number. The
// optional BaseURL exists for local integration tests; production defaults to
// Twilio's HTTPS API.
type Config struct {
	AccountSID       string        `json:"account_sid"`
	AuthToken        string        `json:"auth_token"`
	ToNumber         string        `json:"to_number"`
	Sender           string        `json:"sender,omitempty"`
	BaseURL          string        `json:"base_url,omitempty"`
	PollInterval     time.Duration `json:"poll_interval,omitempty"`
	RequestTimeout   time.Duration `json:"request_timeout,omitempty"`
	MaxResponseBytes int64         `json:"max_response_bytes,omitempty"`
	PageLimit        int           `json:"page_limit,omitempty"`
	MaxPages         int           `json:"max_pages,omitempty"`
	Freshness        time.Duration `json:"freshness,omitempty"`
}

type Provider struct {
	config       Config
	base         *url.URL
	client       *http.Client
	messagesPath string
	accountPath  string
	now          func() time.Time
}

func New(config Config) (*Provider, error) {
	config = configWithDefaults(config)
	base, err := validateBaseURL(config.BaseURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(config.AccountSID) == "" || strings.Contains(config.AccountSID, "/") {
		return nil, errors.New("twilio account SID is required and must not contain a slash")
	}
	if strings.TrimSpace(config.AuthToken) == "" {
		return nil, errors.New("twilio auth token is required")
	}
	if strings.TrimSpace(config.ToNumber) == "" {
		return nil, errors.New("twilio receiving number is required")
	}
	if config.PageLimit < 1 || config.PageLimit > maxPageLimit {
		return nil, fmt.Errorf("twilio page limit must be between 1 and %d", maxPageLimit)
	}
	if config.MaxPages < 1 || config.MaxPages > maxPages {
		return nil, fmt.Errorf("twilio max pages must be between 1 and %d", maxPages)
	}
	if config.MaxResponseBytes < 1 || config.MaxResponseBytes > 4<<20 {
		return nil, errors.New("twilio response limit must be between 1 byte and 4 MiB")
	}
	if config.PollInterval <= 0 || config.PollInterval > time.Minute {
		return nil, errors.New("twilio poll interval must be between 1 nanosecond and 1 minute")
	}
	if config.RequestTimeout <= 0 || config.RequestTimeout > time.Minute {
		return nil, errors.New("twilio request timeout must be between 1 nanosecond and 1 minute")
	}
	if config.Freshness <= 0 || config.Freshness > time.Hour {
		return nil, errors.New("twilio freshness must be between 1 nanosecond and 1 hour")
	}
	accountPath := "/2010-04-01/Accounts/" + url.PathEscape(config.AccountSID) + ".json"
	return &Provider{
		config:       config,
		base:         base,
		client:       httpguard.NewClient(config.RequestTimeout),
		messagesPath: strings.TrimSuffix(accountPath, ".json") + "/Messages.json",
		accountPath:  accountPath,
		now:          time.Now,
	}, nil
}

func configWithDefaults(config Config) Config {
	if config.BaseURL == "" {
		config.BaseURL = defaultBaseURL
	}
	if config.PollInterval == 0 {
		config.PollInterval = defaultPollInterval
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = defaultRequestTimeout
	}
	if config.MaxResponseBytes == 0 {
		config.MaxResponseBytes = defaultMaxResponseSize
	}
	if config.PageLimit == 0 {
		config.PageLimit = defaultPageLimit
	}
	if config.MaxPages == 0 {
		config.MaxPages = defaultMaxPages
	}
	if config.Freshness == 0 {
		config.Freshness = defaultFreshness
	}
	return config
}

func validateBaseURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("twilio base URL must be an absolute HTTP(S) URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("twilio base URL must use HTTP or HTTPS")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("twilio base URL must not contain credentials, a path, query, or fragment")
	}
	parsed.Path = ""
	return parsed, nil
}

// Health validates the credentials against the read-only Account resource.
func (p *Provider) Health(ctx context.Context) error {
	target := p.target(p.accountPath, nil)
	body, err := p.get(ctx, target, "health")
	if err != nil {
		return err
	}
	var response struct {
		SID string `json:"sid"`
	}
	if err := json.Unmarshal(body, &response); err != nil || response.SID != p.config.AccountSID {
		return safeShapeError("health")
	}
	return nil
}

// Arm snapshots recent message SIDs and their newest timestamp before Yodel is
// allowed to send a new code.
func (p *Provider) Arm(ctx context.Context, filter otp.Filter) (otp.Armed, error) {
	if filter.Pairing {
		return otp.Armed{}, errors.New("twilio does not use supervised inbox pairing")
	}
	filter = p.effectiveFilter(filter)
	armedAt := p.now().UTC()
	if filter.FreshAfter.IsZero() {
		filter.FreshAfter = armedAt.Add(-p.config.Freshness)
	} else {
		filter.FreshAfter = filter.FreshAfter.UTC()
	}
	messages, _, err := p.listPage(ctx, p.firstPageURL())
	if err != nil {
		return otp.Armed{}, err
	}
	ids := make([]string, 0, len(messages))
	var newest time.Time
	for _, message := range messages {
		receivedAt, err := message.receivedAt()
		if err != nil || strings.TrimSpace(message.SID) == "" {
			return otp.Armed{}, safeShapeError("arm")
		}
		ids = append(ids, message.SID)
		if receivedAt.After(newest) {
			newest = receivedAt
		}
	}
	return otp.Armed{
		Provider: Kind,
		ArmedAt:  armedAt,
		Cursor:   otp.Cursor{Timestamp: newest, IDs: ids},
		Filter:   filter,
	}, nil
}

func (p *Provider) WaitForCode(ctx context.Context, armed otp.Armed) (otp.Message, error) {
	if armed.Provider != Kind {
		return otp.Message{}, errors.New("twilio received an arm from another provider")
	}
	if armed.Filter.Pairing {
		return otp.Message{}, errors.New("twilio does not use supervised inbox pairing")
	}
	filter := p.effectiveFilter(armed.Filter)
	seen := otp.NewDeduper(p.config.PageLimit*p.config.MaxPages*2, armed.Cursor.IDs...)
	cursorTime := armed.Cursor.Timestamp.UTC()
	for {
		oldestNeeded := filter.FreshAfter
		if cursorTime.After(oldestNeeded) {
			oldestNeeded = cursorTime
		}
		wire, err := p.listMessages(ctx, oldestNeeded)
		if err != nil {
			return otp.Message{}, err
		}
		raw := make([]otp.RawMessage, 0, len(wire))
		nextCursorTime := cursorTime
		for _, message := range wire {
			receivedAt, err := message.receivedAt()
			if err != nil || strings.TrimSpace(message.SID) == "" {
				return otp.Message{}, safeShapeError("query")
			}
			if !cursorTime.IsZero() && receivedAt.Before(cursorTime) {
				continue
			}
			if receivedAt.After(nextCursorTime) {
				nextCursorTime = receivedAt
			}
			raw = append(raw, otp.RawMessage{
				ID:         message.SID,
				Body:       message.Body,
				Sender:     message.From,
				Recipient:  message.To,
				Service:    "sms",
				ReceivedAt: receivedAt,
				Inbound:    strings.EqualFold(message.Direction, "inbound"),
			})
		}
		candidates := otp.Select(raw, filter, seen)
		if len(candidates) > 0 {
			return candidates[0], nil
		}
		cursorTime = nextCursorTime
		if err := waitForPoll(ctx, p.config.PollInterval); err != nil {
			return otp.Message{}, err
		}
	}
}

func (p *Provider) effectiveFilter(filter otp.Filter) otp.Filter {
	if filter.Recipient == "" {
		filter.Recipient = p.config.ToNumber
	}
	if filter.Sender == "" {
		filter.Sender = p.config.Sender
	}
	return filter
}

func (p *Provider) listMessages(ctx context.Context, oldestNeeded time.Time) ([]wireMessage, error) {
	target := p.firstPageURL()
	all := make([]wireMessage, 0, p.config.PageLimit)
	for page := 0; page < p.config.MaxPages; page++ {
		messages, next, err := p.listPage(ctx, target)
		if err != nil {
			return nil, err
		}
		all = append(all, messages...)
		coveredCursor := false
		for _, message := range messages {
			receivedAt, err := message.receivedAt()
			if err != nil || strings.TrimSpace(message.SID) == "" {
				return nil, safeShapeError("query")
			}
			if !oldestNeeded.IsZero() && receivedAt.Before(oldestNeeded) {
				coveredCursor = true
			}
		}
		// Twilio returns newest messages first. Once a page crosses the arm or
		// freshness cursor, lifetime history cannot contain a fresh candidate
		// and must not force us to paginate to the end of the account.
		if coveredCursor {
			return all, nil
		}
		if next == "" {
			return all, nil
		}
		if page == p.config.MaxPages-1 {
			return nil, otp.ErrPollWindowExceeded
		}
		target, err = p.nextPageURL(next)
		if err != nil {
			return nil, err
		}
	}
	return nil, otp.ErrPollWindowExceeded
}

func (p *Provider) listPage(ctx context.Context, target *url.URL) ([]wireMessage, string, error) {
	body, err := p.get(ctx, target, "query")
	if err != nil {
		return nil, "", err
	}
	var response listResponse
	if err := json.Unmarshal(body, &response); err != nil || response.Messages == nil || len(response.Messages) > p.config.PageLimit {
		return nil, "", safeShapeError("query")
	}
	return response.Messages, response.NextPageURI, nil
}

func (p *Provider) firstPageURL() *url.URL {
	query := make(url.Values)
	query.Set("To", p.config.ToNumber)
	query.Set("PageSize", fmt.Sprintf("%d", p.config.PageLimit))
	return p.target(p.messagesPath, query)
}

func (p *Provider) nextPageURL(value string) (*url.URL, error) {
	next, err := url.Parse(value)
	if err != nil || next.IsAbs() || next.Host != "" || next.User != nil || next.Fragment != "" || next.Path != p.messagesPath {
		return nil, safeShapeError("query")
	}
	query := next.Query()
	allowed := map[string]bool{"To": true, "PageSize": true, "Page": true, "PageToken": true}
	for key := range query {
		if !allowed[key] {
			return nil, safeShapeError("query")
		}
	}
	query.Set("To", p.config.ToNumber)
	query.Set("PageSize", fmt.Sprintf("%d", p.config.PageLimit))
	return p.target(p.messagesPath, query), nil
}

func (p *Provider) target(path string, query url.Values) *url.URL {
	target := *p.base
	target.Path = path
	if query != nil {
		target.RawQuery = query.Encode()
	}
	return &target
}

func (p *Provider) get(ctx context.Context, target *url.URL, op string) ([]byte, error) {
	request, err := http.NewRequest(http.MethodGet, target.String(), bytes.NewReader(nil))
	if err != nil {
		return nil, &httpguard.Error{Provider: Kind, Op: op, Reason: "could not build request"}
	}
	request.SetBasicAuth(p.config.AccountSID, p.config.AuthToken)
	request.Header.Set("Accept", "application/json")
	return httpguard.Do(ctx, p.client, request, Kind, op, p.config.MaxResponseBytes)
}

type listResponse struct {
	Messages    []wireMessage `json:"messages"`
	NextPageURI string        `json:"next_page_uri"`
}

type wireMessage struct {
	SID         string `json:"sid"`
	Body        string `json:"body"`
	From        string `json:"from"`
	To          string `json:"to"`
	Direction   string `json:"direction"`
	DateSent    string `json:"date_sent"`
	DateCreated string `json:"date_created"`
}

func (m wireMessage) receivedAt() (time.Time, error) {
	value := m.DateSent
	if value == "" {
		value = m.DateCreated
	}
	formats := []string{time.RFC1123Z, time.RFC1123, time.RFC3339, "Mon, 02 Jan 2006 15:04:05 -0700"}
	for _, format := range formats {
		if parsed, err := time.Parse(format, value); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, errors.New("unparseable Twilio message date")
}

func waitForPoll(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func safeShapeError(op string) error {
	return &httpguard.Error{Provider: Kind, Op: op, Reason: "malformed API response"}
}
