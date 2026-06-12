package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"example.com/feishu-bot/internal/bot"
	"example.com/feishu-bot/internal/codex"
	"example.com/feishu-bot/internal/config"
	"example.com/feishu-bot/internal/metrics"
)

func main() {
	cfg, err := config.LoadFromEnv()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log.Printf("feishu-bot event transport: %s", cfg.FeishuEventTransport)
	if cfg.FeishuEventTransport == "http" {
		if cfg.FeishuVerificationToken == "" {
			log.Printf("[WARN] FEISHU_VERIFICATION_TOKEN is empty; URL verification token check will be skipped (http transport).")
		}
		if cfg.FeishuVerificationToken == "" && cfg.FeishuEncryptKey == "" {
			log.Printf("[WARN] FEISHU_ENCRYPT_KEY is also empty; event callback requests are not authenticated. Do not expose /webhook/event to the public internet.")
		}
	}

	// Default to quieter logs for webhook mode; for WS mode, INFO is useful to
	// confirm the long-connection is established (connected/disconnected).
	logLevel := larkcore.LogLevelError
	if cfg.FeishuEventTransport == "ws" {
		logLevel = larkcore.LogLevelInfo
	}
	if cfg.FeishuDebug {
		logLevel = larkcore.LogLevelDebug
	}

	larkClient := lark.NewClient(
		cfg.FeishuAppID,
		cfg.FeishuAppSecret,
		lark.WithOpenBaseUrl(cfg.FeishuBaseURL),
		lark.WithLogLevel(logLevel),
		lark.WithEnableTokenCache(true),
	)

	// In mention-only group mode, we should only react when THIS bot is @mentioned
	// (not when any user is mentioned). Resolve the bot's open_id proactively so
	// webhook processing stays fast.
	if cfg.FeishuGroupMode == "mention" && strings.TrimSpace(cfg.FeishuBotOpenID) == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		openID, err := bot.FetchBotOpenID(ctx, cfg.FeishuBaseURL, cfg.FeishuAppID, cfg.FeishuAppSecret)
		if err != nil {
			log.Printf("[WARN] failed to resolve bot open_id for mention filtering: %v (set FEISHU_BOT_OPEN_ID to override)", err)
		} else {
			cfg.FeishuBotOpenID = openID
			if cfg.FeishuDebug {
				log.Printf("[DEBUG] resolved bot open_id: %s", openID)
			}
		}
	}

	var codexClient codex.Client
	switch cfg.CodexMode {
	case "cli":
		codexClient = &codex.ExecClient{
			CodexPath:                 cfg.CodexExecPath,
			WorkDir:                   cfg.CodexWorkDir,
			Sandbox:                   cfg.CodexSandbox,
			BypassApprovalsAndSandbox: cfg.CodexBypassApprovalsAndSandbox,
			IsolateDocAndCode:         cfg.CodexIsolateDocAndCode,
			RunAsUser:                 cfg.CodexRunAsUser,
			HomeDir:                   cfg.CodexHomeDir,
			SystemPrompt:              cfg.CodexSystemPrompt,
		}
	case "api":
		codexClient = &codex.OpenAIClient{
			BaseURL:      cfg.CodexBaseURL,
			APIKey:       cfg.CodexAPIKey,
			Model:        cfg.CodexModel,
			API:          cfg.CodexAPI,
			SystemPrompt: cfg.CodexSystemPrompt,
			MaxTokens:    cfg.CodexMaxTokens,
			Temperature:  cfg.CodexTemperature,
			HTTPClient: &http.Client{
				Timeout: cfg.CodexTimeout + 10*time.Second,
			},
		}
	default:
		log.Fatalf("invalid CODEX_MODE=%q (expected cli or api)", cfg.CodexMode)
	}

	var metricsRecorder *metrics.Recorder
	if cfg.MetricsEnabled {
		rec, err := metrics.NewRecorder(metrics.Options{
			FilePath:       cfg.MetricsFilePath,
			RotateMaxBytes: cfg.MetricsRotateMaxBytes,
			FlushInterval:  cfg.MetricsFlushInterval,
			Pricing: metrics.Pricing{
				TotalUSDPer1M:       cfg.MetricsPriceTotalUSDPer1M,
				InputUSDPer1M:       cfg.MetricsPriceInputUSDPer1M,
				CachedInputUSDPer1M: cfg.MetricsPriceCachedInputUSDPer1M,
				OutputUSDPer1M:      cfg.MetricsPriceOutputUSDPer1M,
			},
		})
		if err != nil {
			log.Printf("[WARN] metrics disabled (init failed): %v", err)
		} else {
			metricsRecorder = rec
			go metricsRecorder.Run(rootCtx)
		}
	}

	b := bot.New(cfg, larkClient, codexClient, metricsRecorder)

	evtDispatcher := dispatcher.NewEventDispatcher(cfg.FeishuVerificationToken, cfg.FeishuEncryptKey)
	evtDispatcher.InitConfig(
		larkevent.WithLogLevel(logLevel),
		// Signature verify is only performed when EncryptKey is set; keep it enabled by default.
		larkevent.WithSkipSignVerify(false),
	)
	evtDispatcher.OnP2MessageReceiveV1(b.HandleMessageReceiveV1)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	metrics.RegisterHandlers(mux, metricsRecorder)

	var wsErrCh chan error
	switch cfg.FeishuEventTransport {
	case "http":
		// Feishu event subscription callback URL.
		// Configure it in Feishu developer console, e.g.:
		//   https://your.domain.example/webhook/event
		//
		// Note: FEISHU_VERIFICATION_TOKEN is optional. If not set, we still respond
		// to URL verification (challenge) without checking token. This is convenient
		// for internal deployments, but is less secure if the endpoint is public.
		mux.HandleFunc("/webhook/event", makeFeishuEventHandler(evtDispatcher, cfg.FeishuVerificationToken, cfg.FeishuDebug))
	case "ws":
		// WebSocket transport (no public callback URL required). This is useful when
		// you don't have a public IP. You still need to enable WebSocket/long-conn
		// event subscription in Feishu open platform and subscribe to events.
		wsErrCh = make(chan error, 1)
		wsClient := larkws.NewClient(
			cfg.FeishuAppID,
			cfg.FeishuAppSecret,
			larkws.WithEventHandler(evtDispatcher),
			larkws.WithLogLevel(logLevel),
			larkws.WithDomain(cfg.FeishuBaseURL),
		)
		go func() {
			wsErrCh <- wsClient.Start(rootCtx)
		}()
	default:
		log.Fatalf("invalid FEISHU_EVENT_TRANSPORT=%q (expected http or ws)", cfg.FeishuEventTransport)
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("feishu-bot listening on %s", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	waitForShutdown(srv, wsErrCh, cancel, metricsRecorder)
}

func waitForShutdown(srv *http.Server, wsErrCh <-chan error, cancel context.CancelFunc, metricsRecorder *metrics.Recorder) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-ch:
		// normal shutdown
	case err := <-wsErrCh:
		if err != nil {
			log.Printf("[ERROR] feishu ws client exited: %v", err)
		} else {
			log.Printf("[ERROR] feishu ws client exited without error")
		}
	}

	if cancel != nil {
		cancel()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	if metricsRecorder != nil {
		_ = metricsRecorder.FlushAll(time.Now())
		_ = metricsRecorder.Close()
	}
	fmt.Println("shutdown complete")
}

func makeFeishuEventHandler(evtDispatcher *dispatcher.EventDispatcher, verificationToken string, debug bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		start := time.Now()
		ctx := r.Context()
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			log.Printf("[ERROR] webhook read body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		if debug {
			log.Printf("[DEBUG] webhook request: remote=%s path=%s bytes=%d", r.RemoteAddr, r.URL.Path, len(rawBody))
		}
		eventReq := &larkevent.EventReq{
			Header:     r.Header,
			Body:       rawBody,
			RequestURI: r.RequestURI,
		}

		// Detect url_verification by decrypting the payload first (it may be encrypted).
		cipher, err := evtDispatcher.ParseReq(ctx, eventReq)
		if err != nil {
			log.Printf("[ERROR] webhook ParseReq: %v", err)
			writeEventResp(w, errorResp(err))
			return
		}
		plain, err := evtDispatcher.DecryptEvent(ctx, cipher)
		if err != nil {
			log.Printf("[ERROR] webhook DecryptEvent: %v", err)
			writeEventResp(w, errorResp(err))
			return
		}

		reqType, challenge, token, eventType, err := parseReqTypeChallengeToken(plain)
		if err != nil {
			log.Printf("[ERROR] webhook parse body: %v", err)
			writeEventResp(w, errorResp(err))
			return
		}

		if reqType == larkevent.ReqTypeChallenge {
			// If encryption is enabled, verify signature even for challenge requests.
			if evtDispatcher.Config != nil && !evtDispatcher.Config.SkipSignVerify {
				if err := evtDispatcher.VerifySign(ctx, eventReq); err != nil {
					writeEventResp(w, errorResp(err))
					return
				}
			}

			// Optional: only check token when configured.
			if verificationToken != "" && token != verificationToken {
				log.Printf("[ERROR] url_verification token mismatch (got=%q)", token)
				writeEventResp(w, errorResp(fmt.Errorf("verification token mismatch")))
				return
			}

			if debug {
				log.Printf("[DEBUG] url_verification ok (challenge_len=%d) dur=%s", len(challenge), time.Since(start))
			}
			w.Header().Set("Content-Type", larkevent.DefaultContentType)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(fmt.Sprintf(larkevent.ChallengeResponseFormat, challenge)))
			return
		}

		// Delegate normal event_callback processing to SDK dispatcher.
		eventResp := evtDispatcher.Handle(ctx, eventReq)
		if debug {
			status := 0
			if eventResp != nil {
				status = eventResp.StatusCode
			}
			log.Printf("[DEBUG] event_callback type=%s status=%d dur=%s", eventType, status, time.Since(start))
		}
		writeEventResp(w, eventResp)
	}
}

func parseReqTypeChallengeToken(plainEventJSON string) (larkevent.ReqType, string, string, string, error) {
	fuzzy := &larkevent.EventFuzzy{}
	if err := json.Unmarshal([]byte(plainEventJSON), fuzzy); err != nil {
		return "", "", "", "", fmt.Errorf("event json unmarshal: %w", err)
	}
	if fuzzy.Encrypt != "" {
		return "", "", "", "", fmt.Errorf("event data is encrypted; set FEISHU_ENCRYPT_KEY")
	}

	token := fuzzy.Token
	eventType := ""
	if fuzzy.Header != nil && fuzzy.Header.Token != "" {
		token = fuzzy.Header.Token
	}
	if fuzzy.Header != nil && fuzzy.Header.EventType != "" {
		eventType = fuzzy.Header.EventType
	}
	if eventType == "" && fuzzy.Event != nil {
		if et, ok := fuzzy.Event.Type.(string); ok {
			eventType = et
		}
	}
	return larkevent.ReqType(fuzzy.Type), fuzzy.Challenge, token, eventType, nil
}

func writeEventResp(w http.ResponseWriter, eventResp *larkevent.EventResp) {
	if eventResp == nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	for k, vs := range eventResp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(eventResp.StatusCode)
	if len(eventResp.Body) > 0 {
		_, _ = w.Write(eventResp.Body)
	}
}

func errorResp(err error) *larkevent.EventResp {
	header := http.Header{}
	header.Set("Content-Type", larkevent.DefaultContentType)
	msg := "error"
	if err != nil {
		msg = err.Error()
	}
	return &larkevent.EventResp{
		Header:     header,
		Body:       []byte(fmt.Sprintf(larkevent.WebhookResponseFormat, msg)),
		StatusCode: http.StatusInternalServerError,
	}
}
