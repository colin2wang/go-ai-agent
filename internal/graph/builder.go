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
	case "subtitle-translate":
		return buildSubtitleTranslate(deps)
	default:
		return nil, fmt.Errorf("未知工作流类型 %q，请检查 features/*.yml 的 workflow.type", deps.Workflow)
	}
}

// buildPayslip 工资单提取合并（简化后仅 ocr 一步）：
//
//	entry → ocr(VLM) → END
//
// ocr 节点把每张截图的工资项目数据识别并合并为一份 JSON，不做解释/分析。
// 如需审读/翻译/分析报告，在 payslip-ocr.yml 追加步骤并在此扩容图即可。
func buildPayslip(deps nodes.Deps) (*lggraph.StateRunnable, error) {
	g := lggraph.NewStateGraph()
	g.AddNode("ocr", wrap(nodes.OCR(deps)))
	g.SetEntryPoint("ocr")
	g.AddEdge("ocr", lggraph.END)

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

// buildSubtitleTranslate SRT 字幕 → 目标语言 SRT（线性三段，仅中间段调用 LLM）：
//
//	entry → parse(本地解析) → translate(分批 LLM) → render(本地重组) → END
func buildSubtitleTranslate(deps nodes.Deps) (*lggraph.StateRunnable, error) {
	g := lggraph.NewStateGraph()
	g.AddNode("parse", wrap(nodes.SubtitleParse(deps)))
	g.AddNode("translate", wrap(nodes.SubtitleTranslate(deps)))
	g.AddNode("render", wrap(nodes.SubtitleRender(deps)))
	g.SetEntryPoint("parse")

	g.AddEdge("parse", "translate")
	g.AddEdge("translate", "render")
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
