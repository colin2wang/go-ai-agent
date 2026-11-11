package api

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"go-ai-agent/internal/config"
)

// validateFormParams 按子功能声明校验额外参数：目标语言未提交时用
// yml 的 default 补齐，且必须是 options 白名单内的值（启动期已校验 default 合法）。
func validateFormParams(params map[string]string, in config.Input) error {
	sel := in.TargetLanguage
	if sel == nil {
		return nil
	}
	lang := params[config.TargetLangField]
	if lang == "" {
		lang = sel.Default
		params[config.TargetLangField] = lang
	}
	for _, o := range sel.Options {
		if o == lang {
			return nil
		}
	}
	return fmt.Errorf("目标语言 %q 不在允许的选项中", lang)
}

// runFeatureCore 上传 + 执行 + 记录任务结果的公共流程（HTTP 与桌面端共用）。
// asyncOverride 为 nil 时跟随配置默认；异步模式登记任务后立即返回 job_id 快照。
func (s *Server) runFeatureCore(featureID string, srcs []uploadSource, params map[string]string, asyncOverride *bool) (*JobResult, error) {
	in := config.Input{}
	if f, ok := s.cfg.Feature(featureID); ok {
		in = f.Input
	}
	paths, err := s.saveUploads(in, srcs)
	if err != nil {
		return nil, err
	}
	if err := validateFormParams(params, in); err != nil {
		return nil, err
	}

	if s.asyncRun(featureID, asyncOverride) {
		job, err := s.startAsyncJob(featureID, paths, len(paths), params)
		if err != nil {
			return nil, fmt.Errorf("登记异步任务失败: %w", err)
		}
		return job, nil
	}

	start := time.Now()
	fs, runErr := s.runPipeline(featureID, paths, params)
	job := toJob(newJobID(), featureID, fs, runErr, start)
	s.mu.Lock()
	s.rememberJobLocked(job)
	s.mu.Unlock()
	return job, nil
}

// jobSeq 保证同一纳秒内并发生成的任务 ID 仍唯一。
var jobSeq atomic.Uint64

// newJobID 生成任务 ID（时间戳 + 全局计数，简单可读且无碰撞）。
func newJobID() string {
	return fmt.Sprintf("job-%d-%d", time.Now().UnixNano(), jobSeq.Add(1))
}

// ctxBackground 独立小函数便于测试替换。
func ctxBackground() context.Context { return context.Background() }
