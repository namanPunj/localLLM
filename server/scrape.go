package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// URLScraper is satisfied by *rag.Pipeline.
type URLScraper interface {
	FetchClean(ctx context.Context, rawURL string) (string, error)
}

func (s *Server) handleScrape(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if req.URL == "" {
		http.Error(w, "url is required", http.StatusBadRequest)
		return
	}

	parsed, err := url.ParseRequestURI(req.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		http.Error(w, "invalid or non-http(s) URL", http.StatusBadRequest)
		return
	}

	text, err := s.scraper.FetchClean(r.Context(), req.URL)
	if err != nil {
		http.Error(w, fmt.Sprintf("scrape failed: %v", err), http.StatusBadGateway)
		return
	}

	// Build a human-readable filename from the URL path.
	name := parsed.Hostname()
	if path := strings.Trim(parsed.Path, "/"); path != "" {
		parts := strings.Split(path, "/")
		if last := parts[len(parts)-1]; last != "" {
			name = strings.TrimSuffix(last, ".html")
			name = strings.TrimSuffix(name, ".htm")
		}
	}
	name += ".txt"

	// Cap at 200 KB so it fits in context.
	const maxBytes = 200_000
	if len(text) > maxBytes {
		text = text[:maxBytes] + "\n\n[... content truncated to fit context ...]"
	}

	id, preview, err := s.files.Save(name, []byte(text))
	if err != nil {
		http.Error(w, "store failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]any{
		"file_id": id,
		"name":    name,
		"url":     req.URL,
		"size":    len(text),
		"preview": preview,
	})
}
