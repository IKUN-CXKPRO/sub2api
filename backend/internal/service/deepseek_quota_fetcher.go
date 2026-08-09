package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DeepSeekQuotaFetcher 从 DeepSeek 官方 /user/balance 接口获取账号余额。
// 识别规则: platform=openai + type=apikey + base_url 主机名含 deepseek.com。
// 所有 DeepSeek 账号（含未来新增）自动命中本 fetcher，无需逐账号配置。
type DeepSeekQuotaFetcher struct {
	proxyRepo ProxyRepository
	client    *http.Client
}

// NewDeepSeekQuotaFetcher 创建 DeepSeekQuotaFetcher。
func NewDeepSeekQuotaFetcher(proxyRepo ProxyRepository) *DeepSeekQuotaFetcher {
	return &DeepSeekQuotaFetcher{
		proxyRepo: proxyRepo,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// DeepSeekBalanceInfo 单币种余额。
type DeepSeekBalanceInfo struct {
	Currency        string `json:"currency"`
	TotalBalance    string `json:"total_balance"`
	GrantedBalance  string `json:"granted_balance"`
	ToppedUpBalance string `json:"topped_up_balance"`
}

// DeepSeekBalanceResponse DeepSeek /user/balance 官方响应。
type DeepSeekBalanceResponse struct {
	IsAvailable  bool                   `json:"is_available"`
	BalanceInfos []DeepSeekBalanceInfo `json:"balance_infos"`
}

// IsDeepSeekAccount 判断账号是否为 DeepSeek API Key 账号（openai+apikey+base_url 含 deepseek.com）。
func IsDeepSeekAccount(account *Account) bool {
	if account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeAPIKey {
		return false
	}
	if account.GetCredential("api_key") == "" {
		return false
	}
	baseURL := account.GetOpenAIBaseURL()
	if baseURL == "" {
		return false
	}
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		baseURL = "https://" + baseURL
	}
	host := baseURL
	if u, err := url.Parse(baseURL); err == nil {
		host = u.Hostname()
	}
	host = strings.TrimPrefix(host, "www.")
	return strings.Contains(strings.ToLower(host), "deepseek.com")
}

// CanFetch 判断账号是否为 DeepSeek API Key 账号。
func (f *DeepSeekQuotaFetcher) CanFetch(account *Account) bool {
	return IsDeepSeekAccount(account)
}

// GetProxyURL 返回账号绑定的代理 URL（与推理请求同出口）。
func (f *DeepSeekQuotaFetcher) GetProxyURL(ctx context.Context, account *Account) string {
	if account == nil || account.ProxyID == nil || f == nil || f.proxyRepo == nil {
		return ""
	}
	proxy, err := f.proxyRepo.GetByID(ctx, *account.ProxyID)
	if err != nil || proxy == nil {
		return ""
	}
	return proxy.URL()
}

// BuildDeepSeekBalanceURL 安全拼接余额接口地址，兼容 base_url 带/不带 /v1。
// 官方余额接口为 {root}/user/balance（不带 /v1）。
func BuildDeepSeekBalanceURL(baseURL string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return "", fmt.Errorf("empty base url")
	}
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		baseURL = "https://" + baseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(baseURL, "/v1") {
		baseURL = strings.TrimSuffix(baseURL, "/v1")
	}
	return baseURL + "/user/balance", nil
}

// FetchQuota 获取 DeepSeek 账号余额。
func (f *DeepSeekQuotaFetcher) FetchQuota(ctx context.Context, account *Account, proxyURL string) (*QuotaResult, error) {
	apiKey := account.GetCredential("api_key")
	balanceURL, err := BuildDeepSeekBalanceURL(account.GetOpenAIBaseURL())
	if err != nil {
		return nil, err
	}

	client := f.client
	if proxyURL != "" {
		proxy, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy url: %w", err)
		}
		client = &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				Proxy: http.ProxyURL(proxy),
			},
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, balanceURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
		return &QuotaResult{
			UsageInfo: &UsageInfo{ErrorCode: "balance_unsupported", Error: "upstream does not support /user/balance"},
			Raw:       map[string]any{"http_status": resp.StatusCode, "unsupported": true},
		}, nil
	case resp.StatusCode == http.StatusUnauthorized:
		return &QuotaResult{
			UsageInfo: &UsageInfo{ErrorCode: "unauthorized", Error: "invalid api key (401)"},
			Raw:       map[string]any{"http_status": resp.StatusCode},
		}, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return &QuotaResult{
			UsageInfo: &UsageInfo{ErrorCode: "rate_limited", Error: "rate limited (429)"},
			Raw:       map[string]any{"http_status": resp.StatusCode},
		}, nil
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return &QuotaResult{
			UsageInfo: &UsageInfo{ErrorCode: "http_error", Error: fmt.Sprintf("upstream http %d", resp.StatusCode)},
			Raw:       map[string]any{"http_status": resp.StatusCode},
		}, nil
	}

	var balance DeepSeekBalanceResponse
	if err := json.Unmarshal(body, &balance); err != nil {
		return &QuotaResult{
			UsageInfo: &UsageInfo{ErrorCode: "parse_error", Error: "invalid json from upstream"},
			Raw:       map[string]any{"http_status": resp.StatusCode},
		}, nil
	}

	now := time.Now()
	usage := &UsageInfo{
		UpdatedAt: &now,
		DeepSeekBalance: &DeepSeekBalance{
			IsAvailable:  balance.IsAvailable,
			BalanceInfos: balance.BalanceInfos,
		},
	}
	raw := map[string]any{
		"http_status":   resp.StatusCode,
		"is_available":  balance.IsAvailable,
		"balance_infos": balance.BalanceInfos,
	}
	return &QuotaResult{UsageInfo: usage, Raw: raw}, nil
}
