package bot

import (
	"encoding/json"
	"regexp"
	"strings"
)

type postOutElement struct {
	Tag  string `json:"tag"`
	Text string `json:"text,omitempty"`
	Href string `json:"href,omitempty"`
}

type postOutPayload struct {
	Title   string             `json:"title"`
	Content [][]postOutElement `json:"content"`
}

var (
	// Be conservative: exclude angle brackets so placeholders like
	// "http://<ticdc-host>:8300" won't be treated as a real URL link.
	urlTokenRe = regexp.MustCompile(`https?://[^\s<>]+`)

	mdHeadingRe = regexp.MustCompile(`^\s{0,3}(#{1,6})\s+(.+?)\s*$`)
)

func renderPostRichText(text string) (msgType string, content string) {
	lines := splitLinesForPost(text)
	lines = normalizeMarkdownCodeFences(lines)
	title, lines := extractPostTitleFromMarkdown(lines)

	rows := make([][]postOutElement, 0, len(lines))
	for _, line := range lines {
		row := renderPostLine(line)
		// The API doesn't like empty rows; ensure at least one element.
		if len(row) == 0 {
			row = []postOutElement{{Tag: "text", Text: " "}}
		}
		rows = append(rows, row)
	}

	payload := map[string]postOutPayload{
		// Use zh_cn for maximum compatibility even when the content is English.
		"zh_cn": {
			Title:   title,
			Content: rows,
		},
	}
	raw, _ := json.Marshal(payload)
	return "post", string(raw)
}

func splitLinesForPost(text string) []string {
	t := strings.ReplaceAll(text, "\r\n", "\n")
	t = strings.ReplaceAll(t, "\r", "\n")
	// Keep trailing empty lines from exploding; cap them.
	t = strings.TrimRight(t, "\n")
	if t == "" {
		return []string{""}
	}
	return strings.Split(t, "\n")
}

func renderPostLine(line string) []postOutElement {
	// Preserve blank lines as a visible spacer.
	if strings.TrimSpace(line) == "" {
		return []postOutElement{{Tag: "text", Text: " "}}
	}

	// Normalize common markdown bullets into a nicer bullet prefix.
	normalized := normalizeMarkdownLine(line)
	normalized = normalizeBullets(normalized)

	matches := urlTokenRe.FindAllStringIndex(normalized, -1)
	if len(matches) == 0 {
		return []postOutElement{{Tag: "text", Text: normalized}}
	}

	var out []postOutElement
	last := 0
	for _, m := range matches {
		start, end := m[0], m[1]
		if start < last {
			continue
		}

		if start > last {
			seg := normalized[last:start]
			if seg != "" {
				out = append(out, postOutElement{Tag: "text", Text: seg})
			}
		}

		token := normalized[start:end]
		url, trailing := splitURLTrailingPunct(token)
		if url != "" {
			out = append(out, postOutElement{Tag: "a", Text: url, Href: url})
			if trailing != "" {
				out = append(out, postOutElement{Tag: "text", Text: trailing})
			}
		} else {
			out = append(out, postOutElement{Tag: "text", Text: token})
		}
		last = end
	}

	if last < len(normalized) {
		seg := normalized[last:]
		if seg != "" {
			out = append(out, postOutElement{Tag: "text", Text: seg})
		}
	}

	return mergeAdjacentPostText(out)
}

func normalizeBullets(line string) string {
	// Keep indentation, but normalize the bullet marker.
	trimLeft := strings.TrimLeft(line, " \t")
	indent := line[:len(line)-len(trimLeft)]

	switch {
	case strings.HasPrefix(trimLeft, "- "):
		return indent + "• " + strings.TrimPrefix(trimLeft, "- ")
	case strings.HasPrefix(trimLeft, "* "):
		return indent + "• " + strings.TrimPrefix(trimLeft, "* ")
	default:
		return line
	}
}

func extractPostTitleFromMarkdown(lines []string) (title string, remaining []string) {
	// Pick the first markdown heading as the post title, and drop that heading
	// line from the body (also drop following blank lines).
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		m := mdHeadingRe.FindStringSubmatch(lines[i])
		if m == nil {
			break
		}
		title = strings.TrimSpace(m[2])
		j := i + 1
		for j < len(lines) && strings.TrimSpace(lines[j]) == "" {
			j++
		}
		remaining = append([]string(nil), lines[:i]...)
		remaining = append(remaining, lines[j:]...)
		return title, remaining
	}
	return "", lines
}

func normalizeMarkdownCodeFences(lines []string) []string {
	// Remove ``` fences and render code blocks as plain text (indented) so the
	// output remains readable in Feishu rich text.
	var out []string
	inFence := false
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			out = append(out, "    "+line)
		} else {
			out = append(out, line)
		}
	}
	return out
}

func normalizeMarkdownLine(line string) string {
	// Convert common Markdown constructs to a cleaner plain-text style suitable
	// for Feishu post messages.
	//
	// Goal: avoid leaking literal Markdown markers like "##" while keeping the
	// content readable.
	t := line

	// Strip markdown headings (the first one is promoted to post title elsewhere).
	if m := mdHeadingRe.FindStringSubmatch(t); m != nil {
		// Keep any leading indentation before the heading marker.
		trimLeft := strings.TrimLeft(t, " \t")
		indent := t[:len(t)-len(trimLeft)]
		t = indent + strings.TrimSpace(m[2])
	}

	// Remove common emphasis markers.
	t = strings.ReplaceAll(t, "**", "")
	t = strings.ReplaceAll(t, "__", "")

	// Inline code: backticks do not render as code in post messages, so strip them.
	t = strings.ReplaceAll(t, "`", "")

	// Blockquote: keep content, but make it visually distinct.
	trimLeft := strings.TrimLeft(t, " \t")
	if strings.HasPrefix(trimLeft, ">") {
		indent := t[:len(t)-len(trimLeft)]
		trimLeft = strings.TrimSpace(strings.TrimPrefix(trimLeft, ">"))
		t = indent + "│ " + trimLeft
	}

	return t
}

func mergeAdjacentPostText(in []postOutElement) []postOutElement {
	if len(in) == 0 {
		return in
	}
	out := make([]postOutElement, 0, len(in))
	for _, el := range in {
		// Keep whitespace-only text elements, because they are sometimes the only
		// separator between two URL elements (e.g. "url1 url2").
		// Only drop truly empty strings.
		if el.Tag == "text" && el.Text == "" {
			continue
		}
		if el.Tag == "text" && len(out) > 0 && out[len(out)-1].Tag == "text" {
			out[len(out)-1].Text += el.Text
			continue
		}
		out = append(out, el)
	}
	if len(out) == 0 {
		return []postOutElement{{Tag: "text", Text: " "}}
	}
	return out
}
