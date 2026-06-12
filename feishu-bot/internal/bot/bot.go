package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"example.com/feishu-bot/internal/codex"
	"example.com/feishu-bot/internal/config"
	"example.com/feishu-bot/internal/metrics"
)

type Bot struct {
	cfg   *config.Config
	lark  *lark.Client
	codex codex.Client
	exec  *keyedSerialExecutor
	met   *metrics.Recorder

	mu        sync.Mutex
	processed map[string]time.Time

	histMu        sync.Mutex
	threadHistory map[string][]historyItem
}

func New(cfg *config.Config, larkClient *lark.Client, codexClient codex.Client, metricsRecorder *metrics.Recorder) *Bot {
	return &Bot{
		cfg:       cfg,
		lark:      larkClient,
		codex:     codexClient,
		exec:      newKeyedSerialExecutor(),
		met:       metricsRecorder,
		processed: make(map[string]time.Time),

		threadHistory: make(map[string][]historyItem),
	}
}

func (b *Bot) HandleMessageReceiveV1(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
	// Do not block Feishu callback: process asynchronously.
	msgID := ""
	chatType := ""
	mentions := 0
	botMentioned := false
	msgType := ""
	threadID := ""
	rootID := ""
	parentID := ""
	chatID := ""
	var mentionList []*larkim.MentionEvent

	if event == nil || event.Event == nil || event.Event.Message == nil {
		return nil
	}
	m := event.Event.Message
	if m.MessageId != nil {
		msgID = *m.MessageId
	}
	if msgID == "" {
		return nil
	}

	if b.isDuplicate(msgID, 30*time.Minute) {
		if b.cfg.FeishuDebug {
			log.Printf("[DEBUG] duplicate message ignored: message_id=%s", msgID)
		}
		return nil
	}

	if m.ChatType != nil {
		chatType = *m.ChatType
	}
	if m.MessageType != nil {
		msgType = *m.MessageType
	}
	if m.Mentions != nil {
		mentionList = m.Mentions
		mentions = len(mentionList)
	}
	if m.ThreadId != nil {
		threadID = *m.ThreadId
	}
	if m.RootId != nil {
		rootID = *m.RootId
	}
	if m.ParentId != nil {
		parentID = *m.ParentId
	}
	if m.ChatId != nil {
		chatID = *m.ChatId
	}

	parsed := parseMessageContent(msgType, m.Content)
	parsed.Text = stripMentions(parsed.Text)
	parsed.Text = strings.TrimSpace(parsed.Text)

	// Record message for thread context (even if we won't reply to it).
	b.recordThreadHistory(msgID, chatID, chatType, threadID, rootID, parentID, msgType, parsed)

	// Filter group messages if configured.
	if chatType != "p2p" && b.cfg.FeishuGroupMode == "mention" {
		botMentioned = isBotMentionedByOpenID(mentionList, b.cfg.FeishuBotOpenID)
		if !botMentioned {
			if b.cfg.FeishuDebug {
				log.Printf("[DEBUG] group message ignored (need @bot): message_id=%s chat_type=%s mentions=%d", msgID, chatType, mentions)
			}
			return nil
		}
	}

	switch msgType {
	case "text", "post", "image":
		// handled by parseMessageContent
	default:
		go func() {
			if err := b.replyText(context.Background(), msgID, fmt.Sprintf("Unsupported message type: %s (only text/post/image are supported)", msgType)); err != nil {
				log.Printf("[ERROR] reply unsupported msgType failed: message_id=%s err=%v", msgID, err)
			}
		}()
		return nil
	}

	contextText, contextImages := b.buildThreadContext(chatID, threadID, rootID, msgID, 12, 1)

	question := strings.TrimSpace(parsed.Text)
	if question == "" {
		switch {
		case len(parsed.ImageKeys) > 0 || len(contextImages) > 0:
			question = "Please interpret the image(s) and provide a concise conclusion/recommendation based on the context."
		case strings.TrimSpace(contextText) != "":
			question = "Please provide a concise conclusion/recommendation based on the thread context above."
		default:
			go func() {
				msg := "Please send your question (describe what you need, or attach a screenshot/image)."
				if err := b.replyText(context.Background(), msgID, msg); err != nil {
					log.Printf("[ERROR] reply ask-for-question failed: message_id=%s err=%v", msgID, err)
				}
			}()
			return nil
		}
	}

	imageRefs := make([]imageRef, 0, 2)
	for _, k := range parsed.ImageKeys {
		if strings.TrimSpace(k) == "" {
			continue
		}
		imageRefs = append(imageRefs, imageRef{MessageID: msgID, FileKey: k})
	}
	if len(imageRefs) == 0 && len(contextImages) > 0 {
		imageRefs = append(imageRefs, contextImages...)
	}

	queueKey := processingKey(chatID, threadID, rootID)
	b.exec.Enqueue(queueKey, func() {
		ctx2, cancel := context.WithTimeout(context.Background(), b.cfg.CodexTimeout)
		defer cancel()

		start := time.Now()
		if b.met != nil {
			b.met.IncInFlight()
			defer b.met.DecInFlight()
		}
		if b.cfg.FeishuDebug {
			log.Printf("[DEBUG] codex start: message_id=%s chat_type=%s", msgID, chatType)
		}

		ctxTextFinal := contextText
		imageRefsFinal := append([]imageRef(nil), imageRefs...)

		// Best-effort: fetch root message to enrich context (useful if the bot
		// started after the topic began).
		if strings.TrimSpace(rootID) != "" {
			_, rootParsed, err := b.fetchMessageContent(ctx2, rootID)
			if err != nil && b.cfg.FeishuDebug {
				log.Printf("[DEBUG] fetch root message failed: root_id=%s err=%v", rootID, err)
			}
			if err == nil {
				if strings.TrimSpace(rootParsed.Text) != "" {
					ctxTextFinal = strings.TrimSpace(strings.Join([]string{
						"Root message:",
						rootParsed.Text,
						"",
						ctxTextFinal,
					}, "\n"))
				}
				if len(imageRefsFinal) == 0 && len(rootParsed.ImageKeys) > 0 {
					for _, k := range rootParsed.ImageKeys {
						if strings.TrimSpace(k) == "" {
							continue
						}
						imageRefsFinal = append(imageRefsFinal, imageRef{MessageID: rootID, FileKey: k})
					}
				}
			}
		}

		// Download images (best-effort). The Codex CLI will read them from local paths.
		imagePaths := make([]string, 0, len(imageRefsFinal))
		for _, ref := range imageRefsFinal {
			p, err := b.downloadMessageImage(ctx2, ref.MessageID, ref.FileKey)
			if err != nil {
				log.Printf("[WARN] download image failed: message_id=%s file_key=%s err=%v", ref.MessageID, ref.FileKey, err)
				continue
			}
			imagePaths = append(imagePaths, p)
			defer os.Remove(p)
		}

		// If the user explicitly sent images in the current message but we failed
		// to download any of them, avoid asking Codex to guess.
		if len(parsed.ImageKeys) > 0 && len(imagePaths) == 0 {
			msgText := "Failed to download the image, so I can't read its content. Please check the app permissions for downloading message resources, or resend the image and try again."
			if err := b.replyText(context.Background(), msgID, msgText); err != nil {
				log.Printf("[ERROR] reply image download failed: message_id=%s err=%v", msgID, err)
			}
			return
		}

		req := codex.Request{
			Question:   question,
			Context:    ctxTextFinal,
			ImagePaths: imagePaths,
		}

		// Always record one "request" metric for each Codex attempt (success or failure).
		recordMetrics := func(success bool, usage codex.Usage) {
			if b.met == nil {
				return
			}
			b.met.Add(start, success, usage)
		}

		processingIndicator := "message"
		if b.cfg != nil && strings.TrimSpace(b.cfg.FeishuProcessingIndicator) != "" {
			processingIndicator = strings.ToLower(strings.TrimSpace(b.cfg.FeishuProcessingIndicator))
		}

		// Optional: add an emoji reaction on the user's message to indicate the bot is working.
		processingReactionID := ""
		if processingIndicator == "reaction" {
			reactCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			processingReactionID = b.tryAddProcessingReaction(reactCtx, msgID)
			cancel()
			if processingReactionID != "" {
				defer func(messageID, reactionID string) {
					delCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if err := b.deleteMessageReaction(delCtx, messageID, reactionID); err != nil && b.cfg != nil && b.cfg.FeishuDebug {
						log.Printf("[DEBUG] delete reaction failed: message_id=%s reaction_id=%s err=%v", messageID, reactionID, err)
					}
				}(msgID, processingReactionID)
			}
		}

		// If enabled, first send a placeholder "running" reply, then replace it
		// with the final answer. We intentionally do NOT show intermediate Codex
		// output to users.
		if b.cfg.FeishuProgressUpdates && processingIndicator != "reaction" {
			placeholder := "⏳ Working on it…"
			replyID, err := b.replyTextWithID(context.Background(), msgID, placeholder)
			if err != nil {
				log.Printf("[WARN] send placeholder reply failed, fallback to direct reply: message_id=%s err=%v", msgID, err)
			} else if strings.TrimSpace(replyID) != "" {
				var onProgress func(string)
				if b.cfg.CodexLogOutput {
					onProgress = func(line string) {
						log.Printf("[CODEX] %s", line)
					}
				}

				var answer string
				var usage codex.Usage
				var ansErr error
				if sc, ok := b.codex.(codex.MeteredRequestStreamingClient); ok {
					answer, usage, ansErr = sc.AnswerRequestStreamWithUsage(ctx2, req, onProgress)
				} else if sc, ok := b.codex.(codex.RequestStreamingClient); ok {
					answer, ansErr = sc.AnswerRequestStream(ctx2, req, onProgress)
				} else if rc, ok := b.codex.(codex.MeteredRequestClient); ok {
					answer, usage, ansErr = rc.AnswerRequestWithUsage(ctx2, req)
				} else if rc, ok := b.codex.(codex.RequestClient); ok {
					answer, ansErr = rc.AnswerRequest(ctx2, req)
				} else if sc, ok := b.codex.(codex.StreamingClient); ok {
					// Fallback for older clients: inline context into the question and drop images.
					inline := inlineContext(question, req.Context)
					answer, ansErr = sc.AnswerStream(ctx2, inline, onProgress)
				} else {
					inline := inlineContext(question, req.Context)
					answer, ansErr = b.codex.Answer(ctx2, inline)
				}

				if ansErr != nil {
					recordMetrics(false, usage)
					log.Printf("[ERROR] codex failed: message_id=%s dur=%s err=%v", msgID, time.Since(start), ansErr)
					if b.cfg.CodexLogOutput {
						var ee *codex.ExecError
						if errors.As(ansErr, &ee) && strings.TrimSpace(ee.DebugOutput) != "" {
							log.Printf("[DEBUG] codex exec output (truncated only by logger):\n%s", ee.DebugOutput)
						}
					}
					msgText := fmt.Sprintf("Failed to get an answer: %v", ansErr)
					if err2 := b.updateMessageText(context.Background(), replyID, msgText); err2 != nil {
						log.Printf("[ERROR] update error message failed: reply_id=%s err=%v", replyID, err2)
						if err3 := b.replyText(context.Background(), msgID, msgText); err3 != nil {
							log.Printf("[ERROR] reply codex error failed: message_id=%s err=%v", msgID, err3)
						}
					}
					return
				}

				answer = strings.TrimSpace(answer)
				if answer == "" {
					recordMetrics(false, usage)
					log.Printf("[ERROR] codex returned empty answer: message_id=%s dur=%s", msgID, time.Since(start))
					msgText := "Received an empty answer (possibly blocked or empty). Please rephrase and try again."
					if err2 := b.updateMessageText(context.Background(), replyID, msgText); err2 != nil {
						log.Printf("[ERROR] update empty answer failed: reply_id=%s err=%v", replyID, err2)
						_ = b.replyText(context.Background(), msgID, msgText)
					}
					return
				}
				recordMetrics(true, usage)

				replyFmt := strings.ToLower(strings.TrimSpace(b.cfg.FeishuReplyFormat))
				switch replyFmt {
				case "post":
					// Post (rich text) renders explicit link elements, so we should NOT
					// inject extra whitespace around URLs. But we still want to:
					// - rewrite local doc/code refs to public URLs
					// - unwrap any <https://...> form that could be swallowed as HTML
					// - convert markdown links [text](url) into plain text
					answer = rewriteAnswerPublicLinks(answer, b.cfg.CodexWorkDir, "en", false)
					answer = unwrapAngleBracketAutolinks(answer)
					answer = rewriteMarkdownLinksToAutolinks(answer)
				case "markdown":
					answer = rewriteAnswerPublicLinks(answer, b.cfg.CodexWorkDir, "en", true)
				default:
					answer = rewriteAnswerPublicLinks(answer, b.cfg.CodexWorkDir, "en", false)
				}

				if b.cfg.FeishuDebug {
					log.Printf("[DEBUG] codex ok: message_id=%s dur=%s", msgID, time.Since(start))
				}
				if err2 := b.updateAndReplyAllParts(context.Background(), msgID, replyID, answer); err2 != nil {
					log.Printf("[ERROR] send answer failed: message_id=%s reply_id=%s err=%v", msgID, replyID, err2)
				}
				return
			}
		}

		// Fallback: no placeholder (or placeholder failed), direct reply when done.
		var answer string
		var usage codex.Usage
		var err error
		if rc, ok := b.codex.(codex.MeteredRequestClient); ok {
			answer, usage, err = rc.AnswerRequestWithUsage(ctx2, req)
		} else if rc, ok := b.codex.(codex.RequestClient); ok {
			answer, err = rc.AnswerRequest(ctx2, req)
		} else {
			answer, err = b.codex.Answer(ctx2, inlineContext(question, req.Context))
		}
		if err != nil {
			recordMetrics(false, usage)
			log.Printf("[ERROR] codex failed: message_id=%s dur=%s err=%v", msgID, time.Since(start), err)
			if b.cfg.CodexLogOutput {
				var ee *codex.ExecError
				if errors.As(err, &ee) && strings.TrimSpace(ee.DebugOutput) != "" {
					log.Printf("[DEBUG] codex exec output (truncated only by logger):\n%s", ee.DebugOutput)
				}
			}
			msgText := fmt.Sprintf("Failed to get an answer: %v", err)
			if err2 := b.replyText(context.Background(), msgID, msgText); err2 != nil {
				log.Printf("[ERROR] reply codex error failed: message_id=%s err=%v", msgID, err2)
			}
			return
		}

		answer = strings.TrimSpace(answer)
		if answer == "" {
			recordMetrics(false, usage)
			log.Printf("[ERROR] codex returned empty answer: message_id=%s dur=%s", msgID, time.Since(start))
			msgText := "Received an empty answer (possibly blocked or empty). Please rephrase and try again."
			if err2 := b.replyText(context.Background(), msgID, msgText); err2 != nil {
				log.Printf("[ERROR] reply empty answer failed: message_id=%s err=%v", msgID, err2)
			}
			return
		}

		recordMetrics(true, usage)

		replyFmt := strings.ToLower(strings.TrimSpace(b.cfg.FeishuReplyFormat))
		switch replyFmt {
		case "post":
			answer = rewriteAnswerPublicLinks(answer, b.cfg.CodexWorkDir, "en", false)
			answer = unwrapAngleBracketAutolinks(answer)
			answer = rewriteMarkdownLinksToAutolinks(answer)
		case "markdown":
			answer = rewriteAnswerPublicLinks(answer, b.cfg.CodexWorkDir, "en", true)
		default:
			answer = rewriteAnswerPublicLinks(answer, b.cfg.CodexWorkDir, "en", false)
		}

		if b.cfg.FeishuDebug {
			log.Printf("[DEBUG] codex ok: message_id=%s dur=%s", msgID, time.Since(start))
		}
		if err2 := b.replyAllParts(context.Background(), msgID, answer); err2 != nil {
			log.Printf("[ERROR] reply failed: message_id=%s err=%v", msgID, err2)
		}
	})

	return nil
}

func (b *Bot) updateAndReplyAllParts(ctx context.Context, originalMessageID, replyID, fullText string) error {
	parts := b.splitForFeishu(fullText)
	if len(parts) == 0 {
		return errors.New("empty answer after split")
	}
	if len(parts) == 1 {
		if err := b.updateMessageText(ctx, replyID, parts[0]); err != nil {
			// Fallback: reply as a new message.
			if err2 := b.replyText(ctx, originalMessageID, parts[0]); err2 != nil {
				return fmt.Errorf("update failed: %w; fallback reply failed: %w", err, err2)
			}
		}
		return nil
	}

	// Update the placeholder reply with part 1, then send the remaining parts as
	// additional replies anchored to the original user message.
	if err := b.updateMessageText(ctx, replyID, parts[0]); err != nil {
		// If update fails, fall back to replying all parts as new messages.
		if err2 := b.replyAllParts(ctx, originalMessageID, fullText); err2 != nil {
			return fmt.Errorf("update part1 failed: %w; fallback multipart reply failed: %w", err, err2)
		}
		return nil
	}
	for i := 1; i < len(parts); i++ {
		if err := b.replyText(ctx, originalMessageID, parts[i]); err != nil {
			return fmt.Errorf("reply part %d/%d failed: %w", i+1, len(parts), err)
		}
	}
	return nil
}

func (b *Bot) replyAllParts(ctx context.Context, originalMessageID, fullText string) error {
	parts := b.splitForFeishu(fullText)
	if len(parts) == 0 {
		return errors.New("empty answer after split")
	}
	for i, p := range parts {
		if err := b.replyText(ctx, originalMessageID, p); err != nil {
			return fmt.Errorf("reply part %d/%d failed: %w", i+1, len(parts), err)
		}
	}
	return nil
}

func (b *Bot) splitForFeishu(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	maxBytes := maxReplyContentBytes(b)
	render := func(s string) (string, string) { return b.renderReply(s) }

	// First, split without headers.
	raw := splitTextByRenderedContentBytes(text, maxBytes, render)
	if len(raw) <= 1 {
		if len(raw) == 1 && strings.ToLower(strings.TrimSpace(b.cfg.FeishuReplyFormat)) == "markdown" {
			raw[0] = closeUnbalancedCodeFence(raw[0])
		}
		return raw
	}

	// Add part headers, and if headers make a chunk exceed the size limit, split
	// that chunk further with a header-aware renderer, then retry.
	for iter := 0; iter < 8; iter++ {
		n := len(raw)
		allFit := true
		for i := 0; i < n; i++ {
			hdr := fmt.Sprintf("Part %d of %d\n\n", i+1, n)
			_, content := render(hdr + raw[i])
			if len(content) <= maxBytes {
				continue
			}

			allFit = false
			// Split this raw chunk into smaller pieces, taking the header into
			// account when checking rendered size.
			sub := splitTextByRenderedContentBytes(raw[i], maxBytes, func(body string) (string, string) {
				return render(hdr + body)
			})
			if len(sub) == 0 {
				// Give up on this chunk; we'll fall back to returning raw without headers.
				return raw
			}
			raw = append(raw[:i], append(sub, raw[i+1:]...)...)
			break
		}
		if !allFit {
			continue
		}

		out := make([]string, 0, n)
		for i := 0; i < n; i++ {
			hdr := fmt.Sprintf("Part %d of %d\n\n", i+1, n)
			part := hdr + raw[i]
			if strings.ToLower(strings.TrimSpace(b.cfg.FeishuReplyFormat)) == "markdown" {
				part = closeUnbalancedCodeFence(part)
			}
			out = append(out, part)
		}
		return out
	}

	// Fallback: too many iterations (should not happen). Return raw chunks.
	for i := range raw {
		if strings.ToLower(strings.TrimSpace(b.cfg.FeishuReplyFormat)) == "markdown" {
			raw[i] = closeUnbalancedCodeFence(raw[i])
		}
	}
	return raw
}

func maxReplyContentBytes(b *Bot) int {
	// Feishu OpenAPI request body size limits:
	// - text message: ~150KB
	// - post / interactive: ~30KB
	//
	// We keep headroom because the request body contains other fields besides
	// `content`, and because JSON escaping can inflate the byte size.
	replyFmt := ""
	if b != nil && b.cfg != nil {
		replyFmt = strings.ToLower(strings.TrimSpace(b.cfg.FeishuReplyFormat))
	}
	switch replyFmt {
	case "text":
		return 100 * 1024
	case "post", "markdown":
		return 25 * 1024
	default:
		return 25 * 1024
	}
}

func splitTextByRenderedContentBytes(text string, maxBytes int, render func(string) (msgType string, content string)) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if maxBytes <= 0 {
		return []string{text}
	}

	// Fast path: whole text fits.
	_, content := render(text)
	if len(content) <= maxBytes {
		return []string{text}
	}

	lines := strings.Split(text, "\n")
	var chunks []string
	var cur []string

	flushCur := func() {
		if len(cur) == 0 {
			return
		}
		s := strings.TrimSpace(strings.Join(cur, "\n"))
		if s != "" {
			chunks = append(chunks, s)
		}
		cur = nil
	}

	for _, line := range lines {
		// Always preserve blank lines as structure, but avoid creating empty chunks.
		next := append(cur, line)
		cand := strings.Join(next, "\n")
		_, candContent := render(strings.TrimSpace(cand))
		if len(candContent) <= maxBytes {
			cur = next
			continue
		}

		// Candidate doesn't fit. Flush current chunk first.
		if len(cur) > 0 {
			flushCur()
		}

		// Now handle this line alone; it may still be too big.
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		for trimmed != "" {
			part, rest := splitLongStringToFit(trimmed, maxBytes, render)
			part = strings.TrimSpace(part)
			if part != "" {
				chunks = append(chunks, part)
			}
			trimmed = strings.TrimSpace(rest)
		}
	}

	flushCur()

	// Last-resort: never return empty.
	if len(chunks) == 0 {
		return []string{truncateRunes(text, 800)}
	}
	return chunks
}

func splitLongStringToFit(s string, maxBytes int, render func(string) (msgType string, content string)) (part string, rest string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}

	// If it already fits, don't split.
	_, content := render(s)
	if len(content) <= maxBytes {
		return s, ""
	}

	r := []rune(s)
	lo := 1
	hi := len(r)
	best := 1

	for lo <= hi {
		mid := (lo + hi) / 2
		cand := string(r[:mid])
		_, candContent := render(cand)
		if len(candContent) <= maxBytes {
			best = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}

	part = string(r[:best])
	rest = string(r[best:])
	return part, rest
}

func (b *Bot) replyText(ctx context.Context, messageID, text string) error {
	_, err := b.replyTextWithID(ctx, messageID, text)
	return err
}

func (b *Bot) tryAddProcessingReaction(ctx context.Context, messageID string) (reactionID string) {
	if b == nil || b.cfg == nil || b.lark == nil {
		return ""
	}
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return ""
	}

	// Best-effort: try a small set of emoji types so users can configure a
	// preferred one, while still having a safe fallback.
	candidates := []string{
		strings.TrimSpace(b.cfg.FeishuProcessingReactionEmojiType),
		"THINKING",
		"DONE",
	}
	seen := make(map[string]struct{}, len(candidates))
	for _, emojiType := range candidates {
		emojiType = strings.TrimSpace(emojiType)
		if emojiType == "" {
			continue
		}
		if _, ok := seen[emojiType]; ok {
			continue
		}
		seen[emojiType] = struct{}{}

		id, err := b.addMessageReaction(ctx, messageID, emojiType)
		if err == nil && strings.TrimSpace(id) != "" {
			return strings.TrimSpace(id)
		}
		if b.cfg.FeishuDebug && err != nil {
			log.Printf("[DEBUG] add reaction failed: message_id=%s emoji_type=%s err=%v", messageID, emojiType, err)
		}
	}
	return ""
}

func (b *Bot) addMessageReaction(ctx context.Context, messageID, emojiType string) (reactionID string, err error) {
	if b == nil || b.lark == nil {
		return "", errors.New("nil bot/lark client")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	messageID = strings.TrimSpace(messageID)
	emojiType = strings.TrimSpace(emojiType)
	if messageID == "" {
		return "", errors.New("empty message_id")
	}
	if emojiType == "" {
		return "", errors.New("empty emoji_type")
	}

	emoji := larkim.NewEmojiBuilder().EmojiType(emojiType).Build()
	body := larkim.NewCreateMessageReactionReqBodyBuilder().ReactionType(emoji).Build()
	req := larkim.NewCreateMessageReactionReqBuilder().MessageId(messageID).Body(body).Build()

	resp, err := b.lark.Im.V1.MessageReaction.Create(ctx, req)
	if err != nil {
		return "", err
	}
	if resp == nil || !resp.Success() {
		code := 0
		msg := ""
		if resp != nil {
			code = resp.Code
			msg = resp.Msg
		}
		return "", fmt.Errorf("feishu create reaction failed: code=%d msg=%s", code, msg)
	}
	if resp.Data != nil && resp.Data.ReactionId != nil {
		reactionID = strings.TrimSpace(*resp.Data.ReactionId)
	}
	if reactionID == "" {
		return "", errors.New("feishu create reaction returned empty reaction_id")
	}
	return reactionID, nil
}

func (b *Bot) deleteMessageReaction(ctx context.Context, messageID, reactionID string) error {
	if b == nil || b.lark == nil {
		return errors.New("nil bot/lark client")
	}
	if strings.TrimSpace(reactionID) == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	messageID = strings.TrimSpace(messageID)
	reactionID = strings.TrimSpace(reactionID)
	if messageID == "" {
		return errors.New("empty message_id")
	}

	req := larkim.NewDeleteMessageReactionReqBuilder().
		MessageId(messageID).
		ReactionId(reactionID).
		Build()
	resp, err := b.lark.Im.V1.MessageReaction.Delete(ctx, req)
	if err != nil {
		return err
	}
	if resp == nil || !resp.Success() {
		code := 0
		msg := ""
		if resp != nil {
			code = resp.Code
			msg = resp.Msg
		}
		return fmt.Errorf("feishu delete reaction failed: code=%d msg=%s", code, msg)
	}
	return nil
}

func (b *Bot) replyTextWithID(ctx context.Context, messageID, text string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	msgType, content := b.renderReply(text)
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			MsgType(msgType).
			Content(content).
			Build()).
		Build()

	resp, err := b.lark.Im.V1.Message.Reply(ctx, req)
	if err != nil {
		// Fallback: if markdown card failed, try plain text so users still get an answer.
		if msgType != "text" {
			replyID, fbErr := b.replyTextWithIDFallbackPlainText(ctx, messageID, text)
			if fbErr == nil {
				return replyID, nil
			}
		}
		return "", err
	}
	if resp == nil || !resp.Success() {
		code := 0
		msg := ""
		if resp != nil {
			code = resp.Code
			msg = resp.Msg
		}
		// Fallback: if markdown card failed, try plain text so users still get an answer.
		if msgType != "text" {
			replyID, fbErr := b.replyTextWithIDFallbackPlainText(ctx, messageID, text)
			if fbErr == nil {
				return replyID, nil
			}
		}
		return "", fmt.Errorf("feishu reply failed: code=%d msg=%s", code, msg)
	}
	replyID := ""
	if resp.Data != nil && resp.Data.MessageId != nil {
		replyID = *resp.Data.MessageId
	}
	return replyID, nil
}

func (b *Bot) updateMessageText(ctx context.Context, messageID, text string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if b.cfg != nil && strings.ToLower(strings.TrimSpace(b.cfg.FeishuReplyFormat)) == "markdown" {
		// For card messages, use PATCH (Update only supports text/post).
		_, content := renderInteractiveCardMarkdown(text)
		req := larkim.NewPatchMessageReqBuilder().
			MessageId(messageID).
			Body(larkim.NewPatchMessageReqBodyBuilder().
				Content(content).
				Build()).
			Build()
		resp, err := b.lark.Im.V1.Message.Patch(ctx, req)
		if err != nil {
			// If the original placeholder was a plain text fallback, PATCH will fail.
			// Try text update as a fallback.
			if fbErr := b.updateMessageTextFallbackPlainText(ctx, messageID, text); fbErr == nil {
				return nil
			}
			return err
		}
		if resp == nil || !resp.Success() {
			code := 0
			msg := ""
			if resp != nil {
				code = resp.Code
				msg = resp.Msg
			}
			if fbErr := b.updateMessageTextFallbackPlainText(ctx, messageID, text); fbErr == nil {
				return nil
			}
			return fmt.Errorf("feishu patch failed: code=%d msg=%s", code, msg)
		}
		return nil
	}

	if b.cfg != nil && strings.ToLower(strings.TrimSpace(b.cfg.FeishuReplyFormat)) == "post" {
		// Rich text mode: use Update (supports text/post).
		_, content := renderPostRichText(text)
		req := larkim.NewUpdateMessageReqBuilder().
			MessageId(messageID).
			Body(larkim.NewUpdateMessageReqBodyBuilder().
				MsgType("post").
				Content(content).
				Build()).
			Build()
		resp, err := b.lark.Im.V1.Message.Update(ctx, req)
		if err != nil {
			// If the original placeholder was a plain text fallback, updating as
			// "post" may fail; fall back to updating plain text.
			if fbErr := b.updateMessageTextFallbackPlainText(ctx, messageID, text); fbErr == nil {
				return nil
			}
			return err
		}
		if resp == nil || !resp.Success() {
			code := 0
			msg := ""
			if resp != nil {
				code = resp.Code
				msg = resp.Msg
			}
			if fbErr := b.updateMessageTextFallbackPlainText(ctx, messageID, text); fbErr == nil {
				return nil
			}
			return fmt.Errorf("feishu update failed: code=%d msg=%s", code, msg)
		}
		return nil
	}

	// Text mode: use Update (supports text/post).
	contentBytes, _ := json.Marshal(map[string]string{"text": text})
	content := string(contentBytes)
	req := larkim.NewUpdateMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewUpdateMessageReqBodyBuilder().
			MsgType("text").
			Content(content).
			Build()).
		Build()

	resp, err := b.lark.Im.V1.Message.Update(ctx, req)
	if err != nil {
		// If this fails because the message is actually an interactive card,
		// fall back to PATCH with a markdown card payload.
		if fbErr := b.updateMessageTextFallbackPatchCard(ctx, messageID, text); fbErr == nil {
			return nil
		}
		return err
	}
	if resp == nil || !resp.Success() {
		code := 0
		msg := ""
		if resp != nil {
			code = resp.Code
			msg = resp.Msg
		}
		// Feishu returns code=230001 with ext=invalid msg_type when trying to
		// update an interactive-card message via Update(). In that case, retry
		// using PATCH with a card payload so progress updates still work.
		if code == 230001 && strings.Contains(strings.ToLower(msg), "invalid msg_type") {
			if fbErr := b.updateMessageTextFallbackPatchCard(ctx, messageID, text); fbErr == nil {
				return nil
			}
		}
		return fmt.Errorf("feishu update failed: code=%d msg=%s", code, msg)
	}
	return nil
}

func (b *Bot) replyTextWithIDFallbackPlainText(ctx context.Context, messageID, text string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	contentBytes, _ := json.Marshal(map[string]string{"text": text})
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			MsgType("text").
			Content(string(contentBytes)).
			Build()).
		Build()
	resp, err := b.lark.Im.V1.Message.Reply(ctx, req)
	if err != nil {
		return "", err
	}
	if resp == nil || !resp.Success() {
		code := 0
		msg := ""
		if resp != nil {
			code = resp.Code
			msg = resp.Msg
		}
		return "", fmt.Errorf("feishu reply failed: code=%d msg=%s", code, msg)
	}
	replyID := ""
	if resp.Data != nil && resp.Data.MessageId != nil {
		replyID = *resp.Data.MessageId
	}
	return replyID, nil
}

func (b *Bot) updateMessageTextFallbackPlainText(ctx context.Context, messageID, text string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	contentBytes, _ := json.Marshal(map[string]string{"text": text})
	req := larkim.NewUpdateMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewUpdateMessageReqBodyBuilder().
			MsgType("text").
			Content(string(contentBytes)).
			Build()).
		Build()
	resp, err := b.lark.Im.V1.Message.Update(ctx, req)
	if err != nil {
		return err
	}
	if resp == nil || !resp.Success() {
		code := 0
		msg := ""
		if resp != nil {
			code = resp.Code
			msg = resp.Msg
		}
		return fmt.Errorf("feishu update failed: code=%d msg=%s", code, msg)
	}
	return nil
}

func (b *Bot) renderReply(text string) (msgType string, content string) {
	if b == nil || b.cfg == nil {
		// Safe default.
		return renderPlainText(text)
	}

	switch strings.ToLower(strings.TrimSpace(b.cfg.FeishuReplyFormat)) {
	case "", "text":
		return renderPlainText(text)
	case "post":
		return renderPostRichText(text)
	case "markdown":
		return renderInteractiveCardMarkdown(text)
	default:
		return renderPlainText(text)
	}
}

func renderPlainText(text string) (msgType string, content string) {
	raw, _ := json.Marshal(map[string]string{"text": text})
	return "text", string(raw)
}

func renderInteractiveCardMarkdown(text string) (msgType string, content string) {
	// Render markdown via an interactive card.
	card := map[string]any{
		"config": map[string]any{
			"wide_screen_mode": true,
			// Required for shared card updates (PATCH) in group chats.
			"update_multi": true,
		},
		"elements": []any{
			map[string]any{
				"tag": "div",
				"text": map[string]any{
					"tag":     "lark_md",
					"content": text,
				},
			},
		},
	}
	raw, _ := json.Marshal(card)
	return "interactive", string(raw)
}

func (b *Bot) updateMessageTextFallbackPatchCard(ctx context.Context, messageID, text string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	_, content := renderInteractiveCardMarkdown(text)
	req := larkim.NewPatchMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewPatchMessageReqBodyBuilder().
			Content(content).
			Build()).
		Build()
	resp, err := b.lark.Im.V1.Message.Patch(ctx, req)
	if err != nil {
		return err
	}
	if resp == nil || !resp.Success() {
		code := 0
		msg := ""
		if resp != nil {
			code = resp.Code
			msg = resp.Msg
		}
		return fmt.Errorf("feishu patch failed: code=%d msg=%s", code, msg)
	}
	return nil
}

func parseTextContent(contentJSON *string) string {
	if contentJSON == nil || strings.TrimSpace(*contentJSON) == "" {
		return ""
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(*contentJSON), &payload); err != nil {
		// Some clients may send different JSON; fall back to raw string.
		return *contentJSON
	}
	return payload.Text
}

type parsedContent struct {
	Text      string
	ImageKeys []string
}

func parseMessageContent(msgType string, contentJSON *string) parsedContent {
	switch msgType {
	case "text":
		return parsedContent{Text: parseTextContent(contentJSON)}
	case "post":
		return parsePostContentAndImages(contentJSON)
	case "image":
		return parsedContent{ImageKeys: parseImageKeys(contentJSON)}
	default:
		return parsedContent{}
	}
}

type postElement struct {
	Tag      string `json:"tag"`
	Text     string `json:"text"`
	UserID   string `json:"user_id"`
	UserName string `json:"user_name"`
	Href     string `json:"href"`
	ImageKey string `json:"image_key"`
}

type postPayload struct {
	Title   string          `json:"title"`
	Content [][]postElement `json:"content"`
}

func parsePostContent(contentJSON *string) string {
	pc := parsePostContentAndImages(contentJSON)
	return pc.Text
}

func parsePostContentAndImages(contentJSON *string) parsedContent {
	if contentJSON == nil || strings.TrimSpace(*contentJSON) == "" {
		return parsedContent{}
	}

	raw := strings.TrimSpace(*contentJSON)

	// Common formats:
	// 1) {"title":"...","content":[[{"tag":"text","text":"..."}]]}
	// 2) {"zh_cn":{...}} (multi-lang)
	// 3) {"post":{"zh_cn":{...}}} (some send/receive variants)
	var direct postPayload
	if err := json.Unmarshal([]byte(raw), &direct); err == nil && (direct.Title != "" || len(direct.Content) > 0) {
		text, images := renderPostToTextAndImages(direct)
		return parsedContent{Text: text, ImageKeys: images}
	}

	var locales map[string]postPayload
	if err := json.Unmarshal([]byte(raw), &locales); err == nil && len(locales) > 0 {
		if zh, ok := locales["zh_cn"]; ok && (zh.Title != "" || len(zh.Content) > 0) {
			text, images := renderPostToTextAndImages(zh)
			return parsedContent{Text: text, ImageKeys: images}
		}
		for _, p := range locales {
			if p.Title != "" || len(p.Content) > 0 {
				text, images := renderPostToTextAndImages(p)
				return parsedContent{Text: text, ImageKeys: images}
			}
		}
	}

	var wrapper struct {
		Post map[string]postPayload `json:"post"`
	}
	if err := json.Unmarshal([]byte(raw), &wrapper); err == nil && len(wrapper.Post) > 0 {
		if zh, ok := wrapper.Post["zh_cn"]; ok && (zh.Title != "" || len(zh.Content) > 0) {
			text, images := renderPostToTextAndImages(zh)
			return parsedContent{Text: text, ImageKeys: images}
		}
		for _, p := range wrapper.Post {
			if p.Title != "" || len(p.Content) > 0 {
				text, images := renderPostToTextAndImages(p)
				return parsedContent{Text: text, ImageKeys: images}
			}
		}
	}

	// Last resort: return raw JSON (still better than empty).
	return parsedContent{Text: raw}
}

func renderPostToTextAndImages(p postPayload) (string, []string) {
	title := strings.TrimSpace(p.Title)
	var lines []string
	var imageKeys []string

	for _, row := range p.Content {
		var sb strings.Builder
		for _, el := range row {
			switch el.Tag {
			case "text", "a":
				sb.WriteString(el.Text)
			case "at":
				// Treat mentions as noise for question extraction.
				continue
			case "img", "image":
				if strings.TrimSpace(el.ImageKey) != "" {
					imageKeys = append(imageKeys, strings.TrimSpace(el.ImageKey))
				}
				// Keep a minimal marker in the extracted text.
				sb.WriteString("[image]")
			default:
				// Best-effort: some tags still carry readable text.
				if el.Text != "" {
					sb.WriteString(el.Text)
				}
			}
		}
		line := strings.TrimSpace(sb.String())
		if line != "" {
			lines = append(lines, line)
		}
	}

	body := strings.TrimSpace(strings.Join(lines, "\n"))
	if title != "" && body != "" {
		return title + "\n" + body, imageKeys
	}
	if body != "" {
		return body, imageKeys
	}
	return title, imageKeys
}

func parseImageKeys(contentJSON *string) []string {
	if contentJSON == nil || strings.TrimSpace(*contentJSON) == "" {
		return nil
	}
	var payload struct {
		ImageKey string `json:"image_key"`
	}
	if err := json.Unmarshal([]byte(*contentJSON), &payload); err != nil {
		return nil
	}
	if strings.TrimSpace(payload.ImageKey) == "" {
		return nil
	}
	return []string{strings.TrimSpace(payload.ImageKey)}
}

var atTagRe = regexp.MustCompile(`<at[^>]*>.*?</at>`)

func stripMentions(s string) string {
	// In Feishu text messages, mentions are often represented as <at ...>...</at>.
	s = atTagRe.ReplaceAllString(s, "")
	// Remove common punctuation left after removing mentions.
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, ":")
	s = strings.TrimSpace(s)
	return s
}

func isBotMentionedByOpenID(mentions []*larkim.MentionEvent, botOpenID string) bool {
	botOpenID = strings.TrimSpace(botOpenID)
	if botOpenID == "" {
		return false
	}
	for _, m := range mentions {
		if m == nil || m.Id == nil || m.Id.OpenId == nil {
			continue
		}
		if strings.TrimSpace(*m.Id.OpenId) == botOpenID {
			return true
		}
	}
	return false
}

func (b *Bot) isDuplicate(messageID string, ttl time.Duration) bool {
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()

	// Opportunistic cleanup.
	for id, ts := range b.processed {
		if now.Sub(ts) > ttl {
			delete(b.processed, id)
		}
	}

	if ts, ok := b.processed[messageID]; ok {
		if now.Sub(ts) <= ttl {
			return true
		}
	}
	b.processed[messageID] = now
	return false
}

func runeLen(s string) int {
	return len([]rune(s))
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

func closeUnbalancedCodeFence(markdown string) string {
	// If we truncated in the middle of a fenced code block, Markdown rendering in
	// Feishu (lark_md) can break badly. Close the last fence best-effort.
	if strings.Count(markdown, "```")%2 == 1 {
		return strings.TrimRight(markdown, "\n") + "\n```"
	}
	return markdown
}

type historyItem struct {
	MessageID string
	ChatID    string
	ChatType  string

	ThreadID string
	RootID   string
	ParentID string

	MsgType    string
	Text       string
	ImageKeys  []string
	ReceivedAt time.Time
}

type imageRef struct {
	MessageID string
	FileKey   string
}

func inlineContext(question, contextText string) string {
	q := strings.TrimSpace(question)
	ctxText := strings.TrimSpace(contextText)
	if ctxText == "" {
		return q
	}
	if q == "" {
		q = "Please provide a concise conclusion/recommendation based on the context above."
	}
	return strings.Join([]string{
		"Thread context:",
		ctxText,
		"",
		"Question:",
		q,
	}, "\n")
}

func threadHistoryKey(chatID, threadID, rootID string) string {
	cid := strings.TrimSpace(chatID)
	if cid == "" {
		cid = "chat:unknown"
	} else {
		cid = "chat:" + cid
	}

	tid := strings.TrimSpace(threadID)
	if tid != "" {
		return cid + "/thread:" + tid
	}
	rid := strings.TrimSpace(rootID)
	if rid != "" {
		return cid + "/root:" + rid
	}
	return ""
}

func (b *Bot) recordThreadHistory(messageID, chatID, chatType, threadID, rootID, parentID, msgType string, parsed parsedContent) {
	key := threadHistoryKey(chatID, threadID, rootID)
	if key == "" {
		return
	}

	b.histMu.Lock()
	defer b.histMu.Unlock()

	const maxPerThread = 80

	items := b.threadHistory[key]
	items = append(items, historyItem{
		MessageID:  messageID,
		ChatID:     chatID,
		ChatType:   chatType,
		ThreadID:   threadID,
		RootID:     rootID,
		ParentID:   parentID,
		MsgType:    msgType,
		Text:       parsed.Text,
		ImageKeys:  append([]string(nil), parsed.ImageKeys...),
		ReceivedAt: time.Now(),
	})
	if len(items) > maxPerThread {
		items = append([]historyItem(nil), items[len(items)-maxPerThread:]...)
	}
	b.threadHistory[key] = items
}

func (b *Bot) buildThreadContext(chatID, threadID, rootID, currentMessageID string, maxMessages, maxImages int) (string, []imageRef) {
	key := threadHistoryKey(chatID, threadID, rootID)
	if key == "" {
		return "", nil
	}
	if maxMessages <= 0 {
		maxMessages = 12
	}
	if maxImages < 0 {
		maxImages = 0
	}

	b.histMu.Lock()
	items := append([]historyItem(nil), b.threadHistory[key]...)
	b.histMu.Unlock()

	// Select last N messages (excluding current).
	selected := make([]historyItem, 0, maxMessages)
	for i := len(items) - 1; i >= 0 && len(selected) < maxMessages; i-- {
		it := items[i]
		if it.MessageID == currentMessageID {
			continue
		}
		if strings.TrimSpace(it.Text) == "" && len(it.ImageKeys) == 0 {
			continue
		}
		selected = append(selected, it)
	}
	// reverse to chronological
	for i, j := 0, len(selected)-1; i < j; i, j = i+1, j-1 {
		selected[i], selected[j] = selected[j], selected[i]
	}

	// Pick recent images from the thread (excluding current).
	var images []imageRef
	if maxImages > 0 {
		for i := len(items) - 1; i >= 0 && len(images) < maxImages; i-- {
			it := items[i]
			if it.MessageID == currentMessageID {
				continue
			}
			for _, k := range it.ImageKeys {
				if len(images) >= maxImages {
					break
				}
				k = strings.TrimSpace(k)
				if k == "" {
					continue
				}
				images = append(images, imageRef{MessageID: it.MessageID, FileKey: k})
			}
		}
	}

	// Render context text.
	lines := make([]string, 0, len(selected))
	for _, it := range selected {
		if strings.TrimSpace(it.Text) != "" {
			const maxLineRunes = 300
			line := it.Text
			if runeLen(line) > maxLineRunes {
				line = truncateRunes(line, maxLineRunes-1) + "…"
			}
			lines = append(lines, line)
			continue
		}
		if len(it.ImageKeys) > 0 {
			lines = append(lines, "[image]")
		}
	}

	return strings.TrimSpace(strings.Join(lines, "\n")), images
}

// processingKey controls concurrency:
// - same chat+thread/root runs sequentially
// - different threads run concurrently
// - non-thread messages run sequentially per chat
func processingKey(chatID, threadID, rootID string) string {
	tid := strings.TrimSpace(threadID)
	rid := strings.TrimSpace(rootID)
	if tid != "" || rid != "" {
		return threadHistoryKey(chatID, threadID, rootID)
	}

	cid := strings.TrimSpace(chatID)
	if cid != "" {
		return "chat:" + cid
	}
	return ""
}

func (b *Bot) fetchMessageContent(ctx context.Context, messageID string) (string, parsedContent, error) {
	if strings.TrimSpace(messageID) == "" {
		return "", parsedContent{}, errors.New("empty message_id")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	resp, err := b.lark.Im.V1.Message.Get(ctx, larkim.NewGetMessageReqBuilder().MessageId(messageID).Build())
	if err != nil {
		return "", parsedContent{}, err
	}
	if resp == nil || !resp.Success() {
		code := 0
		msg := ""
		if resp != nil {
			code = resp.Code
			msg = resp.Msg
		}
		return "", parsedContent{}, fmt.Errorf("feishu get message failed: code=%d msg=%s", code, msg)
	}
	if resp.Data == nil || len(resp.Data.Items) == 0 || resp.Data.Items[0] == nil {
		return "", parsedContent{}, errors.New("feishu get message returned empty items")
	}
	item := resp.Data.Items[0]
	mType := ""
	if item.MsgType != nil {
		mType = *item.MsgType
	}
	var content *string
	if item.Body != nil {
		content = item.Body.Content
	}
	parsed := parseMessageContent(mType, content)
	parsed.Text = stripMentions(parsed.Text)
	parsed.Text = strings.TrimSpace(parsed.Text)
	return mType, parsed, nil
}

func (b *Bot) downloadMessageImage(ctx context.Context, messageID, fileKey string) (string, error) {
	if strings.TrimSpace(messageID) == "" || strings.TrimSpace(fileKey) == "" {
		return "", errors.New("message_id and file_key must not be empty")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req := larkim.NewGetMessageResourceReqBuilder().
		MessageId(messageID).
		FileKey(fileKey).
		Type("image").
		Build()

	resp, err := b.lark.Im.V1.MessageResource.Get(ctx, req)
	if err != nil {
		return "", err
	}
	if resp == nil || !resp.Success() {
		code := 0
		msg := ""
		if resp != nil {
			code = resp.Code
			msg = resp.Msg
		}
		return "", fmt.Errorf("feishu get message resource failed: code=%d msg=%s", code, msg)
	}

	ext := filepath.Ext(resp.FileName)
	if ext == "" {
		ext = ".png"
	}
	f, err := os.CreateTemp("", "feishu-image-*"+ext)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = f.Close()
	}()

	if _, err := io.Copy(f, resp.File); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func inferReplyLanguage(primaryText, fallbackText string) string {
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

var langOverrideKVRe = regexp.MustCompile(`(?i)^(?:lang|language|reply)\s*[:=]\s*([^\s:：,，]+)\s*(.*)$`)

// extractLanguageOverride detects a user-specified language prefix and strips it
// from the message text.
//
// Supported examples (case-insensitive for latin tokens):
// - "en ..." / "en: ..." / "/en ..."
// - "zh ..." / "zh-cn: ..." / "/zh ..."
// - "英文 ..." / "中文：..."
// - "lang=en ..." / "language: zh ..." / "reply=english ..."
//
// If no override is found, it returns ("", originalText).
func extractLanguageOverride(text string) (lang string, cleaned string) {
	orig := text
	t := strings.TrimSpace(text)
	if t == "" {
		return "", orig
	}

	// Key-value style: lang=en / language:zh / reply=english
	if m := langOverrideKVRe.FindStringSubmatch(t); m != nil {
		if l := normalizeLangToken(m[1]); l != "" {
			return l, stripLeadingLangSeparators(m[2])
		}
	}

	// Prefix style: /en ... or en: ...
	t2 := t
	if strings.HasPrefix(t2, "/") {
		t2 = strings.TrimSpace(strings.TrimPrefix(t2, "/"))
	}
	token, rest := splitFirstTokenAndRemainder(t2)
	if l := normalizeLangToken(token); l != "" {
		return l, stripLeadingLangSeparators(rest)
	}

	return "", orig
}

func splitFirstTokenAndRemainder(s string) (token, rest string) {
	t := strings.TrimSpace(s)
	if t == "" {
		return "", ""
	}
	for i, r := range t {
		if unicode.IsSpace(r) || r == ':' || r == '：' || r == '\n' || r == '\r' {
			return t[:i], t[i:]
		}
	}
	return t, ""
}

func stripLeadingLangSeparators(s string) string {
	t := strings.TrimSpace(s)
	for t != "" {
		switch {
		case strings.HasPrefix(t, ":"):
			t = strings.TrimSpace(strings.TrimPrefix(t, ":"))
		case strings.HasPrefix(t, "："):
			t = strings.TrimSpace(strings.TrimPrefix(t, "："))
		case strings.HasPrefix(t, ","):
			t = strings.TrimSpace(strings.TrimPrefix(t, ","))
		case strings.HasPrefix(t, "，"):
			t = strings.TrimSpace(strings.TrimPrefix(t, "，"))
		case strings.HasPrefix(t, "-"):
			t = strings.TrimSpace(strings.TrimPrefix(t, "-"))
		case strings.HasPrefix(t, "—"):
			t = strings.TrimSpace(strings.TrimPrefix(t, "—"))
		default:
			return t
		}
	}
	return ""
}

func normalizeLangToken(token string) string {
	tok := strings.TrimSpace(token)
	if tok == "" {
		return ""
	}
	// Allow bracketed forms like "[en]" or "(zh)".
	tok = strings.Trim(tok, "[](){}<>（）【】")

	lower := strings.ToLower(strings.TrimSpace(tok))
	switch lower {
	case "en", "eng", "english":
		return "en"
	case "zh", "zh-cn", "zh_cn", "cn", "chs", "chinese":
		return "zh"
	}

	switch tok {
	case "英文":
		return "en"
	case "中文":
		return "zh"
	}
	return ""
}
