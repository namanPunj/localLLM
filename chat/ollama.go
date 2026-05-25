package chat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type Ollama struct {
	BaseURL       string
	ChatModel     string // thinking-capable model (e.g. qwen3:4b)
	InstructModel string // non-thinking instruct model
	CoderModel    string // workspace/code-focused model (e.g. qwen2.5-coder:7b)
	EmbedModel    string
	Temperature   float64
	NumCtx        int
	HTTP          *http.Client
}

// ChatOptions overrides model/sampling parameters for a single request.
// Zero values fall back to the Ollama struct's defaults.
type ChatOptions struct {
	Model       string
	Temperature float64
	NumCtx      int
	Think       bool
	UseCoder    bool // use CoderModel (workspace mode)
}

func NewOllama(baseURL, chatModel, instructModel, coderModel, embedModel string) *Ollama {
	return &Ollama{
		BaseURL:       baseURL,
		ChatModel:     chatModel,
		InstructModel: instructModel,
		CoderModel:    coderModel,
		EmbedModel:    embedModel,
		Temperature: 0.2,
		NumCtx:      4096,
		HTTP: &http.Client{
			Timeout: 5 * time.Minute,
			Transport: &http.Transport{
				MaxIdleConns:    10,
				IdleConnTimeout: 90 * time.Second,
			},
		},
	}
}

// UnloadModel evicts the named model from Ollama's GPU/RAM immediately.
func (o *Ollama) UnloadModel(ctx context.Context, model string) error {
	body, _ := json.Marshal(map[string]any{"model": model, "keep_alive": "0"})
	req, err := http.NewRequestWithContext(ctx, "POST", o.BaseURL+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// PreloadModel loads the model into memory with the given num_ctx and blocks
// until Ollama confirms it is ready (empty-prompt generate call).
func (o *Ollama) PreloadModel(ctx context.Context, model string, numCtx int) error {
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"prompt":     "",
		"keep_alive": "30m",
		"options":    map[string]any{"num_ctx": numCtx},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", o.BaseURL+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("preload: status %d", resp.StatusCode)
	}
	// Drain response — Ollama returns {done:true} immediately for empty prompts
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
	}
	return sc.Err()
}

// Summarize compresses a slice of messages into a dense summary paragraph
// using the instruct model (fast, no thinking). Used for rolling history
// compression — instead of dropping old messages, we summarize them.
func (o *Ollama) Summarize(ctx context.Context, messages []Message) (string, error) {
	if len(messages) == 0 {
		return "", nil
	}

	// Build the conversation text to summarize
	var conv strings.Builder
	for _, m := range messages {
		conv.WriteString(strings.ToUpper(m.Role))
		conv.WriteString(": ")
		conv.WriteString(m.Content)
		conv.WriteString("\n")
	}

	model := o.InstructModel
	if model == "" {
		model = o.ChatModel
	}

	reqBody, _ := json.Marshal(ollamaChatRequest{
		Model: model,
		Messages: []ollamaMessage{
			{Role: "system", Content: "Summarize the following conversation in 2-3 concise sentences. Preserve key facts, decisions, names, and any pending action items. Do NOT add commentary — just the summary."},
			{Role: "user", Content: conv.String()},
		},
		Stream:    false,
		KeepAlive: "30m",
		Options: map[string]any{
			"temperature": 0.1,
			"num_ctx":     2048,
		},
	})

	req, err := http.NewRequestWithContext(ctx, "POST", o.BaseURL+"/api/chat", bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("summarize: status %d", resp.StatusCode)
	}

	var result struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Message.Content), nil
}

type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content,omitempty"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

type ollamaToolCall struct {
	Function struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"function"`
}

type ollamaChatChunk struct {
	Message struct {
		Content   string           `json:"content"`
		Thinking  string           `json:"thinking,omitempty"`
		ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
	} `json:"message"`
	Done bool `json:"done"`
}

type ollamaChatRequest struct {
	Model     string          `json:"model"`
	Messages  []ollamaMessage `json:"messages"`
	Stream    bool            `json:"stream"`
	KeepAlive string          `json:"keep_alive,omitempty"`
	Tools     []any           `json:"tools,omitempty"`
	Options   map[string]any  `json:"options,omitempty"`
	Think     bool            `json:"think,omitempty"`
}

// chatStreamRaw sends msgs directly to the Ollama /api/chat endpoint and
// streams back tokens and tool calls. It is the core implementation used by
// both ChatStream and the agentic pipeline loop.
//
// When Think is true, qwen3:4b is used with think:true (full reasoning mode).
// When Think is false, the instruct model is used with think:false (fast mode).
func (o *Ollama) chatStreamRaw(
	ctx context.Context,
	msgs []ollamaMessage,
	tools []any,
	opts *ChatOptions,
	onToken func(string) error,
	onAction func(Action) error,
	onThinkToken ...func(string) error,
) error {
	think := false
	if opts != nil {
		think = opts.Think
	}

	// Pick model: coder > instruct > chat, depending on mode
	model := o.ChatModel
	if !think && o.InstructModel != "" {
		model = o.InstructModel
	}
	// CoderModel overrides for workspace/agentic code tasks when explicitly requested
	if opts != nil && opts.UseCoder && o.CoderModel != "" {
		model = o.CoderModel
	}
	temperature := o.Temperature
	numCtx := o.NumCtx
	if opts != nil {
		if opts.Model != "" {
			model = opts.Model
		}
		if opts.Temperature > 0 {
			temperature = opts.Temperature
		}
		if opts.NumCtx > 0 {
			numCtx = opts.NumCtx
		}
	}

	var thinkFn func(string) error
	if len(onThinkToken) > 0 {
		thinkFn = onThinkToken[0]
	}

	return o.doStream(ctx, model, msgs, tools, think, temperature, numCtx, onToken, onAction, thinkFn)
}

// doStream performs a single Ollama /api/chat streaming call.
func (o *Ollama) buildChatBody(model string, msgs []ollamaMessage, tools []any, think bool, temperature float64, numCtx int) []byte {
	body, _ := json.Marshal(ollamaChatRequest{
		Model:     model,
		Messages:  msgs,
		Stream:    true,
		KeepAlive: "30m",
		Tools:     tools,
		Think:     think,
		Options: map[string]any{
			"temperature": temperature,
			"num_ctx":     numCtx,
		},
	})
	return body
}

func (o *Ollama) doStream(
	ctx context.Context,
	model string,
	msgs []ollamaMessage,
	tools []any,
	think bool,
	temperature float64,
	numCtx int,
	onToken func(string) error,
	onAction func(Action) error,
	onThinkToken func(string) error,
) error {
	body := o.buildChatBody(model, msgs, tools, think, temperature, numCtx)

	req, err := http.NewRequestWithContext(ctx, "POST", o.BaseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.HTTP.Do(req)
	if err != nil {
		return err
	}

	// If model rejected the request (e.g. doesn't support tools),
	// retry once without tools — text-based tool extraction handles the fallback.
	// Keep the original think flag so reasoning models still output <think> blocks.
	if resp.StatusCode == 400 && len(tools) > 0 {
		resp.Body.Close()
		body = o.buildChatBody(model, msgs, nil, think, temperature, numCtx)
		req2, err2 := http.NewRequestWithContext(ctx, "POST", o.BaseURL+"/api/chat", bytes.NewReader(body))
		if err2 != nil {
			return err2
		}
		req2.Header.Set("Content-Type", "application/json")
		resp, err = o.HTTP.Do(req2)
		if err != nil {
			return err
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("ollama: status %d", resp.StatusCode)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ch ollamaChatChunk
		if err := json.Unmarshal(line, &ch); err != nil {
			continue
		}
		if ch.Message.Thinking != "" && onThinkToken != nil {
			if err := onThinkToken(ch.Message.Thinking); err != nil {
				return err
			}
		}
		if ch.Message.Content != "" {
			if err := onToken(ch.Message.Content); err != nil {
				return err
			}
		}
		for _, tc := range ch.Message.ToolCalls {
			if err := onAction(Action{Name: tc.Function.Name, Args: tc.Function.Arguments}); err != nil {
				return err
			}
		}
		if ch.Done {
			break
		}
	}
	return sc.Err()
}

// ChatStream streams tokens and tool calls from Ollama.
// opts overrides model/temperature/num_ctx for this request; nil uses struct defaults.
func (o *Ollama) ChatStream(
	ctx context.Context,
	messages []Message,
	tools []any,
	opts *ChatOptions,
	onToken func(string) error,
	onAction func(Action) error,
	onThinkToken ...func(string) error,
) error {
	msgs := make([]ollamaMessage, len(messages))
	for i, m := range messages {
		msgs[i] = ollamaMessage{Role: m.Role, Content: m.Content}
	}
	return o.chatStreamRaw(ctx, msgs, tools, opts, onToken, onAction, onThinkToken...)
}

type embedRequest struct {
	Model string `json:"model"`
	Input any    `json:"input"` // string or []string — Ollama accepts both
}
type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// EmbedBatch returns embedding vectors for all texts in a single API call.
func (o *Ollama) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, _ := json.Marshal(embedRequest{Model: o.EmbedModel, Input: texts})
	req, err := http.NewRequestWithContext(ctx, "POST", o.BaseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("embed: status %d", resp.StatusCode)
	}
	var er embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, err
	}
	if len(er.Embeddings) != len(texts) {
		return nil, fmt.Errorf("embed: expected %d vectors, got %d", len(texts), len(er.Embeddings))
	}
	return er.Embeddings, nil
}

// Embed returns one embedding vector for the given text.
func (o *Ollama) Embed(ctx context.Context, text string) ([]float32, error) {
	vecs, err := o.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}
