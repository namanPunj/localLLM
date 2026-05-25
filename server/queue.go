package server

import (
	"sync"

	"assistant/chat"
)

const (
	maxBufEvents = 4000 // max events kept per session for stream replay
)

// queueEntry is one pending chat request in the global queue.
type queueEntry struct {
	req       chat.ChatRequest
	termMode  bool
	cancelled bool // set to true to skip this entry without processing

	// events is the pipe from the worker → originating HTTP handler.
	// Large buffer so the worker never blocks on a disconnected client.
	events chan chat.StreamEvent
}

// sessionReplay holds the replay buffer and live subscribers for one session,
// allowing clients to reconnect mid-stream after a page reload.
type sessionReplay struct {
	mu      sync.Mutex
	buf     []chat.StreamEvent
	bufDone bool
	subs    map[chan<- chat.StreamEvent]struct{}
}

func newSessionReplay() *sessionReplay {
	return &sessionReplay{subs: make(map[chan<- chat.StreamEvent]struct{})}
}

func (sr *sessionReplay) clear() {
	sr.mu.Lock()
	sr.buf = sr.buf[:0]
	sr.bufDone = false
	sr.mu.Unlock()
}

// subscribe returns the replay buffer and registers ch for live events.
func (sr *sessionReplay) subscribe(ch chan<- chat.StreamEvent) []chat.StreamEvent {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	out := make([]chat.StreamEvent, len(sr.buf))
	copy(out, sr.buf)
	if !sr.bufDone {
		sr.subs[ch] = struct{}{}
	}
	return out
}

func (sr *sessionReplay) unsubscribe(ch chan<- chat.StreamEvent) {
	sr.mu.Lock()
	delete(sr.subs, ch)
	sr.mu.Unlock()
}

// broadcast appends ev to the replay buffer, sends it to the originating
// HTTP handler (non-blocking), and fans it out to live reconnect subscribers.
func (sr *sessionReplay) broadcast(ev chat.StreamEvent, entryCh chan<- chat.StreamEvent) {
	sr.mu.Lock()
	if len(sr.buf) < maxBufEvents {
		sr.buf = append(sr.buf, ev)
	}
	if ev.Type == "done" || ev.Type == "error" {
		sr.bufDone = true
	}
	subs := make([]chan<- chat.StreamEvent, 0, len(sr.subs))
	for ch := range sr.subs {
		subs = append(subs, ch)
	}
	sr.mu.Unlock()

	// Originating HTTP handler — non-blocking; if the client is gone the
	// buffer fills up and we skip (events are in sr.buf for later replay).
	if entryCh != nil {
		select {
		case entryCh <- ev:
		default:
		}
	}
	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// GlobalQueue is a single ordered queue shared across all sessions.
// Requests are processed one at a time — matching Ollama's single-threaded
// inference, so there is no benefit to parallel processing.
type GlobalQueue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	items   []*queueEntry
	current *queueEntry     // entry currently being processed by the worker
	cancel  func()          // cancels the currently running pipeline

	// replays holds per-session replay buffers, created on first use.
	replays map[string]*sessionReplay
}

func NewGlobalQueue() *GlobalQueue {
	gq := &GlobalQueue{
		replays: make(map[string]*sessionReplay),
	}
	gq.cond = sync.NewCond(&gq.mu)
	return gq
}

// Enqueue adds an entry to the back of the global queue.
// Returns false if the queue already holds maxQueueDepth entries.
func (gq *GlobalQueue) Enqueue(e *queueEntry) bool {
	gq.mu.Lock()
	defer gq.mu.Unlock()
	const maxQueueDepth = 50
	if len(gq.items) >= maxQueueDepth {
		return false
	}
	gq.items = append(gq.items, e)
	gq.cond.Signal()
	return true
}

// CancelSession cancels the currently running request if it belongs to sid,
// and removes all pending entries for sid from the queue.
func (gq *GlobalQueue) CancelSession(sid string) {
	gq.mu.Lock()
	// Remove pending entries for this session.
	filtered := gq.items[:0]
	for _, e := range gq.items {
		if e.req.SessionID == sid {
			close(e.events) // unblock the waiting HTTP handler
		} else {
			filtered = append(filtered, e)
		}
	}
	gq.items = filtered
	// If the current item belongs to this session, cancel it.
	cancel := gq.cancel
	isCurrent := gq.current != nil && gq.current.req.SessionID == sid
	gq.mu.Unlock()

	if isCurrent && cancel != nil {
		cancel()
	}
}

// Status returns the number of queued (waiting) entries and whether a request
// for the given session is currently active or waiting.
func (gq *GlobalQueue) Status(sid string) (active bool, queued int) {
	gq.mu.Lock()
	defer gq.mu.Unlock()
	if gq.current != nil && gq.current.req.SessionID == sid {
		active = true
	}
	for _, e := range gq.items {
		if e.req.SessionID == sid {
			queued++
		}
	}
	return
}

// GlobalQueued returns total entries waiting (all sessions).
func (gq *GlobalQueue) GlobalQueued() int {
	gq.mu.Lock()
	defer gq.mu.Unlock()
	return len(gq.items)
}

// GetOrCreateReplay returns the sessionReplay for sid, creating it if needed.
func (gq *GlobalQueue) GetOrCreateReplay(sid string) *sessionReplay {
	gq.mu.Lock()
	defer gq.mu.Unlock()
	if sr, ok := gq.replays[sid]; ok {
		return sr
	}
	sr := newSessionReplay()
	gq.replays[sid] = sr
	return sr
}

// GetReplay returns the sessionReplay for sid, or nil if it doesn't exist.
func (gq *GlobalQueue) GetReplay(sid string) *sessionReplay {
	gq.mu.Lock()
	defer gq.mu.Unlock()
	return gq.replays[sid]
}

// next blocks until a non-cancelled entry is available, then returns it.
func (gq *GlobalQueue) next() *queueEntry {
	gq.mu.Lock()
	defer gq.mu.Unlock()
	for {
		for len(gq.items) == 0 {
			gq.cond.Wait()
		}
		e := gq.items[0]
		gq.items = gq.items[1:]
		if e.cancelled {
			close(e.events)
			continue
		}
		gq.current = e
		return e
	}
}

func (gq *GlobalQueue) setCancel(cancel func()) {
	gq.mu.Lock()
	gq.cancel = cancel
	gq.mu.Unlock()
}

func (gq *GlobalQueue) clearCurrent() {
	gq.mu.Lock()
	gq.current = nil
	gq.cancel = nil
	gq.mu.Unlock()
}
