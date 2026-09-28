// wiredump.go HTTP 层原始报文抓包：io-log.debug 开启时，把发给 LLM 端点的
// 请求体与端点返回的原始响应体完整落到日志（请求体中的 base64 只记长度不倾倒），
// 用于定位 langchaingo → 端点 之间的实际 wire 内容（如多模态格式、错误响应体）。
package llm

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// wireDumpTransport 包装 http.RoundTripper，记录请求/响应原文。
type wireDumpTransport struct {
	next http.RoundTripper
}

// redactedHeaders 必须在 wire 日志中遮蔽的请求头（凭据一律不落盘、不打印）。
// 即便 io-log.debug 开启也不输出原文——调试端点交互不需要拿到密钥。
var redactedHeaders = map[string]bool{
	"authorization":  true,
	"api-key":        true,
	"x-api-key":      true,
	"x-goog-api-key": true,
	"cookie":         true,
	"set-cookie":     true,
}

// headerLogAttrs 把请求头整理为日志属性，敏感头替换为遮蔽标记。
func headerLogAttrs(h http.Header) []slog.Attr {
	if len(h) == 0 {
		return nil
	}
	out := make([]slog.Attr, 0, len(h))
	for k, v := range h {
		name := strings.ToLower(k)
		if redactedHeaders[name] {
			out = append(out, slog.String(name, "***redacted***"))
			continue
		}
		out = append(out, slog.String(name, strings.Join(v, "; ")))
	}
	return out
}

// truncateBase64 将请求体里的 data:image/...;base64,<...> 替换为长度标记，避免日志刷屏。
func truncateBase64(s string) string {
	const marker = ";base64,"
	var b strings.Builder
	for {
		i := strings.Index(s, marker)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		// 找 base64 段结束（引号或空白）
		start := i + len(marker)
		end := start
		for end < len(s) && !isB64Delim(s[end]) {
			end++
		}
		b.WriteString(s[:start])
		b.WriteString("<...base64 len=")
		b.WriteString(itoa(end - start))
		b.WriteString(">...")
		s = s[end:]
	}
}

func isB64Delim(c byte) bool {
	return c == '"' || c == '\'' || c == ' ' || c == '\\' || c == '\n' || c == '\r' || c == '}'
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d [20]byte
	i := len(d)
	for n > 0 {
		i--
		d[i] = byte('0' + n%10)
		n /= 10
	}
	return string(d[i:])
}

// limitedBuffer 只保留前 max 字节的写入缓冲（超出部分丢弃，dropped 置位）。
// 永远返回 len(p), nil，保证 TeeReader 不因日志缓冲写满而中断数据流。
type limitedBuffer struct {
	buf     bytes.Buffer
	max     int
	dropped bool
}

// String 返回已镜像的字节（调用须在读尽/关闭之后）。
func (w *limitedBuffer) String() string { return w.buf.String() }

func (w *limitedBuffer) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		b := p
		if len(b) > room {
			b = b[:room]
			w.dropped = true
		}
		w.buf.Write(b)
	} else if len(p) > 0 {
		w.dropped = true
	}
	return len(p), nil
}

// wireDumpBody 流式响应体包装：原始字节全部透传给调用方（Read 原样返回），
// 同时把前 dump.max 字节镜像到日志缓冲；读尽（EOF）或 Close 时输出抓包日志。
//
// 注意：不能像请求体那样整体 ReadAll 后重建响应体——SSE 流式响应必须边读边转发，
// 否则调用方只能拿到截断后的前缀（曾导致流式回答被静默截断、JSON 残缺、
// 后续解析全部失败）。
type wireDumpBody struct {
	io.ReadCloser
	dump *limitedBuffer
	log  func(body string)
	once sync.Once
}

func (w *wireDumpBody) Read(p []byte) (int, error) {
	n, err := w.ReadCloser.Read(p)
	if n > 0 {
		_, _ = w.dump.Write(p[:n])
	}
	if err == io.EOF {
		w.finish()
	}
	return n, err
}

func (w *wireDumpBody) Close() error {
	err := w.ReadCloser.Close()
	w.finish()
	return err
}

func (w *wireDumpBody) finish() {
	w.once.Do(func() { w.log(w.dump.String()) })
}

func (t *wireDumpTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// 请求体快照（请求为一次性缓冲体，可整体读出后重建）
	if req.Body != nil {
		body, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err == nil {
			attrs := []slog.Attr{
				slog.String("method", req.Method),
				slog.String("url", req.URL.String()),
			}
			if hh := headerLogAttrs(req.Header); len(hh) > 0 {
				// slog.Group 接受的是 any（Attr 或 kv 对），这里已持有 []slog.Attr，
				// 直接用 GroupValue 组装避免额外的类型转换。
				attrs = append(attrs, slog.Attr{Key: "headers", Value: slog.GroupValue(hh...)})
			}
			attrs = append(attrs, slog.String("body", truncateBase64(string(body))))
			slog.LogAttrs(req.Context(), slog.LevelInfo, "llm wire request", attrs...)
			req.Body = io.NopCloser(bytes.NewReader(body))
		} else {
			// 读失败也要把 body 还原为可读流
			req.Body = io.NopCloser(bytes.NewReader(nil))
		}
	}
	start := time.Now()
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		slog.Warn("llm wire response error", slog.String("error", err.Error()),
			slog.Int64("latency_ms", time.Since(start).Milliseconds()))
		return resp, err
	}
	// 流式（SSE）响应：TeeReader 镜像前 16KB 用于日志，原始字节全部透传给调用方。
	dump := &limitedBuffer{max: 16 << 10}
	resp.Body = &wireDumpBody{
		ReadCloser: resp.Body,
		dump:       dump,
		log: func(s string) {
			followup := ""
			if dump.dropped {
				followup = "...[截断]"
			}
			slog.Info("llm wire response",
				slog.String("status", resp.Status),
				slog.Int64("latency_ms", time.Since(start).Milliseconds()),
				slog.String("body", truncateBase64(s)+followup),
			)
		},
	}
	return resp, nil
}
