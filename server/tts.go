package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// TikTok TTS — free, no API key, returns base64 MP3 in JSON.
// Male voice options: en_male_narration, en_us_010, en_uk_001, en_uk_003
const (
	tikTokTTSURL = "https://tiktok-tts.weilnet.workers.dev/api/generation"
	ttsVoice     = "en_uk_001" // deep natural male narrator
)

type ttsRequest struct {
	Text    string `json:"text"`
	Emotion string `json:"emotion"` // reserved for future use
}

func (s *Server) handleTTS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req ttsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	text := strings.TrimSpace(req.Text)
	if text == "" {
		http.Error(w, "empty text", http.StatusBadRequest)
		return
	}

	// Buffer all chunks then send with Content-Length so Chrome can range-seek.
	var buf bytes.Buffer
	for _, chunk := range splitText(text, 200) {
		if err := fetchChunk(&buf, chunk); err != nil {
			http.Error(w, "TTS error: "+err.Error(), http.StatusBadGateway)
			return
		}
	}

	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(buf.Bytes()) //nolint:errcheck
}

// fetchChunk calls TikTok TTS for one chunk, decodes the base64 MP3, and
// appends the raw bytes to dst.
func fetchChunk(dst *bytes.Buffer, text string) error {
	body, _ := json.Marshal(map[string]string{
		"text":  text,
		"voice": ttsVoice,
	})

	resp, err := http.Post(tikTokTTSURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	var result struct {
		Success bool   `json:"success"`
		Data    string `json:"data"`  // base64-encoded MP3
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	if !result.Success {
		return fmt.Errorf("TTS API: %s", result.Error)
	}

	mp3, err := base64.StdEncoding.DecodeString(result.Data)
	if err != nil {
		return fmt.Errorf("decode audio: %w", err)
	}

	dst.Write(mp3) //nolint:errcheck
	return nil
}

// splitText breaks text into chunks ≤ maxLen chars, splitting on sentence
// boundaries where possible so the TTS engine doesn't cut words.
func splitText(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}
	var chunks []string
	for len(text) > 0 {
		if len(text) <= maxLen {
			chunks = append(chunks, text)
			break
		}
		cut := maxLen
		for _, sep := range []string{". ", "! ", "? ", ", ", " "} {
			if idx := strings.LastIndex(text[:maxLen], sep); idx > 0 {
				cut = idx + len(sep)
				break
			}
		}
		chunks = append(chunks, strings.TrimSpace(text[:cut]))
		text = strings.TrimSpace(text[cut:])
	}
	return chunks
}
