package prompt

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain/model"
)

type Slot string

const (
	SlotArchitectStoryDesign Slot = "architect.story_design"
	SlotArchitectArcExpand   Slot = "architect.arc_expand"
	SlotWriterChapterPlan    Slot = "writer.chapter_plan"
	SlotWriterChapterDraft   Slot = "writer.chapter_draft"
	SlotWriterRewrite        Slot = "writer.rewrite"
	SlotEditorStoryReview    Slot = "editor.story_review"
	SlotEditorStyleReview    Slot = "editor.style_review"
	// SlotChiefConverse 是主编的对话方法（D74）：主编不是 Worker，不经任务编译。
	SlotChiefConverse Slot = "chief.converse"
)

// Slots 是 Pack 能覆盖的全部 Prompt 槽位。
var Slots = []Slot{
	SlotArchitectStoryDesign, SlotArchitectArcExpand, SlotWriterChapterPlan, SlotWriterChapterDraft,
	SlotWriterRewrite, SlotEditorStoryReview, SlotEditorStyleReview, SlotChiefConverse,
}

type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// WorkerProfile 是一个固定职责的 Worker：用哪些 Prompt 槽位、哪些工具、哪个模型角色，
// 以及何时算完成（StopCondition，随稳定前缀交给模型）。
type WorkerProfile struct {
	ID            string
	Version       string
	PromptSlots   []Slot
	Tools         []ToolSchema
	ModelRole     string
	StopCondition string
}

// ModelRoles 是可单独绑定模型的角色：Worker Profile 声明前三个，主编用 ChiefRole；配置的
// 角色映射按这些键覆盖默认模型。
var ModelRoles = []string{"architect", "writer", "editor", ChiefRole}

// ChiefRole 是主编的模型角色（D74）。
const ChiefRole = model.ChiefRole

func KnownModelRole(role string) bool {
	for _, known := range ModelRoles {
		if known == role {
			return true
		}
	}
	return false
}

type VersionedPack struct {
	Revision model.Revision     `json:"revision"`
	Manifest model.PackManifest `json:"manifest"`
}

type VersionedCreatorProfile struct {
	Revision model.Revision       `json:"revision"`
	Profile  model.CreatorProfile `json:"profile"`
}

type CompileRequest struct {
	ProjectID       string
	Worker          WorkerProfile
	Packs           []VersionedPack
	CreatorProfiles []VersionedCreatorProfile
	Intent          model.Intent
	// Ownership、StoryContext、Task 是已渲染成故事语言的模型视图（D66），编译器只做规范化。
	Ownership              json.RawMessage
	OverlayRules           []string
	StoryContext           json.RawMessage
	Task                   json.RawMessage
	BaseRevision           model.Revision
	ProjectOverlayRevision model.Revision
}

type SourceKind string

const (
	SourceInstruction SourceKind = "instruction"
	SourceData        SourceKind = "data"
)

type Source struct {
	Layer    string         `json:"layer"`
	ID       string         `json:"id"`
	Revision model.Revision `json:"revision,omitempty"`
	Kind     SourceKind     `json:"kind"`
	Slots    []Slot         `json:"slots,omitempty"`
}

// Compiled 是编译好的 Execution Profile：ProfileDigest 是其持久化记录的内容
// 身份，Operation 以 Snapshot.ConfigDigest 引用它。
type Compiled struct {
	ProjectID           string       `json:"project_id"`
	WorkerProfile       string       `json:"worker_profile"`
	CoreProtocolVersion string       `json:"core_protocol_version"`
	StablePrefix        string       `json:"stable_prefix"`
	DynamicTail         string       `json:"dynamic_tail"`
	Tools               []ToolSchema `json:"ordered_tool_schemas"`
	Sources             []Source     `json:"sources"`
	PromptDigest        string       `json:"prompt_digest"`
	ToolSchemaDigest    string       `json:"tool_schema_digest"`
	ProfileDigest       string       `json:"execution_profile_digest"`
}

// record 是 Compiled 的持久化形态；Digest 由记录内容推导，与 ProfileDigest 一致。
func (c Compiled) record(createdAt time.Time) (model.ExecutionProfileRecord, error) {
	tools, err := json.Marshal(c.Tools)
	if err != nil {
		return model.ExecutionProfileRecord{}, fmt.Errorf("encode compiled tools: %w", err)
	}
	sources, err := json.Marshal(c.Sources)
	if err != nil {
		return model.ExecutionProfileRecord{}, fmt.Errorf("encode prompt sources: %w", err)
	}
	record := model.ExecutionProfileRecord{
		ProjectID: c.ProjectID, WorkerProfile: c.WorkerProfile,
		CoreProtocolVersion: c.CoreProtocolVersion,
		PromptDigest:        c.PromptDigest, ToolSchemaDigest: c.ToolSchemaDigest,
		StablePrefix: c.StablePrefix, DynamicTail: c.DynamicTail,
		Tools: tools, Sources: sources, CreatedAt: createdAt,
	}
	if record.Digest, err = record.Identity(); err != nil {
		return model.ExecutionProfileRecord{}, fmt.Errorf("digest execution profile: %w", err)
	}
	return record, nil
}

// ExecutorIdentity 是 LLM 执行族的身份（D45）：agent 循环的族与版本。模型不在
// 身份里——它是运行时绑定（D57），每次尝试在尝试摘要里记下实际使用的模型。
const ExecutorIdentity = "llm.agent@1"
