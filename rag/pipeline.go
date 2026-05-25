package rag

import (
	"context"
	"sync"
)

// ProgressFunc is called at each pipeline stage so callers can stream progress to the client.
// Stages emitted:
//
//	"search_start"      {query}
//	"search_results"    {count, urls}
//	"search_failed"     {error}
//	"fetch_start"       {url}
//	"fetch_done"        {url, chars}
//	"fetch_skipped"     {url, reason}
//	"embed_start"       {chunks}
//	"embed_done"        {kept}
//	"rank_done"         {selected}
type ProgressFunc = func(stage string, data map[string]any)

// Embedder is the minimal interface Pipeline needs.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
}

// Config holds tuning knobs for the RAG pipeline.
type Config struct {
	MaxSearchResults int // Total URLs to fetch from DDG (pool size)
	TargetSources    int // How many successful scrapes to aim for
	ChunkSize        int // Characters per chunk
	ChunkOverlap     int // Overlap between chunks
	MaxCharsPerSite  int // Upper limit on scraped text per website
}

type Pipeline struct {
	Embed     Embedder
	UserAgent string
	mu        sync.RWMutex
	Cfg       Config
}

// SetConfig replaces the pipeline's tuning config atomically.
func (p *Pipeline) SetConfig(cfg Config) {
	p.mu.Lock()
	p.Cfg = cfg
	p.mu.Unlock()
}

func NewPipeline(embed Embedder, userAgent string, cfg Config) *Pipeline {
	// Apply defaults if zero
	if cfg.MaxSearchResults <= 0 {
		cfg.MaxSearchResults = 12
	}
	if cfg.TargetSources <= 0 {
		cfg.TargetSources = 5
	}
	if cfg.ChunkSize <= 0 {
		cfg.ChunkSize = 1200
	}
	if cfg.ChunkOverlap <= 0 {
		cfg.ChunkOverlap = 150
	}
	if cfg.MaxCharsPerSite <= 0 {
		cfg.MaxCharsPerSite = 15000
	}
	return &Pipeline{Embed: embed, UserAgent: userAgent, Cfg: cfg}
}

// Search runs the full RAG flow and returns the top-K text snippets + their source URLs.
// Each stage emits a progress event.
func (p *Pipeline) Search(ctx context.Context, query string, k int, progress ProgressFunc) ([]string, []string, error) {
	if progress == nil {
		progress = func(string, map[string]any) {}
	}
	p.mu.RLock()
	cfg := p.Cfg
	p.mu.RUnlock()

	progress("search_start", map[string]any{"query": query})

	results, err := p.Search_DDG(ctx, query, cfg.MaxSearchResults)
	if err != nil {
		progress("search_failed", map[string]any{"error": err.Error()})
		return nil, nil, err
	}
	if len(results) == 0 {
		progress("search_failed", map[string]any{"error": "no results"})
		return nil, nil, nil
	}

	urls := make([]string, len(results))
	for i, r := range results {
		urls[i] = r.URL
	}
	progress("search_results", map[string]any{
		"count": len(results),
		"urls":  urls,
	})

	// Fetch pages concurrently. We search a larger pool (MaxSearchResults)
	// and keep the first TargetSources successful scrapes.
	type fetched struct {
		url, text string
		ok        bool
	}
	pages := make([]fetched, len(results))
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i, r := range results {
		wg.Add(1)
		go func(i int, r SearchResult) {
			defer wg.Done()
			mu.Lock()
			progress("fetch_start", map[string]any{"url": r.URL})
			mu.Unlock()

			txt, err := p.FetchClean(ctx, r.URL)
			mu.Lock()
			if err != nil {
				progress("fetch_skipped", map[string]any{
					"url":    r.URL,
					"reason": err.Error(),
				})
				pages[i] = fetched{r.URL, "", false}
			} else if len(txt) < 200 {
				progress("fetch_skipped", map[string]any{
					"url":    r.URL,
					"reason": "page too short",
				})
				pages[i] = fetched{r.URL, "", false}
			} else {
				// Truncate to MaxCharsPerSite
				if len(txt) > cfg.MaxCharsPerSite {
					txt = txt[:cfg.MaxCharsPerSite]
				}
				progress("fetch_done", map[string]any{
					"url":   r.URL,
					"chars": len(txt),
				})
				pages[i] = fetched{r.URL, txt, true}
			}
			mu.Unlock()
		}(i, r)
	}
	wg.Wait()

	// Keep first TargetSources successful pages (preserves search-rank order)
	var kept []fetched
	for _, pg := range pages {
		if pg.ok {
			kept = append(kept, pg)
			if len(kept) >= cfg.TargetSources {
				break
			}
		}
	}

	if len(kept) == 0 {
		progress("rank_done", map[string]any{"selected": 0})
		return nil, nil, nil
	}

	// Chunk every page
	var items []VecItem
	for _, pg := range kept {
		for _, c := range Chunk(pg.text, cfg.ChunkSize, cfg.ChunkOverlap) {
			items = append(items, VecItem{Text: c, Source: pg.url})
		}
	}
	if len(items) == 0 {
		progress("rank_done", map[string]any{"selected": 0})
		return nil, nil, nil
	}

	progress("embed_start", map[string]any{"chunks": len(items)})

	// Batch-embed all chunks + query in a single API call
	texts := make([]string, len(items)+1)
	for i, it := range items {
		texts[i] = it.Text
	}
	texts[len(items)] = query // query is the last element

	vecs, err := p.Embed.EmbedBatch(ctx, texts)
	if err != nil {
		progress("search_failed", map[string]any{"error": "embed batch: " + err.Error()})
		return nil, nil, err
	}

	keptCount := 0
	for i := range items {
		items[i].Vec = vecs[i]
		keptCount++
	}
	qv := vecs[len(items)]

	progress("embed_done", map[string]any{"kept": keptCount})

	// Drop items that failed to embed
	clean := items[:0]
	for _, it := range items {
		if len(it.Vec) > 0 {
			clean = append(clean, it)
		}
	}
	if len(clean) == 0 {
		progress("rank_done", map[string]any{"selected": 0})
		return nil, nil, nil
	}

	top := TopK(clean, qv, k)
	snippets := make([]string, len(top))
	sources := make([]string, len(top))
	for i, idx := range top {
		snippets[i] = clean[idx].Text
		sources[i] = clean[idx].Source
	}

	progress("rank_done", map[string]any{"selected": len(snippets)})
	return snippets, sources, nil
}
