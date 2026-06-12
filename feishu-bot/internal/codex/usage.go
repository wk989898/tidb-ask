package codex

import "context"

// Usage captures token usage for a single Codex/OpenAI request.
//
// Values are best-effort:
// - In CODEX_MODE=cli, they come from `codex exec --json` turn.completed events.
// - In CODEX_MODE=api, they come from OpenAI API response usage fields.
type Usage struct {
	InputTokens       int
	CachedInputTokens int
	OutputTokens      int
}

func (u Usage) TotalInputTokens() int {
	return u.InputTokens + u.CachedInputTokens
}

func (u Usage) TotalTokens() int {
	return u.InputTokens + u.CachedInputTokens + u.OutputTokens
}

type MeteredClient interface {
	AnswerWithUsage(ctx context.Context, question string) (string, Usage, error)
}

// MeteredRequestClient is implemented by codex clients that can return token
// usage for structured requests (context + images).
type MeteredRequestClient interface {
	AnswerRequestWithUsage(ctx context.Context, req Request) (string, Usage, error)
}

// MeteredRequestStreamingClient is implemented by codex clients that can return
// token usage for structured requests while streaming progress logs.
type MeteredRequestStreamingClient interface {
	AnswerRequestStreamWithUsage(ctx context.Context, req Request, onProgress func(line string)) (string, Usage, error)
}
