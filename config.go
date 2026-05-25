package main

import (
	"os"
	"strconv"
)

type Config struct {
	Addr             string
	OllamaURL        string
	ChatModel        string // primary model (qwen3:4b) — used with think:true and think:false
	InstructModel    string // optional separate instruct model override
	CoderModel       string // model used in workspace/project mode (e.g. qwen2.5-coder)
	EmbedModel       string
	DBPath           string
	UploadDir        string
	MaxContextTokens int
	SearchUserAgent  string

	// RAG tuning
	MaxSearchResults int // Total URLs to fetch from search engine (pool size)
	TargetSources    int // How many successful scrapes to aim for
	ChunkSize        int // Characters per text chunk
	ChunkOverlap     int // Overlap between consecutive chunks
	MaxCharsPerSite  int // Upper limit on scraped text per website
}

func LoadConfig() Config {
	return Config{
		Addr:             env("ADDR", ":8080"),
		OllamaURL:        env("OLLAMA_URL", "http://localhost:11434"),
		ChatModel:     env("CHAT_MODEL", "qwen3:4b"),
		InstructModel: env("INSTRUCT_MODEL", "qwen3:4b-instruct-2507-q4_K_M"),
		CoderModel:    env("CODER_MODEL", "deepseek-r1:1.5b"),
		EmbedModel:    env("EMBED_MODEL", "nomic-embed-text"),
		DBPath:           env("DB_PATH", "./data/assistant.db"),
		UploadDir:        env("UPLOAD_DIR", "./data/uploads"),
		MaxContextTokens: envInt("MAX_CONTEXT_TOKENS", 6000),
		SearchUserAgent:  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",

		MaxSearchResults: envInt("MAX_SEARCH_RESULTS", 12),
		TargetSources:    envInt("TARGET_SOURCES", 3),
		ChunkSize:        envInt("CHUNK_SIZE", 1200),
		ChunkOverlap:     envInt("CHUNK_OVERLAP", 150),
		MaxCharsPerSite:  envInt("MAX_CHARS_PER_SITE", 15000),
	}
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envInt(k string, d int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return d
}
