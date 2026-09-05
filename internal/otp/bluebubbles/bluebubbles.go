// Package bluebubbles implements the query-only BlueBubbles OTP adapter. The
// only HTTP operations in this package are authenticated GET /api/v1/ping and
// POST /api/v1/message/query.
package bluebubbles

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
	Kind = "bluebubbles"

	defaultPollInterval    = time.Second
	defaultRequestTimeout  = 10 * time.Second
	defaultMaxResponseSize = int64(1 << 20)
	defaultPageLimit       = 20
	defaultMaxPages        = 3
	defaultFreshness       = 5 * time.Minute

	maxPageLimit = 50
	maxPages     = 5
)

var _ otp.Provider = (*Provider)(nil)
var _ otp.PairingProvider = (*Provider)(nil)

// Config contains decrypted runtime configuration. Callers must never log or
// persist this value in plaintext because Password authenticates the entire
// BlueBubbles server, not just the read-only endpoints used here.
type Config struct {
	BaseURL          string        `json:"base_url"`
	Password         string        `json:"password"`
	ChatGUID         string        `json:"chat_guid,omitempty"`
	Sender           string        `json:"sender,omitempty"`
	Service          string        `json:"service,omitempty"`
	PollInterval     time.Duration `json:"poll_interval,omitempty"`
	RequestTimeout   time.Duration `json:"request_timeout,omitempty"`
	MaxResponseBytes int64         `json:"max_response_bytes,omitempty"`
	PageLimit        int           `json:"page_limit,omitempty"`
	MaxPages         int           `json:"max_pages,omitempty"`
	Freshness        time.Duration `json:"freshness,omitempty"`
}

type Provider struct {
	config Config
	base   *url.URL
	client *http.Client
	now    func() time.Time
}

// New constructs a provider with a client that ignores proxy environment
// variables and refuses redirects.
func New(config Config) (*Provider, error) {
	config = configWithDefaults(config)
	base, err := validateBaseURL(config.BaseURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(config.Password) == "" {
		return nil, errors.New("bluebubbles password is required")
	}
	if config.PageLimit < 1 || config.PageLimit > maxPageLimit {
		return nil, fmt.Errorf("bluebubbles page limit must be between 1 and %d", maxPageLimit)
	}
	if config.MaxPages < 1 || config.MaxPages > maxPages {
		return nil, fmt.Errorf("bluebubbles max pages must be between 1 and %d", maxPages)
	}
	if config.MaxResponseBytes < 1 || config.MaxResponseBytes > 4<<20 {
		return nil, errors.New("bluebubbles response limit must be between 1 byte and 4 MiB")
	}
	if config.PollInterval <= 0 || config.PollInterval > time.Minute {
		return nil, errors.New("bluebubbles poll interval must be between 1 nanosecond and 1 minute")
	}
	if config.RequestTimeout <= 0 || config.RequestTimeout > time.Minute {
		return nil, errors.New("bluebubbles request timeout must be between 1 nanosecond and 1 minute")
	}
	if config.Freshness <= 0 || config.Freshness > time.Hour {
		return nil, errors.New("bluebubbles freshness must be between 1 nanosecond and 1 hour")
	}
	return &Provider{
		config: config,
		base:   base,
		client: httpguard.NewClient(config.RequestTimeout),
		now:    time.Now,
	}, nil
}

func configWithDefaults(config Config) Config {
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
		return nil, errors.New("bluebubbles base URL must be an absolute HTTP(S) URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("bluebubbles base URL must use HTTP or HTTPS")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("bluebubbles base URL must not contain credentials, a path, query, or fragment")
	}
	parsed.Path = ""
	return parsed, nil
}

// Health verifies authenticated access without reading the inbox.
func (p *Provider) Health(ctx context.Context) error {
	body, err := p.do(ctx, http.MethodGet, "/api/v1/ping", nil, "health")
	if err != nil {
		return err
	}
	var response envelope
	if err := json.Unmarshal(body, &response); err != nil || len(response.Data) == 0 {
		return safeShapeError("health")
	}
	if !validEnvelopeStatus(response.Status) {
		return &httpguard.Error{Provider: Kind, Op: "health", Reason: "API reported failure"}
	}
	var pong string
	if err := json.Unmarshal(response.Data, &pong); err != nil || !strings.EqualFold(pong, "pong") {
		return safeShapeError("health")
	}
	return nil
}

// Arm snapshots the latest ROWID before Yodel is allowed to request an OTP.
func (p *Provider) Arm(ctx context.Context, filter otp.Filter) (otp.Armed, error) {
	filter, err := p.effectiveFilter(filter)
	if err != nil {
		return otp.Armed{}, err
	}
	armedAt := p.now().UTC()
	if filter.FreshAfter.IsZero() {
		filter.FreshAfter = armedAt.Add(-p.config.Freshness)
	} else {
		filter.FreshAfter = filter.FreshAfter.UTC()
	}
	request := queryRequest{
		ChatGUID: filter.ChatGUID,
		With:     []string{"chat"},
		Offset:   0,
		Limit:    1,
		Sort:     "DESC",
	}
	response, err := p.query(ctx, request)
	if err != nil {
		return otp.Armed{}, err
	}
	var cursor int64
	for _, message := range response.Messages {
		if message.OriginalROWID > cursor {
			cursor = message.OriginalROWID
		}
	}
	return otp.Armed{
		Provider: Kind,
		ArmedAt:  armedAt,
		Cursor:   otp.Cursor{Position: cursor},
		Filter:   filter,
	}, nil
}

// WaitForCode polls until a single matching code is available or ctx ends.
// For supervised pairing, use WaitForPairingCandidates to retain all fresh
// candidates returned by the same bounded polling window.
func (p *Provider) WaitForCode(ctx context.Context, armed otp.Armed) (otp.Message, error) {
	candidates, err := p.wait(ctx, armed)
	if err != nil {
		return otp.Message{}, err
	}
	return candidates[0], nil
}

// WaitForPairingCandidates supports the supervised one-time pairing flow. The
// returned codes and sender metadata are transient and must not be persisted.
func (p *Provider) WaitForPairingCandidates(ctx context.Context, armed otp.Armed) ([]otp.Message, error) {
	if !armed.Filter.Pairing {
		return nil, errors.New("bluebubbles pairing candidates require a pairing arm")
	}
	return p.wait(ctx, armed)
}

func (p *Provider) wait(ctx context.Context, armed otp.Armed) ([]otp.Message, error) {
	if armed.Provider != Kind {
		return nil, errors.New("bluebubbles received an arm from another provider")
	}
	filter, err := p.effectiveFilter(armed.Filter)
	if err != nil {
		return nil, err
	}
	cursor := armed.Cursor.Position
	seen := otp.NewDeduper(p.config.PageLimit*p.config.MaxPages*2, armed.Cursor.IDs...)
	for {
		raw, nextCursor, err := p.fetchAfter(ctx, cursor, filter)
		if err != nil {
			return nil, err
		}
		candidates := otp.Select(raw, filter, seen)
		if len(candidates) > 0 {
			return candidates, nil
		}
		if nextCursor > cursor {
			cursor = nextCursor
		}
		if err := waitForPoll(ctx, p.config.PollInterval); err != nil {
			return nil, err
		}
	}
}

func (p *Provider) effectiveFilter(filter otp.Filter) (otp.Filter, error) {
	if filter.Pairing {
		filter.ChatGUID = ""
		filter.Sender = ""
		filter.Service = ""
		filter.RequireYodel = true
		return filter, nil
	}
	if filter.ChatGUID == "" {
		filter.ChatGUID = p.config.ChatGUID
	}
	if filter.Sender == "" {
		filter.Sender = p.config.Sender
	}
	if filter.Service == "" {
		filter.Service = p.config.Service
	}
	if strings.TrimSpace(filter.ChatGUID) == "" || strings.TrimSpace(filter.Sender) == "" || strings.TrimSpace(filter.Service) == "" {
		return otp.Filter{}, errors.New("bluebubbles must be paired to an exact chat, sender, and service")
	}
	return filter, nil
}

func (p *Provider) fetchAfter(ctx context.Context, cursor int64, filter otp.Filter) ([]otp.RawMessage, int64, error) {
	where := []whereClause{{
		Statement: "message.ROWID > :minRowID",
		Args:      map[string]int64{"minRowID": cursor},
	}}
	all := make([]otp.RawMessage, 0, p.config.PageLimit)
	maxCursor := cursor
	for page := 0; page < p.config.MaxPages; page++ {
		request := queryRequest{
			ChatGUID: filter.ChatGUID,
			With:     []string{"chat"},
			Offset:   page * p.config.PageLimit,
			Limit:    p.config.PageLimit,
			Sort:     "ASC",
			Where:    where,
		}
		response, err := p.query(ctx, request)
		if err != nil {
			return nil, cursor, err
		}
		if response.Total > p.config.PageLimit*p.config.MaxPages {
			return nil, cursor, otp.ErrPollWindowExceeded
		}
		for _, message := range response.Messages {
			if message.OriginalROWID <= cursor {
				continue
			}
			if message.OriginalROWID > maxCursor {
				maxCursor = message.OriginalROWID
			}
			all = append(all, normalize(message, filter.ChatGUID))
		}
		if request.Offset+len(response.Messages) >= response.Total {
			return all, maxCursor, nil
		}
		if len(response.Messages) == 0 {
			return nil, cursor, safeShapeError("query")
		}
	}
	return nil, cursor, otp.ErrPollWindowExceeded
}

func normalize(message wireMessage, expectedChat string) otp.RawMessage {
	chatGUID := ""
	if expectedChat != "" {
		for _, chat := range message.Chats {
			if chat.GUID == expectedChat {
				chatGUID = chat.GUID
				break
			}
		}
	} else if len(message.Chats) == 1 {
		chatGUID = message.Chats[0].GUID
	}
	sender, service := "", ""
	if message.Handle != nil {
		sender = message.Handle.Address
		service = message.Handle.Service
	}
	return otp.RawMessage{
		ID:         message.GUID,
		Body:       message.Text,
		Sender:     sender,
		ChatGUID:   chatGUID,
		Service:    service,
		ReceivedAt: time.UnixMilli(message.DateCreated).UTC(),
		Cursor:     message.OriginalROWID,
		Inbound:    !message.IsFromMe,
	}
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

type whereClause struct {
	Statement string           `json:"statement"`
	Args      map[string]int64 `json:"args"`
}

type queryRequest struct {
	ChatGUID string        `json:"chatGuid,omitempty"`
	With     []string      `json:"with"`
	Offset   int           `json:"offset"`
	Limit    int           `json:"limit"`
	Sort     string        `json:"sort"`
	Where    []whereClause `json:"where,omitempty"`
}

type wireMessage struct {
	OriginalROWID int64       `json:"originalROWID"`
	GUID          string      `json:"guid"`
	Text          string      `json:"text"`
	Handle        *wireHandle `json:"handle"`
	Chats         []wireChat  `json:"chats"`
	DateCreated   int64       `json:"dateCreated"`
	IsFromMe      bool        `json:"isFromMe"`
}

type wireHandle struct {
	Address string `json:"address"`
	Service string `json:"service"`
}

type wireChat struct {
	GUID string `json:"guid"`
}

type envelope struct {
	Status   int             `json:"status"`
	Data     json.RawMessage `json:"data"`
	Metadata *queryMetadata  `json:"metadata"`
}

type queryMetadata struct {
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
	Total  int `json:"total"`
	Count  int `json:"count"`
}

type queryResponse struct {
	Messages []wireMessage
	Total    int
}

func (p *Provider) query(ctx context.Context, request queryRequest) (queryResponse, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return queryResponse{}, safeShapeError("query")
	}
	body, err := p.do(ctx, http.MethodPost, "/api/v1/message/query", payload, "query")
	if err != nil {
		return queryResponse{}, err
	}
	var response envelope
	if err := json.Unmarshal(body, &response); err != nil || len(response.Data) == 0 || response.Metadata == nil {
		return queryResponse{}, safeShapeError("query")
	}
	if !validEnvelopeStatus(response.Status) {
		return queryResponse{}, &httpguard.Error{Provider: Kind, Op: "query", Reason: "API reported failure"}
	}
	if string(response.Data) == "null" {
		return queryResponse{}, safeShapeError("query")
	}
	var messages []wireMessage
	if err := json.Unmarshal(response.Data, &messages); err != nil {
		return queryResponse{}, safeShapeError("query")
	}
	metadata := response.Metadata
	if len(messages) > request.Limit || metadata.Count != len(messages) || metadata.Limit != request.Limit || metadata.Offset != request.Offset || metadata.Total < len(messages) {
		return queryResponse{}, safeShapeError("query")
	}
	for _, message := range messages {
		if message.OriginalROWID < 1 || strings.TrimSpace(message.GUID) == "" {
			return queryResponse{}, safeShapeError("query")
		}
	}
	return queryResponse{Messages: messages, Total: metadata.Total}, nil
}

func (p *Provider) do(ctx context.Context, method, path string, payload []byte, op string) ([]byte, error) {
	target := *p.base
	target.Path = path
	query := target.Query()
	query.Set("password", p.config.Password)
	target.RawQuery = query.Encode()
	request, err := http.NewRequest(method, target.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, &httpguard.Error{Provider: Kind, Op: op, Reason: "could not build request"}
	}
	request.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	return httpguard.Do(ctx, p.client, request, Kind, op, p.config.MaxResponseBytes)
}

func validEnvelopeStatus(status int) bool {
	return status >= http.StatusOK && status < http.StatusMultipleChoices
}

func safeShapeError(op string) error {
	return &httpguard.Error{Provider: Kind, Op: op, Reason: "malformed API response"}
}
