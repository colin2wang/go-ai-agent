package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"go-ai-agent/internal/config"
)

// newTestServer 构造一个最小可用 Server（无存储、无子功能）。
func newTestServer(t *testing.T, tokens []string) *Server {
	t.Helper()
	cfg := &config.Config{
		LLM: config.LLM{
			Providers: []config.Provider{
				{Name: "local", Type: "openai", BaseURL: "http://localhost:1234/v1", APIKey: "test-key"},
			},
		},
	}
	cfg.Server.APITokens = tokens
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestNewAllowsEmptyProviders(t *testing.T) {
	// New 不做 provider 数量校验（那是 config.Load 的职责），零 provider 也应可初始化
	if s, err := New(&config.Config{}); err != nil {
		t.Fatalf("零 provider 应可初始化，得到 %v", err)
	} else {
		s.Close()
	}
}

func TestHealthEndpoint(t *testing.T) {
	s := newTestServer(t, nil)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health 状态码 = %d", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q", body["status"])
	}
}

func TestAuthRequiredForAPI(t *testing.T) {
	s := newTestServer(t, []string{"secret-token"})
	ts := httptest.NewServer(s)
	defer ts.Close()

	// 无令牌 → 401
	resp, _ := http.Get(ts.URL + "/api/features")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("无令牌应 401，得到 %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 错误令牌 → 401
	req, _ := http.NewRequest("GET", ts.URL+"/api/features", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("错误令牌应 401，得到 %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Bearer 正确令牌 → 通过
	req, _ = http.NewRequest("GET", ts.URL+"/api/features", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Bearer 正确令牌应 200，得到 %d", resp.StatusCode)
	}
	resp.Body.Close()

	// X-API-Key 正确令牌 → 通过
	req, _ = http.NewRequest("GET", ts.URL+"/api/features", nil)
	req.Header.Set("X-API-Key", "secret-token")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("X-API-Key 正确令牌应 200，得到 %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAuthSkipsNonAPIPaths(t *testing.T) {
	s := newTestServer(t, []string{"secret-token"})
	ts := httptest.NewServer(s)
	defer ts.Close()

	// 静态资源路径不鉴权（回退 index.html，应为 200）
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Error("非 /api 路径不应鉴权")
	}
}

func TestAuthExcludeHealth(t *testing.T) {
	cfg := &config.Config{
		LLM: config.LLM{
			Providers: []config.Provider{{Name: "local", Type: "openai", BaseURL: "http://x/v1", APIKey: "test-key"}},
		},
	}
	cfg.Server.APITokens = []string{"t1"}
	cfg.Server.AuthExcludeHealth = true
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, _ := http.Get(ts.URL + "/api/health")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("auth-exclude-health=true 时 health 应 200，得到 %d", resp.StatusCode)
	}

	// 其它 /api 仍需令牌
	resp2, _ := http.Get(ts.URL + "/api/features")
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("其它 /api 仍应 401，得到 %d", resp2.StatusCode)
	}
}

func TestFeaturesEndpointShape(t *testing.T) {
	s := newTestServer(t, nil)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/features")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		App       map[string]any   `json:"app"`
		Features  []map[string]any `json:"features"`
		Providers []map[string]any `json:"providers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("应返回 JSON 对象: %v", err)
	}
	if len(body.Features) != 0 {
		t.Errorf("无子功能时应返回空数组，得到 %d 项", len(body.Features))
	}
	if body.App == nil || body.Providers == nil {
		t.Error("响应应包含 app 与 providers 字段")
	}
}

func TestNewServerUsesTempDir(t *testing.T) {
	s := newTestServer(t, nil)
	if s.tmpDir == "" {
		t.Fatal("tmpDir 不应为空")
	}
	_ = filepath.Base(s.tmpDir)
}
