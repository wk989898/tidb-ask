package codex

import (
	"regexp"
	"strings"
)

var (
	thinkTagRe      = regexp.MustCompile(`(?is)<think>.*?</think>`)
	analysisTagRe   = regexp.MustCompile(`(?is)<analysis>.*?</analysis>`)
	thinkBlockRe    = regexp.MustCompile(`(?is)\[think\].*?\[/think\]`)
	thinkingBlockRe = regexp.MustCompile(`(?is)\[thinking\].*?\[/thinking\]`)

	// ANSI escape sequences that may leak from tool outputs.
	ansiCSIRe = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	ansiOSCRe = regexp.MustCompile(`\x1b\][^\x07]*(?:\x07|\x1b\\)`)
	// Some terminals emit cursor position reports like "[41;96R" (ESC may be stripped by intermediate layers).
	cursorPosReportRe = regexp.MustCompile(`\[[0-9]{1,3};[0-9]{1,3}R`)

	// Internal implementation details that should not leak into end-user answers.
	feishuMcpRe = regexp.MustCompile(`(?i)\bfei(?:shu|chu)\s*mcp\b`)
	skillNameRe = regexp.MustCompile(`(?i)\b(?:read-local-(?:doc|code)|search-issue)\b`)

	// Path-like prefixes that frequently show up in tool outputs.
	dotDocSlashRe  = regexp.MustCompile(`(?i)\./doc/`)
	dotCodeSlashRe = regexp.MustCompile(`(?i)\./code/`)
	dotDocRe       = regexp.MustCompile(`(?i)\b\./doc\b`)
	dotCodeRe      = regexp.MustCompile(`(?i)\b\./code\b`)

	standaloneMcpRe = regexp.MustCompile(`(?i)\bmcp\b`)
)

func sanitizeAnswerText(s string) string {
	t := strings.TrimSpace(s)
	if t == "" {
		return ""
	}

	// Strip ANSI escapes early to avoid breaking markdown rendering in chat clients.
	t = ansiOSCRe.ReplaceAllString(t, "")
	t = ansiCSIRe.ReplaceAllString(t, "")
	t = cursorPosReportRe.ReplaceAllString(t, "")

	// Remove common "hidden reasoning" wrappers that some models/providers leak.
	t = thinkTagRe.ReplaceAllString(t, "")
	t = analysisTagRe.ReplaceAllString(t, "")
	t = thinkBlockRe.ReplaceAllString(t, "")
	t = thinkingBlockRe.ReplaceAllString(t, "")

	t = redactInternalDetails(t)
	return strings.TrimSpace(t)
}

func redactInternalDetails(s string) string {
	t := s

	// Normalize tool-ish and infra-ish names into user-friendly wording.
	// (Avoid exposing internal protocol / tool names.)
	t = feishuMcpRe.ReplaceAllString(t, "document retrieval tool")
	t = skillNameRe.ReplaceAllString(t, "local retrieval")

	// Hide workspace root folder names if they appear in answers.
	// Prefer keeping the remainder of the path so users still get useful pointers.
	t = dotDocSlashRe.ReplaceAllString(t, "")
	t = dotCodeSlashRe.ReplaceAllString(t, "")
	t = dotDocRe.ReplaceAllString(t, "local docs")
	t = dotCodeRe.ReplaceAllString(t, "local code")

	// Replace bare "MCP" mentions, but keep URLs intact (e.g. ".../mcp/...").
	t = replaceStandaloneMCP(t, "tool")

	return t
}

func replaceStandaloneMCP(s, replacement string) string {
	locs := standaloneMcpRe.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return s
	}

	var b strings.Builder
	b.Grow(len(s) + len(locs)*len(replacement))

	last := 0
	for _, loc := range locs {
		start, end := loc[0], loc[1]
		if start < last {
			continue
		}
		b.WriteString(s[last:start])

		prev := byte(0)
		if start > 0 {
			prev = s[start-1]
		}
		// If this looks like part of a URL path or scheme, leave it unchanged.
		if prev == '/' || prev == ':' {
			b.WriteString(s[start:end])
		} else {
			b.WriteString(replacement)
		}
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}
