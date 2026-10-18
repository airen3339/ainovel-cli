package capability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/domain/change"
	domainmodel "github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/ainovel-cli/internal/domain/narrative"
	"github.com/voocel/ainovel-cli/internal/infra/activity"
	"github.com/voocel/ainovel-cli/internal/infra/capability/prompt"
	"github.com/voocel/ainovel-cli/internal/infra/llm/models"
	"github.com/voocel/ainovel-cli/internal/infra/store"
	"github.com/voocel/ainovel-cli/internal/infra/workspace"
	"github.com/voocel/litellm"
	"github.com/voocel/litellm/catalog"
	"github.com/voocel/litellm/litellmtest"
)

func TestRuntimeToolsRespectAuthoritySnapshotAndWorkspaceBoundary(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	authorityStore, err := store.Open(ctx, filepath.Join(t.TempDir(), "ainovel.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer authorityStore.Close()
	target := domainmodel.AuthorityTarget{Kind: domainmodel.AuthorityProject, ID: "book-1"}
	seedRuntimeProject(t, ctx, authorityStore, target, now)

	task := json.RawMessage(`{"chapter_plan_id":"chapter-plan-1"}`)
	operation := domainmodel.Operation{
		ID: "write-1", Kind: domainmodel.OperationWriteChapter, Target: target,
		State: domainmodel.OperationQueued, RunID: createRuntimeTestRun(t, ctx, authorityStore, target.ID, now),
		Snapshot: runtimeSnapshot(task, 1, domainmodel.ApprovalManual, "profile"),
		Input:    task, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := authorityStore.CreateOperation(ctx, operation); err != nil {
		t.Fatalf("create operation: %v", err)
	}
	operation, err = authorityStore.ClaimNextOperation(ctx, "worker-1", time.Minute, now.Add(time.Second))
	if err != nil {
		t.Fatalf("claim operation: %v", err)
	}
	runtime := NewRuntime(authorityStore)
	runtime.now = func() time.Time { return now.Add(2 * time.Second) }

	read := testTool(t, runtime, operation, "writer.compose@1", prompt.ToolAuthorityRead, nil)
	// 回查按故事语言定位（D66）：章号、名称，结果里没有文档 ID 与存储信封。
	chapterView, err := read(ctx, json.RawMessage(`{"chapter":1}`))
	if err != nil || !strings.Contains(string(chapterView), `"chapter":1`) || strings.Contains(string(chapterView), "chapter-plan-1") {
		t.Fatalf("chapter read = %s, %v", chapterView, err)
	}
	entityView, err := read(ctx, json.RawMessage(`{"entity":"主角"}`))
	if err != nil || !strings.Contains(string(entityView), `"predicate":"state.origin"`) || strings.Contains(string(entityView), `"hero"`) {
		t.Fatalf("entity read = %s, %v", entityView, err)
	}
	if _, err := read(ctx, json.RawMessage(`{"chapter":2}`)); !errors.Is(err, domainmodel.ErrNotFound) || !strings.Contains(err.Error(), "第 2 章") {
		t.Fatalf("missing chapter err = %v", err)
	}
	for _, raw := range []string{`{"chapter":1,"entity":"主角"}`, `{"kind":"intent","id":"root","revision":1}`} {
		if _, err := read(ctx, json.RawMessage(raw)); err == nil {
			t.Fatalf("read %s accepted", raw)
		}
	}

	// 章的身份与工作稿键由宿主按任务确定：模型只写段落，回执只有键与版本。
	putChapter := testTool(t, runtime, operation, "writer.compose@1", prompt.ToolWorkspacePutChapter, nil)
	receipt, err := putChapter(ctx, json.RawMessage(`{"chapter":{"blocks":[{"id":"p-1","text":"他抵达山门……"}]}}`))
	if err != nil || string(receipt) != `{"key":"chapter","version":1}` {
		t.Fatalf("put chapter = %s, %v", receipt, err)
	}
	stored, err := authorityStore.GetWorkspaceArtifact(ctx, operation.ID, workspace.ChapterKey)
	if err != nil {
		t.Fatal(err)
	}
	// 工作稿记下它照的作品版本（D87）。
	var draft workspace.Draft
	if err := json.Unmarshal(stored.Content, &draft); err != nil || draft.Revision != operation.Snapshot.BaseRevision ||
		draft.Chapter.ID != "chapter-plan-1" || draft.Chapter.PlanNodeID != "chapter-plan-1" ||
		draft.Chapter.Number != 1 || draft.Chapter.Author != domainmodel.AuthorAI {
		t.Fatalf("host-filled draft = %+v, %v", draft, err)
	}

	var submitted domainmodel.Proposal
	submitTool := testTool(t, runtime, operation, "writer.compose@1", prompt.ToolProposalSubmit, func(proposal domainmodel.Proposal) (domainmodel.Proposal, error) {
		submitted = proposal
		return proposal, nil
	})
	submit := func(facts ...map[string]any) (json.RawMessage, error) {
		submitted = domainmodel.Proposal{}
		raw, err := json.Marshal(map[string]any{"reason": "提交本章", "facts": facts})
		if err != nil {
			t.Fatal(err)
		}
		return submitTool(ctx, raw)
	}
	// 同一主体+谓词就是同一事实（D61）：宿主按自然键并入已有节点并从冻结基线补 old_value。
	if _, err := submit(map[string]any{"subject": "主角", "predicate": "state.origin", "value": "孤儿"}); err != nil {
		t.Fatalf("keyed fact must merge into the existing node: %v", err)
	}
	var merged domainmodel.CanonFact
	if err := json.Unmarshal(submitted.Patches[1].Content, &merged); err != nil || submitted.Patches[1].Document.ID != "hero-origin" ||
		string(merged.PreviousValue) != `"农家子"` || merged.SourceChapterID != "chapter-plan-1" {
		t.Fatalf("merged patch = %+v (%v)", submitted.Patches[1], err)
	}
	result, err := submit(map[string]any{"subject": "主角", "predicate": "event.chapter_outcome", "value": "抵达山门"})
	if err != nil || string(result) != `{"status":"submitted"}` {
		t.Fatalf("submit = %s, %v", result, err)
	}
	if submitted.OperationID != operation.ID || submitted.BaseRevision != 1 || len(submitted.Patches) != 2 {
		t.Fatalf("submitted proposal = %#v", submitted)
	}
	// 故事依赖由宿主按事实变化写入（D40）。
	var written domainmodel.ManuscriptChapter
	if err := json.Unmarshal(submitted.Patches[0].Content, &written); err != nil || len(written.DependsOn) != 1 || written.DependsOn[0].ID != "hero" {
		t.Fatalf("manuscript dependencies = %+v, %v", written.DependsOn, err)
	}
	if _, err := authorityStore.GetProposal(ctx, submitted.ID); !errors.Is(err, domainmodel.ErrNotFound) {
		t.Fatalf("capability wrote authority proposal directly: %v", err)
	}
	for _, tc := range []struct {
		name  string
		facts []map[string]any
		want  string
	}{
		{"unknown subject", []map[string]any{{"subject": "路人", "predicate": "event.x", "value": "x"}}, "实体「路人」不存在"},
		{"canon still required", nil, "Canon Delta"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := submit(tc.facts...)
			if err == nil || !strings.Contains(err.Error(), tc.want) || submitted.ID != "" {
				t.Fatalf("error=%v submitted=%s", err, submitted.ID)
			}
			// 回给模型的错误只有故事标签。
			if strings.Contains(err.Error(), `"chapter-plan-1"`) || strings.Contains(err.Error(), `"hero"`) {
				t.Fatalf("internal id leaked: %v", err)
			}
		})
	}
	legacy, _ := json.Marshal(map[string]any{"reason": "旧协议", "patches": []any{}})
	if _, err := submitTool(ctx, legacy); err == nil {
		t.Fatal("document patches accepted from the model")
	}

	workspaceRead := testTool(t, runtime, operation, "writer.compose@1", prompt.ToolWorkspaceRead, nil)
	readBack, err := workspaceRead(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var readable narrative.ChapterDraft
	if err := json.Unmarshal(readBack, &readable); err != nil || len(readable.Blocks) != 1 || strings.Contains(string(readBack), "plan_node_id") {
		t.Fatalf("workspace must read back the draft shape: %s, %v", readBack, err)
	}
	if _, err := workspaceRead(ctx, json.RawMessage(`{"key":"chapter"}`)); err == nil {
		t.Fatal("the chapter draft key is the host's, not a parameter")
	}
}

// testTool 构造单个工具，故事按任务冻结基线加载。
func testTool(t *testing.T, runtime *Runtime, operation domainmodel.Operation, worker, name string, submit func(domainmodel.Proposal) (domainmodel.Proposal, error)) toolFunc {
	t.Helper()
	execute, err := runtime.toolExecutor(operation, worker, name, runtime.storyLoader(operation), submit, nil)
	if err != nil {
		t.Fatalf("build %s: %v", name, err)
	}
	return execute
}

func TestBuiltinCapabilityToolsHaveRuntimeImplementations(t *testing.T) {
	definitions := prompt.BuiltinCapabilities()
	runtime := &Runtime{}
	for _, definition := range definitions {
		operation := domainmodel.Operation{Kind: definition.OperationKinds[0]}
		compiled := prompt.Compiled{WorkerProfile: definition.Worker.ID + "@" + definition.Worker.Version, Tools: definition.Worker.Tools}
		if _, err := runtime.toolsFor(operation, compiled, func(proposal domainmodel.Proposal) (domainmodel.Proposal, error) {
			return proposal, nil
		}, func(verdict domainmodel.ReviewVerdict) (domainmodel.ReviewVerdict, error) {
			return verdict, nil
		}); err != nil {
			t.Fatalf("worker %s tools: %v", definition.Worker.ID, err)
		}
	}
}

func TestRuntimeRestoresCommittedConversationAndWorkspace(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 18, 13, 0, 0, 0, time.UTC)
	authorityStore, err := store.Open(ctx, filepath.Join(t.TempDir(), "ainovel.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer authorityStore.Close()
	worker, err := prompt.BuiltinWorkerProfile("writer.compose")
	if err != nil {
		t.Fatalf("load writer capability: %v", err)
	}
	task := json.RawMessage(`{"chapter_plan_id":"chapter-plan-1"}`)
	target := domainmodel.AuthorityTarget{Kind: domainmodel.AuthorityProject, ID: "book-recovery"}
	seedRuntimeProject(t, ctx, authorityStore, target, now)
	operation := domainmodel.Operation{
		ID: "recover-draft", Kind: domainmodel.OperationWriteChapter, Target: target,
		State: domainmodel.OperationQueued,
		RunID: createRuntimeTestRun(t, ctx, authorityStore, "book-recovery", now),
		Snapshot: runtimeSnapshot(task, 1, domainmodel.ApprovalManual,
			seedRuntimeProfile(t, ctx, authorityStore, "book-recovery", worker, task, 1)),
		Input: task, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := authorityStore.CreateOperation(ctx, operation); err != nil {
		t.Fatalf("create operation: %v", err)
	}
	first, err := authorityStore.ClaimNextOperation(ctx, "worker-before-crash", time.Minute, now.Add(time.Second))
	if err != nil {
		t.Fatalf("claim first attempt: %v", err)
	}
	chapter := domainmodel.ManuscriptChapter{
		ID: "chapter-1", PlanNodeID: "chapter-plan-1", Number: 1, Author: domainmodel.AuthorAI,
		Blocks: []domainmodel.ManuscriptBlock{{ID: "block-1", Text: "已经写入工作区的半成品。"}},
	}
	if _, err := workspace.New(authorityStore).PutChapter(ctx, first.ID, workspace.Draft{Revision: first.Snapshot.BaseRevision, Chapter: chapter}, first.Attempt, now.Add(2*time.Second)); err != nil {
		t.Fatalf("write recoverable draft: %v", err)
	}
	previous := agentcore.UserText("这是崩溃前已经提交到会话日志的原始任务")
	previous.Time = now.Add(3 * time.Second)
	payload, _ := json.Marshal(previous)
	if _, err := authorityStore.AppendTranscript(ctx, domainmodel.TranscriptEntry{
		OperationID: first.ID, Attempt: first.Attempt, Kind: domainmodel.TranscriptMessage, Payload: payload, CreatedAt: previous.Time,
	}, domainmodel.Usage{}, 0); err != nil {
		t.Fatalf("persist previous message: %v", err)
	}
	previousReply := agentcore.Message{
		Role: litellm.RoleAssistant, Blocks: []litellm.Block{litellm.Text("草稿已保存")},
		Usage: &agentcore.Usage{Usage: litellm.Usage{InputTokens: 100, OutputTokens: 20}}, Time: previous.Time,
	}
	payload, _ = json.Marshal(previousReply)
	if _, err := authorityStore.AppendTranscript(ctx, domainmodel.TranscriptEntry{
		OperationID: first.ID, Attempt: first.Attempt, Kind: domainmodel.TranscriptMessage, Payload: payload, CreatedAt: previousReply.Time,
	}, messageUsage(previousReply.Usage), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := authorityStore.TransitionOperation(
		ctx, first.ID, domainmodel.OperationRunning, domainmodel.OperationFailed, "simulated process failure", now.Add(4*time.Second),
	); err != nil {
		t.Fatalf("fail first attempt: %v", err)
	}
	if _, err := authorityStore.TransitionOperation(
		ctx, first.ID, domainmodel.OperationFailed, domainmodel.OperationQueued, "resume", now.Add(5*time.Second),
	); err != nil {
		t.Fatalf("queue second attempt: %v", err)
	}
	second, err := authorityStore.ClaimNextOperation(ctx, "worker-after-crash", time.Minute, now.Add(6*time.Second))
	if err != nil {
		t.Fatalf("claim second attempt: %v", err)
	}
	proposalArgs, _ := json.Marshal(map[string]any{
		"reason": "继续并提交已有工作稿",
		"facts":  []map[string]any{{"subject": "主角", "predicate": "event.chapter_outcome", "value": "完成已有工作稿"}},
	})
	model := recoveryScript(proposalArgs)
	runtime := boundRuntime(authorityStore, testChat(model))
	runtime.now = func() time.Time { return now.Add(8 * time.Second) }
	outcome, err := runtime.Execute(ctx, second)
	if err != nil {
		t.Fatalf("resume runtime: %v", err)
	}
	if outcome.Proposal == nil || len(outcome.Proposal.Patches) != 2 || outcome.Proposal.Patches[0].Document.ID != chapter.ID {
		t.Fatalf("outcome = %#v", outcome)
	}
	if len(model.Requests()) != 2 {
		t.Fatalf("successful submission made extra model calls: %d", len(model.Requests()))
	}
	firstRequest := model.Requests()[0].Messages
	foundPrevious, foundRecovery, foundFailure := false, false, false
	for _, message := range firstRequest {
		text := requestText(message)
		foundPrevious = foundPrevious || text == previous.Text()
		foundRecovery = foundRecovery || strings.Contains(text, "从已有工件继续")
		foundFailure = foundFailure || strings.Contains(text, "上一次尝试失败原因：simulated process failure")
	}
	if !foundPrevious || !foundRecovery || !foundFailure {
		t.Fatalf("restored request missing history, recovery instruction or failure reason: %#v", firstRequest)
	}
	// 每次尝试的用量只记自己花的：恢复的历史消息不再计入第二次尝试；摘要记下实际模型。
	attempts, err := authorityStore.ListOperationAttempts(ctx, second.ID)
	if err != nil || len(attempts) != 2 {
		t.Fatalf("attempts = %+v, %v", attempts, err)
	}
	if attempts[0].Usage != (domainmodel.Usage{Input: 100, Output: 20}) || attempts[0].Outcome != domainmodel.OperationFailed {
		t.Fatalf("first attempt = %+v", attempts[0])
	}
	if attempts[1].Usage != (domainmodel.Usage{}) || attempts[1].Agent.Model == "" || attempts[1].Agent.Role == "" {
		t.Fatalf("resumed attempt = %+v", attempts[1])
	}
}

type transcriptStore struct {
	runtimeStore
	attempts   []domainmodel.OperationAttempt
	transcript []domainmodel.TranscriptEntry
}

func (s transcriptStore) ListOperationAttempts(context.Context, string) ([]domainmodel.OperationAttempt, error) {
	return s.attempts, nil
}

func (s transcriptStore) ListTranscript(context.Context, string) ([]domainmodel.TranscriptEntry, error) {
	return s.transcript, nil
}

// 连接中途断开的回合以 StopError 记下，可以一个字都没有：续跑照常恢复；判断不是会话消息，
// 不带回；租约回收判的失败只说明执行者失联、模型服务的失败与写作无关，都不当作失败原因
// 告诉模型。
func TestRestoreKeepsFailedTurnsAndExecutionFailures(t *testing.T) {
	task := json.RawMessage(`{"role":"user","blocks":[{"type":"text","text":"写第 1 章"}]}`)
	failed := json.RawMessage(`{"role":"assistant","blocks":null,"stop":"error","provider":"openai","model":"m"}`)
	runtime := &Runtime{store: transcriptStore{
		attempts: []domainmodel.OperationAttempt{
			{Attempt: 1, Outcome: domainmodel.OperationFailed, Error: "提交被拒：章节字数不足"},
			{Attempt: 2, Outcome: domainmodel.OperationFailed, Error: "worker lease expired", LeaseExpired: true},
			{Attempt: 3, Outcome: domainmodel.OperationFailed, Error: "compat: request failed: no such host", FailureCode: domainmodel.FailureModelUnreachable},
		},
		transcript: []domainmodel.TranscriptEntry{
			{Sequence: 3, Attempt: 1, Kind: domainmodel.TranscriptMessage, Payload: task},
			{Sequence: 7, Attempt: 1, Kind: domainmodel.TranscriptMessage, Payload: failed},
			{Sequence: 8, Attempt: 1, Kind: domainmodel.TranscriptJudgment, Payload: json.RawMessage(`{"model":"judge"}`)},
		},
	}}
	messages, lastFailure, err := runtime.restoreMessages(context.Background(), domainmodel.Operation{ID: "op", Attempt: 4})
	if err != nil || len(messages) != 2 || messages[1].Stop != agentcore.StopError || lastFailure != "提交被拒：章节字数不足" {
		t.Fatalf("restore = %d messages, failure %q, err = %v", len(messages), lastFailure, err)
	}
}

func TestRuntimeReviewSubmitsVerdictWithoutProposal(t *testing.T) {
	// D30：审阅 Worker 以 verdict_submit 收尾——不产 Proposal、不造空 Patch；
	// 裁定 Revision 由宿主绑定启动快照，越界或漏审的裁定在工具层被拒绝。
	ctx := context.Background()
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	authorityStore, err := store.Open(ctx, filepath.Join(t.TempDir(), "ainovel.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer authorityStore.Close()
	worker, err := prompt.BuiltinWorkerProfile("editor.review")
	if err != nil {
		t.Fatalf("load review capability: %v", err)
	}
	task := json.RawMessage(`{"chapter_ids":["chapter-1","chapter-2"],"requirements":[{"id":"directive:hook","text":"每章结尾留钩子"},{"id":"intent:forbidden:0","text":"不写感情线","settle":true}],"basis":{"documents":[{"ref":{"kind":"manuscript","id":"chapter-1"},"revision":1}]}}`)
	target := domainmodel.AuthorityTarget{Kind: domainmodel.AuthorityProject, ID: "book-review"}
	seedWrittenChapters(t, ctx, authorityStore, target, now, 2)
	operation := domainmodel.Operation{
		ID: "review-1", Kind: domainmodel.OperationReviewRange,
		Target: target,
		State:  domainmodel.OperationQueued,
		RunID:  createRuntimeTestRun(t, ctx, authorityStore, "book-review", now),
		Snapshot: runtimeSnapshot(task, 1, domainmodel.ApprovalAuto,
			seedRuntimeProfile(t, ctx, authorityStore, "book-review", worker, task, 1)),
		Input: task, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := authorityStore.CreateOperation(ctx, operation); err != nil {
		t.Fatalf("create operation: %v", err)
	}
	operation, err = authorityStore.ClaimNextOperation(ctx, "worker-1", time.Minute, now.Add(time.Second))
	if err != nil {
		t.Fatalf("claim operation: %v", err)
	}
	// 递进式纠错：漏掉要求核验被拒 → 必须下结论的项给 pending 被拒 → 完整裁定通过。
	// 章节范围与发现由宿主从任务输入和审阅记录填入（D60），模型不再复述，
	// "漏审范围""与记录不一致"这两类错误在结构上已不可能发生。
	findings := []map[string]any{{"chapter": 2, "severity": "note", "note": "第二章节奏偏慢"}}
	review, _ := json.Marshal(map[string]any{
		"key": "review/findings", "findings": findings,
	})
	noChecks, _ := json.Marshal(map[string]any{"review_key": "review/findings"})
	unsettled, _ := json.Marshal(map[string]any{"review_key": "review/findings", "checks": []map[string]any{
		{"id": "directive:hook", "status": "pending"}, {"id": "intent:forbidden:0", "status": "pending"},
	}})
	complete, _ := json.Marshal(map[string]any{"review_key": "review/findings", "checks": []map[string]any{
		{"id": "directive:hook", "status": "pending"}, {"id": "intent:forbidden:0", "status": "satisfied"},
	}})
	// 每次回应计 10 入 3 出，按单价折合 0.01。
	replies := []litellmtest.Reply{callReply("put-review", "workspace_put_review", review)}
	for i, step := range []json.RawMessage{noChecks, unsettled, complete} {
		replies = append(replies, callReply(fmt.Sprintf("verdict-%d", i+1), "verdict_submit", step))
	}
	for i := range replies {
		replies[i].Usage = litellm.Usage{InputTokens: 10, OutputTokens: 3}
	}
	model := litellmtest.New(replies...)
	chat := testChat(model)
	chat.Request.Model = "verdict-model"
	chat.Pricing = &catalog.Pricing{Rates: catalog.Rates{Input: 0.0007, Output: 0.001}}
	runtime := boundRuntime(authorityStore, chat)
	runtime.now = func() time.Time { return now.Add(3 * time.Second) }
	hub := activity.NewHub()
	runtime.SetActivitySink(hub)
	outcome, err := runtime.Execute(ctx, operation)
	if err != nil {
		t.Fatalf("execute review: %v", err)
	}
	if outcome.Proposal != nil || len(outcome.Verdict) == 0 {
		t.Fatalf("outcome = %#v, want verdict-only", outcome)
	}
	var verdict domainmodel.ReviewVerdict
	if err := json.Unmarshal(outcome.Verdict, &verdict); err != nil {
		t.Fatalf("decode verdict: %v", err)
	}
	if verdict.Blocking() != 0 || verdict.Revision != 1 ||
		len(verdict.ChapterIDs) != 2 || len(verdict.Findings) != 1 || verdict.Findings[0].ChapterID != "chapter-2" ||
		verdict.ReviewKey != "review/findings" || len(verdict.Checks) != 2 ||
		verdict.CheckStatus("intent:forbidden:0") != domainmodel.CheckSatisfied || verdict.CheckStatus("directive:hook") != domainmodel.CheckPending {
		t.Fatalf("verdict = %#v", verdict)
	}
	artifact, err := authorityStore.GetWorkspaceArtifact(ctx, operation.ID, verdict.ReviewKey)
	if err != nil || artifact.MediaType != domainmodel.ReviewArtifactMediaType {
		t.Fatalf("review artifact = %#v, %v", artifact, err)
	}
	// 活动通道（页面设计 §4）：执行过程以带归属的事件发布——两次被拒的提交
	// 呈现为出错条目（模型自纠可见），最终提交成功收尾。
	feed, ok := hub.Snapshot("book-review")
	if !ok || feed.RunID != operation.RunID {
		t.Fatalf("activity feed identity = %#v ok=%v", feed, ok)
	}
	if len(feed.Tasks) != 1 || !feed.Tasks[0].Done || feed.Tasks[0].OperationID != operation.ID {
		t.Fatalf("successful execution did not close its task: %+v", feed.Tasks)
	}
	var rejected, accepted int
	for _, entry := range feed.Entries {
		if entry.Tool != "verdict_submit" || !entry.Done {
			continue
		}
		if entry.Err != "" {
			rejected++
		} else {
			accepted++
		}
	}
	if rejected != 2 || accepted != 1 {
		t.Fatalf("verdict_submit activity: rejected=%d accepted=%d entries=%#v", rejected, accepted, feed.Entries)
	}
	// 成功提交直接正常收尾，不再请求第五次模型回应。用量随消息落盘，创作团队按尝试汇总：
	// 角色是执行它的 Worker，花费按绑定价目。
	if len(model.Requests()) != 4 {
		t.Fatalf("requests = %d", len(model.Requests()))
	}
	attempts, err := authorityStore.ProjectAttempts(ctx, operation.Target.ID, now.Add(time.Second))
	if err != nil || len(attempts) != 1 {
		t.Fatalf("project attempts = %+v, %v", attempts, err)
	}
	if a := attempts[0]; a.Role != "editor" || a.Usage.Input != 40 || a.Usage.Output != 12 || a.Usage.Cost < 0.039 || a.Usage.Cost > 0.041 ||
		a.StartedAt.IsZero() || a.ActiveAt.Before(a.StartedAt) || !a.Live || a.State != domainmodel.OperationRunning {
		t.Fatalf("persisted attempt usage = %+v", a)
	}
}

func createRuntimeTestRun(
	t *testing.T,
	ctx context.Context,
	authorityStore *store.Store,
	projectID string,
	now time.Time,
) string {
	t.Helper()
	run := domainmodel.CreationRun{
		ID: "run:" + projectID, ProjectID: projectID,
		Goal: domainmodel.NovelGoal{TargetChapters: 3}.Goal(),
		Strategy: domainmodel.CreationRunStrategy{
			AutoRepairBudget: 3,
		},
		Preset: domainmodel.CreationRunPreset{
			Source: "test", Digest: "test-preset", Approval: domainmodel.ApprovalAuto,
		},
		State: domainmodel.RunRunning, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := authorityStore.CreateCreationRun(ctx, run); err != nil {
		t.Fatalf("create runtime test run: %v", err)
	}
	return run.ID
}

// recoveryScript 先读回本章工作稿，再按 proposalArgs 提交，提交成功即收尾：多一次调用就越出脚本。
func recoveryScript(proposalArgs json.RawMessage) *litellmtest.Provider {
	return litellmtest.New(
		callReply("read-draft", "workspace_read", json.RawMessage(`{}`)),
		callReply("submit-existing", "proposal_submit", proposalArgs),
	)
}

// callReply 是只调用一个工具的回应。
func callReply(id, name string, args json.RawMessage) litellmtest.Reply {
	return litellmtest.Respond(litellm.ToolUseBlock{ID: id, Name: name, Arguments: string(args)})
}

// requestText 是请求里一条消息的文本。
func requestText(message litellm.Message) string {
	var text strings.Builder
	for _, block := range message.Blocks {
		if b, ok := block.(litellm.TextBlock); ok {
			text.WriteString(b.Text)
		}
	}
	return text.String()
}

// delta 把一个流式增量包成 agent 事件。
func delta(event litellm.Event) agentcore.Event { return agentcore.MessageDelta{Event: event} }

// TestPublishActivityTranslatesStreamingToolDeltas 守卫真实事件时序（agentcore
// 在整条回复完成后才执行工具）：参数流阶段就要有带工具名的进行中条目
// 与字节进度，ToolStart 不重复建条、ToolEnd 收尾。
// 断网、域名解析失败这类暂时性失败由模型调用就地退避重发：任务照常完成，不算失败，
// 现场留下第几次重试（§6.2）。
func TestRuntimeRidesOutTemporaryModelFailures(t *testing.T) {
	ctx := context.Background()
	authorityStore, operation := claimedReview(t, "book-offline")
	review, _ := json.Marshal(map[string]any{"key": "review/findings", "findings": []map[string]any{}})
	verdict, _ := json.Marshal(map[string]any{"review_key": "review/findings"})
	offline := &litellm.Error{Type: litellm.ErrorTypeNetwork, Message: "dial tcp: lookup relay: no such host", Temporary: true, RetryAfter: time.Millisecond}
	runtime := boundRuntime(authorityStore, testChat(litellmtest.New(
		litellmtest.Fail(offline), callReply("put-review", "workspace_put_review", review), callReply("verdict", "verdict_submit", verdict),
	)))
	hub := activity.NewHub()
	runtime.SetActivitySink(hub)
	outcome, err := runtime.Execute(ctx, operation)
	if err != nil || len(outcome.Verdict) == 0 {
		t.Fatalf("execute through an outage: outcome=%#v err=%v", outcome, err)
	}
	feed, _ := hub.Snapshot("book-offline")
	if !slices.ContainsFunc(feed.Entries, func(entry activity.Entry) bool {
		return entry.Kind == activity.Retry && entry.Attempt == 2 && strings.Contains(entry.Err, "no such host")
	}) {
		t.Fatalf("retry not shown: %#v", feed.Entries)
	}
}

// 模型服务拒绝（密钥失效）重试无用：执行立即失败，失败带上类别，错误文字原样不另加包装。
func TestRuntimeTagsRefusedModelCalls(t *testing.T) {
	authorityStore, operation := claimedReview(t, "book-refused")
	refused := &litellm.Error{Type: litellm.ErrorTypeAuth, Provider: "compat", Message: "invalid api key", StatusCode: 401}
	provider := litellmtest.New(litellmtest.Fail(refused))
	_, err := boundRuntime(authorityStore, testChat(provider)).Execute(context.Background(), operation)
	if domainmodel.FailureCodeFor(err) != domainmodel.FailureModelRefused || err.Error() != refused.Error() || len(provider.Requests()) != 1 {
		t.Fatalf("refused call: code=%q err=%v requests=%d", domainmodel.FailureCodeFor(err), err, len(provider.Requests()))
	}
}

// claimedReview 是写好第 1 章的书上一个已领取的审阅任务。
func claimedReview(t *testing.T, projectID string) (*store.Store, domainmodel.Operation) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	authorityStore, err := store.Open(ctx, filepath.Join(t.TempDir(), "ainovel.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { authorityStore.Close() })
	worker, err := prompt.BuiltinWorkerProfile("editor.review")
	if err != nil {
		t.Fatal(err)
	}
	task := json.RawMessage(`{"chapter_ids":["chapter-1"],"basis":{"documents":[{"ref":{"kind":"manuscript","id":"chapter-1"},"revision":1}]}}`)
	target := domainmodel.AuthorityTarget{Kind: domainmodel.AuthorityProject, ID: projectID}
	seedWrittenChapters(t, ctx, authorityStore, target, now, 1)
	operation := domainmodel.Operation{
		ID: "review-" + projectID, Kind: domainmodel.OperationReviewRange, Target: target, State: domainmodel.OperationQueued,
		RunID: createRuntimeTestRun(t, ctx, authorityStore, projectID, now),
		Snapshot: runtimeSnapshot(task, 1, domainmodel.ApprovalAuto,
			seedRuntimeProfile(t, ctx, authorityStore, projectID, worker, task, 1)),
		Input: task, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := authorityStore.CreateOperation(ctx, operation); err != nil {
		t.Fatal(err)
	}
	claimed, err := authorityStore.ClaimNextOperation(ctx, "worker-1", time.Minute, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return authorityStore, claimed
}

func TestPublishActivityTranslatesStreamingToolDeltas(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	hub := activity.NewHub()
	runtime := &Runtime{now: func() time.Time { return now }, activity: hub}
	operation := domainmodel.Operation{
		ID: "op-1", RunID: "run-1",
		Target: domainmodel.AuthorityTarget{Kind: domainmodel.AuthorityProject, ID: "book-live"},
	}
	stream := newLiveStream(nil)
	call := agentcore.ToolCall{ID: "call-1", Name: "workspace_put_chapter"}
	runtime.publishActivity(operation, delta(litellm.BlockStart{Index: 0, Block: litellm.ToolUseBlock{ID: call.ID, Name: call.Name}}), stream)
	runtime.publishActivity(operation, delta(litellm.ToolUseDelta{Index: 0, Arguments: `{"chapter":{"id":"c1","blocks":[{"id":"p1","text":"正文`}), stream)
	snapshot, ok := hub.Snapshot("book-live")
	if !ok || len(snapshot.Entries) != 1 || snapshot.Entries[0].Tool != "workspace_put_chapter" ||
		snapshot.Entries[0].Done || snapshot.Entries[0].Bytes == 0 {
		t.Fatalf("streaming snapshot = %#v ok=%v", snapshot, ok)
	}
	// 逐字预览：参数流里的正文被解出并归属本次调用（页面设计 §3）。
	if text, callID := feedProse(snapshot); text != "正文" || callID != "call-1" {
		t.Fatalf("prose = %q callID = %q", text, callID)
	}
	runtime.publishActivity(operation, delta(litellm.ToolUseDelta{Index: 0, Arguments: `，一句接一句"}]}}`}), stream)
	runtime.publishActivity(operation, agentcore.ToolStart{Call: call}, stream)
	runtime.publishActivity(operation, agentcore.ToolEnd{Call: call, Result: agentcore.TextResult("{}")}, stream)
	snapshot, _ = hub.Snapshot("book-live")
	if len(snapshot.Entries) != 1 || !snapshot.Entries[0].Done {
		t.Fatalf("after exec = %#v", snapshot.Entries)
	}
	if text, _ := feedProse(snapshot); text != `正文，一句接一句` {
		t.Fatalf("prose after close = %q", text)
	}
}

// feedProse 是快照里正文输出块的文字与所属调用。
func feedProse(snapshot activity.Snapshot) (string, string) {
	var text, callID string
	for _, block := range snapshot.Output {
		if block.Kind == activity.Prose {
			text, callID = text+string(block.Text), block.CallID
		}
	}
	return text, callID
}

// TestPublishActivityAttributesDeltaByBlock 守卫交错归属：并行调用并存时按块序号
// 精确定位所属调用，不取"最后一个"——正文增量归属正文调用，哪怕回复里后面又开了
// 别的调用。
func TestPublishActivityAttributesDeltaByBlock(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	hub := activity.NewHub()
	runtime := &Runtime{now: func() time.Time { return now }, activity: hub}
	operation := domainmodel.Operation{
		ID: "op-2", RunID: "run-2",
		Target: domainmodel.AuthorityTarget{Kind: domainmodel.AuthorityProject, ID: "book-mix"},
	}
	stream := newLiveStream(nil)
	runtime.publishActivity(operation, delta(litellm.BlockStart{Index: 0, Block: litellm.ToolUseBlock{ID: "call-a", Name: "workspace_put_chapter"}}), stream)
	runtime.publishActivity(operation, delta(litellm.BlockStart{Index: 1, Block: litellm.ToolUseBlock{ID: "call-b", Name: "authority_read"}}), stream)
	runtime.publishActivity(operation, delta(litellm.ToolUseDelta{Index: 0, Arguments: `{"chapter":{"blocks":[{"text":"甲稿正文`}), stream)
	snapshot, ok := hub.Snapshot("book-mix")
	if text, callID := feedProse(snapshot); !ok || text != "甲稿正文" || callID != "call-a" {
		t.Fatalf("prose = %q callID = %q ok=%v", text, callID, ok)
	}
	if len(snapshot.Entries) != 1 || snapshot.Entries[0].Tool != "workspace_put_chapter" ||
		snapshot.Entries[0].CallID != "call-a" {
		t.Fatalf("entries = %#v", snapshot.Entries)
	}
}

// TestPublishActivityThinkingTextAndToolError 守卫两处翻译语义：思考增量作为
// 推理文本入快照；出错的工具结果原文进条目。
func TestPublishActivityThinkingTextAndToolError(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	hub := activity.NewHub()
	runtime := &Runtime{now: func() time.Time { return now }, activity: hub}
	operation := domainmodel.Operation{
		ID: "op-9", RunID: "run-9",
		Target: domainmodel.AuthorityTarget{Kind: domainmodel.AuthorityProject, ID: "book-9"},
	}
	stream := newLiveStream(nil)
	runtime.publishActivity(operation, delta(litellm.ReasoningDelta{Text: "回顾伏笔"}), stream)
	runtime.publishActivity(operation, agentcore.ToolEnd{
		Call: agentcore.ToolCall{ID: "c1", Name: "workspace_put_chapter"}, Result: agentcore.ErrorResult(`章节段落 "p-9" 不存在`),
	}, stream)
	snapshot, ok := hub.Snapshot("book-9")
	if !ok || len(snapshot.Output) != 1 || snapshot.Output[0].Kind != activity.Thinking || string(snapshot.Output[0].Text) != "回顾伏笔" {
		t.Fatalf("output = %#v ok=%v", snapshot.Output, ok)
	}
	if len(snapshot.Entries) != 1 || snapshot.Entries[0].Err != `章节段落 "p-9" 不存在` {
		t.Fatalf("entries = %#v", snapshot.Entries)
	}
}

// 后继任务接手前任的工作稿（D87）：稿子照的作品版本早于本任务时，读回工作稿一并列出这之后
// 书里每次修改的说明；整篇重写后照的就是本任务的版本，不再列。
func TestInheritedDraftNamesTheChangesSinceIt(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	authorityStore, err := store.Open(ctx, filepath.Join(t.TempDir(), "ainovel.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer authorityStore.Close()
	target := domainmodel.AuthorityTarget{Kind: domainmodel.AuthorityProject, ID: "book-1"}
	seedRuntimeProject(t, ctx, authorityStore, target, now)
	origin, _ := json.Marshal(domainmodel.CanonFact{
		ID: "hero-origin", Kind: domainmodel.CanonState, SubjectID: "hero", Predicate: "state.origin", Value: json.RawMessage(`"孤儿"`),
	})
	change := approvedRuntimeProposal("change", target, 1, now, domainmodel.Patch{
		Document: domainmodel.DocumentRef{Kind: domainmodel.DocumentCanon, ID: "hero-origin"}, Operation: domainmodel.PatchPut, Content: origin,
	})
	change.Reason = "主角改成孤儿"
	pending := change
	pending.ApprovalState, pending.DecidedBy, pending.DecidedAt = domainmodel.ApprovalPending, nil, nil
	if _, err := authorityStore.SaveProposal(ctx, pending); err != nil {
		t.Fatalf("save change: %v", err)
	}
	if _, err := authorityStore.CommitProposal(ctx, change); err != nil {
		t.Fatalf("commit change: %v", err)
	}
	task := json.RawMessage(`{"chapter_plan_id":"chapter-plan-1"}`)
	operation := domainmodel.Operation{
		ID: "write-1:redo2", Kind: domainmodel.OperationWriteChapter, Target: target,
		State: domainmodel.OperationQueued, RunID: createRuntimeTestRun(t, ctx, authorityStore, target.ID, now),
		Snapshot: runtimeSnapshot(task, 2, domainmodel.ApprovalManual, "profile"),
		Input:    task, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := authorityStore.CreateOperation(ctx, operation); err != nil {
		t.Fatalf("create operation: %v", err)
	}
	if operation, err = authorityStore.ClaimNextOperation(ctx, "worker-1", time.Minute, now.Add(time.Second)); err != nil {
		t.Fatalf("claim operation: %v", err)
	}
	runtime := NewRuntime(authorityStore)
	read := testTool(t, runtime, operation, "writer.compose@1", prompt.ToolWorkspaceRead, nil)
	// 还没写的工作稿读回来是空的，不算工具报错。
	if view, err := read(ctx, json.RawMessage(`{}`)); err != nil || string(view) != `{"blocks":[]}` {
		t.Fatalf("empty draft = %s, %v", view, err)
	}
	inherited := domainmodel.ManuscriptChapter{ID: "chapter-plan-1", PlanNodeID: "chapter-plan-1", Number: 1, Author: domainmodel.AuthorAI,
		Blocks: []domainmodel.ManuscriptBlock{{ID: "p-1", Text: "农家子走进山门。"}}}
	if _, err := workspace.New(authorityStore).PutChapter(ctx, operation.ID, workspace.Draft{Revision: 1, Chapter: inherited}, operation.Attempt, now); err != nil {
		t.Fatalf("put inherited draft: %v", err)
	}
	if view, err := read(ctx, json.RawMessage(`{}`)); err != nil || !strings.Contains(string(view), `"changed_since":["主角改成孤儿"]`) {
		t.Fatalf("inherited draft = %s, %v", view, err)
	}
	put := testTool(t, runtime, operation, "writer.compose@1", prompt.ToolWorkspacePutChapter, nil)
	if _, err := put(ctx, json.RawMessage(`{"chapter":{"blocks":[{"id":"p-1","text":"孤儿走进山门。"}]}}`)); err != nil {
		t.Fatal(err)
	}
	if view, err := read(ctx, json.RawMessage(`{}`)); err != nil || strings.Contains(string(view), "changed_since") {
		t.Fatalf("rewritten draft = %s, %v", view, err)
	}
}

func approvedRuntimeProposal(
	id string,
	target domainmodel.AuthorityTarget,
	base domainmodel.Revision,
	at time.Time,
	patches ...domainmodel.Patch,
) domainmodel.Proposal {
	return domainmodel.Proposal{
		ID: id, Target: target, BaseRevision: base,
		Author: domainmodel.Author{Kind: domainmodel.AuthorUser, ID: "user-1"},
		Reason: "seed", Patches: patches, ApprovalState: domainmodel.ApprovalApproved,
		DecidedBy: &domainmodel.Author{Kind: domainmodel.AuthorUser, ID: "user-1"}, DecidedAt: &at, CreatedAt: at,
	}
}

func TestRuntimeSemanticComplianceUsesIndependentStructuredCall(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	authorityStore, err := store.Open(ctx, filepath.Join(t.TempDir(), "ainovel.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer authorityStore.Close()
	target := domainmodel.AuthorityTarget{Kind: domainmodel.AuthorityProject, ID: "book-semantic"}
	seed := approvedRuntimeProposal("seed-semantic", target, 0, now,
		domainmodel.Patch{
			Document:  domainmodel.DocumentRef{Kind: domainmodel.DocumentIntent, ID: "root"},
			Operation: domainmodel.PatchPut, Content: json.RawMessage(`{"premise":"守住底线"}`),
		},
		domainmodel.Patch{
			Document:  domainmodel.DocumentRef{Kind: domainmodel.DocumentCanon, ID: "hero-bottom-line"},
			Operation: domainmodel.PatchPut,
			Content:   json.RawMessage(`{"id":"hero-bottom-line","kind":"world_rule","subject_id":"hero","predicate":"rule.bottom_line","new_value":"不伤无辜"}`),
		},
	)
	pending := seed
	pending.ApprovalState, pending.DecidedBy, pending.DecidedAt = domainmodel.ApprovalPending, nil, nil
	if _, err := authorityStore.SaveProposal(ctx, pending); err != nil {
		t.Fatalf("save seed: %v", err)
	}
	if _, err := authorityStore.CommitProposal(ctx, seed); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	task := json.RawMessage(`{"chapter_plan_id":"chapter-plan-1"}`)
	operation := domainmodel.Operation{
		ID: "semantic-operation", Kind: domainmodel.OperationWriteChapter, Target: target,
		State: domainmodel.OperationQueued, RunID: createRuntimeTestRun(t, ctx, authorityStore, target.ID, now),
		Snapshot: runtimeSnapshot(task, 1, domainmodel.ApprovalAuto, "profile"),
		Input:    task, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := authorityStore.CreateOperation(ctx, operation); err != nil {
		t.Fatalf("create operation: %v", err)
	}
	operation, err = authorityStore.ClaimNextOperation(ctx, "worker-1", time.Minute, now.Add(time.Second))
	if err != nil {
		t.Fatalf("claim operation: %v", err)
	}
	reply := litellmtest.Respond(litellm.ReasoningBlock{Text: "底线是不伤无辜，候选正文没有越界"}, litellm.Text(`{"status":"pass","findings":[]}`))
	reply.Usage = litellm.Usage{InputTokens: 10, OutputTokens: 3}
	model := litellmtest.New(reply)
	runtime := boundRuntime(authorityStore, testChat(model))
	runtime.now = func() time.Time { return now.Add(2 * time.Second) }
	hub := activity.NewHub()
	runtime.SetActivitySink(hub)
	report, err := runtime.AnalyzeSemanticCompliance(ctx, operation, domainmodel.Proposal{
		Patches: []domainmodel.Patch{{
			Document:  domainmodel.DocumentRef{Kind: domainmodel.DocumentManuscript, ID: "chapter-1"},
			Operation: domainmodel.PatchPut, Content: json.RawMessage(`{"candidate":"正文"}`),
		}},
	}, []domainmodel.OwnershipRule{{
		Target: domainmodel.DocumentRef{Kind: domainmodel.DocumentCanon, ID: "hero-bottom-line"}, Control: domainmodel.ControlLocked,
	}})
	if err != nil {
		t.Fatalf("analyze semantic compliance: %v", err)
	}
	if report.Status != domainmodel.SemanticCompliancePass || !usedJSONSchema(model) {
		t.Fatalf("report = %#v, json schema = %v", report, usedJSONSchema(model))
	}
	// 收尾阶段的模型判断同样进活动流：画面上是"核对语义合规"这一步与它的思考，
	// 结构化输出只报接收进度；用量随判断结果落盘，并入所在尝试。
	feed, ok := hub.Snapshot(operation.Target.ID)
	if !ok || len(feed.Entries) != 1 || feed.Entries[0].Tool != "semantic_compliance" || !feed.Entries[0].Done ||
		feed.Entries[0].Err != "" || feed.Entries[0].Bytes != len(`{"status":"pass","findings":[]}`) {
		t.Fatalf("compliance activity = %#v ok=%v", feed.Entries, ok)
	}
	if len(feed.Output) != 1 || feed.Output[0].Kind != activity.Thinking || string(feed.Output[0].Text) != "底线是不伤无辜，候选正文没有越界" {
		t.Fatalf("compliance thinking = %#v", feed.Output)
	}
	if feed.Waiting {
		t.Fatal("a finished judgment must not leave the feed waiting")
	}
	// 用量经 llm.Structured 回传后并入所在尝试，不能在收口时丢掉；判断不在 Agent 会话里，
	// 对话记录记下实际用的模型与结论。
	if attempts, err := authorityStore.ListOperationAttempts(ctx, operation.ID); err != nil || len(attempts) != 1 ||
		attempts[0].Usage != (domainmodel.Usage{Input: 10, Output: 3}) || attempts[0].Attempt != operation.Attempt || attempts[0].Agent.Model != "" {
		t.Fatalf("compliance usage must count toward its attempt: %+v, %v", attempts, err)
	}
	transcript, err := authorityStore.ListTranscript(ctx, operation.ID)
	if err != nil || len(transcript) != 1 || transcript[0].Kind != domainmodel.TranscriptJudgment {
		t.Fatalf("transcript = %#v, %v", transcript, err)
	}
	var checked semanticJudgment
	if err := json.Unmarshal(transcript[0].Payload, &checked); err != nil || checked.Model == "" || checked.Report == nil || checked.Report.Status != domainmodel.SemanticCompliancePass {
		t.Fatalf("judgment lacks its model or report: %s", transcript[0].Payload)
	}
}

// usedJSONSchema 报告模型的首次调用是否要了 JSON Schema 约束的输出。
func usedJSONSchema(model *litellmtest.Provider) bool {
	requests := model.Requests()
	return len(requests) > 0 && requests[0].ResponseFormat != nil && requests[0].ResponseFormat.Type == litellm.ResponseFormatJSONSchema
}

// 语义影响分析只看故事语言（D66）：改前改后的设定与可能受牵连的正文，章节按章号；结论里
// 的章号由宿主换回正文引用。分析器看不到任何内部 ID。
func TestRuntimeAnalyzesSemanticImpactInStoryLanguage(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	authorityStore, err := store.Open(ctx, filepath.Join(t.TempDir(), "ainovel.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer authorityStore.Close()
	target := domainmodel.AuthorityTarget{Kind: domainmodel.AuthorityProject, ID: "semantic-book"}
	put := func(kind domainmodel.DocumentKind, id string, value any) domainmodel.Patch {
		content, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return domainmodel.Patch{Document: domainmodel.DocumentRef{Kind: kind, ID: id}, Operation: domainmodel.PatchPut, Content: content}
	}
	fact := domainmodel.CanonFact{
		ID: "fact-key", Kind: domainmodel.CanonEvent, SubjectID: "hero", Predicate: "event.find",
		Value: json.RawMessage(`"捡到一把铜钥匙"`), SourceChapterID: "chapter-1",
	}
	seed := approvedRuntimeProposal("seed-semantic", target, 0, now,
		put(domainmodel.DocumentIntent, domainmodel.SingletonDocumentID, domainmodel.Intent{Premise: "陈渡循着一把铜钥匙找姐姐"}),
		put(domainmodel.DocumentPlan, "volume-1", domainmodel.PlanNode{ID: "volume-1", Kind: domainmodel.PlanVolume, Order: 1, Title: "雨城", Summary: "陈渡找姐姐"}),
		put(domainmodel.DocumentPlan, "arc-1", domainmodel.PlanNode{ID: "arc-1", Kind: domainmodel.PlanArc, ParentID: "volume-1", Order: 1, Title: "来信", Summary: "钥匙指向雨巷"}),
		put(domainmodel.DocumentPlan, "plan-1", domainmodel.PlanNode{ID: "plan-1", Kind: domainmodel.PlanChapter, Turn: "转折", ParentID: "arc-1", Order: 1, Title: "来信"}),
		put(domainmodel.DocumentPlan, "plan-2", domainmodel.PlanNode{ID: "plan-2", Kind: domainmodel.PlanChapter, Turn: "转折", ParentID: "arc-1", Order: 2, Title: "开柜", Summary: "陈渡用铜钥匙打开保险柜"}),
		put(domainmodel.DocumentEntity, "hero", domainmodel.Entity{ID: "hero", Kind: domainmodel.EntityCharacter, Name: "陈渡"}),
		put(domainmodel.DocumentManuscript, "chapter-1", domainmodel.ManuscriptChapter{
			ID: "chapter-1", PlanNodeID: "plan-1", Number: 1, Blocks: []domainmodel.ManuscriptBlock{{ID: "b1", Text: "陈渡捡到一把铜钥匙。"}},
		}),
		put(domainmodel.DocumentCanon, "fact-key", fact),
	)
	pending := seed
	pending.ApprovalState, pending.DecidedBy, pending.DecidedAt = domainmodel.ApprovalPending, nil, nil
	if _, err := authorityStore.SaveProposal(ctx, pending); err != nil {
		t.Fatalf("save seed: %v", err)
	}
	if _, err := authorityStore.CommitProposal(ctx, seed); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	fact.Value = json.RawMessage(`"捡到一枚旧邮票"`)
	proposal := domainmodel.Proposal{
		ID: "semantic-change", Target: target, BaseRevision: 1,
		Author: domainmodel.Author{Kind: domainmodel.AuthorUser, ID: "user-1"}, Reason: "钥匙改成邮票",
		Patches: []domainmodel.Patch{put(domainmodel.DocumentCanon, "fact-key", fact)}, ApprovalState: domainmodel.ApprovalPending, CreatedAt: now.Add(time.Minute),
	}
	analyze := func(replies ...litellmtest.Reply) (domainmodel.SemanticImpactReport, []string, string) {
		t.Helper()
		model := litellmtest.New(replies...)
		reportJSON, err := boundRuntime(authorityStore, testChat(model)).Analyze(ctx, proposal, domainmodel.StructuralImpact{
			Direct: []domainmodel.DocumentRef{{Kind: domainmodel.DocumentCanon, ID: "fact-key"}},
			Affected: []domainmodel.DocumentRef{
				{Kind: domainmodel.DocumentManuscript, ID: "chapter-1"}, {Kind: domainmodel.DocumentPlan, ID: "plan-2"}, {Kind: domainmodel.DocumentPlan, ID: "arc-1"},
				{Kind: domainmodel.DocumentIntent, ID: domainmodel.SingletonDocumentID},
			},
		})
		if err != nil {
			t.Fatalf("analyze semantic impact: %v", err)
		}
		var report domainmodel.SemanticImpactReport
		if err := json.Unmarshal(reportJSON, &report); err != nil || !usedJSONSchema(model) {
			t.Fatalf("report = %s, schema = %v, error = %v", reportJSON, usedJSONSchema(model), err)
		}
		var inputs []string
		for _, request := range model.Requests() {
			input, err := json.Marshal(request.Messages)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"chapter-1", "plan-1", "plan-2", "arc-1", "volume-1", "fact-key", "hero", "b1"} {
				if strings.Contains(string(input), `"`+id+`"`) {
					t.Fatalf("analyzer input leaks %q: %s", id, input)
				}
			}
			inputs = append(inputs, string(input))
		}
		return report, inputs, string(model.Requests()[0].ResponseFormat.JSONSchema.Schema)
	}
	// 两个问题分开问（D87）：规划核对只看改动与没写成正文的规划（大纲、作品前提），看不到已写
	// 正文，对要核对的每一处都下结论，漏了一处就带着原因重给；还按旧设定写的换回章纲、故事弧
	// 梗概或作品前提，不再问已写章节。
	report, inputs, schema := analyze(litellmtest.Text(`{"第 2 章":{"explanation":"章纲还写着铜钥匙","stale":true}}`), litellmtest.Text(`{
		"第 1 个故事弧":{"explanation":"梗概还写着钥匙","stale":true},
		"第 2 章":{"explanation":"章纲还写着铜钥匙","stale":true},
		"作品前提":{"explanation":"前提还写着铜钥匙","stale":true}}`))
	if len(inputs) != 2 || !strings.Contains(inputs[0], "陈渡用铜钥匙打开保险柜") || !strings.Contains(inputs[0], "捡到一枚旧邮票") ||
		!strings.Contains(inputs[0], `\"premise\":\"陈渡循着一把铜钥匙找姐姐\"`) ||
		strings.Contains(inputs[0], "陈渡捡到一把铜钥匙") || !strings.Contains(schema, `"required":["第 1 个故事弧","第 2 章","作品前提"]`) ||
		!strings.Contains(inputs[1], "漏了第 1 个故事弧、作品前提") {
		t.Fatalf("planning check input = %q, schema = %s", inputs, schema)
	}
	for i, want := range []domainmodel.DocumentRef{
		{Kind: domainmodel.DocumentPlan, ID: "arc-1"}, {Kind: domainmodel.DocumentPlan, ID: "plan-2"},
		{Kind: domainmodel.DocumentIntent, ID: domainmodel.SingletonDocumentID},
	} {
		if document := report.Findings[i].Document; document == nil || *document != want {
			t.Fatalf("outline finding %d = %#v, want %v", i, report.Findings[i], want)
		}
	}
	// 大纲没问题才问已写章节：它看不到大纲，出入与重写只落在已写的章上。
	report, inputs, _ = analyze(litellmtest.Text(`{
		"第 1 个故事弧":{"explanation":"只写到钥匙指路，邮票也能指路","stale":false},
		"第 2 章":{"explanation":"对得上","stale":false},
		"作品前提":{"explanation":"前提只是起点","stale":false}}`), litellmtest.Text(`{
		"status":"conflict",
		"findings":[{"chapter":1,"explanation":"第 1 章写的是铜钥匙"}],
		"options":[
			{"strategy":"rewrite_affected","chapters":[1],"explanation":"重写第 1 章"},
			{"strategy":"reinterpret_future","explanation":"在后文重新解释"},
			{"strategy":"abandon","explanation":"放弃变更"}
		]}`))
	if len(inputs) != 2 || !strings.Contains(inputs[1], "陈渡捡到一把铜钥匙") || !strings.Contains(inputs[1], "捡到一枚旧邮票") ||
		strings.Contains(inputs[1], "陈渡用铜钥匙打开保险柜") {
		t.Fatalf("chapter analysis input = %q", inputs)
	}
	if document := report.Findings[0].Document; report.Status != domainmodel.SemanticImpactConflict || document == nil ||
		*document != (domainmodel.DocumentRef{Kind: domainmodel.DocumentManuscript, ID: "chapter-1"}) ||
		!slices.Equal(report.Options[0].ChapterIDs, []string{"chapter-1"}) {
		t.Fatalf("report = %#v", report)
	}
}

func TestRuntimePlanSubmissionEnforcesLengthBound(t *testing.T) {
	// §6.3 篇幅上界前移到工具边界（D67 章数由规划者决定，上界仍是硬边界）：越过固定
	// 篇幅在 proposal_submit 当场被拒，模型在同一会话内纠正后重新提交，而不是收尾时
	// 把整个 Operation 打死。
	ctx := context.Background()
	now := time.Date(2026, 8, 29, 14, 0, 0, 0, time.UTC)
	authorityStore, err := store.Open(ctx, filepath.Join(t.TempDir(), "ainovel.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer authorityStore.Close()
	worker, err := prompt.BuiltinWorkerProfile("architect.design")
	if err != nil {
		t.Fatalf("load architect capability: %v", err)
	}
	task := json.RawMessage(`{"fixed_chapters":1}`)
	operation := domainmodel.Operation{
		ID: "plan-1", Kind: domainmodel.OperationDevelopPlan,
		Target: domainmodel.AuthorityTarget{Kind: domainmodel.AuthorityProject, ID: "book-plan"},
		State:  domainmodel.OperationQueued,
		RunID:  createRuntimeTestRun(t, ctx, authorityStore, "book-plan", now),
		Snapshot: runtimeSnapshot(task, 1, domainmodel.ApprovalAuto,
			seedRuntimeProfile(t, ctx, authorityStore, "book-plan", worker, task, 1)),
		Input: task, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := authorityStore.CreateOperation(ctx, operation); err != nil {
		t.Fatalf("create operation: %v", err)
	}
	operation, err = authorityStore.ClaimNextOperation(ctx, "worker-1", time.Minute, now.Add(time.Second))
	if err != nil {
		t.Fatalf("claim operation: %v", err)
	}
	// 规划按序号编辑卷弧章（D66），ID 与顺序由宿主给。
	structure := map[string]any{
		"compass": map[string]any{"ending": "抵达"},
		"threads": []map[string]any{{"name": "远方", "question": "他能否抵达远方"}},
		"volumes": []map[string]any{{"volume": 1, "title": "第一卷", "summary": "开端"}},
		"arcs":    []map[string]any{{"arc": 1, "volume": 1, "title": "第一幕", "summary": "启程"}},
	}
	submission := func(reason string, chapters ...map[string]any) json.RawMessage {
		args := map[string]any{"reason": reason, "chapters": chapters}
		maps.Copy(args, structure)
		raw, _ := json.Marshal(args)
		return raw
	}
	overshoot := submission("越过固定篇幅",
		map[string]any{"chapter": 1, "arc": 1, "title": "第一章", "summary": "出发", "turn": "他上了路"},
		map[string]any{"chapter": 2, "arc": 1, "title": "第二章", "summary": "多余", "turn": "多余"})
	exact := submission("按固定篇幅规划一章", map[string]any{"chapter": 1, "arc": 1, "title": "第一章", "summary": "出发", "turn": "他上了路"})
	model := litellmtest.New(callReply("plan-submit-1", "proposal_submit", overshoot), callReply("plan-submit-2", "proposal_submit", exact))
	runtime := boundRuntime(authorityStore, testChat(model))
	runtime.now = func() time.Time { return now.Add(3 * time.Second) }
	outcome, err := runtime.Execute(ctx, operation)
	if err != nil {
		t.Fatalf("execute plan: %v", err)
	}
	if outcome.Proposal == nil || len(outcome.Proposal.Patches) != 4 {
		t.Fatalf("outcome = %#v, want corrected 4-patch proposal (compass, volume, arc, chapter)", outcome)
	}
	if len(model.Requests()) != 2 {
		t.Fatalf("model requests = %d, want overshoot rejected then corrected resubmission", len(model.Requests()))
	}
}

// runtimeSnapshot 按 D45 形状冻结快照；config 为已落盘的 Execution Profile 摘要，
// 只走工具校验不执行模型的用例可以用任意非空值。
func runtimeSnapshot(input json.RawMessage, base domainmodel.Revision, policy domainmodel.ApprovalPolicy, config string) domainmodel.ExecutionSnapshot {
	return domainmodel.ExecutionSnapshot{
		Executor: prompt.ExecutorIdentity, BaseRevision: base, InputDigest: domainmodel.Digest(input),
		ConfigDigest: config, ApprovalPolicy: policy,
	}
}

// seedRuntimeProfile 编译并落盘一份 Execution Profile，返回其摘要供快照引用。
func seedRuntimeProfile(
	t *testing.T,
	ctx context.Context,
	authorityStore *store.Store,
	projectID string,
	worker prompt.WorkerProfile,
	task json.RawMessage,
	base domainmodel.Revision,
) string {
	t.Helper()
	compiled, err := prompt.NewRegistry(authorityStore).Reload(ctx, prompt.CompileRequest{
		ProjectID: projectID, Worker: worker, Ownership: json.RawMessage(`[]`),
		Intent: domainmodel.Intent{Premise: "测试作品"}, StoryContext: json.RawMessage(`{"revision":` + fmt.Sprint(base) + `}`),
		Task: task, BaseRevision: base, ProjectOverlayRevision: base,
	}, time.Date(2026, 8, 18, 11, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("seed execution profile: %v", err)
	}
	return compiled.ProfileDigest
}

// TestWriterSubmissionRequiresRedeclaringChapterFacts：工具边界当场执行 D41 的重申报与
// 来源归属，偏差回给模型在同一会话内纠正。
func TestWriterSubmissionRequiresRedeclaringChapterFacts(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	authorityStore, err := store.Open(ctx, filepath.Join(t.TempDir(), "ainovel.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer authorityStore.Close()
	target := domainmodel.AuthorityTarget{Kind: domainmodel.AuthorityProject, ID: "book-1"}
	document := func(ref domainmodel.DocumentRef, value any) domainmodel.Patch {
		content, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return domainmodel.Patch{Document: ref, Operation: domainmodel.PatchPut, Content: content}
	}
	seed, err := change.New(authorityStore).Prepare(ctx, domainmodel.Proposal{
		ID: "seed", Target: target, Author: domainmodel.Author{Kind: domainmodel.AuthorUser, ID: "user-1"}, Reason: "seed",
		Patches: []domainmodel.Patch{
			document(domainmodel.DocumentRef{Kind: domainmodel.DocumentIntent, ID: "root"}, domainmodel.Intent{Premise: "凡人修仙"}),
			document(domainmodel.DocumentRef{Kind: domainmodel.DocumentPlan, ID: "volume-1"}, domainmodel.PlanNode{ID: "volume-1", Kind: domainmodel.PlanVolume, Title: "卷一", Summary: "入道"}),
			document(domainmodel.DocumentRef{Kind: domainmodel.DocumentPlan, ID: "arc-1"}, domainmodel.PlanNode{ID: "arc-1", Kind: domainmodel.PlanArc, ParentID: "volume-1", Title: "弧一", Summary: "山门"}),
			document(domainmodel.DocumentRef{Kind: domainmodel.DocumentPlan, ID: "plan-1"}, domainmodel.PlanNode{ID: "plan-1", Kind: domainmodel.PlanChapter, Turn: "转折", ParentID: "arc-1", Order: 1, Title: "第一章", Summary: "抵达"}),
			document(domainmodel.DocumentRef{Kind: domainmodel.DocumentEntity, ID: "hero"}, domainmodel.Entity{ID: "hero", Kind: domainmodel.EntityCharacter, Name: "主角"}),
			document(domainmodel.DocumentRef{Kind: domainmodel.DocumentManuscript, ID: "chapter-1"}, domainmodel.ManuscriptChapter{
				ID: "chapter-1", PlanNodeID: "plan-1", Number: 1, Author: domainmodel.AuthorAI,
				Blocks: []domainmodel.ManuscriptBlock{{ID: "block-1", Text: "旧正文"}},
			}),
			document(domainmodel.DocumentRef{Kind: domainmodel.DocumentCanon, ID: "fact-1"}, domainmodel.CanonFact{
				ID: "fact-1", Kind: domainmodel.CanonEvent, SubjectID: "hero", Predicate: "event.done", Value: json.RawMessage(`true`), SourceChapterID: "chapter-1",
			}),
		},
		ApprovalState: domainmodel.ApprovalPending, CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("prepare seed: %v", err)
	}
	if seed, err = change.Decide(seed, domainmodel.ApprovalApproved, domainmodel.Author{Kind: domainmodel.AuthorUser, ID: "user-1"}, now); err != nil {
		t.Fatalf("approve seed: %v", err)
	}
	if _, err := change.New(authorityStore).Commit(ctx, seed); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	input := json.RawMessage(`{"chapter_id":"chapter-1","chapter_plan_id":"plan-1","findings":["结尾仓促"]}`)
	operation := domainmodel.Operation{
		ID: "rewrite-1", Kind: domainmodel.OperationRewriteChapter, Target: target, State: domainmodel.OperationQueued,
		RunID: createRuntimeTestRun(t, ctx, authorityStore, "book-1", now), Snapshot: runtimeSnapshot(input, 1, domainmodel.ApprovalAuto, "profile"),
		Input: input, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := authorityStore.CreateOperation(ctx, operation); err != nil {
		t.Fatalf("create operation: %v", err)
	}
	if operation, err = authorityStore.ClaimOperationForExecutor(ctx, operation.ID, "worker-1", prompt.ExecutorIdentity, time.Minute, now); err != nil {
		t.Fatalf("claim operation: %v", err)
	}
	chapter := domainmodel.ManuscriptChapter{
		ID: "chapter-1", PlanNodeID: "plan-1", Number: 1, Author: domainmodel.AuthorAI,
		Blocks: []domainmodel.ManuscriptBlock{{ID: "block-1", Text: "新正文"}},
	}
	if _, err := workspace.New(authorityStore).PutChapter(ctx, operation.ID,
		workspace.Draft{Revision: operation.Snapshot.BaseRevision, Chapter: chapter}, operation.Attempt, now); err != nil {
		t.Fatalf("put workspace chapter: %v", err)
	}
	runtime := NewRuntime(authorityStore)
	var last domainmodel.Proposal
	submitTool := testTool(t, runtime, operation, "writer.revise@1", prompt.ToolProposalSubmit, func(proposal domainmodel.Proposal) (domainmodel.Proposal, error) {
		last = proposal
		return proposal, nil
	})
	submit := func(fields map[string]any) error {
		args := map[string]any{"reason": "重写提交"}
		maps.Copy(args, fields)
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		_, err = submitTool(ctx, raw)
		return err
	}
	fresh := []map[string]any{{"subject": "主角", "predicate": "event.left", "value": true}}
	// 漏掉重申报：错误用故事标签指出是哪条事实，模型不需要知道它的 ID。
	err = submit(map[string]any{"facts": fresh})
	if !errors.Is(err, domainmodel.ErrStructuralConflict) || !strings.Contains(err.Error(), "must redeclare canon 「主角」event.done（第 1 章）") ||
		strings.Contains(err.Error(), `"fact-1"`) {
		t.Fatalf("missing redeclaration err = %v", err)
	}
	foreign := []map[string]any{{"subject": "主角", "predicate": "event.elsewhere", "value": true, "chapter": 9}}
	if err := submit(map[string]any{"facts": foreign}); !errors.Is(err, domainmodel.ErrInvalid) || !strings.Contains(err.Error(), "第 9 章") {
		t.Fatalf("foreign source err = %v", err)
	}
	done := []map[string]any{{"subject": "主角", "predicate": "event.done"}}
	for name, fields := range map[string]map[string]any{
		"confirm and add": {"facts": fresh, "confirm_facts": done},
		"confirm only":    {"confirm_facts": done},
		"remove and add":  {"facts": fresh, "remove_facts": done},
	} {
		if err := submit(fields); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// 解析只看冻结基线：之后的用户修改不会成为确认的原值。
	edited, _ := json.Marshal(domainmodel.CanonFact{ID: "fact-1", Kind: domainmodel.CanonEvent, SubjectID: "hero", Predicate: "event.done",
		PreviousValue: json.RawMessage(`true`), Value: json.RawMessage(`false`), SourceChapterID: "chapter-1"})
	userEdit := approvedRuntimeProposal("user-edit", target, 1, now, domainmodel.Patch{
		Document: domainmodel.DocumentRef{Kind: domainmodel.DocumentCanon, ID: "fact-1"}, Operation: domainmodel.PatchPut, Content: edited})
	pending := userEdit
	pending.ApprovalState, pending.DecidedBy, pending.DecidedAt = domainmodel.ApprovalPending, nil, nil
	if _, err := authorityStore.SaveProposal(ctx, pending); err != nil {
		t.Fatal(err)
	}
	if _, err := authorityStore.CommitProposal(ctx, userEdit); err != nil {
		t.Fatal(err)
	}
	frozen := testTool(t, runtime, operation, "writer.revise@1", prompt.ToolProposalSubmit, func(proposal domainmodel.Proposal) (domainmodel.Proposal, error) {
		last = proposal
		return proposal, nil
	})
	raw, _ := json.Marshal(map[string]any{"reason": "确认", "confirm_facts": done})
	if _, err := frozen(ctx, raw); err != nil {
		t.Fatal(err)
	}
	var confirmed domainmodel.CanonFact
	if err := json.Unmarshal(last.Patches[1].Content, &confirmed); err != nil || string(confirmed.Value) != "true" || string(confirmed.PreviousValue) != "true" {
		t.Fatalf("confirmation must copy the frozen baseline: %s, %v", last.Patches[1].Content, err)
	}
}

// 返工重申报本章的章题与章纲（D87）：变了的随正文入账，工具边界照常校验。
func TestReworkSubmissionCarriesItsOutline(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	authorityStore, err := store.Open(ctx, filepath.Join(t.TempDir(), "ainovel.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer authorityStore.Close()
	target := domainmodel.AuthorityTarget{Kind: domainmodel.AuthorityProject, ID: "book-1"}
	seedWrittenChapters(t, ctx, authorityStore, target, now, 2)
	input := json.RawMessage(`{"chapter_id":"chapter-1","chapter_plan_id":"plan-1","directives":[` +
		`{"id":"rework","scope":"plan_node:plan-1","text":"钥匙改成邮票，重写","status":"active","rework":true}]}`)
	operation := domainmodel.Operation{
		ID: "rework-1", Kind: domainmodel.OperationReworkChapter, Target: target, State: domainmodel.OperationQueued,
		RunID: createRuntimeTestRun(t, ctx, authorityStore, "book-1", now), Snapshot: runtimeSnapshot(input, 1, domainmodel.ApprovalAuto, "profile"),
		Input: input, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := authorityStore.CreateOperation(ctx, operation); err != nil {
		t.Fatalf("create operation: %v", err)
	}
	if operation, err = authorityStore.ClaimOperationForExecutor(ctx, operation.ID, "worker-1", prompt.ExecutorIdentity, time.Minute, now); err != nil {
		t.Fatalf("claim operation: %v", err)
	}
	chapter := domainmodel.ManuscriptChapter{ID: "chapter-1", PlanNodeID: "plan-1", Number: 1, Author: domainmodel.AuthorAI,
		Blocks: []domainmodel.ManuscriptBlock{{ID: "b1", Text: "信里是一枚旧邮票。"}}}
	if _, err := workspace.New(authorityStore).PutChapter(ctx, operation.ID,
		workspace.Draft{Revision: operation.Snapshot.BaseRevision, Chapter: chapter}, operation.Attempt, now); err != nil {
		t.Fatalf("put workspace chapter: %v", err)
	}
	var submitted domainmodel.Proposal
	submit := testTool(t, NewRuntime(authorityStore), operation, "writer.rework@1", prompt.ToolProposalSubmit, func(proposal domainmodel.Proposal) (domainmodel.Proposal, error) {
		submitted = proposal
		return proposal, nil
	})
	if _, err := submit(ctx, json.RawMessage(`{"reason":"按返工要求重写","outline":{"title":"火漆印下的旧邮票","summary":"推进","turn":"信里是一枚旧邮票"},`+
		`"entities":[{"name":"陈渡","kind":"character"}],"facts":[{"subject":"陈渡","predicate":"event.find","value":"信里是一枚旧邮票"}]}`)); err != nil {
		t.Fatalf("submit rework: %v", err)
	}
	index := slices.IndexFunc(submitted.Patches, func(patch domainmodel.Patch) bool {
		return patch.Document == domainmodel.DocumentRef{Kind: domainmodel.DocumentPlan, ID: "plan-1"}
	})
	if index < 0 {
		t.Fatalf("patches = %+v", submitted.Patches)
	}
	var node domainmodel.PlanNode
	if err := json.Unmarshal(submitted.Patches[index].Content, &node); err != nil || node.Title != "火漆印下的旧邮票" || node.Summary != "推进" || node.Turn != "信里是一枚旧邮票" || node.ParentID != "arc-1" {
		t.Fatalf("outline = %+v, %v", node, err)
	}
}

// seedWrittenChapters 提交一个写完 count 章的最小作品：正文 chapter-N 实现计划 plan-N。
func seedWrittenChapters(t *testing.T, ctx context.Context, authorityStore *store.Store, target domainmodel.AuthorityTarget, now time.Time, count int) {
	t.Helper()
	document := func(ref domainmodel.DocumentRef, value any) domainmodel.Patch {
		content, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return domainmodel.Patch{Document: ref, Operation: domainmodel.PatchPut, Content: content}
	}
	patches := []domainmodel.Patch{
		document(domainmodel.DocumentRef{Kind: domainmodel.DocumentIntent, ID: "root"}, domainmodel.Intent{Premise: "凡人修仙"}),
		document(domainmodel.DocumentRef{Kind: domainmodel.DocumentPlan, ID: "volume-1"}, domainmodel.PlanNode{ID: "volume-1", Kind: domainmodel.PlanVolume, Title: "卷一", Summary: "入道"}),
		document(domainmodel.DocumentRef{Kind: domainmodel.DocumentPlan, ID: "arc-1"}, domainmodel.PlanNode{ID: "arc-1", Kind: domainmodel.PlanArc, ParentID: "volume-1", Title: "弧一", Summary: "山门"}),
	}
	for number := 1; number <= count; number++ {
		plan, chapter := fmt.Sprintf("plan-%d", number), fmt.Sprintf("chapter-%d", number)
		patches = append(patches,
			document(domainmodel.DocumentRef{Kind: domainmodel.DocumentPlan, ID: plan}, domainmodel.PlanNode{
				ID: plan, Kind: domainmodel.PlanChapter, Turn: "转折", ParentID: "arc-1", Order: number, Title: fmt.Sprintf("第%d章", number), Summary: "推进"}),
			document(domainmodel.DocumentRef{Kind: domainmodel.DocumentManuscript, ID: chapter}, domainmodel.ManuscriptChapter{
				ID: chapter, PlanNodeID: plan, Number: number, Author: domainmodel.AuthorAI,
				Blocks: []domainmodel.ManuscriptBlock{{ID: "b1", Text: "正文"}}}),
		)
	}
	seed := approvedRuntimeProposal("seed", target, 0, now, patches...)
	pending := seed
	pending.ApprovalState, pending.DecidedBy, pending.DecidedAt = domainmodel.ApprovalPending, nil, nil
	if _, err := authorityStore.SaveProposal(ctx, pending); err != nil {
		t.Fatalf("save seed: %v", err)
	}
	if _, err := authorityStore.CommitProposal(ctx, seed); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
}

// seedRuntimeProject 提交一个结构完整的最小作品（intent、卷/弧/章计划、实体 hero、
// 一条状态事实）到 revision 1：工具边界会跑 change 引擎的结构校验，夹具必须真实。
func seedRuntimeProject(t *testing.T, ctx context.Context, authorityStore *store.Store, target domainmodel.AuthorityTarget, now time.Time) {
	t.Helper()
	document := func(ref domainmodel.DocumentRef, value any) domainmodel.Patch {
		content, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return domainmodel.Patch{Document: ref, Operation: domainmodel.PatchPut, Content: content}
	}
	seed := approvedRuntimeProposal("seed", target, 0, now,
		document(domainmodel.DocumentRef{Kind: domainmodel.DocumentIntent, ID: "root"}, domainmodel.Intent{Premise: "凡人修仙"}),
		document(domainmodel.DocumentRef{Kind: domainmodel.DocumentPlan, ID: "volume-1"}, domainmodel.PlanNode{ID: "volume-1", Kind: domainmodel.PlanVolume, Title: "卷一", Summary: "入道"}),
		document(domainmodel.DocumentRef{Kind: domainmodel.DocumentPlan, ID: "arc-1"}, domainmodel.PlanNode{ID: "arc-1", Kind: domainmodel.PlanArc, ParentID: "volume-1", Title: "弧一", Summary: "山门"}),
		document(domainmodel.DocumentRef{Kind: domainmodel.DocumentPlan, ID: "chapter-plan-1"}, domainmodel.PlanNode{ID: "chapter-plan-1", Kind: domainmodel.PlanChapter, Turn: "转折", ParentID: "arc-1", Order: 1, Title: "第一章", Summary: "抵达山门"}),
		document(domainmodel.DocumentRef{Kind: domainmodel.DocumentEntity, ID: "hero"}, domainmodel.Entity{ID: "hero", Kind: domainmodel.EntityCharacter, Name: "主角"}),
		document(domainmodel.DocumentRef{Kind: domainmodel.DocumentCanon, ID: "hero-origin"}, domainmodel.CanonFact{
			ID: "hero-origin", Kind: domainmodel.CanonState, SubjectID: "hero", Predicate: "state.origin", Value: json.RawMessage(`"农家子"`),
		}),
	)
	pending := seed
	pending.ApprovalState, pending.DecidedBy, pending.DecidedAt = domainmodel.ApprovalPending, nil, nil
	if _, err := authorityStore.SaveProposal(ctx, pending); err != nil {
		t.Fatalf("save seed: %v", err)
	}
	if _, err := authorityStore.CommitProposal(ctx, seed); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
}

// testChat 是脚本化 Provider 上名为 "model" 的模型。
func testChat(p litellm.Provider) agentcore.Model {
	client, err := litellm.New(p)
	if err != nil {
		panic(err)
	}
	return agentcore.Model{Client: client, Request: litellm.Request{Model: "model"}}
}

// boundRuntime 是测试里的常驻 Runtime 加一套默认绑定；模型摘要固定为 "model"。
func boundRuntime(authorityStore *store.Store, chat agentcore.Model) *Runtime {
	runtime := NewRuntime(authorityStore)
	runtime.Bind(models.Bindings{Default: models.Binding{Provider: "test", Model: "model", Digest: "model", Chat: chat}})
	return runtime
}

// TestPublishActivityStreamsItemsAndToolDetail 守卫结构化产出的直播：规划提交的参数
// 一边生成，一边按条进入同一栏目的输出块；参数收齐时条目补上作用对象。
func TestPublishActivityStreamsItemsAndToolDetail(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	hub := activity.NewHub()
	runtime := &Runtime{now: func() time.Time { return now }, activity: hub}
	operation := domainmodel.Operation{
		ID: "op-plan", RunID: "run-plan", Kind: domainmodel.OperationDevelopPlan,
		Target: domainmodel.AuthorityTarget{Kind: domainmodel.AuthorityProject, ID: "book-plan"},
	}
	stream := newLiveStream(nil)
	call := agentcore.ToolCall{ID: "call-p", Name: "proposal_submit", Args: json.RawMessage(`{"reason":"首次规划"}`)}
	runtime.publishActivity(operation, delta(litellm.BlockStart{Index: 0, Block: litellm.ToolUseBlock{ID: call.ID, Name: call.Name}}), stream)
	for _, piece := range []string{`{"reason":"首次规划","chapters":[{"chapter":1,"title":"无人`, `签收"},{"chap`, `ter":2,"title":"雨夜来客"}],"entities":[{"name":"陈渡","kind":"character"}]}`} {
		runtime.publishActivity(operation, delta(litellm.ToolUseDelta{Index: 0, Arguments: piece}), stream)
	}
	runtime.publishActivity(operation, agentcore.ToolStart{Call: call}, stream)
	snapshot, _ := hub.Snapshot("book-plan")
	if len(snapshot.Output) != 2 || snapshot.Output[0].Section != "大纲" || string(snapshot.Output[0].Text) != "第 1 章 · 无人签收\n第 2 章 · 雨夜来客" ||
		snapshot.Output[1].Section != "设定" || string(snapshot.Output[1].Text) != "陈渡（人物）" {
		t.Fatalf("item blocks = %#v", snapshot.Output)
	}
	if len(snapshot.Entries) != 1 || snapshot.Entries[0].Detail != "首次规划" || snapshot.Entries[0].Bytes == 0 {
		t.Fatalf("entries = %#v", snapshot.Entries)
	}
}

// 偏好分析只看故事语言（D66）：改前改后的文字与章号，没有作品、候选、正文与段落的编号。
func TestRuntimeLearnsPreferenceInStoryLanguage(t *testing.T) {
	ctx := context.Background()
	authorityStore, err := store.Open(ctx, filepath.Join(t.TempDir(), "ainovel.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer authorityStore.Close()
	model := litellmtest.New(litellmtest.Text(`{
		"summary":"偏好克制","evidence":["第 1 章把直白的悲伤改成了动作"],
		"proposed_rules":["用动作表现情绪"],"proposed_style_preferences":{}}`))
	runtime := boundRuntime(authorityStore, testChat(model))
	candidate, err := runtime.AnalyzePreference(ctx, domainmodel.PreferenceLearningInput{
		CandidateID: "restraint", ProfileID: "user-1", Scope: "global", ProjectID: "book-learning",
		FromRevision: 2, ToRevision: 3, ManuscriptEdits: []domainmodel.ManuscriptEditEvidence{{
			ChapterID: "chapter-1", Chapter: 1, BlockID: "block-1", Before: "他感到非常悲伤。", After: "他捏皱袖口。",
		}},
	})
	if err != nil || candidate.ID != "restraint" || candidate.ProposedRules[0] != "用动作表现情绪" {
		t.Fatalf("candidate = %#v, %v", candidate, err)
	}
	request, err := json.Marshal(model.Requests()[0].Messages)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(request), `\"chapter\":1`) || !strings.Contains(string(request), "他捏皱袖口") {
		t.Fatalf("analyzer input misses the edit: %s", request)
	}
	for _, id := range []string{"chapter-1", "block-1", "book-learning", "restraint"} {
		if strings.Contains(string(request), id) {
			t.Fatalf("analyzer input leaks %q: %s", id, request)
		}
	}
}
