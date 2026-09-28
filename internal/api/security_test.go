package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go-ai-agent/internal/config"
)

func boolPtr(b bool) *bool { return &b }

// newServerWithSwagger 构造指定 Swagger 开关的 Server。
func newServerWithSwagger(t *testing.T, enabled *bool) *Server {
	t.Helper()
	cfg := &config.Config{
		LLM: config.LLM{
			Providers: []config.Provider{{Name: "local", Type: "openai", BaseURL: "http://localhost:1234/v1", APIKey: "k"}},
		},
	}
	cfg.Server.SwaggerEnabled = enabled
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// swaggerDisabledMarker 关闭时的响应特征（用响应体而非状态码判断，
// 避免把 swaggo 自身的重定向/404 误判为「已关闭」）。
const swaggerDisabledMarker = "接口文档已在配置中关闭"

// getSwaggerBody 请求 swagger 首页并返回状态码与响应体。
func getSwaggerBody(t *testing.T, s *Server) (int, string) {
	t.Helper()
	ts := httptest.NewServer(s)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/swagger/index.html")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestSwaggerEnabledByDefault(t *testing.T) {
	code, body := getSwaggerBody(t, newServerWithSwagger(t, nil))
	if strings.Contains(body, swaggerDisabledMarker) {
		t.Errorf("未配置 swagger-enabled 时应保持开启（向后兼容），得到 %d / %q", code, body)
	}
}

func TestSwaggerDisabledByConfig(t *testing.T) {
	code, body := getSwaggerBody(t, newServerWithSwagger(t, boolPtr(false)))
	if code != http.StatusNotFound {
		t.Errorf("显式关闭时应返回 404，得到 %d", code)
	}
	if !strings.Contains(body, swaggerDisabledMarker) {
		t.Errorf("关闭时应给出可行动的提示，得到 %q", body)
	}
}

func TestSwaggerExplicitlyEnabled(t *testing.T) {
	_, body := getSwaggerBody(t, newServerWithSwagger(t, boolPtr(true)))
	if strings.Contains(body, swaggerDisabledMarker) {
		t.Errorf("显式开启时不应返回关闭提示，得到 %q", body)
	}
}

func TestSwaggerOnHelper(t *testing.T) {
	var s config.Server
	if !s.SwaggerOn() {
		t.Error("未配置时默认应为 true")
	}
	s.SwaggerEnabled = boolPtr(false)
	if s.SwaggerOn() {
		t.Error("显式 false 应为 false")
	}
	s.SwaggerEnabled = boolPtr(true)
	if !s.SwaggerOn() {
		t.Error("显式 true 应为 true")
	}
}

func TestStaticSecurityHeaders(t *testing.T) {
	s := newTestServer(t, nil)
	ts := httptest.NewServer(s)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	csp := resp.Header.Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("静态页面应下发 CSP 响应头")
	}
	for _, directive := range []string{"default-src 'self'", "object-src 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP 缺少 %q：%s", directive, csp)
		}
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q", got)
	}
}

func TestStaticCacheHeaders(t *testing.T) {
	s := newTestServer(t, nil)
	ts := httptest.NewServer(s)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// 入口页必须 no-cache，否则前端重新构建后浏览器仍用旧入口
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "no-cache") {
		t.Errorf("index.html 的 Cache-Control = %q", got)
	}
}
