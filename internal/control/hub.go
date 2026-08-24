package control

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/otp"
)

var (
	ErrDecisionNotPending = errors.New("job is not awaiting a decision")
	ErrDecisionAlreadySet = errors.New("job decision was already submitted")
	ErrPairingNotPending  = errors.New("job is not awaiting a pairing choice")
	ErrPairingAlreadySet  = errors.New("pairing choice was already submitted")
)

type LiveEvent struct {
	Kind string
	Data any
}

type OTPState struct {
	Code      string
	ExpiresAt time.Time
}

type decisionWaiter struct {
	ch      chan string
	decided bool
}

type pairingWaiter struct {
	ch         chan otp.Message
	candidates map[string]otp.Message
	selected   bool
}

// Hub contains deliberately ephemeral state only. Raw OTP values and live
// approval commands never enter SQLite or durable job events.
type Hub struct {
	mu          sync.RWMutex
	subscribers map[string]map[chan LiveEvent]struct{}
	otps        map[string]OTPState
	decisions   map[string]*decisionWaiter
	pairings    map[string]*pairingWaiter
}

func NewHub() *Hub {
	return &Hub{
		subscribers: make(map[string]map[chan LiveEvent]struct{}),
		otps:        make(map[string]OTPState),
		decisions:   make(map[string]*decisionWaiter),
		pairings:    make(map[string]*pairingWaiter),
	}
}

func (h *Hub) Subscribe(jobID string) (<-chan LiveEvent, func()) {
	ch := make(chan LiveEvent, 32)
	h.mu.Lock()
	if h.subscribers[jobID] == nil {
		h.subscribers[jobID] = make(map[chan LiveEvent]struct{})
	}
	h.subscribers[jobID][ch] = struct{}{}
	if otp, ok := h.activeOTPLocked(jobID, time.Now()); ok {
		ch <- LiveEvent{Kind: "otp", Data: map[string]any{"active": true, "code": otp.Code}}
	}
	if pairing, ok := h.pairings[jobID]; ok {
		ch <- LiveEvent{Kind: "pairing", Data: pairingPublicData(pairing)}
	}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if subscribers := h.subscribers[jobID]; subscribers != nil {
			if _, ok := subscribers[ch]; ok {
				delete(subscribers, ch)
				close(ch)
			}
			if len(subscribers) == 0 {
				delete(h.subscribers, jobID)
			}
		}
		h.mu.Unlock()
	}
}

// SetPairingCandidates retains candidate codes and fingerprints only in
// memory. The public event includes the active code and masked sender so an
// operator can confirm the right Messages source without persisting either.
func (h *Hub) SetPairingCandidates(jobID string, candidates []otp.Message) error {
	if len(candidates) == 0 {
		return ErrPairingNotPending
	}
	waiter := &pairingWaiter{ch: make(chan otp.Message, 1), candidates: make(map[string]otp.Message, len(candidates))}
	for _, candidate := range candidates {
		if candidate.ID != "" {
			waiter.candidates[candidate.ID] = candidate
		}
	}
	if len(waiter.candidates) == 0 {
		return ErrPairingNotPending
	}
	h.mu.Lock()
	if _, exists := h.pairings[jobID]; exists {
		h.mu.Unlock()
		return ErrPairingAlreadySet
	}
	h.pairings[jobID] = waiter
	h.mu.Unlock()
	h.Publish(jobID, LiveEvent{Kind: "pairing", Data: pairingPublicData(waiter)})
	return nil
}

func pairingPublicData(waiter *pairingWaiter) map[string]any {
	views := make([]map[string]any, 0, len(waiter.candidates))
	for _, candidate := range waiter.candidates {
		views = append(views, map[string]any{
			"id": candidate.ID, "code": candidate.Code,
			"masked_sender": candidate.MaskedSender(), "service": candidate.Service,
		})
	}
	return map[string]any{"active": true, "candidates": views}
}

func (h *Hub) ChoosePairing(jobID, messageID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	waiter, ok := h.pairings[jobID]
	if !ok {
		return ErrPairingNotPending
	}
	if waiter.selected {
		return ErrPairingAlreadySet
	}
	candidate, ok := waiter.candidates[messageID]
	if !ok {
		return ErrPairingNotPending
	}
	waiter.selected = true
	waiter.ch <- candidate
	return nil
}

func (h *Hub) WaitPairing(ctx context.Context, jobID string) (otp.Message, error) {
	h.mu.RLock()
	waiter, ok := h.pairings[jobID]
	h.mu.RUnlock()
	if !ok {
		return otp.Message{}, ErrPairingNotPending
	}
	select {
	case selected := <-waiter.ch:
		h.ClearPairing(jobID)
		return selected, nil
	case <-ctx.Done():
		h.ClearPairing(jobID)
		return otp.Message{}, ctx.Err()
	}
}

func (h *Hub) ClearPairing(jobID string) {
	h.mu.Lock()
	delete(h.pairings, jobID)
	h.mu.Unlock()
	h.Publish(jobID, LiveEvent{Kind: "pairing", Data: map[string]any{"active": false}})
}

func (h *Hub) Publish(jobID string, event LiveEvent) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.subscribers[jobID] {
		select {
		case ch <- event:
		default:
			// A stale browser must never block the worker. The next durable state
			// event or page refresh reconciles non-secret status.
		}
	}
}

func (h *Hub) SetOTP(jobID, code string, expiresAt time.Time) {
	h.mu.Lock()
	h.otps[jobID] = OTPState{Code: code, ExpiresAt: expiresAt}
	h.mu.Unlock()
	h.Publish(jobID, LiveEvent{Kind: "otp", Data: map[string]any{"active": true, "code": code}})
	if !expiresAt.IsZero() {
		go h.expireOTP(jobID, code, expiresAt)
	}
}

func (h *Hub) expireOTP(jobID, code string, expiresAt time.Time) {
	timer := time.NewTimer(time.Until(expiresAt))
	defer timer.Stop()
	<-timer.C
	h.mu.Lock()
	current, ok := h.otps[jobID]
	if ok && current.Code == code && current.ExpiresAt.Equal(expiresAt) {
		delete(h.otps, jobID)
	}
	h.mu.Unlock()
	if ok && current.Code == code && current.ExpiresAt.Equal(expiresAt) {
		h.Publish(jobID, LiveEvent{Kind: "otp", Data: map[string]any{"active": false}})
	}
}

func (h *Hub) OTP(jobID string, now time.Time) (OTPState, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.activeOTPLocked(jobID, now)
}

func (h *Hub) activeOTPLocked(jobID string, now time.Time) (OTPState, bool) {
	otp, ok := h.otps[jobID]
	if !ok {
		return OTPState{}, false
	}
	if !otp.ExpiresAt.IsZero() && !now.Before(otp.ExpiresAt) {
		delete(h.otps, jobID)
		return OTPState{}, false
	}
	return otp, true
}

func (h *Hub) ClearOTP(jobID string) {
	h.mu.Lock()
	delete(h.otps, jobID)
	h.mu.Unlock()
	h.Publish(jobID, LiveEvent{Kind: "otp", Data: map[string]any{"active": false}})
}

func (h *Hub) BeginDecision(jobID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.decisions[jobID]; exists {
		return ErrDecisionAlreadySet
	}
	h.decisions[jobID] = &decisionWaiter{ch: make(chan string, 1)}
	return nil
}

func (h *Hub) Decide(jobID, decision string) error {
	h.mu.Lock()
	waiter, ok := h.decisions[jobID]
	if !ok {
		h.mu.Unlock()
		return ErrDecisionNotPending
	}
	if waiter.decided {
		h.mu.Unlock()
		return ErrDecisionAlreadySet
	}
	waiter.decided = true
	waiter.ch <- decision
	h.mu.Unlock()
	return nil
}

func (h *Hub) WaitDecision(ctx context.Context, jobID string) (string, error) {
	h.mu.RLock()
	waiter, ok := h.decisions[jobID]
	h.mu.RUnlock()
	if !ok {
		return "", ErrDecisionNotPending
	}
	select {
	case decision := <-waiter.ch:
		h.mu.Lock()
		delete(h.decisions, jobID)
		h.mu.Unlock()
		return decision, nil
	case <-ctx.Done():
		h.mu.Lock()
		delete(h.decisions, jobID)
		h.mu.Unlock()
		return "", ctx.Err()
	}
}

func (h *Hub) CancelJob(jobID string) {
	h.ClearOTP(jobID)
	h.ClearPairing(jobID)
	h.mu.Lock()
	if waiter, ok := h.decisions[jobID]; ok && !waiter.decided {
		waiter.decided = true
		waiter.ch <- "cancel"
	}
	h.mu.Unlock()
}
