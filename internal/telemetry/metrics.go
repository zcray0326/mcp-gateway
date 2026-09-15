package telemetry

import (
	"context"
	"time"
)

// ToolCallOutcome 与 PromptCallOutcome 将结果限制为稳定枚举，避免指标标签值无限增长。
type (
	ToolCallOutcome   string
	PromptCallOutcome string
)

const (
	// ToolCallOutcomeSuccess 表示工具调用成功。
	ToolCallOutcomeSuccess ToolCallOutcome = "success"
	// ToolCallOutcomeError 表示工具调用失败。
	ToolCallOutcomeError ToolCallOutcome = "error"
)

const (
	// PromptCallOutcomeSuccess 表示 Prompt 获取成功。
	PromptCallOutcomeSuccess PromptCallOutcome = "success"
	// PromptCallOutcomeError 表示 Prompt 获取失败。
	PromptCallOutcomeError PromptCallOutcome = "error"
)

// CustomMetrics 隔离业务层与具体监控 SDK。
// 启用监控时注入 OpenTelemetry 实现，关闭时注入 no-op 实现，业务代码无需散落 enabled 和 nil 判断。
type CustomMetrics interface {
	// RecordToolCall 记录工具调用次数、耗时和结果。
	RecordToolCall(ctx context.Context, serverName, toolName string, outcome ToolCallOutcome, elapsedTime time.Duration)

	// RecordPromptCall 记录 Prompt 获取次数、耗时和结果。
	RecordPromptCall(ctx context.Context, serverName, promptName string, outcome PromptCallOutcome, elapsedTime time.Duration)
}
