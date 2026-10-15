// Package llm 收口通用模型调用：单次结构化输出、严格解码与 usage 回传。
// 它不理解故事——提示词、输入输出契约与调用理由由各使用方就近持有
// （架构文档 §12.1 原则 4）；Agent 循环与工具执行仍归 capability。
package llm

import (
	"context"
	"fmt"
	"reflect"
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
	// Check 在严格解码后校验输出合不合调用方的契约：Schema 表达不了的（逐处都有结论、章号
	// 只能是已写的章），以及服务商不强制的（DeepSeek 等的 Schema 输出只是提示词加 JSON 模式）。
	// 为空时只做严格解码。
	Check func() error
}

// Structured 用绑定的模型（含其思考强度）发起一次严格 JSON Schema 约束的流式调用，
// 并把响应严格解码到 out、经 Check 校验；暂时性失败按 models.Retry 退避重发，最终失败经
// models.Classify 标上类别。输出不合契约时把原因告诉模型重给一次，再不合就是模型一侧的
// 失败（model_malformed）。返回本次调用（含重给）的 usage 供调用方落审计事件。
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
	var usage litellm.Usage
	for repaired := false; ; repaired = true {
		response, err := complete(ctx, chat.Client, request, call)
		if err != nil {
			return usage, err
		}
		usage.Add(response.Usage)
		err = decode(response.Text(), out, call.Check)
		if err == nil {
			return usage, nil
		}
		if repaired {
			// 模型一侧的失败，不是调用方给错了：不沿用校验错误的类别（如 ErrInvalid），免得被
			// 读成输入不合规。
			return usage, &model.Failure{Code: model.FailureModelMalformed, Err: fmt.Errorf("structured response %q: %v", call.Schema.Name, err)}
		}
		request.Messages = append(request.Messages, litellm.AssistantText(response.Text()),
			litellm.UserText("这份输出不合要求："+err.Error()+"。按同样的要求重新给出完整的输出。"))
	}
}

// complete 发出一次调用：暂时性失败按 models.Retry 退避重发。
func complete(ctx context.Context, client *litellm.Client, request litellm.Request, call Call) (*litellm.Response, error) {
	policy := models.Retry()
	for attempt := 1; ; attempt++ {
		response, err := stream(ctx, client, request, call.Observe)
		if err == nil {
			return response, nil
		}
		if ctx.Err() != nil || !litellm.IsTemporaryError(err) || attempt >= policy.MaxAttempts {
			return nil, models.Classify(err)
		}
		delay, ok := policy.Delay(attempt, litellm.RetryAfter(err))
		if !ok {
			return nil, models.Classify(err)
		}
		if call.Retried != nil {
			call.Retried(attempt+1, delay, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
}

// decode 把输出严格解码到清零的 out 再校验：重给的输出不能带上前一份残留的字段或键。
func decode(text string, out any, check func() error) error {
	reflect.ValueOf(out).Elem().SetZero()
	if err := model.DecodeStrict([]byte(text), out); err != nil {
		return err
	}
	if check == nil {
		return nil
	}
	return check()
}

func stream(ctx context.Context, client *litellm.Client, request litellm.Request, observe func(litellm.Event) error) (*litellm.Response, error) {
	events, err := client.Stream(ctx, request)
	if err != nil {
		return nil, err
	}
	defer events.Close()
	response, err := litellm.Handle(events, observe)
	if err == nil && response.FinishReason == litellm.FinishReasonError {
		// 厂商以错误结束的响应多是一过性的（服务过载、模型写坏了输出），同 agentcore 一样当暂时性失败重发。
		return nil, &litellm.Error{Type: litellm.ErrorTypeProvider, Provider: response.Provider, Message: "the model ended its response with " + response.FinishReasonRaw, Temporary: true}
	}
	return response, err
}
