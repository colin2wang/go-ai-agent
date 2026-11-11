package api

import (
	"context"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"go-ai-agent/internal/llm"
	"go-ai-agent/internal/stats"
)

// FileUpload 桌面端上传文件：前端读入字节后以 base64 传入（Data 不含 data: 前缀）。
type FileUpload struct {
	Name string `json:"name"`
	Data string `json:"data"` // 纯 base64
}

// App Wails 绑定层：前端经 window.go.main.App.* 直接调用（不再有 HTTP 服务）。
// 聊天流式响应通过 Wails 事件推送（频道 "chat:<reqID>"，事件 meta/delta/done/error/collect）。
type App struct {
	srv *Server

	ctx     context.Context // Wails 运行时上下文（OnStartup 注入，用于 EventsEmit）
	mu      sync.Mutex
	cancels map[string]context.CancelFunc // reqID → 取消函数（停止生成用）
}

// NewApp 构造绑定层（须在 wails.Run 的 Bind 中注册）。
func NewApp(srv *Server) *App {
	return &App{srv: srv, cancels: map[string]context.CancelFunc{}}
}

// OnStartup 由 Wails 在窗口启动后调用，注入运行时上下文。
func (a *App) OnStartup(ctx context.Context) {
	a.ctx = ctx
}

// appInfo 应用元信息（前端页头 / 概览页用）。
func (a *App) appInfo() map[string]string {
	return map[string]string{"name": a.srv.cfg.App.Name, "title": a.srv.cfg.App.Title, "version": a.srv.Version}
}

// Features 子功能清单（左侧菜单 + 流程链 + 上传控件声明）。
func (a *App) Features() map[string]any {
	return map[string]any{
		"app":       a.appInfo(),
		"features":  a.srv.featureInfos(),
		"providers": a.srv.providerInfos(),
	}
}

// Overview 概览统计（days 为统计天数，缺省/非法值取 14，上限 180）。
func (a *App) Overview(days int) (map[string]any, error) {
	if days <= 0 || days > 180 {
		days = 14
	}
	var ov *stats.Overview
	if a.srv.recorder != nil {
		o, err := a.srv.recorder.Overview(days)
		if err != nil {
			return nil, fmt.Errorf("聚合统计失败: %w", err)
		}
		ov = o
	}
	return map[string]any{
		"app":         a.appInfo(),
		"providers":   a.srv.providerInfos(),
		"stats":       ov,
		"days":        days,
		"server_time": time.Now().Format(time.RFC3339),
	}, nil
}

// Health 桌面模式下后端与窗口同生命周期，恒可用。
func (a *App) Health() bool { return true }

// RunFeature 执行子功能工作流。asyncOverride 为 nil 时跟随配置默认（persist-jobs）；
// 异步模式立即返回 job_id 快照（status=processing），随后用 Job 轮询进度与终态。
func (a *App) RunFeature(featureID string, files []FileUpload, extra map[string]string, asyncOverride *bool) (*JobResult, error) {
	if _, ok := a.srv.pipelines[featureID]; !ok {
		return nil, fmt.Errorf("子功能未启用或不存在: %s", featureID)
	}
	if f, ok := a.srv.cfg.Feature(featureID); ok && f.IsChat() {
		return nil, fmt.Errorf("对话类子功能不支持上传执行，请使用 ChatSend")
	}
	srcs := make([]uploadSource, 0, len(files))
	for _, fu := range files {
		data, err := base64.StdEncoding.DecodeString(fu.Data)
		if err != nil {
			return nil, fmt.Errorf("文件 %s 不是合法 base64: %w", fu.Name, err)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("文件 %s 内容为空", fu.Name)
		}
		srcs = append(srcs, sourceFromBytes(fu.Name, data))
	}
	return a.srv.runFeatureCore(featureID, srcs, extra, asyncOverride)
}

// Job 按 jobID 查询任务进度与结果（异步任务轮询用）。
func (a *App) Job(id string) (*JobResult, error) {
	job, ok := a.srv.lookupJob(id)
	if !ok {
		return nil, fmt.Errorf("job not found: %s", id)
	}
	return job, nil
}

// ProviderCheck 端点连通性探测：provider 为空则并发测试全部端点。
func (a *App) ProviderCheck(provider string) map[string]any {
	names := make([]string, 0, len(a.srv.cfg.LLM.Providers))
	if provider != "" {
		names = append(names, provider)
	} else {
		for _, p := range a.srv.cfg.LLM.Providers {
			names = append(names, p.Name)
		}
	}
	results := make([]llm.ProbeResult, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			results[i] = a.srv.factory.Probe(name)
		}(i, name)
	}
	wg.Wait()
	return map[string]any{"results": results}
}

// ProviderModels 拉取指定 provider 的模型列表（代理 OpenAI 兼容 GET /v1/models）。
func (a *App) ProviderModels(provider string) (map[string]any, error) {
	return a.srv.providerModels(a.requestContext(), provider)
}

// ModelCaps 探测并缓存模型能力（vision/reasoning/stream）。
func (a *App) ModelCaps(provider, model string, force bool) (map[string]any, error) {
	return a.srv.modelCapsFor(a.requestContext(), provider, model, force)
}

// StreamCheck 探测端点是否支持流式返回。
func (a *App) StreamCheck(provider, model string) (map[string]any, error) {
	return a.srv.streamCheck(a.requestContext(), provider, model)
}

// ChatSend 发送对话消息。流式模式下逐事件推送 Wails 事件（频道 "chat:<reqID>"，
// 载荷 {"event": "...", "data": {...}}，事件同旧版 SSE：meta/delta/done/error，
// 端点不支持流式自动降级时发 collect 承载一次性完整结果），并返回 mode=stream；
// 非流式直接返回 mode=collect + 完整结果。reqID 由前端生成，用于事件订阅与取消。
func (a *App) ChatSend(reqID string, req chatSendRequest) (map[string]any, error) {
	if reqID == "" {
		reqID = newJobID()
	}
	cp, err := a.srv.chatPrepare(req)
	if err != nil {
		return nil, err
	}
	if !cp.stream {
		out, err := a.srv.collectChatCore(a.requestContext(), cp.provider, cp, "")
		if err != nil {
			return nil, err
		}
		out["mode"] = "collect"
		return out, nil
	}

	// 流式：可取消的独立上下文，事件经 WailS 运行时推送到 "chat:<reqID>" 频道
	ctx, cancel := context.WithCancel(a.requestContext())
	a.mu.Lock()
	a.cancels[reqID] = cancel
	a.mu.Unlock()
	defer func() {
		cancel()
		a.mu.Lock()
		delete(a.cancels, reqID)
		a.mu.Unlock()
	}()

	sink := &eventSink{ctx: a.ctx, channel: "chat:" + reqID}
	if err := a.srv.streamChatCore(ctx, sink, cp.provider, cp); err != nil {
		sink.Emit("error", map[string]any{"error": err.Error()})
	}
	return map[string]any{"mode": "stream", "provider": cp.provider.Name, "model": cp.model}, nil
}

// ChatCancel 取消一次进行中的流式生成（对应 ChatSend 的 reqID）。
func (a *App) ChatCancel(reqID string) {
	a.mu.Lock()
	cancel, ok := a.cancels[reqID]
	a.mu.Unlock()
	if ok {
		cancel()
	}
}

// requestContext 返回可用上下文：Wails 启动前（测试场景）回退 Background。
func (a *App) requestContext() context.Context {
	if a.ctx != nil {
		return a.ctx
	}
	return context.Background()
}

// eventSink 把 chat 流事件转发为 Wails 运行时事件。
type eventSink struct {
	ctx     context.Context
	channel string
}

// Emit 推送单个事件：载荷统一为 {"event": 名称, "data": 负载}。
func (s *eventSink) Emit(event string, payload any) {
	if s.ctx == nil {
		return // 窗口未就绪（异常路径），丢弃事件避免 panic
	}
	runtime.EventsEmit(s.ctx, s.channel, map[string]any{"event": event, "data": payload})
}
