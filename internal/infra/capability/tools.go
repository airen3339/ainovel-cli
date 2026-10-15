package capability

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/ainovel-cli/internal/domain/narrative"
	"github.com/voocel/ainovel-cli/internal/infra/capability/prompt"
	"github.com/voocel/ainovel-cli/internal/infra/workspace"
	"github.com/voocel/litellm"
)

func (r *Runtime) toolsFor(
	operation model.Operation,
	compiled prompt.Compiled,
	submit func(model.Proposal) (model.Proposal, error),
	submitVerdict func(model.ReviewVerdict) (model.ReviewVerdict, error),
) ([]agentcore.Tool, error) {
	story := r.storyLoader(operation)
	tools := make([]agentcore.Tool, 0, len(compiled.Tools))
	for _, definition := range compiled.Tools {
		var schema map[string]any
		if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
			return nil, fmt.Errorf("decode tool schema %q: %w", definition.Name, err)
		}
		execute, err := r.toolExecutor(operation, compiled.WorkerProfile, definition.Name, story, submit, submitVerdict)
		if err != nil {
			return nil, err
		}
		// 提交成功即收尾：结果先由循环落盘，不再请求模型。
		terminate := definition.Name == prompt.ToolProposalSubmit || definition.Name == prompt.ToolVerdictSubmit
		tools = append(tools, agentcore.Tool{
			Name: definition.Name, Description: definition.Description, Schema: schema,
			Run: func(ctx context.Context, args json.RawMessage) (agentcore.Result, error) {
				result, err := execute(ctx, args)
				if err != nil {
					return agentcore.Result{}, err
				}
				return agentcore.Result{Content: []litellm.Block{litellm.Text(string(result))}, Terminate: terminate}, nil
			},
		})
	}
	return tools, nil
}

// storyLoader 按任务冻结基线懒加载故事索引（D66），同一执行内的工具共用一份；
// 加载失败不缓存，下次调用重试。
func (r *Runtime) storyLoader(operation model.Operation) func(context.Context) (*narrative.Story, error) {
	var mu sync.Mutex
	var cached *narrative.Story
	return func(ctx context.Context) (*narrative.Story, error) {
		mu.Lock()
		defer mu.Unlock()
		if cached != nil {
			return cached, nil
		}
		story, err := r.storyAt(ctx, operation.Target, operation.Snapshot.BaseRevision)
		if err != nil {
			return nil, err
		}
		cached = story
		return story, nil
	}
}

// draftView 是写手读回的本章工作稿。工作稿照的作品版本早于本任务时（前任留下的稿，D87），
// ChangedSince 按先后列出这之间书里每次修改的说明：草稿可能还照着改前的样子写。
type draftView struct {
	Blocks       []model.ManuscriptBlock `json:"blocks"`
	ChangedSince []string                `json:"changed_since,omitempty"`
}

// changesBetween 是作品从 from 到 to 之间每次修改的说明。
func (r *Runtime) changesBetween(ctx context.Context, target model.AuthorityTarget, from, to model.Revision) ([]string, error) {
	reasons := make([]string, 0, to-from)
	for revision := from + 1; revision <= to; revision++ {
		changeSet, err := r.store.GetChangeSetByRevision(ctx, target, revision)
		if err != nil {
			return nil, err
		}
		reasons = append(reasons, changeSet.Reason)
	}
	return reasons, nil
}

// storyAt 是作品在 revision 上的故事索引。任务读冻结基线，从不读最新 Revision：之后的用户
// 修改不会成为 old_value 的来源。
func (r *Runtime) storyAt(ctx context.Context, target model.AuthorityTarget, revision model.Revision) (*narrative.Story, error) {
	content, err := r.contentAt(ctx, target, revision)
	if err != nil {
		return nil, err
	}
	return narrative.New(content), nil
}

// contentAt 读作品在 revision 上的大纲、实体、设定与正文；作品意图由用得着的一方另读。
func (r *Runtime) contentAt(ctx context.Context, target model.AuthorityTarget, revision model.Revision) (narrative.Content, error) {
	var content narrative.Content
	if revision <= model.InitialRevision {
		return content, nil
	}
	var err error
	if content.Plan, err = documentsAt[model.PlanNode](ctx, r.store, target, revision, model.DocumentPlan); err != nil {
		return narrative.Content{}, err
	}
	if content.Entities, err = documentsAt[model.Entity](ctx, r.store, target, revision, model.DocumentEntity); err != nil {
		return narrative.Content{}, err
	}
	if content.Canon, err = documentsAt[model.CanonFact](ctx, r.store, target, revision, model.DocumentCanon); err != nil {
		return narrative.Content{}, err
	}
	if content.Manuscript, err = documentsAt[model.ManuscriptChapter](ctx, r.store, target, revision, model.DocumentManuscript); err != nil {
		return narrative.Content{}, err
	}
	return content, nil
}

func documentsAt[T any](ctx context.Context, store runtimeStore, target model.AuthorityTarget, revision model.Revision, kind model.DocumentKind) ([]T, error) {
	documents, err := store.ListDocuments(ctx, target, kind, revision)
	if err != nil {
		return nil, fmt.Errorf("read %s baseline: %w", kind, err)
	}
	values := make([]T, 0, len(documents))
	for _, document := range documents {
		var value T
		if err := json.Unmarshal(document.Content, &value); err != nil {
			return nil, fmt.Errorf("decode %s: %w", document.Document.Key(), err)
		}
		values = append(values, value)
	}
	return values, nil
}

type toolFunc = func(context.Context, json.RawMessage) (json.RawMessage, error)

// toolExecutor 构造单个工具；worker 是提交提案时署名的 Worker Profile 标识。返回给模型的
// 错误一律把内部 ID 换成故事标签（D66）。
func (r *Runtime) toolExecutor(
	operation model.Operation,
	worker, name string,
	story func(context.Context) (*narrative.Story, error),
	submit func(model.Proposal) (model.Proposal, error),
	submitVerdict func(model.ReviewVerdict) (model.ReviewVerdict, error),
) (toolFunc, error) {
	execute, err := r.executor(operation, worker, name, story, submit, submitVerdict)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		result, err := execute(ctx, raw)
		if err != nil {
			if base, loadErr := story(ctx); loadErr == nil {
				err = base.Humanize(err)
			}
		}
		return result, err
	}, nil
}

func (r *Runtime) executor(
	operation model.Operation,
	worker, name string,
	story func(context.Context) (*narrative.Story, error),
	submit func(model.Proposal) (model.Proposal, error),
	submitVerdict func(model.ReviewVerdict) (model.ReviewVerdict, error),
) (toolFunc, error) {
	switch name {
	case prompt.ToolAuthorityRead:
		return func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			var query narrative.Query
			if err := decodeToolArgs(raw, &query); err != nil {
				return nil, err
			}
			base, err := story(ctx)
			if err != nil {
				return nil, err
			}
			view, err := base.Read(query)
			if err != nil {
				return nil, err
			}
			return json.Marshal(view)
		}, nil
	case prompt.ToolWorkspaceRead:
		return func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			var args struct{}
			if err := decodeToolArgs(raw, &args); err != nil {
				return nil, err
			}
			draft, err := r.workspace.Chapter(ctx, operation.ID)
			if err != nil {
				return nil, err
			}
			view := draftView{Blocks: draft.Chapter.Blocks}
			if draft.Revision < operation.Snapshot.BaseRevision {
				if view.ChangedSince, err = r.changesBetween(ctx, operation.Target, draft.Revision, operation.Snapshot.BaseRevision); err != nil {
					return nil, err
				}
			}
			return json.Marshal(view)
		}, nil
	case prompt.ToolWorkspacePutChapter:
		return func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			var args struct {
				Chapter narrative.ChapterDraft `json:"chapter"`
			}
			if err := decodeToolArgs(raw, &args); err != nil {
				return nil, err
			}
			input, err := model.DecodeTaskInput(operation.Kind, operation.Input)
			if err != nil {
				return nil, err
			}
			base, err := story(ctx)
			if err != nil {
				return nil, err
			}
			// 章的身份由宿主按任务确定（D66）：模型只写段落。
			chapter, err := base.Draft(input, args.Chapter)
			if err != nil {
				return nil, err
			}
			artifact, err := r.workspace.PutChapter(ctx, operation.ID, workspace.Draft{Revision: operation.Snapshot.BaseRevision, Chapter: chapter}, operation.Attempt, r.now())
			if err != nil {
				return nil, err
			}
			return json.Marshal(receiptOf(artifact))
		}, nil
	case prompt.ToolWorkspaceReplaceBlock:
		return func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			var args struct {
				BlockID string `json:"block_id"`
				Text    string `json:"text"`
			}
			if err := decodeToolArgs(raw, &args); err != nil {
				return nil, err
			}
			artifact, err := r.workspace.ReplaceChapterBlock(ctx, operation.ID, args.BlockID, args.Text, operation.Attempt, r.now())
			if err != nil {
				return nil, err
			}
			return json.Marshal(receiptOf(artifact))
		}, nil
	case prompt.ToolWorkspacePutCandidate:
		return func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			var args struct {
				Key     string          `json:"key"`
				Content json.RawMessage `json:"content"`
			}
			if err := decodeToolArgs(raw, &args); err != nil {
				return nil, err
			}
			artifact, err := r.store.PutWorkspaceArtifact(ctx, model.WorkspaceArtifact{
				OperationID: operation.ID, Key: args.Key, MediaType: "application/json",
				Content: args.Content, UpdatedAt: r.now(),
			}, nil, operation.Attempt)
			if err != nil {
				return nil, err
			}
			return json.Marshal(receiptOf(artifact))
		}, nil
	case prompt.ToolWorkspacePutReview:
		return func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			var args struct {
				Key      string              `json:"key"`
				Findings []narrative.Finding `json:"findings"`
			}
			if err := decodeToolArgs(raw, &args); err != nil {
				return nil, err
			}
			input, err := model.TaskInputAs[model.ReviewRangeInput](operation)
			if err != nil {
				return nil, err
			}
			base, err := story(ctx)
			if err != nil {
				return nil, err
			}
			findings, err := base.Findings(input, args.Findings)
			if err != nil {
				return nil, err
			}
			content, err := json.Marshal(findings)
			if err != nil {
				return nil, fmt.Errorf("encode review findings: %w", err)
			}
			artifact, err := r.store.PutWorkspaceArtifact(ctx, model.WorkspaceArtifact{
				OperationID: operation.ID, Key: args.Key,
				MediaType: model.ReviewArtifactMediaType, Content: content, UpdatedAt: r.now(),
			}, nil, operation.Attempt)
			if err != nil {
				return nil, err
			}
			return json.Marshal(receiptOf(artifact))
		}, nil
	case prompt.ToolProposalSubmit:
		return func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			var args struct {
				Reason string `json:"reason"`
				narrative.Submission
			}
			if err := decodeToolArgs(raw, &args); err != nil {
				return nil, err
			}
			drafts, err := r.chapterDrafts(ctx, operation)
			if err != nil {
				return nil, err
			}
			input, err := model.DecodeTaskInput(operation.Kind, operation.Input)
			if err != nil {
				return nil, err
			}
			base, err := story(ctx)
			if err != nil {
				return nil, err
			}
			patches, err := base.Resolve(input, args.Submission, drafts)
			if err != nil {
				return nil, err
			}
			// 正文的故事依赖由宿主按事实变化写入（D40）。
			if patches, err = model.NormalizeSubmission(patches); err != nil {
				return nil, err
			}
			proposal := model.Proposal{
				ID: operation.ID + "-proposal", OperationID: operation.ID,
				Target: operation.Target, BaseRevision: operation.Snapshot.BaseRevision,
				Author: model.Author{Kind: model.AuthorAI, ID: worker},
				Reason: args.Reason, Patches: patches,
				ApprovalState: model.ApprovalPending, CreatedAt: r.now(),
			}
			// 确定性校验（结构、事实、任务提交契约，D41/D63/D64）在工具边界当场反馈，收尾的
			// PrepareExecution 执行同一份：否则模型看到"提交成功"，任务却在收尾失败且无从自纠。
			// 报错可能指向本次新建的文档，用应用后的故事翻译。
			if err := proposal.Validate(); err != nil {
				return nil, err
			}
			if err := r.validator.Validate(ctx, proposal); err != nil {
				if projected, applyErr := base.Apply(patches); applyErr == nil {
					return nil, projected.Humanize(err)
				}
				return nil, err
			}
			if _, err := submit(proposal); err != nil {
				return nil, err
			}
			return json.Marshal(struct {
				Status string `json:"status"`
			}{"submitted"})
		}, nil
	case prompt.ToolVerdictSubmit:
		return func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			var args struct {
				ReviewKey string                   `json:"review_key"`
				Checks    []model.RequirementCheck `json:"checks"`
			}
			if err := decodeToolArgs(raw, &args); err != nil {
				return nil, err
			}
			// 宿主已知的事实由宿主盖入，不让模型复述再校验（D60）：Revision 与证据基线取自
			// 启动快照和任务输入，章节范围就是任务请求的范围，发现取自工作区审阅记录的当前版本，
			// 结论由发现推出（D81）。
			input, err := model.TaskInputAs[model.ReviewRangeInput](operation)
			if err != nil {
				return nil, err
			}
			findings, err := r.reviewFindings(ctx, operation, args.ReviewKey)
			if err != nil {
				return nil, err
			}
			verdict := model.ReviewVerdict{
				Revision:   operation.Snapshot.BaseRevision,
				ChapterIDs: input.ChapterIDs, ReviewKey: args.ReviewKey, Basis: input.Basis.Normalize(),
				Checks: args.Checks, Findings: findings,
			}
			if err := model.ValidateReviewVerdictForOperation(operation, verdict); err != nil {
				return nil, err
			}
			verdict, err = submitVerdict(verdict)
			if err != nil {
				return nil, err
			}
			status := model.ReviewPass
			if verdict.Blocking() > 0 {
				status = model.ReviewBlocked
			}
			return json.Marshal(struct {
				Status string `json:"status"`
			}{status})
		}, nil
	default:
		return nil, fmt.Errorf("worker tool %q has no host implementation: %w", name, model.ErrInvalid)
	}
}

// reviewFindings 读取审阅记录当前版本的发现。同一执行内工作区只有本执行串行写入，
// 当前版本就是模型刚写完的那份；提交后被改由收尾时的证据检查拒绝。
func (r *Runtime) reviewFindings(ctx context.Context, operation model.Operation, key string) ([]model.ReviewFinding, error) {
	artifact, err := r.store.GetWorkspaceArtifact(ctx, operation.ID, key)
	if err != nil {
		return nil, fmt.Errorf("read review record %q (write it with workspace_put_review first): %w", key, err)
	}
	if artifact.MediaType != model.ReviewArtifactMediaType {
		return nil, fmt.Errorf("workspace artifact %q is not a review record: %w", key, model.ErrInvalid)
	}
	var findings []model.ReviewFinding
	if err := decodeToolArgs(artifact.Content, &findings); err != nil {
		return nil, fmt.Errorf("decode review record %q: %w", key, err)
	}
	return findings, nil
}

func decodeToolArgs(raw json.RawMessage, target any) error {
	if err := model.DecodeStrict(raw, target); err != nil {
		return fmt.Errorf("decode tool arguments: %w", err)
	}
	return nil
}

// workspaceReceipt 是工作区写入的回执：提交只需要键与版本。
type workspaceReceipt struct {
	Key     string `json:"key"`
	Version int64  `json:"version"`
}

func receiptOf(artifact model.WorkspaceArtifact) workspaceReceipt {
	return workspaceReceipt{Key: artifact.Key, Version: artifact.Version}
}
