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
	"github.com/voocel/ainovel-cli/internal/infra/llm/models"
	"github.com/voocel/litellm"
	"github.com/voocel/litellm/catalog"
)

// 这些语义判断都是三分法第二类（§11 第 7 条）：边界清晰的单次 LLM 函数。
// 提示词、输入组装与输出契约就近留在这里，通用调用与严格解码收口在 llm。

// preferenceEdit 是偏好分析看到的一处用户修改：故事语言（D66），按章号说，不带内部编号。
type preferenceEdit struct {
	Chapter int    `json:"chapter"`
	Before  string `json:"before,omitempty"`
	After   string `json:"after,omitempty"`
}

func (r *Runtime) AnalyzePreference(ctx context.Context, input model.PreferenceLearningInput) (model.PreferenceCandidate, error) {
	if err := input.Validate(); err != nil {
		return model.PreferenceCandidate{}, err
	}
	edits := make([]preferenceEdit, 0, len(input.ManuscriptEdits))
	for _, edit := range input.ManuscriptEdits {
		edits = append(edits, preferenceEdit{Chapter: edit.Chapter, Before: edit.Before, After: edit.After})
	}
	payload, err := json.Marshal(struct {
		Edits []preferenceEdit `json:"edits"`
	}{edits})
	if err != nil {
		return model.PreferenceCandidate{}, fmt.Errorf("encode preference learning input: %w", err)
	}
	cacheKey, err := prompt.CacheKey(input.ProjectID, "preference.learn@2", model.Digest(payload), input.CandidateID)
	if err != nil {
		return model.PreferenceCandidate{}, err
	}
	binding, ok := r.bindingFor("")
	if !ok {
		return model.PreferenceCandidate{}, fmt.Errorf("preference analysis requires a bound model: %w", model.ErrInvalid)
	}
	var candidate model.PreferenceCandidate
	if _, err := llm.Structured(ctx, binding, llm.Call{
		System: "你是写作偏好分析器。edits 是用户亲手改过的正文段落（before 改前、after 改后，chapter 是章号）。" +
			"只从这些修改中归纳可复用的写作偏好；证据不足就明确报错，不得猜测人格或题材偏好。" +
			"规则写给之后写新章节的写作者：说怎么写，不说怎么删改；短、可执行，修改支撑到什么程度就说到什么程度，不夸大成绝对的禁令。" +
			"证据按章号引用具体的改前改后文字。",
		Input: string(payload),
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

// Analyze 判断改动的语义影响（S4、D87）。分两个问题问，各自只看自己的材料：先核对没写成正文
// 的规划还按不按改前的设定写，有就把那几处报回去，修改由提出者补齐；再判断改动与已写正文是否冲突。
// 分析器只看故事语言（D66），章节按章号说，宿主再换回文档引用。
func (r *Runtime) Analyze(
	ctx context.Context,
	proposal model.Proposal,
	structural model.StructuralImpact,
) (json.RawMessage, error) {
	content, err := r.contentAt(ctx, proposal.Target, proposal.BaseRevision)
	if err != nil {
		return nil, err
	}
	// 作品前提也是规划，改设定时要一并核对（D87）。
	intent, err := r.store.GetDocument(ctx, proposal.Target, model.DocumentRef{Kind: model.DocumentIntent, ID: model.SingletonDocumentID}, proposal.BaseRevision)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(intent.Content, &content.Intent); err != nil {
		return nil, fmt.Errorf("decode intent: %w", err)
	}
	story := narrative.New(content)
	impact, planning, err := story.Impact(proposal.Reason, proposal.Patches, structural.Direct, structural.Affected)
	if err != nil {
		return nil, err
	}
	binding, ok := r.bindingFor("")
	if !ok {
		return nil, fmt.Errorf("%w: model is not bound", change.ErrSemanticUnavailable)
	}
	if len(planning.Checks()) > 0 {
		stale, err := r.stalePlanning(ctx, binding, proposal, planning)
		if err != nil {
			return nil, err
		}
		if len(stale) > 0 {
			return json.Marshal(model.SemanticImpactReport{Status: model.SemanticImpactConflict, Findings: stale})
		}
	}
	if len(impact.Chapters) == 0 {
		return json.Marshal(model.SemanticImpactReport{Status: model.SemanticImpactConsistent})
	}
	input, err := json.Marshal(impact)
	if err != nil {
		return nil, fmt.Errorf("encode semantic impact input: %w", err)
	}
	cacheKey, err := prompt.CacheKey(proposal.Target.ID, "semantic.impact@3", model.Digest(input), proposal.ID)
	if err != nil {
		return nil, err
	}
	// 出入与重写都只能落在给出的已写章上。
	numbers := make([]int, len(impact.Chapters))
	for i, chapter := range impact.Chapters {
		numbers[i] = chapter.Chapter
	}
	written := map[string]any{"type": "integer", "enum": numbers}
	var report narrative.ImpactReport
	var converted model.SemanticImpactReport
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
							"chapter":     map[string]any{"type": "integer", "enum": numbers, "description": "出入所在的已写章号；不落在某一章时省略"},
							"explanation": map[string]any{"type": "string"},
						}, "required": []string{"explanation"}, "additionalProperties": false,
					}},
					"options": map[string]any{"type": "array", "items": map[string]any{
						"type": "object", "properties": map[string]any{
							"strategy":    map[string]any{"type": "string", "enum": []string{"rewrite_affected", "reinterpret_future", "abandon"}},
							"chapters":    map[string]any{"type": "array", "items": written},
							"explanation": map[string]any{"type": "string"},
						}, "required": []string{"strategy", "explanation"}, "additionalProperties": false,
					}},
				},
				"required": []string{"status", "findings", "options"}, "additionalProperties": false,
			},
		},
		CacheKey: cacheKey, SessionID: proposal.ID + ":semantic-impact",
		Check: func() (err error) {
			if converted, err = story.Report(report); err != nil {
				return err
			}
			return converted.Validate()
		},
	}, &report); err != nil {
		return nil, err
	}
	return json.Marshal(converted)
}

// stalePlanning 核对没写成正文的规划（D87）：只给改动与规划，不给已写正文——改后的规划与旧
// 正文对不上是已写章的事，由用户选处置，不能记到规划头上。要核对的每一处都是必填的键，逐处
// 下结论。
func (r *Runtime) stalePlanning(ctx context.Context, binding models.Binding, proposal model.Proposal, planning narrative.PlanningInput) ([]model.SemanticImpactFinding, error) {
	input, err := json.Marshal(planning)
	if err != nil {
		return nil, fmt.Errorf("encode planning check input: %w", err)
	}
	cacheKey, err := prompt.CacheKey(proposal.Target.ID, "semantic.planning@1", model.Digest(input), proposal.ID)
	if err != nil {
		return nil, err
	}
	checks := planning.Checks()
	verdict := map[string]any{
		"type": "object", "properties": map[string]any{
			"explanation": map[string]any{"type": "string"},
			"stale":       map[string]any{"type": "boolean"},
		}, "required": []string{"explanation", "stale"}, "additionalProperties": false,
	}
	properties := make(map[string]any, len(checks))
	for _, label := range checks {
		properties[label] = verdict
	}
	var verdicts map[string]narrative.PlanningVerdict
	var stale []model.SemanticImpactFinding
	if _, err := llm.Structured(ctx, binding, llm.Call{
		System: "你核对一次设定修改之后还没写成正文的规划。before/after 是这次改动的设定与人物；outline 是还没写的章纲与卷、故事弧的标题和梗概，" +
			"planned_facts 是规划时定下的设定，premise 是作品前提，都已是这次修改之后的样子。" +
			"输出的每个键是一处要核对的规划（第几卷、第几个故事弧、第几章，「主体」谓词，作品前提），每一处都下结论：explanation 说它写到这次改动的什么、" +
			"与改后的设定对不对得上；它仍按改前的设定写、与改后的设定矛盾时 stale 为 true。只对照这次改动判断，不评价写得好不好。",
		Input: string(input),
		Schema: llm.Schema{
			Name: "story_planning_check", Description: "逐处判断没写成正文的规划是否还按改前的设定写",
			JSON: map[string]any{"type": "object", "properties": properties, "required": checks, "additionalProperties": false},
		},
		CacheKey: cacheKey, SessionID: proposal.ID + ":planning-check",
		Check: func() (err error) { stale, err = planning.Stale(verdicts); return err },
	}, &verdicts); err != nil {
		return nil, err
	}
	return stale, nil
}

func (r *Runtime) AnalyzeSemanticCompliance(
	ctx context.Context,
	operation model.Operation,
	proposal model.Proposal,
	constraints []model.OwnershipRule,
) (model.SemanticComplianceReport, error) {
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
		Observe: feed.observe, Retried: feed.retried, Check: func() error { return report.Validate() },
	}, &report)
	feed.finish(err)
	// 判断用的是默认绑定、不在 Agent 会话里：对话记录记下实际模型与结论，用量按同一价目
	// 并入所在尝试，创作团队的统计计入所在任务。
	judgment := semanticJudgment{Provider: binding.Provider, Model: binding.Model}
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
