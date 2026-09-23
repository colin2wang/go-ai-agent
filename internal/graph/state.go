// Package graph 定义工作流状态与 langgraphgo StateGraph 组装。
package graph

import (
	"go-ai-agent/internal/graph/nodes"
)

// State 全流程共享状态；实体定义在 nodes 包（避免 graph ↔ nodes 循环引用）。
type State = nodes.State
