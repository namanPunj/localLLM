package sessions

import (
	"sync"
	"time"

	"github.com/google/uuid"

	"assistant/chat"
)

// MemoryStore is an in-memory SessionStore used for incognito chats.
// Sessions are never persisted to disk and don't appear in /sessions list.
// Idle entries are evicted after the configured TTL.
type MemoryStore struct {
	mu       sync.Mutex
	sessions map[string]*memSession
	ttl      time.Duration
}

type memSession struct {
	messages       []chat.Message
	touched        time.Time
	contextSummary string
	contextCursor  int
}

func NewMemoryStore(ttl time.Duration) *MemoryStore {
	if ttl <= 0 {
		ttl = time.Hour
	}
	m := &MemoryStore{
		sessions: map[string]*memSession{},
		ttl:      ttl,
	}
	go m.gcLoop()
	return m
}

func (m *MemoryStore) GetOrCreate(id string) (string, []chat.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id == "" {
		id = uuid.NewString()
	}
	s, ok := m.sessions[id]
	if !ok {
		m.sessions[id] = &memSession{touched: time.Now()}
		return id, nil, nil
	}
	s.touched = time.Now()
	out := make([]chat.Message, len(s.messages))
	copy(out, s.messages)
	return id, out, nil
}

func (m *MemoryStore) AppendMessage(id string, msg chat.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		s = &memSession{}
		m.sessions[id] = s
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	s.messages = append(s.messages, msg)
	s.touched = time.Now()
	return nil
}

func (m *MemoryStore) ReplaceHistory(id string, msgs []chat.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil
	}
	out := make([]chat.Message, len(msgs))
	copy(out, msgs)
	s.messages = out
	s.touched = time.Now()
	return nil
}

func (m *MemoryStore) GetContextSummary(id string) (string, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return "", 0, nil
	}
	return s.contextSummary, s.contextCursor, nil
}

func (m *MemoryStore) SetContextSummary(id string, summary string, cursor int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil
	}
	s.contextSummary = summary
	s.contextCursor = cursor
	return nil
}

func (m *MemoryStore) gcLoop() {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for range t.C {
		m.mu.Lock()
		now := time.Now()
		for k, s := range m.sessions {
			if now.Sub(s.touched) > m.ttl {
				delete(m.sessions, k)
			}
		}
		m.mu.Unlock()
	}
}
