// Package llm 收口通用模型调用：单次结构化输出、严格解码与 usage 回传。
// 它不理解故事——提示词、输入输出契约与调用理由由各使用方就近持有
// （架构文档 §12.1 原则 4）；Agent 循环与工具执行仍归 capability。
package llm

import (
	"context"
	"fmt"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/ainovel-cli/internal/infra/llm/models"
	"github.com/voocel/litellm"
)

// Schema 是一次结构化调用的输出约束。
type Schema struct {
	Name        string
	Description string
	JSON        map[string]any
}

// Call 是一次单轮结构化调用的完整输入。CacheKey 与 SessionID 由调用方
// 按项目、契约版本与输入摘要派生，保证同一判断可复现。
type Call struct {
	System    string
	Input     string
	Schema    Schema
	CacheKey  string
	SessionID string
	// Observe 按到达顺序收到流式事件（思考与输出增量、用量、结束）；返回错误即中止调用。
	// 为空时只等完整结果。
	Observe func(litellm.Event) error
	// Retried 在暂时性失败后、等 delay 重发前收到第 attempt 次重发的通知；重发的流从头再来。
	Retried func(attempt int, delay time.Duration, err error)
}

// Structured 用绑定的模型（含其思考强度）发起一次严格 JSON Schema 约束的流式调用，
// 并把响应严格解码到 out；暂时性失败按 models.Retry 退避重发，最终失败经 models.Classify
// 标上类别。返回本次调用的 usage 供调用方落审计事件。
func Structured(ctx context.Context, binding models.Binding, call Call, out any) (litellm.Usage, error) {
	format, err := litellm.NewResponseFormatJSONSchema(call.Schema.Name, call.Schema.Description, call.Schema.JSON)
	if err != nil {
		return litellm.Usage{}, fmt.Errorf("structured call %q schema: %w", call.Schema.Name, err)
	}
	format.JSONSchema.Strict = new(true)
	chat := binding.Routed(call.CacheKey, call.SessionID)
	request := chat.Request
	request.Messages = []litellm.Message{litellm.System(call.System), litellm.UserText(call.Input)}
	request.ResponseFormat = format
	policy := models.Retry()
	var response *litellm.Response
	for attempt := 1; ; attempt++ {
		response, err = stream(ctx, chat.Client, request, call.Observe)
		if err == nil {
			break
		}
		if ctx.Err() != nil || !litellm.IsTemporaryError(err) || attempt >= policy.MaxAttempts {
			return litellm.Usage{}, models.Classify(err)
		}
		delay, ok := policy.Delay(attempt, litellm.RetryAfter(err))
		if !ok {
			return litellm.Usage{}, models.Classify(err)
		}
		if call.Retried != nil {
			call.Retried(attempt+1, delay, err)
		}
		select {
		case <-ctx.Done():
			return litellm.Usage{}, ctx.Err()
		case <-time.After(delay):
		}
	}
	if err := model.DecodeStrict([]byte(response.Text()), out); err != nil {
		return litellm.Usage{}, fmt.Errorf("decode structured response %q: %w", call.Schema.Name, err)
	}
	return response.Usage, nil
}

func stream(ctx context.Context, client *litellm.Client, request litellm.Request, observe func(litellm.Event) error) (*litellm.Response, error) {
	events, err := client.Stream(ctx, request)
	if err != nil {
		return nil, err
	}
	defer events.Close()
	return litellm.Handle(events, observe)
}
