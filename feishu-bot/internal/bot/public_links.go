package bot

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	// Code refs look like:
	// - code/pingcap/tidb/sessionctx/variable/sysvar.go:123
	// - pingcap/tidb/sessionctx/variable/sysvar.go#L123
	codeRefRe = regexp.MustCompile(`(?i)(?:\./)?(?:code/)?[a-z0-9_.-]+/[a-z0-9_.-]+/[a-z0-9_./-]+\.(?:go|rs|proto|toml|yaml|yml|sql|py|java|kt|ts|tsx|js|jsx|sh|md)(?:(?::|#L?)[0-9]+)?`)

	// Doc refs look like:
	// - doc/br/br-auto-tune.md
	// - br/br-auto-tune.md:12
	// - command-line-flags-for-tidb-configuration.md#L45
	docRefRe = regexp.MustCompile(`(?i)(?:\./)?(?:doc/)?[a-z0-9_./-]+\.md(?:(?::|#L?)[0-9]+)?`)

	// Standalone repo refs (no file path), to avoid leaking local directory layout.
	// Examples:
	// - pingcap/tidb
	// - code/tikv/tikv
	repoRefRe = regexp.MustCompile(`(?i)(?:\./)?(?:code/)?(?:pingcap|tikv)/[a-z0-9_.-]+`)
)

func rewriteAnswerPublicLinks(answer, workDir, replyLang string, markdown bool) string {
	t := strings.TrimSpace(answer)
	if t == "" {
		return answer
	}

	// If reply language isn't provided by the caller, infer it from the answer
	// text itself (best-effort) so we can generate the matching docs.pingcap.com
	// language prefix.
	replyLang = strings.ToLower(strings.TrimSpace(replyLang))
	if replyLang == "" {
		if looksLikeChinese(t) {
			replyLang = "zh"
		} else {
			replyLang = "en"
		}
	}

	wd := resolveWorkspaceDir(workDir)
	docDir := ""
	codeDir := ""
	if wd != "" {
		docDir = filepath.Join(wd, "doc")
		codeDir = filepath.Join(wd, "code")
	}

	// Rewrite code refs first (so Markdown files inside code repos become GitHub links,
	// not docs-site links).
	t = rewriteByRegex(t, codeRefRe, func(full string, start int) (string, bool) {
		if looksLikeURLContext(t, start) {
			return "", false
		}

		pathPart, line := splitLineSuffix(full)
		pathPart = strings.TrimPrefix(pathPart, "./")

		// code/<org>/<repo>/...
		if strings.HasPrefix(strings.ToLower(pathPart), "code/") {
			pathPart = pathPart[len("code/"):]
		}

		org, repo, relPath, ok := splitOrgRepoPath(pathPart)
		if !ok {
			return "", false
		}
		if codeDir == "" || !dirExists(filepath.Join(codeDir, org, repo, ".git")) {
			return "", false
		}
		url := githubBlobURL(codeDir, org, repo, relPath, line)
		if url == "" {
			return "", false
		}
		if markdown {
			label := strings.TrimPrefix(full, "./")
			labelLower := strings.ToLower(label)
			if strings.HasPrefix(labelLower, "code/") {
				label = label[len("code/"):]
			}
			return markdownLink(label, url), true
		}
		return url, true
	})

	// Then rewrite doc refs to the official docs site.
	t = rewriteByRegex(t, docRefRe, func(full string, start int) (string, bool) {
		if looksLikeURLContext(t, start) {
			return "", false
		}

		pathPart, _ := splitLineSuffix(full)
		pathPart = strings.TrimPrefix(pathPart, "./")
		if strings.HasPrefix(strings.ToLower(pathPart), "doc/") {
			pathPart = pathPart[len("doc/"):]
		}
		url := pingcapDocsURL(docDir, pathPart, replyLang)
		if url == "" {
			return "", false
		}
		if markdown {
			label := strings.TrimPrefix(full, "./")
			labelLower := strings.ToLower(label)
			if strings.HasPrefix(labelLower, "doc/") {
				label = label[len("doc/"):]
			}
			return markdownLink(label, url), true
		}
		return url, true
	})

	// Finally, rewrite standalone repo references (org/repo) to GitHub repo URLs.
	t = rewriteByRegex(t, repoRefRe, func(full string, start int) (string, bool) {
		if looksLikeURLContext(t, start) {
			return "", false
		}
		// Avoid partial replacement of org/repo/path...
		if start+len(full) < len(t) && t[start+len(full)] == '/' {
			return "", false
		}

		// Be conservative: only rewrite org/repo if we can confirm the repo exists
		// in the local ./code checkout. This avoids generating bogus links from
		// product-name slash lists like "TiDB/TiKV/PingCAP".
		if codeDir == "" {
			return "", false
		}

		p := strings.TrimPrefix(full, "./")
		pLower := strings.ToLower(p)
		if strings.HasPrefix(pLower, "code/") {
			p = p[len("code/"):]
		}

		org, repo, ok := splitOrgRepo(p)
		if !ok {
			return "", false
		}
		org = strings.ToLower(org)
		repo = strings.ToLower(repo)
		if !dirExists(filepath.Join(codeDir, org, repo, ".git")) {
			return "", false
		}
		url := "https://github.com/" + org + "/" + repo
		if markdown {
			label := strings.TrimPrefix(full, "./")
			labelLower := strings.ToLower(label)
			if strings.HasPrefix(labelLower, "code/") {
				label = label[len("code/"):]
			}
			return markdownLink(label, url), true
		}
		return url, true
	})

	if markdown {
		// Feishu's lark_md renderer can be picky:
		// - Angle-bracket autolinks like <https://...> may be treated as HTML and disappear
		// - Standard markdown links like [text](https://...) may render inconsistently
		//
		// Convert them into a conservative `label`: https://... form, then sanitize
		// remaining bare URLs to avoid trailing punctuation being swallowed.
		t = unwrapAngleBracketAutolinks(t)
		t = rewriteMarkdownLinksToAutolinks(t)
		t = wrapBareURLsForMarkdown(t)
	}

	return t
}

func looksLikeChinese(s string) bool {
	for _, r := range s {
		// CJK Unified Ideographs (basic range).
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

func markdownLink(label, url string) string {
	label = strings.TrimSpace(label)
	url = strings.TrimSpace(url)
	if url == "" {
		return ""
	}

	// Avoid Feishu quirks: prefer a plain URL (no <...>, no [text](url)),
	// optionally preceded by an inline-code label.
	if label == "" {
		return url
	}

	// Avoid accidental "org/repo" auto-linking and other rich-text quirks by
	// rendering the label as inline code. Keep it conservative: strip newlines
	// and replace backticks so we don't break the surrounding markdown.
	label = strings.ReplaceAll(label, "\r", " ")
	label = strings.ReplaceAll(label, "\n", " ")
	label = strings.ReplaceAll(label, "`", "'")
	label = strings.TrimSpace(label)
	if label == "" {
		return url
	}
	return "`" + label + "`: " + url
}

func wrapBareURLsForMarkdown(markdown string) string {
	// Feishu's autolink parsing can be sensitive to trailing punctuation like ')'
	// or '。', which results in broken URLs. We keep URLs as plain text, but move
	// trailing punctuation outside of the URL token.
	//
	// Best-effort: we avoid touching code fences and existing markdown links.
	const fence = "```"
	parts := strings.Split(markdown, fence)
	for i := 0; i < len(parts); i++ {
		// Even indices are outside fenced code blocks.
		if i%2 == 1 {
			continue
		}
		parts[i] = wrapBareURLsNoCode(parts[i])
	}
	return strings.Join(parts, fence)
}

func unwrapAngleBracketAutolinks(markdown string) string {
	// Convert <https://...> into https://... because lark_md may treat the angle
	// bracket form as HTML and drop it entirely.
	const fence = "```"
	parts := strings.Split(markdown, fence)
	for i := 0; i < len(parts); i++ {
		if i%2 == 1 {
			continue
		}
		parts[i] = unwrapAngleBracketAutolinksNoCode(parts[i])
	}
	return strings.Join(parts, fence)
}

var angleAutolinkRe = regexp.MustCompile(`<https?://[^>\s]+>`)

func unwrapAngleBracketAutolinksNoCode(s string) string {
	return angleAutolinkRe.ReplaceAllStringFunc(s, func(m string) string {
		m = strings.TrimSpace(m)
		m = strings.TrimPrefix(m, "<")
		m = strings.TrimSuffix(m, ">")
		return m
	})
}

func rewriteMarkdownLinksToAutolinks(markdown string) string {
	// Best-effort conversion:
	//   [label](https://example.com)  ->  `label`: https://example.com
	//   [label](<https://example.com>) -> `label`: https://example.com
	//
	// We intentionally do NOT try to handle every Markdown edge case; we only
	// convert the common simple forms and skip fenced code blocks.
	const fence = "```"
	parts := strings.Split(markdown, fence)
	for i := 0; i < len(parts); i++ {
		if i%2 == 1 {
			continue
		}
		parts[i] = rewriteMarkdownLinksNoCode(parts[i])
	}
	return strings.Join(parts, fence)
}

var markdownInlineLinkRe = regexp.MustCompile(`\[[^\]\r\n]+\]\((?:<)?https?://[^\s)<>]+(?:>)?\)`)

func rewriteMarkdownLinksNoCode(s string) string {
	matches := markdownInlineLinkRe.FindAllStringIndex(s, -1)
	if len(matches) == 0 {
		return s
	}

	var b strings.Builder
	b.Grow(len(s) + len(matches)*8)

	last := 0
	for _, m := range matches {
		start, end := m[0], m[1]
		if start < last {
			continue
		}

		// Skip image links: ![alt](url)
		if start > 0 && s[start-1] == '!' {
			continue
		}

		b.WriteString(s[last:start])
		full := s[start:end]

		// Parse `[label](url)`
		mid := strings.Index(full, "](")
		if mid < 0 {
			b.WriteString(full)
			last = end
			continue
		}
		label := strings.TrimSpace(full[1:mid])
		urlPart := strings.TrimSpace(full[mid+2 : len(full)-1])
		urlPart = strings.TrimSpace(strings.Trim(urlPart, "<>"))

		if label == "" || urlPart == "" || !strings.HasPrefix(urlPart, "http") {
			b.WriteString(full)
			last = end
			continue
		}

		// Keep output conservative for Feishu markdown.
		label = strings.ReplaceAll(label, "\r", " ")
		label = strings.ReplaceAll(label, "\n", " ")
		label = strings.ReplaceAll(label, "`", "'")
		label = strings.TrimSpace(label)

		if strings.EqualFold(label, urlPart) {
			b.WriteString(urlPart)
		} else if label != "" {
			b.WriteString("`")
			b.WriteString(label)
			b.WriteString("`: ")
			b.WriteString(urlPart)
		} else {
			b.WriteString(urlPart)
		}

		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

func wrapBareURLsNoCode(s string) string {
	urlRe := regexp.MustCompile(`https?://[^\s<]+`)
	matches := urlRe.FindAllStringIndex(s, -1)
	if len(matches) == 0 {
		return s
	}

	var b strings.Builder
	b.Grow(len(s) + len(matches)*4)

	last := 0
	for _, m := range matches {
		start, end := m[0], m[1]
		if start < last {
			continue
		}
		// Skip URLs that are already part of a markdown link destination: ](url) or ](<url>)
		if start >= 2 && s[start-2:start] == "](" {
			continue
		}
		if start >= 3 && s[start-3:start] == "](<" {
			continue
		}

		b.WriteString(s[last:start])

		token := s[start:end]
		url, trail := splitURLTrailingPunct(token)
		if url == "" {
			b.WriteString(token)
		} else {
			b.WriteString(url)
			if trail != "" {
				// Add a space so punctuation won't be swallowed into the URL token.
				b.WriteString(" ")
				b.WriteString(trail)
			}
		}
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

func splitURLTrailingPunct(token string) (url string, trailing string) {
	u := strings.TrimSpace(token)
	if u == "" {
		return "", ""
	}

	// Trim common trailing punctuation that chat markdown linkifiers often include.
	// Keep it conservative; only strip brackets if there's no opening bracket in the URL.
	trimSet := func(r rune) bool {
		switch r {
		case '.', ',', ';', ':', '!', '?',
			'。', '，', '；', '：', '！', '？',
			'、':
			return true
		}
		return false
	}

	var removed []rune
	for len(u) > 0 {
		r, size := utf8.DecodeLastRuneInString(u)
		if r == utf8.RuneError && size == 1 {
			// Invalid UTF-8; treat last byte as a rune.
			r = rune(u[len(u)-1])
			size = 1
		}
		if trimSet(r) {
			removed = append(removed, r)
			u = u[:len(u)-size]
			continue
		}

		switch r {
		case ')':
			if !strings.ContainsRune(u, '(') {
				removed = append(removed, r)
				u = u[:len(u)-size]
				continue
			}
		case '>':
			if !strings.ContainsRune(u, '<') {
				removed = append(removed, r)
				u = u[:len(u)-size]
				continue
			}
		case '）':
			if !strings.ContainsRune(u, '（') {
				removed = append(removed, r)
				u = u[:len(u)-size]
				continue
			}
		case ']':
			if !strings.ContainsRune(u, '[') {
				removed = append(removed, r)
				u = u[:len(u)-size]
				continue
			}
		case '】':
			if !strings.ContainsRune(u, '【') {
				removed = append(removed, r)
				u = u[:len(u)-size]
				continue
			}
		case '}':
			if !strings.ContainsRune(u, '{') {
				removed = append(removed, r)
				u = u[:len(u)-size]
				continue
			}
		}
		break
	}

	if len(removed) > 0 {
		// removed is collected from end; reverse it to keep original order.
		var sb strings.Builder
		for i := len(removed) - 1; i >= 0; i-- {
			sb.WriteRune(removed[i])
		}
		trailing = sb.String()
	}

	return strings.TrimSpace(u), trailing
}

func rewriteByRegex(s string, re *regexp.Regexp, f func(full string, start int) (replacement string, ok bool)) string {
	matches := re.FindAllStringIndex(s, -1)
	if len(matches) == 0 {
		return s
	}

	var b strings.Builder
	b.Grow(len(s) + len(matches)*16)

	last := 0
	for _, m := range matches {
		start, end := m[0], m[1]
		if start < last {
			continue
		}
		b.WriteString(s[last:start])
		full := s[start:end]
		if rep, ok := f(full, start); ok && strings.TrimSpace(rep) != "" {
			b.WriteString(rep)
		} else {
			b.WriteString(full)
		}
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

func resolveWorkspaceDir(workDir string) string {
	wd := strings.TrimSpace(workDir)
	if wd != "" {
		if abs, err := filepath.Abs(wd); err == nil {
			wd = abs
		}
		return wd
	}

	// Fallback: try cwd and its parent.
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	cwd, _ = filepath.Abs(cwd)
	if dirExists(filepath.Join(cwd, "doc")) && dirExists(filepath.Join(cwd, "code")) {
		return cwd
	}
	parent := filepath.Dir(cwd)
	if parent != "" && parent != cwd && dirExists(filepath.Join(parent, "doc")) && dirExists(filepath.Join(parent, "code")) {
		return parent
	}
	return ""
}

func looksLikeURLContext(s string, start int) bool {
	// Avoid rewriting inside an existing URL like:
	//   https://github.com/pingcap/tidb/blob/.../file.go#L1
	// We look back to the start of the current "token" and check for "://".
	if start <= 0 {
		return false
	}
	tokenStart := start
	for tokenStart > 0 {
		c := s[tokenStart-1]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			break
		}
		tokenStart--
	}
	prefix := s[tokenStart:start]
	return strings.Contains(prefix, "://")
}

func splitLineSuffix(ref string) (path string, line string) {
	s := ref
	// Prefer #L123
	lower := strings.ToLower(s)
	if idx := strings.LastIndex(lower, "#l"); idx >= 0 {
		cand := s[idx+2:]
		if isDigits(cand) {
			return s[:idx], cand
		}
	}
	if idx := strings.LastIndex(s, "#"); idx >= 0 {
		cand := s[idx+1:]
		if isDigits(cand) {
			return s[:idx], cand
		}
	}
	// Then :123
	if idx := strings.LastIndex(s, ":"); idx >= 0 {
		cand := s[idx+1:]
		if isDigits(cand) {
			return s[:idx], cand
		}
	}
	return s, ""
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func splitOrgRepoPath(p string) (org, repo, rel string, ok bool) {
	p = strings.TrimPrefix(p, "/")
	parts := strings.SplitN(p, "/", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}
	org = strings.TrimSpace(parts[0])
	repo = strings.TrimSpace(parts[1])
	rel = strings.TrimPrefix(strings.TrimSpace(parts[2]), "/")
	if org == "" || repo == "" || rel == "" {
		return "", "", "", false
	}
	return org, repo, rel, true
}

func splitOrgRepo(p string) (org, repo string, ok bool) {
	p = strings.TrimPrefix(strings.TrimSpace(p), "/")
	parts := strings.SplitN(p, "/", 3)
	if len(parts) < 2 {
		return "", "", false
	}
	org = strings.TrimSpace(parts[0])
	repo = strings.TrimSpace(parts[1])
	if org == "" || repo == "" {
		return "", "", false
	}
	return org, repo, true
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func githubBlobURL(codeDir, org, repo, relPath, line string) string {
	repoDir := filepath.Join(codeDir, org, repo)
	if !dirExists(filepath.Join(repoDir, ".git")) {
		return ""
	}

	ref := gitHead(repoDir)
	if ref == "" {
		// Best-effort fallback; many repos still use master.
		ref = "master"
	}

	u := "https://github.com/" + org + "/" + repo + "/blob/" + ref + "/" + filepath.ToSlash(relPath)
	if line != "" {
		u += "#L" + line
	}
	return u
}

func pingcapDocsURL(docDir, docRelPath, replyLang string) string {
	docRelPath = strings.TrimSpace(strings.TrimPrefix(docRelPath, "/"))
	if docRelPath == "" {
		return ""
	}
	if !strings.HasSuffix(strings.ToLower(docRelPath), ".md") {
		return ""
	}

	base := strings.TrimSuffix(filepath.Base(docRelPath), filepath.Ext(docRelPath))
	if base == "" {
		return ""
	}
	switch strings.ToLower(base) {
	case "readme", "license", "contributing", "changelog", "code_of_conduct", "security":
		// These are typically repo meta-files, not user-facing docs pages.
		return ""
	}
	// Hugo-like index files are not stable as direct user-facing pages.
	if strings.HasPrefix(base, "_") {
		return ""
	}

	langPrefix := "https://docs.pingcap.com/"
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(replyLang)), "zh") {
		langPrefix = "https://docs.pingcap.com/zh/"
	}

	// Determine whether this doc belongs to TiDB Cloud docs.
	isCloud := strings.HasPrefix(docRelPath, "tidb-cloud/")
	if !isCloud && docDir != "" {
		// If the reference is just a basename (e.g. api-overview.md), infer cloud
		// by checking the known cloud docs directory.
		if !strings.Contains(docRelPath, "/") {
			if fileExists(filepath.Join(docDir, "tidb-cloud", filepath.Base(docRelPath))) {
				isCloud = true
			}
		}
	}

	if isCloud {
		return langPrefix + "tidbcloud/" + base + "/"
	}

	versionSeg := docsVersionSegment(docDir)
	return langPrefix + "tidb/" + versionSeg + "/" + base + "/"
}

func docsVersionSegment(docDir string) string {
	// Default to "stable" if we can't detect.
	if docDir == "" {
		return "stable"
	}
	if !dirExists(filepath.Join(docDir, ".git")) {
		return "stable"
	}

	br := strings.TrimSpace(gitOutput(docDir, "rev-parse", "--abbrev-ref", "HEAD"))
	br = strings.TrimSpace(br)
	if br == "" || br == "HEAD" {
		return "stable"
	}

	lower := strings.ToLower(br)
	if lower == "master" || lower == "main" {
		return "dev"
	}
	if strings.HasPrefix(lower, "release-") {
		rest := strings.TrimPrefix(lower, "release-")
		// release-cloud -> no versioned TiDB docs; keep stable as a safe default.
		if rest == "cloud" {
			return "stable"
		}
		// release-8.5 -> v8.5
		if looksLikeMajorMinor(rest) {
			return "v" + rest
		}
	}

	return "stable"
}

func looksLikeMajorMinor(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 2 {
		return false
	}
	return isDigits(parts[0]) && isDigits(parts[1])
}

func gitHead(repoDir string) string {
	return strings.TrimSpace(gitOutput(repoDir, "rev-parse", "HEAD"))
}

func gitOutput(repoDir string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoDir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_ASKPASS=", "GIT_TERMINAL_PROMPT=0", "SSH_ASKPASS=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	return string(out)
}
