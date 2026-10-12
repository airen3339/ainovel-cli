package models

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const factsList = `{
	"deepseek-v4-flash": {"mode": "chat", "litellm_provider": "deepseek", "max_input_tokens": 1000000},
	"openrouter/anthropic/claude-sonnet-4.5": {"mode": "chat", "litellm_provider": "openrouter", "max_input_tokens": 1000000},
	"anthropic/claude-sonnet-4.5": {"mode": "chat", "litellm_provider": "anthropic", "max_input_tokens": 200000}
}`

// 上下文窗口先按「厂商/模型」找，再按模型名本身找（中转站转发官方模型多用原名）；清单
// 没收录的是 0。
func TestFactsFindTheContextWindowByProviderThenModelName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(factsList), 0o600); err != nil {
		t.Fatal(err)
	}
	facts := NewFacts(path)
	facts.Read()
	for _, tc := range []struct {
		provider, model string
		want            int
	}{
		{"openai", "deepseek-v4-flash", 1000000},
		{"anthropic", "claude-sonnet-4.5", 200000},
		{"openrouter", "anthropic/claude-sonnet-4.5", 1000000},
		{"openai", "relay-renamed", 0},
	} {
		if got := facts.ContextWindow(tc.provider, tc.model); got != tc.want {
			t.Errorf("%s/%s = %d, want %d", tc.provider, tc.model, got, tc.want)
		}
	}
}

// 本地清单在有效期内不联网；缺失或过期时下载并写回；下载失败时旧清单照样能查。
func TestFactsDownloadOnlyWhenTheLocalListIsMissingOrStale(t *testing.T) {
	downloads := 0
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads++
		w.WriteHeader(status)
		w.Write([]byte(factsList))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "models.json")
	refresh := func() (*Facts, bool, error) {
		facts := NewFacts(path)
		facts.url = server.URL
		facts.Read()
		updated, err := facts.Refresh(context.Background())
		return facts, updated, err
	}

	if facts, updated, err := refresh(); err != nil || !updated || downloads != 1 || facts.ContextWindow("deepseek", "deepseek-v4-flash") != 1000000 {
		t.Fatalf("missing list must download: updated=%t downloads=%d err=%v", updated, downloads, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != factsList {
		t.Fatalf("downloaded list must be saved: %q %v", data, err)
	}
	if _, updated, err := refresh(); err != nil || updated || downloads != 1 {
		t.Fatalf("a fresh list must not download again: updated=%t downloads=%d err=%v", updated, downloads, err)
	}
	stale := time.Now().Add(-2 * factsTTL)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatal(err)
	}
	status = http.StatusBadGateway
	if facts, updated, err := refresh(); err == nil || updated || downloads != 2 || facts.ContextWindow("deepseek", "deepseek-v4-flash") != 1000000 {
		t.Fatalf("a stale list must try to download and keep the old facts on failure: updated=%t downloads=%d err=%v", updated, downloads, err)
	}
}
