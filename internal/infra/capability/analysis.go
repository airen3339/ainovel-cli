package capability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/voocel/ainovel-cli/internal/domain/change"
	"github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/ainovel-cli/internal/domain/narrative"
	"github.com/voocel/ainovel-cli/internal/infra/capability/prompt"
	"github.com/voocel/ainovel-cli/internal/infra/llm"
	"github.com/voocel/litellm"
	"github.com/voocel/litellm/catalog"
)

// 三个语义判断都是三分法第二类（§11 第 7 条）：边界清晰的单次 LLM 函数。
// 提示词、输入组装与输出契约就近留在这里，通用调用与严格解码收口在 llm。

func (r *Runtime) AnalyzePreference(ctx context.Context, input model.PreferenceLearningInput) (model.PreferenceCandidate, error) {
	if err := input.Validate(); err != nil {
		return model.PreferenceCandidate{}, err
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return model.PreferenceCandidate{}, fmt.Errorf("encode preference learning input: %w", err)
	}
	cacheKey, err := prompt.CacheKey(input.ProjectID, "preference.learn@1", model.Digest(payload), input.CandidateID)
	if err != nil {
		return model.PreferenceCandidate{}, err
	}
	binding, ok := r.bindingFor("")
	if !ok {
		return model.PreferenceCandidate{}, fmt.Errorf("preference analysis requires a bound model: %w", model.ErrInvalid)
	}
	var candidate model.PreferenceCandidate
	if _, err := llm.Structured(ctx, binding, llm.Call{
		System: "你是写作偏好分析器。只从用户实际 before/after 修改中归纳可复用偏好；证据不足就明确报错，不得猜测人格或题材偏好。规则要短、可执行，并引用具体修改证据。",
		Input:  string(payload),
		Schema: llm.Schema{
			Name: "creator_preference_candidate", Description: "从用户正文修改中提出可确认的写作偏好",
			JSON: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"summary":        map[string]any{"type": "string"},
					"evidence":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"proposed_rules": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"proposed_style_preferences": map[string]any{
						"type": "object", "additionalProperties": map[string]any{"type": "string"},
					},
				},
				"required":             []string{"summary", "evidence", "proposed_rules", "proposed_style_preferences"},
				"additionalProperties": false,
			},
		},
		CacheKey: cacheKey, SessionID: input.CandidateID,
	}, &candidate); err != nil {
		return model.PreferenceCandidate{}, err
	}
	candidate.ID = input.CandidateID
	candidate.SourceProjectID = input.ProjectID
	candidate.SourceRevision = input.ToRevision
	if err := candidate.Validate(); err != nil {
		return model.PreferenceCandidate{}, err
	}
	return candidate, nil
}

// Analyze 判断改动与已写的故事是否冲突（S4）。分析器只看故事语言（D66）：改前改后的那几处、
// 可能受牵连的章节正文与设定，章节按章号说，宿主再换回文档引用。
func (r *Runtime) Analyze(
	ctx context.Context,
	proposal model.Proposal,
	structural change.StructuralImpact,
) (json.RawMessage, error) {
	story, err := r.storyAt(ctx, proposal.Target, proposal.BaseRevision)
	if err != nil {
		return nil, err
	}
	impact, err := story.Impact(proposal.Reason, proposal.Patches, structural.Direct, structural.Affected)
	if err != nil {
		return nil, err
	}
	input, err := json.Marshal(impact)
	if err != nil {
		return nil, fmt.Errorf("encode semantic impact input: %w", err)
	}
	cacheKey, err := prompt.CacheKey(proposal.Target.ID, "semantic.impact@2", model.Digest(input), proposal.ID)
	if err != nil {
		return nil, err
	}
	binding, ok := r.bindingFor("")
	if !ok {
		return nil, fmt.Errorf("%w: model is not bound", change.ErrSemanticUnavailable)
	}
	chapters := map[string]any{"type": "array", "items": map[string]any{"type": "integer", "minimum": 1}}
	var report narrative.ImpactReport
	if _, err := llm.Structured(ctx, binding, llm.Call{
		System: "你是小说变更影响分析器。对照改前改后与已写章节，判断这次改动与已经发生的故事事实、人物动机和因果链是否冲突。consistent 时 findings/options 必须为空；conflict 或 uncertain 时必须给出 rewrite_affected、reinterpret_future、abandon 三种明确选项，rewrite_affected 列出要重写的章号。章节一律按章号说。不得替用户作决定。",
		Input:  string(input),
		Schema: llm.Schema{
			Name: "story_semantic_impact", Description: "分析故事变更的语义影响和可选处理方案",
			JSON: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"status": map[string]any{"type": "string", "enum": []string{"consistent", "conflict", "uncertain"}},
					"findings": map[string]any{"type": "array", "items": map[string]any{
						"type": "object", "properties": map[string]any{
							"chapter":     map[string]any{"type": "integer", "minimum": 1, "description": "出入所在的章号；不落在某一章时省略"},
							"explanation": map[string]any{"type": "string"},
						}, "required": []string{"explanation"}, "additionalProperties": false,
					}},
					"options": map[string]any{"type": "array", "items": map[string]any{
						"type": "object", "properties": map[string]any{
							"strategy":    map[string]any{"type": "string", "enum": []string{"rewrite_affected", "reinterpret_future", "abandon"}},
							"chapters":    chapters,
							"explanation": map[string]any{"type": "string"},
						}, "required": []string{"strategy", "explanation"}, "additionalProperties": false,
					}},
				},
				"required": []string{"status", "findings", "options"}, "additionalProperties": false,
			},
		},
		CacheKey: cacheKey, SessionID: proposal.ID + ":semantic-impact",
	}, &report); err != nil {
		return nil, err
	}
	converted, err := story.Report(report)
	if err != nil {
		return nil, err
	}
	if err := converted.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(converted)
}

func (r *Runtime) AnalyzeSemanticCompliance(
	ctx context.Context,
	operation model.Operation,
	proposal model.Proposal,
	constraints []model.OwnershipRule,
) (model.SemanticComplianceReport, error) {
	if operation.State != model.OperationRunning || operation.Snapshot.Executor != r.Identity() {
		return model.SemanticComplianceReport{}, fmt.Errorf("semantic compliance operation context is invalid: %w", model.ErrStateConflict)
	}
	// 合规检查由 Operation Engine 在执行结束后单独调用，用调用时刻的默认绑定（D57）。
	binding, ok := r.bindingFor("")
	if !ok {
		return model.SemanticComplianceReport{}, fmt.Errorf("semantic compliance requires a bound model: %w", model.ErrStateConflict)
	}
	type constraintDocument struct {
		Rule    model.OwnershipRule `json:"rule"`
		Content json.RawMessage     `json:"authoritative_content"`
	}
	constraintDocuments := make([]constraintDocument, 0, len(constraints))
	for _, rule := range constraints {
		document, err := r.store.GetDocument(ctx, operation.Target, rule.Target, operation.Snapshot.BaseRevision)
		if err != nil {
			return model.SemanticComplianceReport{}, err
		}
		constraintDocuments = append(constraintDocuments, constraintDocument{
			Rule: rule, Content: append(json.RawMessage(nil), document.Content...),
		})
	}
	input, err := json.Marshal(struct {
		Constraints []constraintDocument `json:"constraints"`
		Patches     []model.Patch        `json:"candidate_patches"`
	}{Constraints: constraintDocuments, Patches: proposal.Patches})
	if err != nil {
		return model.SemanticComplianceReport{}, fmt.Errorf("encode semantic compliance input: %w", err)
	}
	cacheKey, err := prompt.CacheKey(
		operation.Target.ID, "semantic.compliance@1", operation.Snapshot.ConfigDigest,
		operation.ID+":semantic-compliance",
	)
	if err != nil {
		return model.SemanticComplianceReport{}, err
	}
	var report model.SemanticComplianceReport
	feed := r.judgmentFeed(operation, "semantic_compliance")
	spent, err := llm.Structured(ctx, binding, llm.Call{
		System: "你是独立的小说事实合规检查器。只判断候选正文是否违背用户 locked/guided 约束；不得改写正文。证据不足必须返回 uncertain。pass 时 findings 必须为空。",
		Input:  string(input),
		Schema: llm.Schema{
			Name: "story_semantic_compliance", Description: "检查候选正文是否符合用户故事约束",
			JSON: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"status": map[string]any{"type": "string", "enum": []string{"pass", "conflict", "uncertain"}},
					"findings": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"constraint": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"kind": map[string]any{"type": "string"},
										"id":   map[string]any{"type": "string"},
									},
									"required": []string{"kind", "id"}, "additionalProperties": false,
								},
								"explanation": map[string]any{"type": "string"},
							},
							"required": []string{"constraint", "explanation"}, "additionalProperties": false,
						},
					},
				},
				"required": []string{"status", "findings"}, "additionalProperties": false,
			},
		},
		CacheKey: cacheKey, SessionID: operation.ID + ":semantic-compliance",
		Observe: feed.observe, Retried: feed.retried,
	}, &report)
	feed.finish(err)
	// 判断用的是默认绑定、不在 Agent 会话里：对话记录记下实际模型与结论，用量按同一价目
	// 并入所在尝试，创作团队的统计计入所在任务。
	judgment := semanticJudgment{Provider: binding.Provider, Model: binding.Model}
	if err == nil {
		err = report.Validate()
	}
	if err != nil {
		judgment.Error = err.Error()
	} else {
		judgment.Report = &report
	}
	if recordErr := r.recordJudgment(ctx, operation, judgment, judgmentUsage(spent, binding.Chat.Pricing)); recordErr != nil {
		return model.SemanticComplianceReport{}, errors.Join(err, recordErr)
	}
	if err != nil {
		return model.SemanticComplianceReport{}, err
	}
	return report, nil
}

// semanticJudgment 是一次语义合规判断的对话记录：实际模型，以及结论或失败原因。
type semanticJudgment struct {
	Provider string                          `json:"provider"`
	Model    string                          `json:"model"`
	Report   *model.SemanticComplianceReport `json:"report,omitempty"`
	Error    string                          `json:"error,omitempty"`
}

func (r *Runtime) recordJudgment(ctx context.Context, operation model.Operation, judgment semanticJudgment, spent model.Usage) error {
	payload, err := json.Marshal(judgment)
	if err != nil {
		return fmt.Errorf("encode semantic judgment: %w", err)
	}
	_, err = r.store.AppendTranscript(ctx, model.TranscriptEntry{
		OperationID: operation.ID, Attempt: operation.Attempt,
		Kind: model.TranscriptJudgment, Payload: payload, CreatedAt: r.now(),
	}, spent, 0)
	return err
}

// judgmentUsage 按绑定价目折算单次调用的用量，与 Agent 消息同一口径：价目缺失或算不出时
// 不计花费。
func judgmentUsage(usage litellm.Usage, pricing *catalog.Pricing) model.Usage {
	spent := model.Usage{Input: usage.InputTokens, Output: usage.OutputTokens, CacheRead: usage.CacheReadTokens}
	if pricing != nil {
		if cost, err := pricing.Cost(usage); err == nil {
			spent.Cost = cost.Total
		}
	}
	return spent
}
