package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

type ExecError struct {
	// Message is safe to show to end users.
	Message string
	// DebugOutput contains full codex stdout/stderr; do NOT show to end users.
	DebugOutput string
	// Cause is the underlying error (exit status, context cancel, io errors, etc).
	Cause error
}

func (e *ExecError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Message) == "" {
		return "codex execution failed"
	}
	return e.Message
}

func (e *ExecError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// ExecClient shells out to local `codex exec` so it can use the local Codex
// configuration, installed skills, MCP servers, and workspace files.
//
// This is the recommended mode when you want the bot to answer based on local
// ./doc and ./code contents (via the skills you created).
type ExecClient struct {
	CodexPath string // default: "codex"
	WorkDir   string // directory that contains ./doc and ./code
	Sandbox   string // read-only|workspace-write|danger-full-access (ignored when BypassApprovalsAndSandbox=true)

	// When true, starts Codex with --dangerously-bypass-approvals-and-sandbox to
	// avoid interactive stalls and (in some environments) sandbox failures.
	// WARNING: this disables Codex's built-in sandbox, so use RunAsUser + FS perms
	// (or external sandboxing) if you need real write protection.
	BypassApprovalsAndSandbox bool

	// When true and running on Linux as root, runs Codex inside a private mount
	// namespace and exposes only ./doc and ./code under WorkDir.
	//
	// This is the recommended way to enforce:
	// - Codex primarily "sees" only doc/ and code/ under the working directory
	//
	// NOTE: Codex can still read other absolute paths outside WorkDir; restrict
	// secrets accordingly if you need strong isolation.
	IsolateDocAndCode bool

	// RunAsUser optionally runs codex under another OS user via `su` (Linux only).
	// This is useful to enforce read-only access to the workspace via filesystem
	// permissions, even when sandbox is bypassed.
	RunAsUser string

	// HomeDir sets HOME for the codex process (so it can read ~/.codex/config.toml
	// and ~/.codex/skills for that user). Recommended when RunAsUser is set.
	HomeDir string

	SystemPrompt string
}

func (c *ExecClient) Answer(ctx context.Context, question string) (string, error) {
	text, _, err := c.AnswerRequestStreamWithUsage(ctx, Request{Question: question}, nil)
	return text, err
}

func (c *ExecClient) AnswerStream(ctx context.Context, question string, onProgress func(line string)) (string, error) {
	text, _, err := c.AnswerRequestStreamWithUsage(ctx, Request{Question: question}, onProgress)
	return text, err
}

func (c *ExecClient) AnswerRequest(ctx context.Context, req Request) (string, error) {
	text, _, err := c.AnswerRequestStreamWithUsage(ctx, req, nil)
	return text, err
}

func (c *ExecClient) AnswerRequestStream(ctx context.Context, req Request, onProgress func(line string)) (string, error) {
	text, _, err := c.AnswerRequestStreamWithUsage(ctx, req, onProgress)
	return text, err
}

func (c *ExecClient) AnswerWithUsage(ctx context.Context, question string) (string, Usage, error) {
	return c.AnswerRequestStreamWithUsage(ctx, Request{Question: question}, nil)
}

func (c *ExecClient) AnswerRequestWithUsage(ctx context.Context, req Request) (string, Usage, error) {
	return c.AnswerRequestStreamWithUsage(ctx, req, nil)
}

func (c *ExecClient) AnswerRequestStreamWithUsage(ctx context.Context, req Request, onProgress func(line string)) (string, Usage, error) {
	q := strings.TrimSpace(req.Question)
	ctxText := strings.TrimSpace(req.Context)

	if q == "" && ctxText != "" {
		q = "Please provide a concise conclusion/recommendation based on the context above."
	}
	if q == "" && len(req.ImagePaths) > 0 {
		q = "Please interpret the image(s) and provide a concise conclusion/recommendation."
	}
	if q == "" {
		return "", Usage{}, errors.New("empty question")
	}

	codexPath := strings.TrimSpace(c.CodexPath)
	if codexPath == "" {
		codexPath = "codex"
	}

	workDir := strings.TrimSpace(c.WorkDir)
	if workDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", Usage{}, fmt.Errorf("getwd: %w", err)
		}
		workDir = wd
		// If we're started from a subdirectory (e.g. ./feishu-bot), try to
		// locate a parent directory that contains both ./doc and ./code.
		if !hasDir(workDir, "doc") || !hasDir(workDir, "code") {
			parent := filepath.Dir(workDir)
			if parent != "" && parent != workDir {
				if hasDir(parent, "doc") && hasDir(parent, "code") {
					workDir = parent
				}
			}
		}
	}
	workDir, _ = filepath.Abs(workDir)

	sandbox := strings.TrimSpace(c.Sandbox)
	if sandbox == "" {
		sandbox = "read-only"
	}

	runAsUser := strings.TrimSpace(c.RunAsUser)
	if runAsUser != "" && runtime.GOOS != "linux" {
		return "", Usage{}, fmt.Errorf("RunAsUser is only supported on linux (GOOS=%s)", runtime.GOOS)
	}

	homeDir := strings.TrimSpace(c.HomeDir)

	prompt := strings.TrimSpace(c.SystemPrompt)
	if prompt == "" {
		prompt = "You are a rigorous engineering assistant. Reply in English only. Provide a clear conclusion and concise steps (or short code snippets) when needed."
	}

	userBlock := q
	if ctxText != "" {
		userBlock = strings.Join([]string{
			"Context:",
			ctxText,
			"",
			"Question:",
			q,
		}, "\n")
	}

	constraintTitle := "Constraints:"
	userTitle := "User question:"
	constraintLines := []string{
		"- Output ONLY the final answer; do NOT output any hidden reasoning (e.g. thinking/Thoughts or <think>...</think>).",
		"- Language: Reply in English ONLY. Do NOT produce bilingual output.",
		"- Links: When outputting any URL, output the full plain URL starting with https:// (copy-pastable and clickable in Lark/Feishu). Do NOT wrap URLs in angle brackets like <https://...>. Do NOT use Markdown link syntax like [text](url). Do NOT put trailing punctuation immediately after the URL; if needed, put punctuation after a space or on the next line.",
		"- Avoid accidental auto-linking in chat clients: do NOT write bare repo-like tokens such as \"org/repo\" unless you intend a GitHub link. If needed, wrap them in inline code like `pingcap/tidb` or use a full URL like https://github.com/pingcap/tidb.",
		"- If the user asks for links, you MUST provide them (no empty bullets). For docs use https://docs.pingcap.com/ URLs. For code use GitHub URLs; for a function/symbol, prefer a blob URL with a line anchor (#Lx).",
		"- Formatting for Lark/Feishu rich-text posts: avoid Markdown headings like \"## ...\" and avoid fenced code blocks like ```; use plain text with short paragraphs; use \"•\" for bullets and \"1.\" for numbered steps.",
		"- Do not expose internal implementation details (local folder names, tool/skill/protocol names, etc.). Provide only the conclusion and necessary explanation/steps.",
		"- Internet access and configured retrieval tools are allowed when necessary; when version/implementation details matter, prefer local docs and source code as ground truth.",
		"- If you need to align to a specific version, you may run git operations (fetch/switch/reset, etc.) in the local repos; do NOT commit/push; do NOT manually edit file contents.",
		"- When citing sources: docs must use https://docs.pingcap.com/ links; code must use GitHub source links (e.g. https://github.com/<org>/<repo>/blob/<ref>/<path>#Lx). Do NOT output any local file paths.",
		"- If the question depends on version differences, align to the requested version first; if the target version does not exist remotely, say so and ask a minimal follow-up.",
	}

	fullPromptParts := []string{prompt, "", constraintTitle}
	fullPromptParts = append(fullPromptParts, constraintLines...)
	fullPromptParts = append(fullPromptParts, "", userTitle, userBlock)
	fullPrompt := strings.Join(fullPromptParts, "\n")

	outFile, err := os.CreateTemp("", "codex-last-message-*.txt")
	if err != nil {
		return "", Usage{}, fmt.Errorf("create temp file: %w", err)
	}
	outPath := outFile.Name()
	_ = outFile.Close()
	defer os.Remove(outPath)

	// Global flags (accepted before the subcommand).
	globalArgs := make([]string, 0, 16)
	if c.BypassApprovalsAndSandbox {
		globalArgs = append(globalArgs, "--dangerously-bypass-approvals-and-sandbox")
	} else {
		globalArgs = append(globalArgs, "-a", "never", "--sandbox", sandbox)
	}

	// Subcommand + options.
	execArgs := []string{
		"exec",
		"--skip-git-repo-check",
		"-C",
		".",
		"--color",
		"never",
		"--json",
		"--output-last-message",
		outPath,
	}

	for _, p := range req.ImagePaths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", Usage{}, fmt.Errorf("abs image path: %w", err)
		}
		if _, err := os.Stat(abs); err != nil {
			return "", Usage{}, fmt.Errorf("image not readable: %s", abs)
		}
		// Note: --image must appear after the `exec` subcommand, otherwise it can
		// be interpreted as running Codex in "prompt mode" (not subcommand mode).
		execArgs = append(execArgs, "--image", abs)
	}

	execArgs = append(execArgs, "-" /* read prompt from stdin */)

	allArgs := append(globalArgs, execArgs...)

	var cmd *exec.Cmd
	if c.IsolateDocAndCode {
		if runtime.GOOS != "linux" {
			return "", Usage{}, fmt.Errorf("IsolateDocAndCode is only supported on linux (GOOS=%s)", runtime.GOOS)
		}
		if os.Geteuid() != 0 {
			return "", Usage{}, errors.New("IsolateDocAndCode requires running as root (needs mount namespace + bind mounts)")
		}

		quoted := make([]string, 0, len(allArgs))
		for _, a := range allArgs {
			quoted = append(quoted, shellQuote(a))
		}
		codexCmd := shellQuote(codexPath) + " " + strings.Join(quoted, " ")

		// Create an isolated view of WorkDir where only doc/ and code/ exist.
		//
		// We intentionally keep doc/ and code/ writable so git-based version
		// switching (fetch/switch/reset) can work for local analysis.
		// We do this by:
		// 1) bind-mount WorkDir to a temporary $ORIG
		// 2) mount tmpfs over WorkDir (hiding its contents)
		// 3) bind-mount $ORIG/doc and $ORIG/code back into WorkDir
		// 4) remount WorkDir itself read-only to prevent other writes
		//
		// This ensures Codex (for relative paths) cannot see sibling directories
		// like ./feishu-bot, while keeping the doc/code repos usable.
		script := strings.Join([]string{
			"set -euo pipefail",
			"WORKDIR=" + shellQuote(workDir),
			"ORIG=$(mktemp -d)",
			"cleanup() {",
			"  set +e",
			"  umount \"$WORKDIR/doc\" 2>/dev/null || true",
			"  umount \"$WORKDIR/code\" 2>/dev/null || true",
			"  umount \"$WORKDIR\" 2>/dev/null || true",
			"  umount \"$ORIG\" 2>/dev/null || true",
			"  rm -rf \"$ORIG\" 2>/dev/null || true",
			"}",
			"trap cleanup EXIT",
			"mount --bind \"$WORKDIR\" \"$ORIG\"",
			"mount -t tmpfs tmpfs \"$WORKDIR\"",
			"mkdir -p \"$WORKDIR/doc\" \"$WORKDIR/code\"",
			"mount --bind \"$ORIG/doc\" \"$WORKDIR/doc\"",
			"mount --bind \"$ORIG/code\" \"$WORKDIR/code\"",
			"umount \"$ORIG\" || true",
			"rmdir \"$ORIG\" 2>/dev/null || true",
			"mount -o remount,ro \"$WORKDIR\"",
			"cd \"$WORKDIR\"",
			codexCmd,
		}, "\n")

		cmd = exec.CommandContext(ctx, "unshare", "-m", "--propagation", "private", "/bin/bash", "-lc", script)
		// Important: do NOT set cmd.Dir to workDir, since we'll mount over it.
		cmd.Dir = "/"
	} else if runAsUser != "" {
		// Make output file writable by the target user, otherwise Codex may fail
		// to write --output-last-message.
		_ = os.Chmod(outPath, 0o666)

		quoted := make([]string, 0, len(allArgs)+1)
		quoted = append(quoted, shellQuote(codexPath))
		for _, a := range allArgs {
			quoted = append(quoted, shellQuote(a))
		}
		cmdStr := strings.Join(quoted, " ")

		cmd = exec.CommandContext(ctx, "su", "-s", "/bin/bash", "-p", "-c", cmdStr, runAsUser)
		cmd.Dir = workDir
	} else {
		cmd = exec.CommandContext(ctx, codexPath, allArgs...)
		cmd.Dir = workDir
	}
	cmd.Stdin = strings.NewReader(fullPrompt)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", Usage{}, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", Usage{}, fmt.Errorf("stderr pipe: %w", err)
	}

	if homeDir != "" {
		cmd.Env = append(os.Environ(), "HOME="+homeDir)
	}

	if err := cmd.Start(); err != nil {
		return "", Usage{}, fmt.Errorf("start codex exec: %w", err)
	}

	var combined bytes.Buffer
	var combinedMu sync.Mutex

	readStream := func(r io.Reader) <-chan error {
		ch := make(chan error, 1)
		go func() {
			defer close(ch)
			br := bufio.NewReader(r)
			for {
				line, readErr := br.ReadString('\n')
				if line != "" {
					combinedMu.Lock()
					_, _ = combined.WriteString(line)
					combinedMu.Unlock()

					if onProgress != nil {
						text := strings.TrimRight(line, "\r\n")
						if strings.TrimSpace(text) != "" {
							onProgress(text)
						}
					}
				}
				if readErr != nil {
					if errors.Is(readErr, io.EOF) {
						ch <- nil
						return
					}
					ch <- readErr
					return
				}
			}
		}()
		return ch
	}

	stdoutErrCh := readStream(stdout)
	stderrErrCh := readStream(stderr)

	waitErr := cmd.Wait()
	stdoutErr := <-stdoutErrCh
	stderrErr := <-stderrErrCh

	if stdoutErr != nil && onProgress != nil {
		onProgress(fmt.Sprintf("[WARN] failed to read codex stdout: %v", stdoutErr))
	}
	if stderrErr != nil && onProgress != nil {
		onProgress(fmt.Sprintf("[WARN] failed to read codex stderr: %v", stderrErr))
	}

	debugOutput := combined.String()
	usage := extractUsageFromCodexJSONLogs(debugOutput)

	// Prefer the last-message output file (it's designed to contain ONLY the final answer).
	if raw, err := os.ReadFile(outPath); err == nil {
		if text := strings.TrimSpace(string(raw)); text != "" {
			return sanitizeAnswerText(text), usage, nil
		}
	}

	// If Codex exited non-zero but still produced an answer, try to recover it
	// from the combined output (best-effort) to avoid spurious user-facing errors.
	if recovered := extractLastCodexAnswerFromLogs(debugOutput); recovered != "" {
		return sanitizeAnswerText(recovered), usage, nil
	}

	// Surface a safe error message to users, and keep debugOutput for logs.
	userMsg := "Codex execution failed. Please try again later."
	if ctx != nil && ctx.Err() != nil {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			userMsg = "Codex timed out while generating an answer. Please try again later (or increase CODEX_TIMEOUT)."
		case errors.Is(ctx.Err(), context.Canceled):
			userMsg = "Codex request was canceled. Please try again later."
		}
	} else if line := lastUserFacingErrorLine(debugOutput); line != "" {
		userMsg = line
	}
	userMsg = sanitizeExecErrorMessage(userMsg, "")

	cause := waitErr
	if cause == nil {
		cause = errors.New("codex produced no usable output")
	}
	return "", usage, &ExecError{Message: userMsg, DebugOutput: debugOutput, Cause: cause}
}

func hasDir(base, name string) bool {
	st, err := os.Stat(filepath.Join(base, name))
	return err == nil && st.IsDir()
}

func shellQuote(s string) string {
	// Safe quoting for POSIX shells.
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\r\n\"'\\$`") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func extractLastCodexAnswerFromLogs(logs string) string {
	// If Codex was run with --json, stdout is JSONL events. Recover the last
	// completed agent message text first.
	if recovered := extractLastCodexAnswerFromJSONLogs(logs); recovered != "" {
		return recovered
	}

	// Codex CLI logs often contain:
	//   thinking
	//   ...
	//   codex
	//   <final answer...>
	//   tokens used
	//
	// We extract the section after the last standalone "codex" line.
	lines := strings.Split(logs, "\n")
	codexIdx := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "codex" {
			codexIdx = i
			break
		}
	}
	if codexIdx < 0 || codexIdx+1 >= len(lines) {
		return ""
	}
	var out []string
	for j := codexIdx + 1; j < len(lines); j++ {
		t := strings.TrimRight(lines[j], "\r")
		if strings.TrimSpace(t) == "tokens used" {
			break
		}
		out = append(out, t)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func extractUsageFromCodexJSONLogs(logs string) Usage {
	var out Usage

	lines := strings.Split(logs, "\n")
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || !strings.HasPrefix(t, "{") {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Usage *struct {
				InputTokens       int `json:"input_tokens"`
				CachedInputTokens int `json:"cached_input_tokens"`
				OutputTokens      int `json:"output_tokens"`
			} `json:"usage,omitempty"`
		}
		if err := json.Unmarshal([]byte(t), &ev); err != nil {
			continue
		}
		if ev.Type != "turn.completed" || ev.Usage == nil {
			continue
		}
		out.InputTokens = ev.Usage.InputTokens
		out.CachedInputTokens = ev.Usage.CachedInputTokens
		out.OutputTokens = ev.Usage.OutputTokens
	}

	return out
}

func extractLastCodexAnswerFromJSONLogs(logs string) string {
	last := ""

	lines := strings.Split(logs, "\n")
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || !strings.HasPrefix(t, "{") {
			continue
		}
		var ev struct {
			Type string `json:"type"`
			Item *struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item,omitempty"`
		}
		if err := json.Unmarshal([]byte(t), &ev); err != nil {
			continue
		}
		if ev.Type != "item.completed" || ev.Item == nil {
			continue
		}
		if strings.TrimSpace(ev.Item.Type) != "agent_message" {
			continue
		}
		if strings.TrimSpace(ev.Item.Text) == "" {
			continue
		}
		last = ev.Item.Text
	}

	return strings.TrimSpace(last)
}

func lastUserFacingErrorLine(logs string) string {
	// Try to find a concise error line for users without leaking full logs.
	// Prefer explicit "Error:" lines if present.
	lines := strings.Split(logs, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		t := strings.TrimSpace(strings.TrimRight(lines[i], "\r"))
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "Error:") || strings.HasPrefix(t, "ERROR:") || strings.HasPrefix(t, "error:") {
			return t
		}
	}
	return ""
}

func sanitizeExecErrorMessage(msg, replyLang string) string {
	replyLang = strings.ToLower(strings.TrimSpace(replyLang))
	if replyLang == "" {
		replyLang = "en"
	}

	t := strings.TrimSpace(msg)
	if t == "" {
		return "Codex execution failed. Please try again later."
	}

	lower := strings.ToLower(t)
	// If the failure is caused by an auxiliary tool/service, hide internal names.
	// (Do not leak "mcp"/server names to end users.)
	if strings.Contains(lower, "mcp") {
		return "Document retrieval service failed to start. Please check the bot configuration/credentials and try again."
	}

	// Strip generic prefixes.
	for _, p := range []string{"Error:", "ERROR:", "error:"} {
		if strings.HasPrefix(t, p) {
			t = strings.TrimSpace(strings.TrimPrefix(t, p))
			break
		}
	}

	// Reuse the same redaction rules as final answers.
	t = sanitizeAnswerText(t)
	if t == "" {
		return "Codex execution failed. Please try again later."
	}

	return t
}
