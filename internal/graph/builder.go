package graph

import (
	"context"
	"fmt"

	lggraph "github.com/smallnest/langgraphgo/graph"

	"go-ai-agent/internal/graph/nodes"
)

// Build 按子功能声明的 workflow.type 组装对应的 StateGraph。
//
// 注：langgraphgo v0.1.0 的 StateGraph 为 interface{} 状态（非泛型），
// 节点内部对 *nodes.State 做类型断言。
func Build(deps nodes.Deps) (*lggraph.StateRunnable, error) {
	switch deps.Workflow {
	case "payslip-ocr", "":
		return buildPayslip(deps)
	case "book-toc":
		return buildBookTOC(deps)
	default:
		return nil, fmt.Errorf("未知工作流类型 %q，请检查 features/*.yml 的 workflow.type", deps.Workflow)
	}
}

// buildPayslip 工资单审读（含条件边回退重试）：
//
//	entry → ocr(VLM) → translate → proofread →(条件边)→ expert → analyze → END
//	                                    ↓(校验失败且未超重试上限) 回退重试 ocr
//
// 重试上限取自子功能配置 workflow.max-retries（deps.MaxRetries）。
func buildPayslip(deps nodes.Deps) (*lggraph.StateRunnable, error) {
	maxRetries := deps.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 2
	}
	g := lggraph.NewStateGraph()
	g.AddNode("ocr", wrap(nodes.OCR(deps)))
	g.AddNode("translate", wrap(nodes.Translate(deps)))
	g.AddNode("proofread", wrap(nodes.Proofread(deps)))
	g.AddNode("expert", wrap(nodes.Expert(deps)))
	g.AddNode("analyze", wrap(nodes.Analyze(deps)))
	g.SetEntryPoint("ocr")

	g.AddEdge("ocr", "translate")
	g.AddEdge("translate", "proofread")
	g.AddConditionalEdge("proofread", func(ctx context.Context, state interface{}) string {
		s, ok := state.(*nodes.State)
		if !ok {
			return "expert"
		}
		// proofread 失败或发现问题且未超上限 → 回退重试 ocr
		if (s.Err != nil || len(s.ReviewNotes) > 0) && s.RetryCount["ocr"] < maxRetries {
			if s.RetryCount == nil {
				s.RetryCount = map[string]int{}
			}
			s.RetryCount["ocr"]++
			return "ocr"
		}
		return "expert"
	})
	g.AddEdge("expert", "analyze")
	g.AddEdge("analyze", lggraph.END)

	return g.Compile()
}

// buildBookTOC 扫描版图书目录 → 文字版目录（线性三段，渲染为本地模板执行）：
//
//	entry → ocr(逐页 VLM) → merge(跨页合并校对) → render(模板渲染) → END
func buildBookTOC(deps nodes.Deps) (*lggraph.StateRunnable, error) {
	g := lggraph.NewStateGraph()
	g.AddNode("ocr", wrap(nodes.TocOCR(deps)))
	g.AddNode("merge", wrap(nodes.TocMerge(deps)))
	g.AddNode("render", wrap(nodes.TocRender(deps)))
	g.SetEntryPoint("ocr")

	g.AddEdge("ocr", "merge")
	g.AddEdge("merge", "render")
	g.AddEdge("render", lggraph.END)

	return g.Compile()
}

// wrap 将强类型节点函数适配为 langgraphgo 的 interface{} 签名。
func wrap(fn func(ctx context.Context, s nodes.State) (nodes.State, error)) func(ctx context.Context, state interface{}) (interface{}, error) {
	return func(ctx context.Context, state interface{}) (interface{}, error) {
		s, ok := state.(*nodes.State)
		if !ok {
			s = &nodes.State{}
		}
		next, err := fn(ctx, *s)
		return &next, err
	}
}
