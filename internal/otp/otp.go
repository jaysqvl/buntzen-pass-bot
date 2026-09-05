// Package otp defines the provider-neutral boundary used to receive Yodel
// one-time passcodes. Message bodies are deliberately confined to the
// short-lived RawMessage matching path; callers receive only the code and the
// minimum metadata needed to identify and consume a message.
package otp

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrPollWindowExceeded = errors.New("otp polling window exceeded")
	codePattern           = regexp.MustCompile(`(?:^|[^0-9])([0-9]{4,8})(?:[^0-9]|$)`)
	codeAfterKeyword      = regexp.MustCompile(`(?i)(?:verification|passcode|code)[^0-9]{0,24}([0-9]{4,8})(?:[^0-9]|$)`)
	codeBeforeKeyword     = regexp.MustCompile(`(?i)(?:^|[^0-9])([0-9]{4,8})[^0-9]{0,24}(?:verification|verify|passcode|code)\b`)
	yodelPattern          = regexp.MustCompile(`(?i)\byodel\b`)
)

// Provider is the complete capability exposed to the control plane. Arm must
// be called before the action requests a code so old inbox messages cannot be
// mistaken for the new challenge.
type Provider interface {
	Health(context.Context) error
	Arm(context.Context, Filter) (Armed, error)
	WaitForCode(context.Context, Armed) (Message, error)
}

// PairingProvider is the optional supervised-pairing capability implemented by
// providers whose inbox does not already have a scoped receiving address.
type PairingProvider interface {
	Provider
	WaitForPairingCandidates(context.Context, Armed) ([]Message, error)
}

// Filter describes the durable, non-secret fingerprint of an OTP source.
// Pairing permits an unpaired inbox scan, but still accepts only fresh inbound
// messages that identify Yodel and contain a plausible OTP.
type Filter struct {
	ChatGUID     string
	Sender       string
	Recipient    string
	Service      string
	FreshAfter   time.Time
	Pairing      bool
	RequireYodel bool
}

// Cursor is an opaque-enough provider cursor shared through the action
// lifecycle. Position is used by BlueBubbles ROWIDs; Timestamp and IDs are
// used by Twilio, whose message SIDs are not ordered integers.
type Cursor struct {
	Position  int64
	Timestamp time.Time
	IDs       []string
}

// Armed proves that a provider snapshot was taken before the action requested
// an OTP.
type Armed struct {
	Provider string
	ArmedAt  time.Time
	Cursor   Cursor
	Filter   Filter
}

// Message is the normalized, transient result returned by a provider. It must
// not be persisted because Code is secret.
type Message struct {
	ID         string
	Code       string
	Sender     string
	Recipient  string
	ChatGUID   string
	Service    string
	ReceivedAt time.Time
	Cursor     int64
}

// MaskedSender returns sender metadata safe for the pairing confirmation UI.
func (m Message) MaskedSender() string {
	return MaskAddress(m.Sender)
}

// RawMessage exists only at the provider/matcher boundary. Match and Select
// intentionally omit Body from their returned Message values.
type RawMessage struct {
	ID         string
	Body       string
	Sender     string
	Recipient  string
	ChatGUID   string
	Service    string
	ReceivedAt time.Time
	Cursor     int64
	Inbound    bool
}

// ExtractCode prefers a standalone 4-8 digit sequence near a code keyword,
// then falls back to the first standalone sequence of that length.
func ExtractCode(body string) (string, bool) {
	for _, pattern := range []*regexp.Regexp{codeAfterKeyword, codeBeforeKeyword, codePattern} {
		match := pattern.FindStringSubmatch(body)
		if len(match) == 2 {
			return match[1], true
		}
	}
	return "", false
}

// IsYodelMessage reports whether body identifies Yodel as a distinct word.
func IsYodelMessage(body string) bool {
	return yodelPattern.MatchString(body)
}

// Match validates direction, freshness, the paired source fingerprint, and
// OTP syntax. It never copies the source message body into the result.
func Match(raw RawMessage, filter Filter) (Message, bool) {
	if !raw.Inbound || strings.TrimSpace(raw.ID) == "" {
		return Message{}, false
	}
	if filter.Pairing && (strings.TrimSpace(raw.ChatGUID) == "" || strings.TrimSpace(raw.Sender) == "" || strings.TrimSpace(raw.Service) == "") {
		return Message{}, false
	}
	if (filter.Pairing || filter.RequireYodel) && !IsYodelMessage(raw.Body) {
		return Message{}, false
	}
	if !filter.FreshAfter.IsZero() && (raw.ReceivedAt.IsZero() || raw.ReceivedAt.Before(filter.FreshAfter)) {
		return Message{}, false
	}
	if filter.ChatGUID != "" && raw.ChatGUID != filter.ChatGUID {
		return Message{}, false
	}
	if filter.Sender != "" && !SameAddress(raw.Sender, filter.Sender) {
		return Message{}, false
	}
	if filter.Recipient != "" && !SameAddress(raw.Recipient, filter.Recipient) {
		return Message{}, false
	}
	if filter.Service != "" && !strings.EqualFold(strings.TrimSpace(raw.Service), strings.TrimSpace(filter.Service)) {
		return Message{}, false
	}
	code, ok := ExtractCode(raw.Body)
	if !ok {
		return Message{}, false
	}
	return Message{
		ID:         raw.ID,
		Code:       code,
		Sender:     raw.Sender,
		Recipient:  raw.Recipient,
		ChatGUID:   raw.ChatGUID,
		Service:    raw.Service,
		ReceivedAt: raw.ReceivedAt,
		Cursor:     raw.Cursor,
	}, true
}

// Select normalizes matching messages, suppresses duplicate provider IDs, and
// returns newest candidates first with a stable cursor tie-break.
func Select(raw []RawMessage, filter Filter, seen *Deduper) []Message {
	result := make([]Message, 0, len(raw))
	for _, candidate := range raw {
		if seen != nil && !seen.Add(candidate.ID) {
			continue
		}
		message, ok := Match(candidate, filter)
		if ok {
			result = append(result, message)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].ReceivedAt.Equal(result[j].ReceivedAt) {
			if result[i].Cursor == result[j].Cursor {
				return result[i].ID > result[j].ID
			}
			return result[i].Cursor > result[j].Cursor
		}
		return result[i].ReceivedAt.After(result[j].ReceivedAt)
	})
	return result
}

// Deduper is a bounded, concurrency-safe provider-message ID set.
type Deduper struct {
	mu       sync.Mutex
	capacity int
	order    []string
	seen     map[string]struct{}
}

func NewDeduper(capacity int, seed ...string) *Deduper {
	if capacity < 1 {
		capacity = 1
	}
	d := &Deduper{capacity: capacity, seen: make(map[string]struct{}, capacity)}
	for _, id := range seed {
		d.Add(id)
	}
	return d
}

// Add records id and reports whether it had not been seen before. Empty IDs
// are always rejected because they cannot be consumed or deduplicated safely.
func (d *Deduper) Add(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.seen[id]; ok {
		return false
	}
	d.seen[id] = struct{}{}
	d.order = append(d.order, id)
	if len(d.order) > d.capacity {
		oldest := d.order[0]
		d.order = d.order[1:]
		delete(d.seen, oldest)
	}
	return true
}

// SameAddress compares phone-like identifiers by digits and other identifiers
// case-insensitively. It does not guess or add country codes.
func SameAddress(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" {
		return left == right
	}
	leftDigits, leftPhone := phoneDigits(left)
	rightDigits, rightPhone := phoneDigits(right)
	if leftPhone && rightPhone {
		return leftDigits == rightDigits
	}
	return strings.EqualFold(left, right)
}

func phoneDigits(value string) (string, bool) {
	var digits strings.Builder
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == '+', r == '-', r == '(', r == ')', r == '.', r == ' ', r == '\t':
		default:
			return "", false
		}
	}
	return digits.String(), digits.Len() > 0
}

// MaskAddress returns sender metadata suitable for supervised-pairing UI.
func MaskAddress(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	if at := strings.LastIndex(value, "@"); at > 0 && at < len(value)-1 {
		return value[:1] + "***" + value[at:]
	}
	digits, phone := phoneDigits(value)
	if phone {
		visible := 2
		if len(digits) >= 7 {
			visible = 4
		}
		if len(digits) <= visible {
			return strings.Repeat("*", len(digits))
		}
		return strings.Repeat("*", len(digits)-visible) + digits[len(digits)-visible:]
	}
	if len(value) == 1 {
		return "*"
	}
	return value[:1] + strings.Repeat("*", len(value)-1)
}
