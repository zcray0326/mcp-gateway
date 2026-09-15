package telemetry

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	labelMCPServerName   = "mcp_server_name"
	labelToolName        = "tool_name"
	labelToolCallOutcome = "outcome"
)

const (
	// 限制标签长度，避免异常名称放大指标存储和传输开销。
	attrValueMaxLen  = 64
	attrValueUnknown = "unknown"
)

// OtelCustomMetrics 封装 OpenTelemetry Counter 和 Histogram，是 CustomMetrics 的真实采集实现。
type OtelCustomMetrics struct {
	toolCalls       metric.Int64Counter
	toolCallLatency metric.Float64Histogram
}

// NewOtelCustomMetrics 创建工具调用计数器和延迟直方图。
// 桶边界覆盖毫秒到几十秒，既能观察常规调用，也能识别慢工具。
func NewOtelCustomMetrics(meter metric.Meter) (CustomMetrics, error) {
	if meter == nil {
		return nil, fmt.Errorf("meter cannot be nil")
	}

	toolInv, err := meter.Int64Counter(
		"mcp_gateway_tool_calls_total",
		metric.WithDescription("Total number of tool calls"),
		metric.WithUnit("1"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create tool calls counter: %w", err)
	}

	toolLat, err := meter.Float64Histogram(
		"mcp_gateway_tool_call_latency_seconds",
		metric.WithDescription("Latency of tool calls in seconds"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create tool latency histogram: %w", err)
	}

	return &OtelCustomMetrics{
		toolCalls:       toolInv,
		toolCallLatency: toolLat,
	}, nil
}

func (m *OtelCustomMetrics) RecordToolCall(
	ctx context.Context, mcpServerName, toolName string, outcome ToolCallOutcome, elapsedTime time.Duration,
) {
	attrs := []attribute.KeyValue{
		attribute.String(labelMCPServerName, boundString(mcpServerName)),
		attribute.String(labelToolName, boundString(toolName)),
		attribute.String(labelToolCallOutcome, string(outcome)),
	}
	m.toolCalls.Add(ctx, 1, metric.WithAttributes(attrs...))
	m.toolCallLatency.Record(ctx, elapsedTime.Seconds(), metric.WithAttributes(attrs...))
}

func (m *OtelCustomMetrics) RecordPromptCall(
	ctx context.Context, mcpServerName, promptName string, outcome PromptCallOutcome, elapsedTime time.Duration,
) {
	attrs := []attribute.KeyValue{
		attribute.String(labelMCPServerName, boundString(mcpServerName)),
		attribute.String(labelToolName, boundString(promptName)),
		attribute.String(labelToolCallOutcome, string(outcome)),
	}
	m.toolCalls.Add(ctx, 1, metric.WithAttributes(attrs...))
	m.toolCallLatency.Record(ctx, elapsedTime.Seconds(), metric.WithAttributes(attrs...))
}

// boundString 将空标签归一为 unknown，并截断过长值，控制监控系统中的标签基数和数据体积。
func boundString(s string) string {
	if s == "" {
		return attrValueUnknown
	}
	if len(s) > attrValueMaxLen {
		return s[:attrValueMaxLen]
	}
	return s
}
