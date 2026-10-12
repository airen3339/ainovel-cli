package capability

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/ainovel-cli/internal/infra/activity"
	"github.com/voocel/ainovel-cli/internal/infra/capability/prompt"
	"github.com/voocel/litellm"
)

// Execute 产出统一 OperationOutcome（D30）：Worker 工具集决定收尾方式——携带
// verdict_submit 的审阅/校验类以结构化 Verdict 收尾，其余以 Proposal 收尾。
func (r *Runtime) Execute(ctx context.Context, operation model.Operation) (model.OperationOutcome, error) {
	if !r.Bound() {
		return model.OperationOutcome{}, fmt.Errorf("capability model is not bound: %w", model.ErrInvalid)
	}
	if operation.State != model.OperationRunning {
		return model.OperationOutcome{}, fmt.Errorf("operation %q is %s: %w", operation.ID, operation.State, model.ErrStateConflict)
	}
	if operation.Snapshot.Executor != r.Identity() {
		return model.OperationOutcome{}, fmt.Errorf("operation executor %q does not match runtime %q: %w", operation.Snapshot.Executor, r.Identity(), model.ErrStateConflict)
	}
	task, err := model.DecodeTaskInput(operation.Kind, operation.Input)
	if err != nil {
		return model.OperationOutcome{}, err
	}
	scope := taskActivityScope(task)
	r.publishTask(operation, activity.TaskStart, scope)
	defer r.publishTask(operation, activity.TaskEnd, scope)
	compiled, err := r.prompts.Load(ctx, operation.Snapshot.ConfigDigest)
	if err != nil {
		return model.OperationOutcome{}, err
	}
	// 本次尝试的模型按 Worker 角色取绑定，读一次沿用到结束（D57）。
	worker, err := prompt.BuiltinWorkerProfile(prompt.WorkerID(compiled.WorkerProfile))
	if err != nil {
		return model.OperationOutcome{}, err
	}
	binding, _ := r.bindingFor(worker.ModelRole)
	wantsVerdict := false
	for _, definition := range compiled.Tools {
		if definition.Name == prompt.ToolVerdictSubmit {
			wantsVerdict = true
		}
	}

	// 工具串行执行（都不声明可并行）；提交与停止判定在同一循环内，结果只在循环结束后读取。
	var submitted *model.Proposal
	var verdict *model.ReviewVerdict
	tools, err := r.toolsFor(operation, compiled, func(proposal model.Proposal) (model.Proposal, error) {
		if submitted != nil {
			same, err := sameSubmission(*submitted, proposal)
			if err != nil {
				return model.Proposal{}, err
			}
			if !same {
				return model.Proposal{}, fmt.Errorf("operation already submitted a different proposal: %w", model.ErrIdempotencyConflict)
			}
			return *submitted, nil
		}
		copy := proposal
		submitted = &copy
		return copy, nil
	}, func(candidate model.ReviewVerdict) (model.ReviewVerdict, error) {
		if verdict != nil {
			same, err := sameVerdict(*verdict, candidate)
			if err != nil {
				return model.ReviewVerdict{}, err
			}
			if !same {
				return model.ReviewVerdict{}, fmt.Errorf("operation already submitted a different verdict: %w", model.ErrIdempotencyConflict)
			}
			return *verdict, nil
		}
		copy := candidate
		verdict = &copy
		return copy, nil
	})
	if err != nil {
		return model.OperationOutcome{}, err
	}
	// 会话血统混入模型配置摘要：换模型或思考强度后的尝试是新会话（§7 缓存纪律）。
	cacheKey, err := prompt.CacheKey(operation.Target.ID, compiled.WorkerProfile, compiled.ProfileDigest, operation.ID+":"+binding.Digest)
	if err != nil {
		return model.OperationOutcome{}, err
	}

	recoveredMessages, lastFailure, err := r.restoreMessages(ctx, operation)
	if err != nil {
		return model.OperationOutcome{}, err
	}
	// 每次尝试记下实际使用的模型：快照不冻结模型配置，历史由尝试摘要解释（D57）。
	if err := r.store.RecordAgentBinding(ctx, operation.ID, operation.Attempt, model.AgentBinding{
		Role: worker.ModelRole, Provider: binding.Provider, Model: binding.Model, Thinking: binding.Thinking, Digest: binding.Digest,
	}, r.now()); err != nil {
		return model.OperationOutcome{}, err
	}
	workspaceArtifacts, err := r.store.ListWorkspaceArtifacts(ctx, operation.ID)
	if err != nil {
		return model.OperationOutcome{}, err
	}
	if resumed := resumedProgress(len(recoveredMessages), len(workspaceArtifacts)); resumed != "" {
		r.publishNotice(operation, activity.ToneRetry, resumed)
	}
	// 恢复提示只能追加在完整任务上下文之后，不得替换 Dynamic Tail：restart 的
	// 新任务同样必须拿到 project_rules、story_context 与 operation_task。
	promptText := compiled.DynamicTail
	if len(recoveredMessages) > 0 || len(workspaceArtifacts) > 0 || lastFailure != "" {
		promptText = fmt.Sprintf(
			"%s\n\n继续已有 Operation 工作区：当前是第 %d 次尝试，已恢复 %d 条消息和 %d 个工件。先用 workspace_list 核对版本，从已有工件继续，不要从头重写。",
			compiled.DynamicTail, operation.Attempt, len(recoveredMessages), len(workspaceArtifacts),
		)
	}
	// 重开的会话必须知道上次为什么失败（D56）：否则只是换个尝试号重复同样的错。
	if lastFailure != "" {
		promptText += fmt.Sprintf("\n上一次尝试失败原因：%s。先针对这个原因修正，再继续。", lastFailure)
	}

	executionCtx, stopExecution := context.WithCancelCause(ctx)
	defer stopExecution(nil)
	stream := newLiveStream(task) // 随本次执行生灭：流式工具调用的身份与参数流的提取状态
	var end agentcore.RunEnd
	config := agentcore.Config{
		Model:      binding.Routed(cacheKey, ""),
		System:     []litellm.Block{litellm.TextBlock{Text: compiled.StablePrefix, Cache: &litellm.CacheControl{}}},
		Tools:      tools,
		Middleware: []agentcore.ToolMiddleware{submissionGuard(stopExecution)},
		Cache:      &litellm.CacheControl{},
		// 每条进入会话的消息先落盘：落盘失败即停止本次执行，提交结果因此先于收尾持久化。
		// 用量与工具报错在这里从消息取出，随同一次写入并入尝试摘要（D72）。
		Emit: func(event agentcore.Event) error {
			switch e := event.(type) {
			case agentcore.MessageEnd:
				payload, err := json.Marshal(e.Message)
				if err != nil {
					return fmt.Errorf("encode agent message: %w", err)
				}
				if _, err := r.store.AppendTranscript(ctx, model.TranscriptEntry{
					OperationID: operation.ID, Attempt: operation.Attempt,
					Kind: model.TranscriptMessage, Payload: payload, CreatedAt: e.Message.Time,
				}, messageUsage(e.Message.Usage), toolErrors(e.Message)); err != nil {
					return err
				}
			case agentcore.RunEnd:
				end = e
			}
			r.publishActivity(operation, event, stream)
			return nil
		},
		// 提交工具成功即收尾（Terminate），不额外请求模型；未提交就想停则提醒继续。
		OnStop: func(_ context.Context, info agentcore.StopInfo) ([]agentcore.Message, error) {
			if submitted != nil || verdict != nil {
				return nil, nil
			}
			return []agentcore.Message{agentcore.UserText(stopReminder(wantsVerdict, info.Message))}, nil
		},
	}
	_, executionErr := agentcore.Run(executionCtx, config, recoveredMessages, agentcore.UserText(promptText))
	if executionErr != nil {
		return model.OperationOutcome{}, fmt.Errorf("capability agent failed: %w", executionErr)
	}
	if wantsVerdict {
		if verdict == nil {
			return model.OperationOutcome{}, endedWithout(end, ErrNoVerdict)
		}
		payload, err := json.Marshal(*verdict)
		if err != nil {
			return model.OperationOutcome{}, fmt.Errorf("encode review verdict: %w", err)
		}
		return model.OperationOutcome{Verdict: payload}, nil
	}
	if submitted == nil {
		return model.OperationOutcome{}, endedWithout(end, ErrNoProposal)
	}
	return model.OperationOutcome{Proposal: submitted}, nil
}

// stopReminder 提醒没提交就想停的模型继续。只有思考、没有输出的回复多半是思考在中途
// 被截断：中转常把截断报成正常结束，agentcore 的续写接不住，这里让它接着已有的分析行动，
// 而不是从头审题再想一遍。
func stopReminder(wantsVerdict bool, last agentcore.Message) string {
	next := "任务尚未形成 Proposal。请继续使用工作区工具完成候选，并调用 proposal_submit；如无法完成，明确返回工具错误。"
	if wantsVerdict {
		next = "任务尚未提交裁定。请完成审阅并调用 verdict_submit 提交覆盖请求范围的结构化裁定；如无法完成，明确返回工具错误。"
	}
	if last.Text() == "" && last.Reasoning() != "" {
		return "上一条回复只有思考、没有输出，多半在思考中途被截断。不要从头重新审题，接着已有的分析直接调用工具。" + next
	}
	return next
}

// activityEvent 是这次执行发出的一条活动事件：归属、任务名与时刻统一在这里填。
func (r *Runtime) activityEvent(operation model.Operation, kind activity.Kind) activity.Event {
	event := activity.Event{
		ProjectID: operation.Target.ID, RunID: operation.RunID, OperationID: operation.ID,
		Kind: kind, At: r.now(),
	}
	if spec, err := model.KindSpec(operation.Kind); err == nil {
		event.TaskLabel = spec.Label
	}
	return event
}

func (r *Runtime) publishTask(operation model.Operation, kind activity.Kind, scope activity.Scope) {
	if r.activity == nil {
		return
	}
	event := r.activityEvent(operation, kind)
	event.Scope = scope
	r.activity.Publish(event)
}

func (r *Runtime) publishNotice(operation model.Operation, tone activity.Tone, text string) {
	if r.activity == nil {
		return
	}
	event := r.activityEvent(operation, activity.Notice)
	event.Tone, event.Text = tone, text
	r.activity.Publish(event)
}

// resumedProgress 说明这次执行从哪里接续：恢复的对话与工作稿；从头开始返回空串。
func resumedProgress(messages, artifacts int) string {
	var parts []string
	if messages > 0 {
		parts = append(parts, fmt.Sprintf("%d 条对话", messages))
	}
	if artifacts > 0 {
		parts = append(parts, fmt.Sprintf("%d 份工作稿", artifacts))
	}
	if len(parts) == 0 {
		return ""
	}
	return "接着上次的进度继续：已恢复 " + strings.Join(parts, "、")
}

// messageUsage 是一条消息花掉的模型用量；用户消息、工具结果没有用量，为零。
func messageUsage(usage *agentcore.Usage) model.Usage {
	if usage == nil {
		return model.Usage{}
	}
	spent := model.Usage{Input: usage.InputTokens, Output: usage.OutputTokens, CacheRead: usage.CacheReadTokens}
	if usage.Cost != nil {
		spent.Cost = usage.Cost.Total
	}
	return spent
}

// toolErrors 是这条消息计入尝试摘要的工具报错数：agentcore 的工具消息只回答一次调用，
// 报错记 1；诊断只读这个计数（D59、D72）。
func toolErrors(message agentcore.Message) int {
	if result, ok := message.ToolResult(); ok && result.IsError {
		return 1
	}
	return 0
}

func taskActivityScope(task model.TaskInput) activity.Scope {
	switch input := task.(type) {
	case *model.WriteChapterInput:
		return activity.Scope{ChapterNumber: input.ChapterNumber, PlanNodeID: input.ChapterPlanID}
	case *model.RewriteChapterInput:
		return activity.Scope{ChapterNumber: input.ChapterNumber, PlanNodeID: input.ChapterPlanID, ChapterIDs: []string{input.ChapterID}}
	case *model.ReviewRangeInput:
		return activity.Scope{ChapterIDs: input.ChapterIDs}
	case *model.RewriteAffectedInput:
		return activity.Scope{ChapterIDs: input.ChapterIDs}
	case *model.ReviseCanonInput:
		return activity.Scope{ChapterIDs: []string{input.ChapterID}}
	default:
		return activity.Scope{}
	}
}

// liveStream 是一次执行的流式状态：回复里开过的工具调用与参数流的提取状态。
// 块序号逐条回复重用，开块时覆盖，不必清空。
type liveStream struct {
	calls map[int]litellm.ToolUseBlock
	args  *argTracker
}

func newLiveStream(task model.TaskInput) *liveStream {
	return &liveStream{calls: make(map[int]litellm.ToolUseBlock), args: newArgTracker(streamSpecs(task))}
}

// publishActivity 把 agent 生命周期事件翻译成带归属的活动事件（页面设计 §3）。
// 参数流上报字节进度，同时增量提取：正文以 Prose 事件逐字预览，大纲、设定与审阅
// 意见按条以 Item 事件发布（易失，权威内容仍以候选稿为准——三层校正链）。监听器
// 同步执行，这里只做 O(增量) 的翻译与非阻塞发布。
func (r *Runtime) publishActivity(operation model.Operation, event agentcore.Event, stream *liveStream) {
	if r.activity == nil {
		return
	}
	out := r.activityEvent(operation, "")
	switch e := event.(type) {
	case agentcore.MessageStart:
		out.Kind = activity.TurnStart
	case agentcore.MessageEnd:
		// 一条消息结束（含中止）即本轮参数流终结：清空提取状态，生命周期以
		// 消息为界，无需容量上限。用量随消息落盘，创作团队的统计从落盘记录汇总。
		stream.args.finishMessage()
		return
	case agentcore.ToolStart:
		out.Kind, out.Tool, out.CallID = activity.ToolStart, e.Call.Name, e.Call.ID
		out.Detail = toolDetail(e.Call.Name, e.Call.Args)
	case agentcore.ToolEnd:
		out.Kind, out.Tool, out.CallID = activity.ToolEnd, e.Call.Name, e.Call.ID
		if e.Result.IsError {
			out.Err = clipActivityText(e.Result.Text())
		}
	case agentcore.MessageDelta:
		switch d := e.Event.(type) {
		case litellm.BlockStart:
			if call, ok := d.Block.(litellm.ToolUseBlock); ok {
				stream.calls[d.Index] = call
			}
			return
		case litellm.ToolUseDelta:
			if d.Arguments == "" {
				return
			}
			// 参数流入先于工具执行（ToolStart 在整条回复完成后才发生）：按块序号
			// 取正在生成参数的工具名与调用 ID，让活动行在落笔的第一时间就出现。
			// 个别网关开块时还不带名字，argTracker 会先缓冲。
			call := stream.calls[d.Index]
			out.Tool, out.CallID = call.Name, call.ID
			text, lines, stalled := stream.args.feed(out.Tool, out.CallID, d.Arguments)
			progress := out
			progress.Kind, progress.Bytes = activity.ToolDelta, len(d.Arguments)
			r.activity.Publish(progress)
			if text != "" {
				prose := out
				prose.Kind, prose.Text = activity.Prose, text
				r.activity.Publish(prose)
			}
			for _, line := range lines {
				item := out
				item.Kind, item.Section, item.Text = activity.Item, line.section, line.text
				r.activity.Publish(item)
			}
			if !stalled {
				return
			}
			out.Kind = activity.ProseStall
		case litellm.ReasoningDelta:
			// 思考增量是 Provider 明确标记的推理文本，原样截尾展示为思考片段——
			// 不是摘要，不作可靠解释承诺（§3）。
			out.Kind, out.Text = activity.Thinking, d.Text
		case litellm.TextDelta:
			if d.Text == "" {
				return
			}
			out.Kind, out.Text = activity.Text, d.Text
		default:
			return
		}
	case agentcore.Retry:
		out.Kind, out.Attempt = activity.Retry, e.Attempt
		out.Err = clipActivityText(e.Err.Error())
	default:
		return
	}
	r.activity.Publish(out)
}

// judgmentFeed 把 Agent 循环之外的单次模型判断（如收尾时的语义合规）接到现场：收尾
// 同样可能等模型十几秒，画面不能静止。它是这次执行里的一步：思考照常直播，结构化输出
// 只报接收进度（JSON 不是给人读的）；用量由调用方随判断结果落盘。
type judgmentFeed struct {
	runtime   *Runtime
	operation model.Operation
	stage     string
}

func (r *Runtime) judgmentFeed(operation model.Operation, stage string) *judgmentFeed {
	feed := &judgmentFeed{runtime: r, operation: operation, stage: stage}
	feed.publish(feed.call(activity.ToolStart))
	feed.publish(feed.event(activity.TurnStart))
	return feed
}

func (f *judgmentFeed) observe(event litellm.Event) error {
	switch e := event.(type) {
	case litellm.ReasoningDelta:
		thinking := f.event(activity.Thinking)
		thinking.Text = e.Text
		f.publish(thinking)
	case litellm.TextDelta:
		progress := f.call(activity.ToolDelta)
		progress.Bytes = len(e.Text)
		f.publish(progress)
	}
	return nil
}

func (f *judgmentFeed) finish(err error) {
	end := f.call(activity.ToolEnd)
	if err != nil {
		end.Err = clipActivityText(err.Error())
	}
	f.publish(end)
}

func (f *judgmentFeed) event(kind activity.Kind) activity.Event {
	return f.runtime.activityEvent(f.operation, kind)
}

// call 是这次判断作为一步的事件：与 Agent 循环的工具调用同形，按 CallID 配对。
func (f *judgmentFeed) call(kind activity.Kind) activity.Event {
	event := f.event(kind)
	event.Tool, event.CallID = f.stage, f.operation.ID+":"+f.stage
	return event
}

func (f *judgmentFeed) publish(event activity.Event) {
	if f.runtime.activity != nil {
		f.runtime.activity.Publish(event)
	}
}

func clipActivityText(text string) string { return clipRunes(text, 200) }

// endedWithout 把缺少收尾提交的结局包装成可诊断错误，保留 agent 的结束原因。
func endedWithout(end agentcore.RunEnd, cause error) error {
	return fmt.Errorf("agent ended with %s after %d turns: %w", end.Reason, end.Turns, cause)
}

// restoreMessages 把之前尝试的对话导回本次会话，并取出最近一次执行失败的原因（D56）。
// 租约回收判的失败只说明执行者失联，模型无从修正，不带回。
func (r *Runtime) restoreMessages(ctx context.Context, operation model.Operation) ([]agentcore.Message, string, error) {
	if operation.Attempt <= 1 {
		return nil, "", nil
	}
	attempts, err := r.store.ListOperationAttempts(ctx, operation.ID)
	if err != nil {
		return nil, "", err
	}
	var lastFailure string
	for _, attempt := range attempts {
		if attempt.Attempt < operation.Attempt && attempt.Outcome == model.OperationFailed && !attempt.LeaseExpired {
			lastFailure = attempt.Error
		}
	}
	entries, err := r.store.ListTranscript(ctx, operation.ID)
	if err != nil {
		return nil, "", err
	}
	messages := make([]agentcore.Message, 0, len(entries))
	for _, entry := range entries {
		if entry.Attempt >= operation.Attempt || entry.Kind != model.TranscriptMessage {
			continue
		}
		var message agentcore.Message
		if err := json.Unmarshal(entry.Payload, &message); err != nil {
			return nil, "", fmt.Errorf("restore transcript entry %d: %w", entry.Sequence, err)
		}
		messages = append(messages, message)
	}
	return messages, lastFailure, nil
}

func sameVerdict(left, right model.ReviewVerdict) (bool, error) {
	leftPayload, err := json.Marshal(left)
	if err != nil {
		return false, fmt.Errorf("encode existing verdict submission: %w", err)
	}
	rightPayload, err := json.Marshal(right)
	if err != nil {
		return false, fmt.Errorf("encode repeated verdict submission: %w", err)
	}
	return bytes.Equal(leftPayload, rightPayload), nil
}

func sameSubmission(left, right model.Proposal) (bool, error) {
	left.CreatedAt = time.Time{}
	right.CreatedAt = time.Time{}
	leftPayload, err := json.Marshal(left)
	if err != nil {
		return false, fmt.Errorf("encode existing proposal submission: %w", err)
	}
	rightPayload, err := json.Marshal(right)
	if err != nil {
		return false, fmt.Errorf("encode repeated proposal submission: %w", err)
	}
	return bytes.Equal(leftPayload, rightPayload), nil
}
