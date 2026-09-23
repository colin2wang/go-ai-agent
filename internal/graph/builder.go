package graph

import (
	"context"
	lggraph "github.com/smallnest/langgraphgo/graph"

	"go-ai-agent/internal/graph/nodes"
)

// Build 组装 StateGraph：
//
//	entry → ocr → translate → proofread →(条件边)→ expert → analyze → END
//	                              ↓(校验失败且未超重试上限) 回退重试 ocr
//
// 注：langgraphgo v0.1.0 的 StateGraph 为 interface{} 状态（非泛型），
// 节点内部对 *nodes.State 做类型断言。
func Build(deps nodes.Deps) (*lggraph.StateRunnable, error) {
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
		// proofread 失败或发现问题且未超上限 → 重试 ocr（最多 2 次）
		if (s.Err != nil || len(s.ReviewNotes) > 0) && s.RetryCount["ocr"] < 2 {
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
