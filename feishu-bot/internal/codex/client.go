package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client interface {
	Answer(ctx context.Context, question string) (string, error)
}

// Request carries optional context and images for a single answer.
//
// ImagePaths are local file paths readable by the Codex process.
type Request struct {
	Question   string
	Context    string
	ImagePaths []string

	// ReplyLanguage optionally requests the output language.
	// Supported values: "en", "zh". Empty means auto (follow the user's message).
	ReplyLanguage string
}

// RequestClient is an optional interface for codex clients that accept structured
// inputs (context + images).
type RequestClient interface {
	AnswerRequest(ctx context.Context, req Request) (string, error)
}

// RequestStreamingClient is an optional interface for codex clients that accept
// structured inputs and can emit progress logs while generating the final answer.
type RequestStreamingClient interface {
	AnswerRequestStream(ctx context.Context, req Request, onProgress func(line string)) (string, error)
}

// StreamingClient is an optional interface implemented by codex clients that
// can emit intermediate progress output while generating the final answer.
//
// The onProgress callback receives human-readable log lines (best-effort).
// Implementations should keep output concise and callers should throttle
// updates to avoid hitting chat platform rate limits.
type StreamingClient interface {
	AnswerStream(ctx context.Context, question string, onProgress func(line string)) (string, error)
}

type OpenAIClient struct {
	BaseURL      string
	APIKey       string
	Model        string
	API          string // "chat" or "responses"
	SystemPrompt string

	MaxTokens   int
	Temperature float64

	HTTPClient *http.Client
}

func (c *OpenAIClient) Answer(ctx context.Context, question string) (string, error) {
	text, _, err := c.AnswerWithUsage(ctx, question)
	return text, err
}

func (c *OpenAIClient) AnswerWithUsage(ctx context.Context, question string) (string, Usage, error) {
	if strings.TrimSpace(question) == "" {
		return "", Usage{}, errors.New("empty question")
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{Timeout: 90 * time.Second}
	}

	switch strings.ToLower(c.API) {
	case "responses":
		return c.answerViaResponsesWithUsage(ctx, question)
	case "chat":
		return c.answerViaChatCompletionsWithUsage(ctx, question)
	default:
		return "", Usage{}, fmt.Errorf("unknown CODEX_API=%q (expected chat or responses)", c.API)
	}
}

func (c *OpenAIClient) AnswerRequest(ctx context.Context, req Request) (string, error) {
	text, _, err := c.AnswerRequestWithUsage(ctx, req)
	return text, err
}

func (c *OpenAIClient) AnswerRequestWithUsage(ctx context.Context, req Request) (string, Usage, error) {
	if len(req.ImagePaths) > 0 {
		return "", Usage{}, errors.New("CODEX_MODE=api does not support image inputs; use CODEX_MODE=cli")
	}

	q := strings.TrimSpace(req.Question)
	ctxText := strings.TrimSpace(req.Context)
	if q == "" && ctxText == "" {
		return "", Usage{}, errors.New("empty question")
	}

	if q == "" {
		q = "Please provide a concise conclusion/recommendation based on the context above."
	}

	if ctxText != "" {
		q = strings.Join([]string{
			"Context:",
			ctxText,
			"",
			"Question:",
			q,
		}, "\n")
	} else {
		q = strings.TrimSpace(q)
	}
	return c.AnswerWithUsage(ctx, q)
}

func (c *OpenAIClient) answerViaChatCompletionsWithUsage(ctx context.Context, question string) (string, Usage, error) {
	endpoint, err := joinURL(c.BaseURL, "/chat/completions")
	if err != nil {
		return "", Usage{}, err
	}

	reqBody := chatCompletionsRequest{
		Model: c.Model,
		Messages: []chatMessage{
			{Role: "system", Content: c.SystemPrompt},
			{Role: "user", Content: question},
		},
		Temperature: c.Temperature,
		MaxTokens:   c.MaxTokens,
	}
	raw, err := doJSON(ctx, c.HTTPClient, endpoint, c.APIKey, reqBody)
	if err != nil {
		return "", Usage{}, err
	}

	var resp chatCompletionsResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", Usage{}, fmt.Errorf("decode chat completion response: %w", err)
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return "", Usage{}, fmt.Errorf("codex error: %s", resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return "", Usage{}, errors.New("codex returned no choices")
	}
	text := strings.TrimSpace(resp.Choices[0].Message.Content)
	if text == "" {
		return "", Usage{}, errors.New("codex returned empty content")
	}
	return text, extractUsageFromChatCompletionsResponse(&resp), nil
}

func (c *OpenAIClient) answerViaResponsesWithUsage(ctx context.Context, question string) (string, Usage, error) {
	endpoint, err := joinURL(c.BaseURL, "/responses")
	if err != nil {
		return "", Usage{}, err
	}

	reqBody := responsesRequest{
		Model: c.Model,
		Input: []responsesInputItem{
			{Role: "system", Content: c.SystemPrompt},
			{Role: "user", Content: question},
		},
		MaxOutputTokens: c.MaxTokens,
		Temperature:     c.Temperature,
	}
	raw, err := doJSON(ctx, c.HTTPClient, endpoint, c.APIKey, reqBody)
	if err != nil {
		return "", Usage{}, err
	}
	return extractResponsesTextAndUsage(raw)
}

func joinURL(base, path string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return "", fmt.Errorf("invalid CODEX_BASE_URL: %w", err)
	}
	if u.Scheme == "" {
		return "", fmt.Errorf("invalid CODEX_BASE_URL=%q (missing scheme)", base)
	}

	// Preserve existing base path (e.g. .../v1) and append.
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	return u.String(), nil
}

func doJSON(ctx context.Context, hc *http.Client, endpoint, apiKey string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request codex: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read codex response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Try to extract an error message; otherwise include raw body.
		msg := strings.TrimSpace(string(respBody))
		if msg == "" {
			msg = resp.Status
		}
		return nil, fmt.Errorf("codex http %d: %s", resp.StatusCode, msg)
	}
	return respBody, nil
}

type chatCompletionsRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatCompletionsResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		PromptDetails    *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details,omitempty"`
	} `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type responsesRequest struct {
	Model           string               `json:"model"`
	Input           []responsesInputItem `json:"input"`
	MaxOutputTokens int                  `json:"max_output_tokens,omitempty"`
	Temperature     float64              `json:"temperature,omitempty"`
}

type responsesInputItem struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func extractResponsesText(raw []byte) (string, error) {
	// First, check if the API returned an "error" object.
	var errWrap struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if json.Unmarshal(raw, &errWrap) == nil {
		if errWrap.Error != nil && strings.TrimSpace(errWrap.Error.Message) != "" {
			return "", fmt.Errorf("codex error: %s", errWrap.Error.Message)
		}
	}

	// Some implementations include output_text directly.
	var direct struct {
		OutputText string `json:"output_text"`
	}
	if json.Unmarshal(raw, &direct) == nil {
		if s := strings.TrimSpace(direct.OutputText); s != "" {
			return s, nil
		}
	}

	// Fall back to scanning `output[].content[].text`.
	var parsed struct {
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("decode responses output: %w", err)
	}

	var sb strings.Builder
	for _, out := range parsed.Output {
		_ = out.Type
		for _, c := range out.Content {
			if strings.TrimSpace(c.Text) == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(c.Text)
		}
	}
	if sb.Len() == 0 {
		return "", errors.New("codex returned no output text")
	}
	return strings.TrimSpace(sb.String()), nil
}

func extractUsageFromChatCompletionsResponse(resp *chatCompletionsResponse) Usage {
	if resp == nil || resp.Usage == nil {
		return Usage{}
	}
	prompt := resp.Usage.PromptTokens
	completion := resp.Usage.CompletionTokens
	cached := 0
	if resp.Usage.PromptDetails != nil {
		cached = resp.Usage.PromptDetails.CachedTokens
	}
	if cached < 0 {
		cached = 0
	}
	if cached > prompt {
		cached = prompt
	}
	return Usage{
		InputTokens:       prompt - cached,
		CachedInputTokens: cached,
		OutputTokens:      completion,
	}
}

func extractResponsesTextAndUsage(raw []byte) (string, Usage, error) {
	text, err := extractResponsesText(raw)
	if err != nil {
		return "", Usage{}, err
	}

	var parsed struct {
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			InputDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details,omitempty"`
		} `json:"usage,omitempty"`
	}
	if json.Unmarshal(raw, &parsed) != nil || parsed.Usage == nil {
		return text, Usage{}, nil
	}

	cached := 0
	if parsed.Usage.InputDetails != nil {
		cached = parsed.Usage.InputDetails.CachedTokens
	}
	if cached < 0 {
		cached = 0
	}
	if cached > parsed.Usage.InputTokens {
		cached = parsed.Usage.InputTokens
	}
	u := Usage{
		InputTokens:       parsed.Usage.InputTokens - cached,
		CachedInputTokens: cached,
		OutputTokens:      parsed.Usage.OutputTokens,
	}
	return text, u, nil
}
