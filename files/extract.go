package files

import (
	"archive/zip"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ledongthuc/pdf"
)

type Store struct {
	dir   string
	mu    sync.RWMutex
	cache map[string]string // file_id -> extracted text
}

func NewStore(dir string) *Store {
	_ = os.MkdirAll(dir, 0o755)
	return &Store{dir: dir, cache: map[string]string{}}
}

// Save persists raw bytes to disk, extracts text, caches it, and returns the file id.
func (s *Store) Save(filename string, data []byte) (string, string, error) {
	hash := sha1.Sum(data)
	id := hex.EncodeToString(hash[:])
	ext := strings.ToLower(filepath.Ext(filename))

	path := filepath.Join(s.dir, id+ext)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", "", err
	}

	text, err := extractText(ext, data)
	if err != nil {
		return "", "", fmt.Errorf("extract: %w", err)
	}

	s.mu.Lock()
	s.cache[id] = text
	s.mu.Unlock()

	preview := text
	if len(preview) > 200 {
		preview = preview[:200] + "…"
	}
	return id, preview, nil
}

func (s *Store) Get(id string) (string, bool) {
	s.mu.RLock()
	t, ok := s.cache[id]
	s.mu.RUnlock()
	if ok {
		return t, true
	}

	// Cache miss — try to reload from disk (survives server restart).
	matches, _ := filepath.Glob(filepath.Join(s.dir, id+".*"))
	if len(matches) == 0 {
		// Try bare filename (no extension)
		p := filepath.Join(s.dir, id)
		if _, err := os.Stat(p); err == nil {
			matches = []string{p}
		}
	}
	if len(matches) == 0 {
		return "", false
	}

	data, err := os.ReadFile(matches[0])
	if err != nil {
		return "", false
	}
	ext := strings.ToLower(filepath.Ext(matches[0]))
	text, err := extractText(ext, data)
	if err != nil {
		return "", false
	}

	s.mu.Lock()
	s.cache[id] = text
	s.mu.Unlock()
	return text, true
}

// DeleteAll removes every file from cache and disk.
func (s *Store) DeleteAll() {
	s.mu.Lock()
	s.cache = map[string]string{}
	s.mu.Unlock()
	matches, _ := filepath.Glob(filepath.Join(s.dir, "*"))
	for _, m := range matches {
		os.Remove(m)
	}
}

// Delete removes a file from the cache and disk. It tries common extensions
// since the file ID doesn't encode the original extension.
func (s *Store) Delete(id string) {
	s.mu.Lock()
	delete(s.cache, id)
	s.mu.Unlock()

	// Try removing files matching id.* from the upload dir
	matches, _ := filepath.Glob(filepath.Join(s.dir, id+".*"))
	for _, m := range matches {
		os.Remove(m)
	}
	// Also try bare id (no extension)
	os.Remove(filepath.Join(s.dir, id))
}

func extractText(ext string, data []byte) (string, error) {
	switch ext {
	case ".txt", ".md", ".markdown", ".csv", ".tsv", ".json", ".log",
		".go", ".py", ".js", ".ts", ".rs", ".c", ".cpp", ".h", ".java",
		".rb", ".sh", ".yaml", ".yml", ".toml", ".html", ".xml":
		return string(data), nil
	case ".pdf":
		return extractPDF(data)
	case ".docx":
		return extractDOCX(data)
	default:
		// best-effort: treat as text if it decodes
		if isTextish(data) {
			return string(data), nil
		}
		return "", fmt.Errorf("unsupported file type: %s", ext)
	}
}

func extractPDF(data []byte) (string, error) {
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for i := 1; i <= r.NumPage(); i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		text, err := p.GetPlainText(nil)
		if err != nil {
			continue
		}
		sb.WriteString(text)
		sb.WriteString("\n\n")
	}
	return strings.TrimSpace(sb.String()), nil
}

func extractDOCX(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	var doc *zip.File
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			doc = f
			break
		}
	}
	if doc == nil {
		return "", fmt.Errorf("docx: missing word/document.xml")
	}
	rc, err := doc.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return "", err
	}
	return stripDocxXML(string(body)), nil
}

// stripDocxXML extracts visible text from document.xml. Crude but works for most docs.
func stripDocxXML(xml string) string {
	var sb strings.Builder
	i := 0
	for i < len(xml) {
		// find a <w:t...> opening
		start := strings.Index(xml[i:], "<w:t")
		if start == -1 {
			break
		}
		start += i
		// move to '>'
		gt := strings.Index(xml[start:], ">")
		if gt == -1 {
			break
		}
		textStart := start + gt + 1
		// find closing </w:t>
		end := strings.Index(xml[textStart:], "</w:t>")
		if end == -1 {
			break
		}
		sb.WriteString(xml[textStart : textStart+end])
		// emit paragraph break when we see </w:p>
		i = textStart + end + len("</w:t>")
		// if the next </w:p> is closer than the next <w:t, add newline
		nextP := strings.Index(xml[i:], "</w:p>")
		nextT := strings.Index(xml[i:], "<w:t")
		if nextP != -1 && (nextT == -1 || nextP < nextT) {
			sb.WriteString("\n\n")
		}
	}
	return strings.TrimSpace(sb.String())
}

func isTextish(data []byte) bool {
	n := len(data)
	if n > 4096 {
		n = 4096
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return false
		}
	}
	return true
}
