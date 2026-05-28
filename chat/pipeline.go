package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"assistant/rag"
)

// Interfaces the pipeline depends on (defined here to avoid import cycles).

type SessionStore interface {
	GetOrCreate(id string) (string, []Message, error)
	AppendMessage(sessionID string, msg Message) error
	// ReplaceHistory atomically replaces all messages for a session.
	// Used by the rolling-summary compressor to persist condensed history.
	ReplaceHistory(sessionID string, msgs []Message) error
	GetContextSummary(sessionID string) (summary string, cursor int, err error)
	SetContextSummary(sessionID string, summary string, cursor int) error
}

type FileStore interface {
	Get(fileID string) (text string, ok bool)
}

// ProgressFunc is called by the RAG layer at each stage so the client can render progress.
type ProgressFunc = func(stage string, data map[string]any)

type RAG interface {
	Search(ctx context.Context, query string, k int, progress ProgressFunc) (snippets []string, sources []string, err error)
}

type ToolExecutor interface {
	Execute(ctx context.Context, name string, args map[string]any) (string, error)
}

// ProgressAware is optionally implemented by executors that can stream
// detailed progress events (e.g. web search stages).
type ProgressAware interface {
	SetProgress(fn func(stage string, data map[string]any))
}

// MemoryStore provides personal memory items to inject into system context.
type MemoryStore interface {
	GetItems() []string
}

// ExchangeStore provides RAG-based session memory — stores and retrieves
// per-turn summaries with embeddings for relevant context retrieval.
type ExchangeStore interface {
	SaveExchange(sessionID, userQuery, summary, fullResponse string, embedding []float32) error
	GetExchanges(sessionID string) ([]Exchange, error)
}

// Exchange represents one past user→assistant turn with a compact summary.
type Exchange struct {
	UserQuery    string
	Summary      string
	FullResponse string
	Embedding    []float32
}

// Pipeline runs one full chat turn: context-build -> model -> stream.
type Pipeline struct {
	Ollama     *Ollama
	Sessions   SessionStore // persistent store
	IncogStore SessionStore // in-memory store for incognito chats
	Exchanges  ExchangeStore
	Files      FileStore
	RAG        RAG
	Executor   ToolExecutor
	MaxCtxTok  int
	Memory     MemoryStore
}

// fileRAG embeds file chunks and returns only the top-k most relevant ones for
// the given query. Falls back to plain concatenation if embedding fails.
func (p *Pipeline) fileRAG(ctx context.Context, fileTexts []string, query string, k int, emit func(StreamEvent) error) string {
	// Chunk all files together
	var items []rag.VecItem
	for _, text := range fileTexts {
		for _, chunk := range rag.Chunk(text, 2000, 200) {
			items = append(items, rag.VecItem{Text: chunk})
		}
	}
	if len(items) == 0 {
		return ""
	}

	_ = emit(StreamEvent{Type: "meta", Meta: map[string]any{
		"stage":  "file_chunked",
		"chunks": len(items),
	}})

	// If all chunks already fit within k, skip embedding and use everything
	if len(items) <= k {
		_ = emit(StreamEvent{Type: "meta", Meta: map[string]any{
			"stage":    "file_rag_done",
			"selected": len(items),
			"total":    len(items),
			"skipped":  true, // all fit, no ranking needed
		}})
		var sb strings.Builder
		for i, it := range items {
			if i > 0 {
				sb.WriteString("\n\n--- NEXT CHUNK ---\n\n")
			}
			sb.WriteString(it.Text)
		}
		return sb.String()
	}

	_ = emit(StreamEvent{Type: "meta", Meta: map[string]any{
		"stage":  "file_rag_embed",
		"chunks": len(items),
		"k":      k,
	}})

	// Batch-embed all chunks + query in a single API call
	texts := make([]string, len(items)+1)
	for i, it := range items {
		texts[i] = it.Text
	}
	texts[len(items)] = query

	vecs, err := p.Ollama.EmbedBatch(ctx, texts)
	if err != nil {
		// Embedding failed — fallback: return first k chunks by order
		var sb strings.Builder
		for i := 0; i < k && i < len(items); i++ {
			if i > 0 {
				sb.WriteString("\n\n--- NEXT CHUNK ---\n\n")
			}
			sb.WriteString(items[i].Text)
		}
		return sb.String()
	}

	for i := range items {
		items[i].Vec = vecs[i]
	}
	qv := vecs[len(items)]

	// Drop items that failed to embed
	clean := items[:0]
	for _, it := range items {
		if len(it.Vec) > 0 {
			clean = append(clean, it)
		}
	}
	if len(clean) == 0 {
		return ""
	}

	top := rag.TopK(clean, qv, k)

	_ = emit(StreamEvent{Type: "meta", Meta: map[string]any{
		"stage":    "file_rag_done",
		"selected": len(top),
		"total":    len(clean),
	}})

	var sb strings.Builder
	for i, idx := range top {
		if i > 0 {
			sb.WriteString("\n\n--- NEXT CHUNK ---\n\n")
		}
		sb.WriteString(clean[idx].Text)
	}
	return sb.String()
}

// retrieveRelevantExchanges embeds the current query and retrieves the top-k
// most relevant past exchanges from this session's exchange store.
// Returns formatted context string and the number of exchanges matched.
func (p *Pipeline) retrieveRelevantExchanges(ctx context.Context, sessionID, query string, k int) (string, int) {
	if p.Exchanges == nil {
		return "", 0
	}
	exchanges, err := p.Exchanges.GetExchanges(sessionID)
	if err != nil || len(exchanges) == 0 {
		return "", 0
	}

	// Embed the current query
	queryVec, err := p.Ollama.Embed(ctx, query)
	if err != nil || len(queryVec) == 0 {
		return "", 0
	}

	// Build vector items from exchanges that have embeddings
	var items []rag.VecItem
	var exMap []int // maps item index -> exchange index
	for i, ex := range exchanges {
		if len(ex.Embedding) > 0 {
			items = append(items, rag.VecItem{Vec: ex.Embedding})
			exMap = append(exMap, i)
		}
	}
	if len(items) == 0 {
		return "", 0
	}

	if k > len(items) {
		k = len(items)
	}
	topIdx := rag.TopK(items, queryVec, k)

	var sb strings.Builder
	for i, idx := range topIdx {
		ex := exchanges[exMap[idx]]
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(fmt.Sprintf("- User asked: %s → %s", ex.UserQuery, ex.Summary))
	}
	return sb.String(), len(topIdx)
}

// summarizeResponse generates a 1-2 line summary of the assistant's response.
func (p *Pipeline) summarizeResponse(ctx context.Context, userQuery, response string) string {
	if len(response) < 100 {
		return response // short enough to use as-is
	}
	// Truncate very long responses for summarization input
	input := response
	if len(input) > 2000 {
		input = input[:2000]
	}
	model := p.Ollama.InstructModel
	if model == "" {
		model = p.Ollama.ChatModel
	}
	reqBody, _ := json.Marshal(ollamaChatRequest{
		Model: model,
		Messages: []ollamaMessage{
			{Role: "system", Content: "Summarize the assistant's response in 1-2 short sentences. Keep key facts, names, numbers. No commentary."},
			{Role: "user", Content: fmt.Sprintf("User asked: %s\n\nAssistant responded: %s", userQuery, input)},
		},
		Stream:    false,
		KeepAlive: "30m",
		Options:   map[string]any{"temperature": 0.1, "num_ctx": 1024},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", p.Ollama.BaseURL+"/api/chat", bytes.NewReader(reqBody))
	if err != nil {
		return truncateStr(response, 200)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.Ollama.HTTP.Do(req)
	if err != nil {
		return truncateStr(response, 200)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return truncateStr(response, 200)
	}
	var result struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return truncateStr(response, 200)
	}
	s := strings.TrimSpace(result.Message.Content)
	if s == "" {
		return truncateStr(response, 200)
	}
	return s
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// addLineNumbers prefixes each line of content with a 1-based line number.
func addLineNumbers(content string) string {
	return readFileRange(content, 0, 0)
}

// extractKeywords pulls meaningful lowercase words (≥4 chars) from a message,
// filtering common stop-words. Used for keyword-based file section matching.
func extractKeywords(msg string) []string {
	stop := map[string]bool{
		"this": true, "that": true, "with": true, "from": true, "have": true,
		"will": true, "help": true, "make": true, "want": true, "need": true,
		"file": true, "code": true, "just": true, "also": true, "like": true,
		"more": true, "some": true, "when": true, "then": true, "them": true,
		"they": true, "your": true, "into": true, "been": true, "does": true,
	}
	words := strings.Fields(strings.ToLower(msg))
	seen := map[string]bool{}
	var kws []string
	for _, w := range words {
		w = strings.Trim(w, `.,!?;:"'()[]{}/\`)
		if len(w) >= 4 && !stop[w] && !seen[w] {
			kws = append(kws, w)
			seen[w] = true
		}
	}
	return kws
}

// autoReadContext greps every project file for keywords from the user message
// and returns the matching lines with ±30 lines of context, up to maxChars.
// This pre-loads exactly the relevant sections without requiring tool calls.
func (p *Pipeline) autoReadContext(userMsg string, agentFiles []FileInfo, maxChars int) string {
	kws := extractKeywords(userMsg)
	if len(kws) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\nAUTO-LOADED CONTEXT (sections matched from your files):\n")
	total := 0
	anyFound := false

	for _, f := range agentFiles {
		if total >= maxChars {
			break
		}
		content, ok := p.Files.Get(f.ID)
		if !ok {
			continue
		}
		lines := strings.Split(content, "\n")
		// Mark lines that match any keyword, plus ±30 neighbours.
		include := make([]bool, len(lines))
		for i, line := range lines {
			ll := strings.ToLower(line)
			for _, kw := range kws {
				if strings.Contains(ll, kw) {
					lo := i - 30
					if lo < 0 {
						lo = 0
					}
					hi := i + 31
					if hi > len(lines) {
						hi = len(lines)
					}
					for j := lo; j < hi; j++ {
						include[j] = true
					}
					break
				}
			}
		}
		// Check if anything matched.
		found := false
		for _, v := range include {
			if v {
				found = true
				break
			}
		}
		if !found {
			continue
		}

		width := len(fmt.Sprintf("%d", len(lines)))
		hdr := fmt.Sprintf("\n=== %s  (file_id: %s) ===\n", f.Name, f.ID)
		sb.WriteString(hdr)
		total += len(hdr)

		inGap := false
		for i, line := range lines {
			if total >= maxChars {
				sb.WriteString("      ... [truncated]\n")
				break
			}
			if include[i] {
				inGap = false
				entry := fmt.Sprintf("%*d | %s\n", width, i+1, line)
				sb.WriteString(entry)
				total += len(entry)
			} else if !inGap {
				sb.WriteString("      ...\n")
				inGap = true
			}
		}
		anyFound = true
	}

	if !anyFound {
		return ""
	}
	return sb.String()
}

// readFileRange returns content with line numbers, optionally restricted to
// [startLine, endLine] (1-based, inclusive). Zero values mean "no limit".
func readFileRange(content string, startLine, endLine int) string {
	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	width := len(fmt.Sprintf("%d", len(lines)))

	start := 0
	end := len(lines)
	if startLine > 0 {
		start = startLine - 1
	}
	if endLine > 0 && endLine < end {
		end = endLine
	}
	if start >= len(lines) {
		return "(line range out of bounds)\n"
	}

	var sb strings.Builder
	for i := start; i < end; i++ {
		fmt.Fprintf(&sb, "%*d | %s\n", width, i+1, lines[i])
	}
	return sb.String()
}

// agentFileLoop runs an agentic multi-turn loop where the model can call
// read_file / edit_file tools. All project files are pre-injected into the
// system context with line numbers so the model has full context from the first
// turn without needing to call read_file first.
// Returns the full assistant response text and any error.
func (p *Pipeline) agentFileLoop(
	ctx context.Context,
	history []Message,
	userMessage string,
	agentFiles []FileInfo,
	snippets []string,
	sources []string,
	opts *ChatOptions,
	maxCtx int,
	emit func(StreamEvent) error,
) (string, error) {
	// Build file manifest — filenames, IDs, line counts.
	var manifest strings.Builder
	manifest.WriteString("WORKSPACE FILES:\n")
	for _, f := range agentFiles {
		kind := "FILE"
		if f.IsLink {
			kind = "WEB PAGE"
		}
		lineCount := 0
		if content, ok := p.Files.Get(f.ID); ok {
			lineCount = strings.Count(content, "\n") + 1
		}
		manifest.WriteString(fmt.Sprintf("- [%s] %s  (file_id: %s, %d lines)\n", kind, f.Name, f.ID, lineCount))
	}

	// Auto-grep files for keywords from the user query and inject relevant sections.
	// This gives the model real context without requiring it to call read_file first.
	autoCtx := p.autoReadContext(userMessage, agentFiles, maxCtx*2)
	if autoCtx != "" {
		manifest.WriteString(autoCtx)
	}

	manifest.WriteString("\nRULES:\n")
	manifest.WriteString("- Use ONLY the content shown above — never invent or guess file content.\n")
	manifest.WriteString("- To propose an edit, output a SEARCH/REPLACE block (copy old content EXACTLY as shown, including indentation):\n")
	manifest.WriteString("  <<<<<<< SEARCH\n  <exact lines to replace>\n  =======\n  <new lines>\n  >>>>>>> REPLACE\n")
	manifest.WriteString("- You can output multiple SEARCH/REPLACE blocks for multiple changes.\n")
	manifest.WriteString("- To read more lines call read_file(file_id, start_line, end_line).\n")
	manifest.WriteString("- Make the smallest change possible. After proposing, state which file was changed and why.\n")

	// Inject manifest into file content slot so it lands in the system message.
	var memItems []string
	if p.Memory != nil {
		memItems = p.Memory.GetItems()
	}
	msgs, usage := BuildMessagesWithLimit(history, userMessage, manifest.String(), snippets, sources, maxCtx, memItems, workspaceSystemPrompt)

	// Emit context usage. num_ctx is the actual Ollama window (prompt budget + 800 response headroom).
	_ = emit(StreamEvent{Type: "meta", Meta: map[string]any{
		"context_usage": map[string]any{
			"system":     usage.System,
			"history":    usage.History,
			"files":      usage.Files,
			"rag":        usage.RAG,
			"user_turn":  usage.UserTurn,
			"available":  usage.Available,
			"limit":      usage.Limit,
			"num_ctx":    usage.Limit + 800,
			"compressed": usage.Compressed,
		},
	}})

	// Convert to internal ollamaMessage slice for the multi-turn loop.
	oMsgs := make([]ollamaMessage, len(msgs))
	for i, m := range msgs {
		oMsgs[i] = ollamaMessage{Role: m.Role, Content: m.Content}
	}

	var fullResp strings.Builder
	const maxRounds = 8

	for round := 0; round < maxRounds; round++ {
		var toolCalls []ollamaToolCall

		err := p.Ollama.chatStreamRaw(ctx, oMsgs, WorkspaceTools, opts,
			func(tok string) error {
				fullResp.WriteString(tok)
				return emit(StreamEvent{Type: "token", Content: tok})
			},
			func(a Action) error {
				toolCalls = append(toolCalls, ollamaToolCall{
					Function: struct {
						Name      string         `json:"name"`
						Arguments map[string]any `json:"arguments"`
					}{Name: a.Name, Arguments: a.Args},
				})
				return nil
			},
			func(tok string) error {
				return emit(StreamEvent{Type: "think_token", Content: tok})
			},
		)
		if err != nil {
			return fullResp.String(), err
		}

		// Recover tool calls dumped as text instead of structured tool_calls.
		if len(toolCalls) == 0 {
			if recovered, _ := extractToolCallsFromText(fullResp.String()); len(recovered) > 0 {
				toolCalls = recovered
			}
		}

		// No tool calls → parse SEARCH/REPLACE blocks from text, then done.
		if len(toolCalls) == 0 {
			for _, blk := range parseSearchReplace(fullResp.String()) {
				matched := false
				for _, f := range agentFiles {
					content, ok := p.Files.Get(f.ID)
					if !ok {
						continue
					}
					if strings.Contains(content, blk.Old) {
						_ = emit(StreamEvent{Type: "action", Name: "propose_edit", Args: map[string]any{
							"file_id":     f.ID,
							"filename":    fileNameByID(agentFiles, f.ID),
							"description": blk.Description,
							"old_content": blk.Old,
							"new_content": blk.New,
						}})
						matched = true
						break
					}
				}
				if !matched {
					_ = emit(StreamEvent{Type: "token", Content: "\n\n> Edit block could not be matched verbatim — model may have guessed content. Use read_file to get exact lines.\n"})
				}
			}
			break
		}

		// Clear any streamed junk (the raw JSON the model printed).
		if fullResp.Len() > 0 {
			_ = emit(StreamEvent{Type: "clear_tokens"})
			fullResp.Reset()
		}

		// Check for ask_followup — emit and break.
		hasFollowup := false
		for _, tc := range toolCalls {
			if tc.Function.Name == "ask_followup" {
				_ = emit(StreamEvent{Type: "action", Name: "ask_followup", Args: tc.Function.Arguments})
				hasFollowup = true
			}
		}
		if hasFollowup {
			break
		}

		// Append the assistant turn that issued these tool calls.
		oMsgs = append(oMsgs, ollamaMessage{Role: "assistant", ToolCalls: toolCalls})

		// Execute each tool call and append the results.
		for _, tc := range toolCalls {
			name := tc.Function.Name
			args := tc.Function.Arguments

			var result string

			if name == "read_file" {
				fileID, _ := args["file_id"].(string)
				// start_line / end_line arrive as float64 from JSON
				startF, _ := args["start_line"].(float64)
				endF, _ := args["end_line"].(float64)
				startLine := int(startF)
				endLine := int(endF)

				fname := fileNameByID(agentFiles, fileID)
				_ = emit(StreamEvent{Type: "meta", Meta: map[string]any{
					"stage":      "agent_read_file",
					"file_id":    fileID,
					"name":       fname,
					"start_line": startLine,
					"end_line":   endLine,
				}})

				content, ok := p.Files.Get(fileID)
				if !ok {
					result = fmt.Sprintf("error: file %q not found", fileID)
				} else {
					result = readFileRange(content, startLine, endLine)
					label := fname
					if startLine > 0 || endLine > 0 {
						label = fmt.Sprintf("%s (lines %d–%d)", fname, startLine, endLine)
					}
					_ = emit(StreamEvent{Type: "meta", Meta: map[string]any{
						"stage":   "agent_read_done",
						"file_id": fileID,
						"name":    label,
						"chars":   len(result),
					}})
				}
			} else if name == "edit_file" {
				fileID, _ := args["file_id"].(string)
				description, _ := args["description"].(string)
				oldContent, _ := args["old_content"].(string)
				newContent, _ := args["new_content"].(string)
				// Verify old_content actually exists in the file
				current, ok := p.Files.Get(fileID)
				if !ok {
					result = fmt.Sprintf("error: file %q not found", fileID)
				} else if !strings.Contains(current, oldContent) {
					result = "error: old_content not found verbatim in file — call read_file again to get current content"
				} else {
					// Emit a propose_edit action — frontend shows Accept/Reject UI
					_ = emit(StreamEvent{Type: "action", Name: "propose_edit", Args: map[string]any{
						"file_id":     fileID,
						"filename":    fileNameByID(agentFiles, fileID),
						"description": description,
						"old_content": oldContent,
						"new_content": newContent,
					}})
					result = "Edit proposed to user. Waiting for Accept or Reject."
				}
			} else {
				// Delegate calendar/task tools to the executor.
				if p.Executor != nil {
					r, execErr := p.Executor.Execute(ctx, name, args)
					if execErr != nil {
						result = fmt.Sprintf("error: %v", execErr)
					} else {
						result = r
					}
				}
				_ = emit(StreamEvent{Type: "action", Name: name, Args: args})
			}

			oMsgs = append(oMsgs, ollamaMessage{Role: "tool", Content: result})
		}
	}

	return fullResp.String(), nil
}

// srBlock holds one parsed SEARCH/REPLACE block.
type srBlock struct {
	Old         string
	New         string
	Description string
}

// parseSearchReplace extracts <<<<<<< SEARCH / ======= / >>>>>>> REPLACE blocks
// from model text. These are treated the same as edit_file tool calls.
func parseSearchReplace(text string) []srBlock {
	var blocks []srBlock
	parts := strings.Split(text, "<<<<<<< SEARCH")
	for _, part := range parts[1:] {
		midIdx := strings.Index(part, "=======")
		if midIdx < 0 {
			continue
		}
		rest := part[midIdx+7:]
		endIdx := strings.Index(rest, ">>>>>>> REPLACE")
		if endIdx < 0 {
			endIdx = strings.Index(rest, ">>>>>>>")
		}
		if endIdx < 0 {
			continue
		}
		oldContent := strings.TrimLeft(strings.TrimRight(part[:midIdx], "\r\n "), "\n")
		newContent := strings.TrimLeft(strings.TrimRight(rest[:endIdx], "\r\n "), "\n")
		if oldContent == "" {
			continue
		}
		blocks = append(blocks, srBlock{Old: oldContent, New: newContent, Description: "Edit proposed by model"})
	}
	return blocks
}

// fileNameByID looks up a filename from the agent file manifest by file_id.
func fileNameByID(files []FileInfo, id string) string {
	for _, f := range files {
		if f.ID == id {
			return f.Name
		}
	}
	return id
}

// buildEffectiveHistory prepends a summary system message to the tail of history
// starting at cursor. If summary is empty, returns the tail unchanged.
func buildEffectiveHistory(fullHistory []Message, summary string, cursor int) []Message {
	tail := fullHistory
	if cursor > 0 && cursor < len(fullHistory) {
		tail = fullHistory[cursor:]
	}
	if summary == "" {
		return tail
	}
	result := make([]Message, 0, 1+len(tail))
	result = append(result, Message{
		Role:    "system",
		Content: fmt.Sprintf("[Summary of %d earlier messages]\n%s", cursor, summary),
	})
	result = append(result, tail...)
	return result
}

// compactContext summarizes history[prevCursor : len(history)-4], combining
// with any prior summary, and returns the new summary and cursor.
func (p *Pipeline) compactContext(ctx context.Context, fullHistory []Message, prevSummary string, prevCursor int) (string, int, error) {
	keepRaw := 4
	newCursor := len(fullHistory) - keepRaw
	if newCursor <= prevCursor || newCursor <= 0 {
		return prevSummary, prevCursor, nil
	}
	var toSummarize []Message
	if prevSummary != "" {
		toSummarize = append(toSummarize, Message{
			Role:    "system",
			Content: "[Previous context summary]\n" + prevSummary,
		})
	}
	toSummarize = append(toSummarize, fullHistory[prevCursor:newCursor]...)
	newSummary, err := p.Ollama.Summarize(ctx, toSummarize)
	if err != nil || newSummary == "" {
		return prevSummary, prevCursor, err
	}
	return newSummary, newCursor, nil
}

// Run executes one turn. Emit is called for each StreamEvent.
func (p *Pipeline) Run(ctx context.Context, req ChatRequest, emit func(StreamEvent) error) error {
	// Pick the right session backing for this turn.
	store := p.Sessions
	if req.Incognito && p.IncogStore != nil {
		store = p.IncogStore
	}

	if req.SessionID == "" {
		req.SessionID = uuid.NewString()
	}
	sessionID, history, err := store.GetOrCreate(req.SessionID)
	if err != nil {
		return emit(StreamEvent{Type: "error", Error: err.Error()})
	}

	// Send session id (and incognito flag) so the client can persist locally.
	if err := emit(StreamEvent{Type: "meta", Meta: map[string]any{
		"session_id": sessionID,
		"incognito":  req.Incognito,
	}}); err != nil {
		return err
	}

	// Wrap emit to capture blob-relevant meta events for persistent restoration.
	// Status-only stages (model_loading, context_compacting, etc.) are skipped
	// since they have no visual representation after the turn ends.
	var capturedMeta []map[string]any
	skipBlobStage := map[string]bool{
		"model_loading": true, "model_loaded": true,
		"history_compressing": true, "history_compress_done": true,
		"context_compacting": true, "context_compacted": true,
		"context_usage": true,
	}
	origEmit := emit
	emit = func(ev StreamEvent) error {
		if ev.Type == "meta" && ev.Meta != nil {
			if stage, ok := ev.Meta["stage"].(string); ok && !skipBlobStage[stage] {
				capturedMeta = append(capturedMeta, ev.Meta)
			}
		}
		return origEmit(ev)
	}

	// Web search is now handled as a tool call (web_search) — the model
	// decides when to search within the agentic loop.
	var snippets, sources []string

	// User message is saved together with the assistant response at the end,
	// so cancelled/superseded requests don't leave orphan messages.
	userMsg := Message{Role: "user", Content: req.Message}

	maxCtx := p.MaxCtxTok
	if req.MaxCtxTok > 0 {
		maxCtx = req.MaxCtxTok
	}
	opts := req.ModelOverride

	// ── RAG-based session memory ──────────────────────────────────────────
	// Instead of keeping full history in context (which grows), we:
	// 1. Keep only the last 2 messages (most recent exchange) as raw context
	// 2. Retrieve top-3 relevant past exchanges via embedding similarity
	// 3. Inject their summaries as lightweight context
	var ragExchangeCtx string
	var ragExchangeCount int
	isWorkspace := len(req.AgentFiles) > 0

	if !isWorkspace && p.Exchanges != nil && len(history) > 2 {
		_ = emit(StreamEvent{Type: "meta", Meta: map[string]any{
			"stage": "exchange_retrieval",
		}})
		ragExchangeCtx, ragExchangeCount = p.retrieveRelevantExchanges(ctx, sessionID, req.Message, 3)
		_ = emit(StreamEvent{Type: "meta", Meta: map[string]any{
			"stage":    "exchange_retrieved",
			"matches":  ragExchangeCount,
		}})
	}

	// For normal chat: keep only the last 2 messages + RAG summaries
	// For workspace: keep full effective history (needs more context)
	var effectiveHistory []Message
	if !isWorkspace && ragExchangeCount > 0 {
		// Last exchange (1 user + 1 assistant) as raw context
		keep := 2
		if keep > len(history) {
			keep = len(history)
		}
		effectiveHistory = history[len(history)-keep:]
	} else {
		// Fallback: use old linear approach with compaction
		summary, cursor, _ := store.GetContextSummary(sessionID)
		effectiveHistory = buildEffectiveHistory(history, summary, cursor)

		historyBudget := maxCtx - 700
		if historyBudget < 200 {
			historyBudget = 200
		}
		histTok := estimateTokensMsg(effectiveHistory)
		if histTok > int(float64(historyBudget)*0.75) && len(history) > 4 {
			_ = emit(StreamEvent{Type: "meta", Meta: map[string]any{
				"stage":    "context_compacting",
				"messages": len(history),
				"cursor":   cursor,
			}})
			newSummary, newCursor, compactErr := p.compactContext(ctx, history, summary, cursor)
			if compactErr == nil && newCursor > cursor {
				_ = store.SetContextSummary(sessionID, newSummary, newCursor)
				summary = newSummary
				cursor = newCursor
				effectiveHistory = buildEffectiveHistory(history, summary, cursor)
			}
			_ = emit(StreamEvent{Type: "meta", Meta: map[string]any{
				"stage": "context_compacted",
				"ok":    compactErr == nil && newCursor > cursor,
			}})
		}
	}

	// ── Agentic path: project files — model reads on demand ──────────────────
	if len(req.AgentFiles) > 0 {
		fileCount, linkCount := 0, 0
		for _, f := range req.AgentFiles {
			if f.IsLink {
				linkCount++
			} else {
				fileCount++
			}
		}
		_ = emit(StreamEvent{Type: "meta", Meta: map[string]any{
			"stage": "agent_files_start",
			"files": fileCount,
			"links": linkCount,
		}})

		fullResp, loopErr := p.agentFileLoop(
			ctx, effectiveHistory, req.Message, req.AgentFiles,
			snippets, sources, opts, maxCtx, emit,
		)
		if loopErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return emit(StreamEvent{Type: "error", Error: loopErr.Error()})
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := store.AppendMessage(sessionID, userMsg); err != nil {
			return emit(StreamEvent{Type: "error", Error: fmt.Sprintf("failed to persist user message: %v", err)})
		}
		metaJSON, _ := json.Marshal(capturedMeta)
		if err := store.AppendMessage(sessionID, Message{Role: "assistant", Content: fullResp, MetaEvents: string(metaJSON)}); err != nil {
			return emit(StreamEvent{Type: "error", Error: fmt.Sprintf("failed to persist assistant message: %v", err)})
		}
		return emit(StreamEvent{Type: "done", Meta: map[string]any{"session_id": sessionID}})
	}

	// ── Standard path: pre-load files (RAG or plain), then single ChatStream ─
	allFileIDs := req.FileIDs
	if req.FileID != "" {
		allFileIDs = append(allFileIDs, req.FileID)
	}
	seen := map[string]bool{}
	var fileTexts []string
	for _, fid := range allFileIDs {
		if fid == "" || seen[fid] {
			continue
		}
		seen[fid] = true
		if t, ok := p.Files.Get(fid); ok {
			fileTexts = append(fileTexts, t)
		}
	}

	var fileContent string
	if len(fileTexts) > 0 {
		_ = emit(StreamEvent{Type: "meta", Meta: map[string]any{
			"stage": "file_read",
			"files": len(fileTexts),
		}})

		if req.FileRAGChunks > 0 {
			fileContent = p.fileRAG(ctx, fileTexts, req.Message, req.FileRAGChunks, emit)
		} else {
			combined := strings.Join(fileTexts, "\n\n--- NEXT FILE ---\n\n")
			maxFileChars := p.MaxCtxTok * 3
			if req.MaxCtxTok > 0 {
				maxFileChars = req.MaxCtxTok * 3
			}
			if len(combined) > maxFileChars {
				combined = combined[:maxFileChars] + "\n\n[... file content truncated to fit context ...]"
				_ = emit(StreamEvent{Type: "meta", Meta: map[string]any{
					"stage":      "file_truncated",
					"total":      len(strings.Join(fileTexts, "")),
					"kept":       maxFileChars,
					"file_count": len(fileTexts),
				}})
			}
			fileContent = combined
		}

		_ = emit(StreamEvent{Type: "meta", Meta: map[string]any{
			"stage":      "file_ready",
			"chars":      len(fileContent),
			"file_count": len(fileTexts),
		}})
	}

	var memItems []string
	if p.Memory != nil {
		memItems = p.Memory.GetItems()
	}
	// Inject RAG-retrieved exchange summaries as conversation memory
	if ragExchangeCtx != "" {
		memItems = append(memItems, "[Relevant past exchanges from this conversation]\n"+ragExchangeCtx)
	}
	msgs, usage := BuildMessagesWithLimit(effectiveHistory, req.Message, fileContent, snippets, sources, maxCtx, memItems)

	_ = emit(StreamEvent{Type: "meta", Meta: map[string]any{
		"context_usage": map[string]any{
			"system":     usage.System,
			"history":    usage.History,
			"files":      usage.Files,
			"rag":        usage.RAG,
			"user_turn":  usage.UserTurn,
			"available":  usage.Available,
			"limit":      usage.Limit,
			"num_ctx":    usage.Limit + 800,
			"compressed": usage.Compressed,
		},
	}})

	// Convert to ollamaMessage for the agentic multi-turn loop.
	oMsgs := make([]ollamaMessage, len(msgs))
	for i, m := range msgs {
		oMsgs[i] = ollamaMessage{Role: m.Role, Content: m.Content}
	}

	var fullResp strings.Builder
	var actions []Action
	const maxRounds = 8

	for round := 0; round < maxRounds; round++ {
		var toolCalls []ollamaToolCall

		loopErr := p.Ollama.chatStreamRaw(ctx, oMsgs, DefaultTools, opts,
			func(tok string) error {
				fullResp.WriteString(tok)
				return emit(StreamEvent{Type: "token", Content: tok})
			},
			func(a Action) error {
				toolCalls = append(toolCalls, ollamaToolCall{
					Function: struct {
						Name      string         `json:"name"`
						Arguments map[string]any `json:"arguments"`
					}{Name: a.Name, Arguments: a.Args},
				})
				return nil
			},
			func(tok string) error {
				return emit(StreamEvent{Type: "think_token", Content: tok})
			},
		)
		if loopErr != nil {
			// If cancelled (superseded by a newer request), don't save anything.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return emit(StreamEvent{Type: "error", Error: loopErr.Error()})
		}

		// If context was cancelled between rounds, bail out without saving.
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Recover tool calls dumped as text instead of structured tool_calls.
		if len(toolCalls) == 0 {
			if recovered, _ := extractToolCallsFromText(fullResp.String()); len(recovered) > 0 {
				toolCalls = recovered
			}
		}

		// No tool calls → model gave its final text answer.
		if len(toolCalls) == 0 {
			break
		}

		// Clear any streamed junk (the raw JSON the model printed).
		if fullResp.Len() > 0 {
			_ = emit(StreamEvent{Type: "clear_tokens"})
			fullResp.Reset()
		}

		// Check for ask_followup — emit it and break the loop (need user input).
		hasFollowup := false
		for _, tc := range toolCalls {
			if tc.Function.Name == "ask_followup" {
				_ = emit(StreamEvent{Type: "action", Name: "ask_followup", Args: tc.Function.Arguments})
				hasFollowup = true
			}
		}
		if hasFollowup {
			break
		}

		// Append the assistant turn that issued these tool calls.
		oMsgs = append(oMsgs, ollamaMessage{Role: "assistant", ToolCalls: toolCalls})

		// Execute each tool call and feed results back to the model.
		for _, tc := range toolCalls {
			name := tc.Function.Name
			args := tc.Function.Arguments
			actions = append(actions, Action{Name: name, Args: args})

			_ = emit(StreamEvent{Type: "meta", Meta: map[string]any{
				"stage": "tool_call",
				"tool":  name,
				"args":  args,
			}})

			var result string
			if p.Executor != nil {
				// Wire progress events for tools that support it (e.g. web_search).
				if pa, ok := p.Executor.(ProgressAware); ok {
					pa.SetProgress(func(stage string, data map[string]any) {
						meta := map[string]any{"stage": stage}
						for k, v := range data {
							meta[k] = v
						}
						_ = emit(StreamEvent{Type: "meta", Meta: meta})
					})
				}
				r, execErr := p.Executor.Execute(ctx, name, args)
				if pa, ok := p.Executor.(ProgressAware); ok {
					pa.SetProgress(nil)
				}
				if execErr != nil {
					result = fmt.Sprintf("error: %v", execErr)
				} else {
					result = r
				}
			}

			_ = emit(StreamEvent{Type: "meta", Meta: map[string]any{
				"stage":  "tool_result",
				"tool":   name,
				"result": result,
			}})
			_ = emit(StreamEvent{Type: "action", Name: name, Args: args})

			oMsgs = append(oMsgs, ollamaMessage{Role: "tool", Content: result})
		}
	}

	// Don't save if this request was superseded by a newer one.
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Save user + assistant messages together so cancelled requests leave no orphans.
	if err := store.AppendMessage(sessionID, userMsg); err != nil {
		return emit(StreamEvent{Type: "error", Error: fmt.Sprintf("failed to persist user message: %v", err)})
	}
	metaJSON, _ := json.Marshal(capturedMeta)
	respText := fullResp.String()
	if err := store.AppendMessage(sessionID, Message{Role: "assistant", Content: respText, MetaEvents: string(metaJSON)}); err != nil {
		return emit(StreamEvent{Type: "error", Error: fmt.Sprintf("failed to persist assistant message: %v", err)})
	}

	// Save exchange for RAG-based session memory (async, don't block response)
	if !isWorkspace && p.Exchanges != nil && respText != "" {
		go func() {
			bgCtx := context.Background()
			summary := p.summarizeResponse(bgCtx, req.Message, respText)
			embedding, _ := p.Ollama.Embed(bgCtx, req.Message)
			_ = p.Exchanges.SaveExchange(sessionID, req.Message, summary, respText, embedding)
		}()
	}

	return emit(StreamEvent{Type: "done", Meta: map[string]any{
		"session_id": sessionID,
		"actions":    len(actions),
	}})
}
