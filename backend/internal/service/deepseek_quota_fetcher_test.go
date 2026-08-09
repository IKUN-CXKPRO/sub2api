package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------- BuildDeepSeekBalanceURL 测试 ----------
func TestBuildDeepSeekBalanceURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://api.deepseek.com", "https://api.deepseek.com/user/balance"},
		{"https://api.deepseek.com/", "https://api.deepseek.com/user/balance"},
		{"https://api.deepseek.com/v1", "https://api.deepseek.com/user/balance"},
		{"https://api.deepseek.com/v1/", "https://api.deepseek.com/user/balance"},
		{"api.deepseek.com", "https://api.deepseek.com/user/balance"},
		{"", ""}, // 期望 error
	}
	for _, c := range cases {
		got, err := BuildDeepSeekBalanceURL(c.in)
		if c.in == "" {
			if err == nil {
				t.Errorf("expected error for empty input")
			}
			continue
		}
		if err != nil {
			t.Errorf("BuildDeepSeekBalanceURL(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("BuildDeepSeekBalanceURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---------- CanFetch 识别测试 ----------
func TestDeepSeekQuotaFetcher_CanFetch(t *testing.T) {
	f := &DeepSeekQuotaFetcher{}

	mkAccount := func(platform, typ, baseURL, apiKey string) *Account {
		a := &Account{Platform: platform, Type: typ, Credentials: map[string]any{}}
		if baseURL != "" {
			a.Credentials["base_url"] = baseURL
		}
		if apiKey != "" {
			a.Credentials["api_key"] = apiKey
		}
		return a
	}

	cases := []struct {
		name string
		acct *Account
		want bool
	}{
		{"deepseek no v1", mkAccount(PlatformOpenAI, AccountTypeAPIKey, "https://api.deepseek.com", "sk-x"), true},
		{"deepseek with v1", mkAccount(PlatformOpenAI, AccountTypeAPIKey, "https://api.deepseek.com/v1", "sk-x"), true},
		{"deepseek no scheme", mkAccount(PlatformOpenAI, AccountTypeAPIKey, "api.deepseek.com", "sk-x"), true},
		{"openai api", mkAccount(PlatformOpenAI, AccountTypeAPIKey, "https://api.openai.com", "sk-x"), false},
		{"openai oauth", mkAccount(PlatformOpenAI, AccountTypeOAuth, "https://api.deepseek.com", ""), false},
		{"anthropic", mkAccount(PlatformAnthropic, AccountTypeAPIKey, "https://api.deepseek.com", "sk-x"), false},
		{"no api key", mkAccount(PlatformOpenAI, AccountTypeAPIKey, "https://api.deepseek.com", ""), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := f.CanFetch(c.acct); got != c.want {
			t.Errorf("%s: CanFetch = %v, want %v", c.name, got, c.want)
		}
	}
}

// ---------- FetchQuota 各状态码测试 ----------
func TestDeepSeekQuotaFetcher_FetchQuota(t *testing.T) {
	// 200 正常（多币种）
	tsOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/balance" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("bad auth header: %s", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[
			{"currency":"CNY","total_balance":"110.00","granted_balance":"10.00","topped_up_balance":"100.00"},
			{"currency":"USD","total_balance":"15.50","granted_balance":"0.00","topped_up_balance":"15.50"}
		]}`))
	}))
	defer tsOK.Close()

	f := &DeepSeekQuotaFetcher{client: tsOK.Client()}
	acct := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": tsOK.URL + "/v1"}}
	res, err := f.FetchQuota(context.Background(), acct, "")
	if err != nil {
		t.Fatalf("FetchQuota error: %v", err)
	}
	if res == nil || res.UsageInfo == nil || res.UsageInfo.DeepSeekBalance == nil {
		t.Fatalf("missing balance result")
	}
	dsb := res.UsageInfo.DeepSeekBalance
	if !dsb.IsAvailable {
		t.Error("IsAvailable should be true")
	}
	if len(dsb.BalanceInfos) != 2 {
		t.Errorf("expected 2 currencies, got %d", len(dsb.BalanceInfos))
	}
	if dsb.BalanceInfos[0].Currency != "CNY" || dsb.BalanceInfos[0].TotalBalance != "110.00" {
		t.Errorf("unexpected CNY balance: %+v", dsb.BalanceInfos[0])
	}
	if dsb.BalanceInfos[1].Currency != "USD" || dsb.BalanceInfos[1].TotalBalance != "15.50" {
		t.Errorf("unexpected USD balance: %+v", dsb.BalanceInfos[1])
	}

	// 404 unsupported
	ts404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts404.Close()
	f404 := &DeepSeekQuotaFetcher{client: ts404.Client()}
	res404, err := f404.FetchQuota(context.Background(),
		&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"api_key": "sk-x", "base_url": ts404.URL}}, "")
	if err != nil {
		t.Fatalf("FetchQuota 404 error: %v", err)
	}
	if res404.UsageInfo.ErrorCode != "balance_unsupported" {
		t.Errorf("expected balance_unsupported, got %s", res404.UsageInfo.ErrorCode)
	}

	// 401
	ts401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts401.Close()
	f401 := &DeepSeekQuotaFetcher{client: ts401.Client()}
	res401, _ := f401.FetchQuota(context.Background(),
		&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"api_key": "sk-x", "base_url": ts401.URL}}, "")
	if res401.UsageInfo.ErrorCode != "unauthorized" {
		t.Errorf("expected unauthorized, got %s", res401.UsageInfo.ErrorCode)
	}

	// 500
	ts500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts500.Close()
	f500 := &DeepSeekQuotaFetcher{client: ts500.Client()}
	res500, _ := f500.FetchQuota(context.Background(),
		&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"api_key": "sk-x", "base_url": ts500.URL}}, "")
	if res500.UsageInfo.ErrorCode != "http_error" {
		t.Errorf("expected http_error, got %s", res500.UsageInfo.ErrorCode)
	}

	// invalid json
	tsBad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer tsBad.Close()
	fBad := &DeepSeekQuotaFetcher{client: tsBad.Client()}
	resBad, _ := fBad.FetchQuota(context.Background(),
		&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"api_key": "sk-x", "base_url": tsBad.URL}}, "")
	if resBad.UsageInfo.ErrorCode != "parse_error" {
		t.Errorf("expected parse_error, got %s", resBad.UsageInfo.ErrorCode)
	}

	// missing balance_infos
	tsEmpty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"is_available":false}`))
	}))
	defer tsEmpty.Close()
	fEmpty := &DeepSeekQuotaFetcher{client: tsEmpty.Client()}
	resEmpty, err := fEmpty.FetchQuota(context.Background(),
		&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"api_key": "sk-x", "base_url": tsEmpty.URL}}, "")
	if err != nil {
		t.Fatalf("FetchQuota empty error: %v", err)
	}
	if resEmpty.UsageInfo.DeepSeekBalance == nil {
		t.Error("expected DeepSeekBalance even with empty balance_infos")
	}
	if resEmpty.UsageInfo.DeepSeekBalance.IsAvailable {
		t.Error("IsAvailable should be false")
	}
}

// ---------- JSON 序列化（金额为字符串，非 float） ----------
func TestDeepSeekBalanceJSON(t *testing.T) {
	b, _ := json.Marshal(DeepSeekBalanceInfo{Currency: "CNY", TotalBalance: "110.00", GrantedBalance: "10.00", ToppedUpBalance: "100.00"})
	s := string(b)
	// 余额必须是字符串（decimal 精度），不能是数字
	if !strings.Contains(s, `"total_balance":"110.00"`) {
		t.Errorf("total_balance must be string, got: %s", s)
	}
}
