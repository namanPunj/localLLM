package main

import (
	"context"
	"log"
	"time"

	"assistant/chat"
	"assistant/files"
	"assistant/rag"
	"assistant/server"
	"assistant/sessions"
)

func main() {
	cfg := LoadConfig()
	ctx := context.Background()

	store, err := sessions.NewStore(cfg.DBPath)
	if err != nil {
		log.Fatalf("sessions store: %v", err)
	}
	memStore := sessions.NewMemoryStore(time.Hour)

	fileStore := files.NewStore(cfg.UploadDir)
	ollama := chat.NewOllama(cfg.OllamaURL, cfg.ChatModel, cfg.InstructModel, cfg.CoderModel, cfg.EmbedModel)
	ragPipe := rag.NewPipeline(ollama, cfg.SearchUserAgent, rag.Config{
		MaxSearchResults: cfg.MaxSearchResults,
		TargetSources:    cfg.TargetSources,
		ChunkSize:        cfg.ChunkSize,
		ChunkOverlap:     cfg.ChunkOverlap,
		MaxCharsPerSite:  cfg.MaxCharsPerSite,
	})

	calSvc, _, err := server.GetGoogleClients(ctx)
	if err != nil {
		log.Fatalf("google clients: %v", err)
	}
	personalMemory := server.NewPersonalMemory("./data/memory.json")
	exec := &server.ToolExecutor{CalendarService: calSvc, RAG: ragPipe, Memory: personalMemory}

	pipeline := &chat.Pipeline{
		Ollama:     ollama,
		Sessions:   store,
		IncogStore: memStore,
		Files:      fileStore,
		RAG:        ragPipe,
		Executor:   exec,
		MaxCtxTok:  cfg.MaxContextTokens,
		Memory:     personalMemory,
	}

	settingsStore := server.NewSettingsStore(
		"./data/settings.json",
		server.RuntimeSettings{
			MaxContextTokens: cfg.MaxContextTokens,
			RAGSnippets:      8,
			TargetSources:    cfg.TargetSources,
			MaxSearchResults: cfg.MaxSearchResults,
			ChunkSize:        cfg.ChunkSize,
			ChunkOverlap:     cfg.ChunkOverlap,
			MaxCharsPerSite:  cfg.MaxCharsPerSite,
			FileRAGChunks:    12,  // retrieve 12 most relevant file chunks by default
		},
		func(s server.RuntimeSettings) {
			// Propagate RAG config changes live.
			ragPipe.SetConfig(rag.Config{
				MaxSearchResults: s.MaxSearchResults,
				TargetSources:    s.TargetSources,
				ChunkSize:        s.ChunkSize,
				ChunkOverlap:     s.ChunkOverlap,
				MaxCharsPerSite:  s.MaxCharsPerSite,
			})
		},
	)

	routineStore := server.NewRoutineStore("./data/routines.json")
	pushStore := server.NewPushStore("./data/vapid.json", "./data/push_subscriptions.json")

	srv := server.New(pipeline, store, fileStore, ragPipe, settingsStore, personalMemory, ollama, routineStore, pushStore)
	log.Printf("listening on %s", cfg.Addr)
	if err := srv.Run(cfg.Addr); err != nil {
		log.Fatal(err)
	}
}