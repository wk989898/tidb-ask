package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// FetchBotOpenID resolves the bot's open_id using app credentials.
//
// This is primarily used to correctly determine whether a group message actually
// @mentioned this bot (as opposed to mentioning someone else).
//
// You can also set FEISHU_BOT_OPEN_ID to skip this network call.
func FetchBotOpenID(ctx context.Context, baseURL, appID, appSecret string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	appID = strings.TrimSpace(appID)
	appSecret = strings.TrimSpace(appSecret)
	if baseURL == "" {
		baseURL = "https://open.feishu.cn"
	}
	if appID == "" || appSecret == "" {
		return "", errors.New("missing feishu app_id/app_secret")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	token, err := fetchTenantAccessToken(ctx, baseURL, appID, appSecret)
	if err != nil {
		return "", err
	}
	openID, err := fetchSelfBotOpenID(ctx, baseURL, token)
	if err != nil {
		return "", err
	}
	openID = strings.TrimSpace(openID)
	if openID == "" {
		return "", errors.New("resolved bot open_id is empty")
	}
	return openID, nil
}

func fetchTenantAccessToken(ctx context.Context, baseURL, appID, appSecret string) (string, error) {
	type tokenReq struct {
		AppID     string `json:"app_id"`
		AppSecret string `json:"app_secret"`
	}
	type tokenResp struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
	}

	u := strings.TrimRight(baseURL, "/") + "/open-apis/auth/v3/tenant_access_token/internal"
	body, _ := json.Marshal(tokenReq{AppID: appID, AppSecret: appSecret})

	httpClient := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("tenant_access_token request failed: http=%d body=%s", resp.StatusCode, truncateForLog(raw, 300))
	}

	var tr tokenResp
	if err := json.Unmarshal(raw, &tr); err != nil {
		return "", fmt.Errorf("tenant_access_token parse failed: %w body=%s", err, truncateForLog(raw, 300))
	}
	if tr.Code != 0 {
		return "", fmt.Errorf("tenant_access_token failed: code=%d msg=%s", tr.Code, tr.Msg)
	}
	if strings.TrimSpace(tr.TenantAccessToken) == "" {
		return "", fmt.Errorf("tenant_access_token is empty: body=%s", truncateForLog(raw, 300))
	}
	return strings.TrimSpace(tr.TenantAccessToken), nil
}

func fetchSelfBotOpenID(ctx context.Context, baseURL, tenantAccessToken string) (string, error) {
	paths := []string{
		"/open-apis/bot/v3/info",
		"/open-apis/bot/v3/info/",
	}

	for _, p := range paths {
		u := strings.TrimRight(baseURL, "/") + p
		openID, retryable, err := fetchBotOpenIDOnce(ctx, u, tenantAccessToken)
		if err == nil && strings.TrimSpace(openID) != "" {
			return strings.TrimSpace(openID), nil
		}
		if !retryable {
			return "", err
		}
	}
	return "", errors.New("failed to resolve bot open_id (all endpoints failed)")
}

func fetchBotOpenIDOnce(ctx context.Context, url string, tenantAccessToken string) (openID string, retryable bool, err error) {
	type botInfoResp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Bot  struct {
			OpenID string `json:"open_id"`
		} `json:"bot"`
		Data struct {
			OpenID string `json:"open_id"`
			Bot    struct {
				OpenID string `json:"open_id"`
			} `json:"bot"`
		} `json:"data"`
	}

	if ctx == nil {
		ctx = context.Background()
	}

	httpClient := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(tenantAccessToken))

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return "", true, fmt.Errorf("bot info endpoint not found: url=%s", url)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", false, fmt.Errorf("bot info request failed: http=%d body=%s", resp.StatusCode, truncateForLog(raw, 300))
	}

	var bi botInfoResp
	if err := json.Unmarshal(raw, &bi); err != nil {
		return "", false, fmt.Errorf("bot info parse failed: %w body=%s", err, truncateForLog(raw, 300))
	}
	if bi.Code != 0 {
		return "", false, fmt.Errorf("bot info failed: code=%d msg=%s", bi.Code, bi.Msg)
	}
	if strings.TrimSpace(bi.Bot.OpenID) != "" {
		return strings.TrimSpace(bi.Bot.OpenID), false, nil
	}
	if strings.TrimSpace(bi.Data.Bot.OpenID) != "" {
		return strings.TrimSpace(bi.Data.Bot.OpenID), false, nil
	}
	if strings.TrimSpace(bi.Data.OpenID) != "" {
		return strings.TrimSpace(bi.Data.OpenID), false, nil
	}

	// Fallback: scan the JSON for a plausible open_id field.
	var anyResp any
	if err := json.Unmarshal(raw, &anyResp); err == nil {
		if s := findFirstStringByKey(anyResp, "open_id"); strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s), false, nil
		}
	}
	return "", false, fmt.Errorf("bot open_id not found in response: body=%s", truncateForLog(raw, 300))
}

func findFirstStringByKey(v any, key string) string {
	switch vv := v.(type) {
	case map[string]any:
		if s, ok := vv[key].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
		for _, child := range vv {
			if s := findFirstStringByKey(child, key); strings.TrimSpace(s) != "" {
				return s
			}
		}
	case []any:
		for _, child := range vv {
			if s := findFirstStringByKey(child, key); strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return ""
}

func truncateForLog(raw []byte, max int) string {
	s := strings.TrimSpace(string(raw))
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
