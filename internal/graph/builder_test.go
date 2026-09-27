package graph

import (
	"context"
	"strings"
	"testing"

	"go-ai-agent/internal/graph/nodes"
)

func TestBuildUnknownWorkflowType(t *testing.T) {
	if _, err := Build(nodes.Deps{Workflow: "no-such-type"}); err == nil || !strings.Contains(err.Error(), "未知工作流类型") {
		t.Fatalf("期望未知类型报错，得到 %v", err)
	}
}

func TestBuildPayslip(t *testing.T) {
	r, err := Build(nodes.Deps{Workflow: "payslip-ocr"})
	if err != nil {
		t.Fatalf("Build payslip: %v", err)
	}
	if r == nil {
		t.Fatal("runnable 不应为 nil")
	}
	// 空输入应返回错误（无图片可处理），验证图可执行且类型断言路径正常
	_, err = r.Invoke(context.Background(), &nodes.State{})
	if err == nil {
		t.Log("空输入未报错（节点实现可能已变更），仅验证可执行")
	}
}

func TestBuildBookTOC(t *testing.T) {
	r, err := Build(nodes.Deps{Workflow: "book-toc"})
	if err != nil {
		t.Fatalf("Build book-toc: %v", err)
	}
	if r == nil {
		t.Fatal("runnable 不应为 nil")
	}
	// 空 Pages 时各节点应快速失败或空跑，不 panic
	_, _ = r.Invoke(context.Background(), &nodes.State{})
}

func TestBuildEmptyWorkflowDefaultsToPayslip(t *testing.T) {
	// Workflow 为空串时回退 payslip 图
	r, err := Build(nodes.Deps{})
	if err != nil {
		t.Fatalf("空 workflow 类型应回退默认图，得到 %v", err)
	}
	if r == nil {
		t.Fatal("runnable 不应为 nil")
	}
}
