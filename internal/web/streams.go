package web

import (
	"net/http"
	"sync"
	"time"
)

const (
	maxStreamsPerUser  = 8
	maxStreams         = 128
	streamWriteTimeout = 5 * time.Second
)

type streamAdmission struct {
	mu    sync.Mutex
	total int
	users map[int64]int
}

func (a *streamAdmission) acquire(userID int64) func() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if userID <= 0 || a.total >= maxStreams || a.users[userID] >= maxStreamsPerUser {
		return nil
	}
	if a.users == nil {
		a.users = make(map[int64]int)
	}
	a.total++
	a.users[userID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.total--
			a.users[userID]--
			if a.users[userID] == 0 {
				delete(a.users, userID)
			}
		})
	}
}

type eventStream struct{ controller *http.ResponseController }

func newEventStream(w http.ResponseWriter) (*eventStream, error) {
	stream := &eventStream{controller: http.NewResponseController(w)}
	// Fail closed if a response wrapper cannot provide transport deadlines.
	if err := stream.controller.SetWriteDeadline(time.Now().Add(streamWriteTimeout)); err != nil {
		return nil, err
	}
	return stream, nil
}

func (s *eventStream) batch(write func() error) error {
	if err := s.controller.SetWriteDeadline(time.Now().Add(streamWriteTimeout)); err != nil {
		return err
	}
	if err := write(); err != nil {
		return err
	}
	if err := s.controller.Flush(); err != nil {
		return err
	}
	// Idle streams may live longer than one write budget. Every later write
	// including keepalives and expiry notifications starts a fresh budget.
	return s.controller.SetWriteDeadline(time.Time{})
}

func (s *eventStream) finish() {
	// net/http flushes a final chunk after the handler returns. Keep that write
	// bounded too, including when cancellation interrupts an idle stream.
	_ = s.controller.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
}
