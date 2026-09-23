package llm

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// probeTimeout 单次探测超时（刻意独立于 provider 的生成超时，避免界面长时间等待）。
const probeTimeout = 6 * time.Second

// ProbeResult 一次端点连通性探测结果。
type ProbeResult struct {
	Provider   string `json:"provider"`
	Status     string `json:"status"` // ok | auth_error | http_error | unreachable | unknown_provider | error
	Reachable  bool   `json:"reachable"`
	HTTPStatus int    `json:"http_status,omitempty"`
	LatencyMS  int64  `json:"latency_ms"`
	Detail     string `json:"detail"`
	CheckedAt  string `json:"checked_at"`
}

// Probe 探测指定 provider 的连通性：请求最轻量的标准端点 GET {base-url}/v1/models。
// 由 API 层在页面加载或用户手动点击时调用（不做后台实时轮询）。
func (f *Factory) Probe(providerName string) ProbeResult {
	res := ProbeResult{
		Provider:  providerName,
		CheckedAt: time.Now().Format(time.RFC3339),
	}

	p, ok := f.providers[providerName]
	if !ok {
		res.Status = "unknown_provider"
		res.Detail = "provider 未在 ai.yml 中声明"
		return res
	}

	req, err := http.NewRequest(http.MethodGet, probeURL(p.BaseURL), nil)
	if err != nil {
		res.Status = "error"
		res.Detail = err.Error()
		return res
	}
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}

	client := &http.Client{Timeout: probeTimeout}
	start := time.Now()
	resp, err := client.Do(req)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Status = "unreachable"
		res.Detail = err.Error()
		return res
	}
	defer resp.Body.Close()
	// 丢弃响应体（只关心状态码），限制读取量避免异常响应拖慢探测
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))

	res.Reachable = true
	res.HTTPStatus = resp.StatusCode
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		res.Status = "auth_error"
		res.Detail = "端点可达，但 API Key 被拒绝（HTTP " + strconv.Itoa(resp.StatusCode) + "）"
	case resp.StatusCode >= http.StatusInternalServerError:
		res.Status = "http_error"
		res.Detail = "端点返回服务端错误 HTTP " + strconv.Itoa(resp.StatusCode)
	default:
		// 2xx 正常；3xx/4xx（如部分后端无 /v1/models 返回 404）也说明服务已就绪
		res.Status = "ok"
		res.Detail = "HTTP " + strconv.Itoa(resp.StatusCode)
	}
	return res
}

// probeURL 组装探测地址：兼容 base-url 是否已带 /v1 后缀两种情况。
func probeURL(baseURL string) string {
	b := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if b == "" {
		return "/v1/models"
	}
	if strings.HasSuffix(b, "/v1") {
		return b + "/models"
	}
	return b + "/v1/models"
}

// ProbeSummary 便于日志/调试的单行描述。
func (r ProbeResult) String() string {
	return fmt.Sprintf("%s: %s (%dms, http=%d) %s", r.Provider, r.Status, r.LatencyMS, r.HTTPStatus, r.Detail)
}
