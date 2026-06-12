package codex

import (
	"strings"
	"unicode"
)

func normalizeReplyLanguage(s string) string {
	t := strings.ToLower(strings.TrimSpace(s))
	if t == "" {
		return ""
	}
	if strings.HasPrefix(t, "en") {
		return "en"
	}
	if strings.HasPrefix(t, "zh") || t == "cn" || t == "chs" || t == "chinese" {
		return "zh"
	}
	return t
}

func inferLanguageHint(primaryText, fallbackText string) string {
	if lang := guessLanguageFromText(primaryText); lang != "" {
		return lang
	}
	return guessLanguageFromText(fallbackText)
}

func guessLanguageFromText(s string) string {
	t := strings.TrimSpace(s)
	if t == "" {
		return ""
	}

	// Heuristic: count CJK (Han) vs Latin letters.
	// - Pure English -> en
	// - Pure Chinese -> zh
	// - Mixed -> choose the dominant one (favor zh unless English is clearly dominant)
	cjk := 0
	latin := 0
	for _, r := range t {
		switch {
		case unicode.Is(unicode.Han, r):
			cjk++
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			latin++
		}
	}

	if cjk == 0 && latin > 0 {
		return "en"
	}
	if cjk > 0 && latin == 0 {
		return "zh"
	}
	if cjk == 0 && latin == 0 {
		return ""
	}

	// Mixed: only switch to English if it's clearly dominant.
	if latin >= cjk*2 {
		return "en"
	}
	return "zh"
}
