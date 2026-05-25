package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// Streamer writes one JSON object per line, flushing after each.
type Streamer struct {
	w   http.ResponseWriter
	f   http.Flusher
	mu  sync.Mutex
	enc *json.Encoder
}

func NewStreamer(w http.ResponseWriter) (*Streamer, bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no") // disable buffering on nginx
	return &Streamer{w: w, f: f, enc: json.NewEncoder(w)}, true
}

// Write writes one JSON value followed by a newline and flushes.
// Returns nil on success, or an error if the client has disconnected.
func (s *Streamer) Write(v any) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("write to closed connection")
		}
	}()
	if err = s.enc.Encode(v); err != nil {
		return err
	}
	s.f.Flush()
	return nil
}
