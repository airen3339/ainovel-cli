package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type Revision int64

const InitialRevision Revision = 0

type DocumentKind string

const (
	DocumentIntent         DocumentKind = "intent"
	DocumentPlan           DocumentKind = "plan"
	DocumentCompass        DocumentKind = "compass"
	DocumentEntity         DocumentKind = "entity"
	DocumentCanon          DocumentKind = "canon"
	DocumentManuscript     DocumentKind = "manuscript"
	DocumentAttachment     DocumentKind = "attachment"
	DocumentOwnership      DocumentKind = "ownership"
	DocumentApproval       DocumentKind = "approval"
	DocumentOverlay        DocumentKind = "overlay"
	DocumentAssets         DocumentKind = "assets"
	DocumentDirective      DocumentKind = "directive"
	DocumentAdjudication   DocumentKind = "adjudication"
	DocumentCreatorProfile DocumentKind = "creator_profile"
	DocumentPack           DocumentKind = "pack"
)

type DocumentRef struct {
	Kind DocumentKind `json:"kind"`
	ID   string       `json:"id"`
}

func (r DocumentRef) Validate() error {
	if _, err := DocumentType(r.Kind); err != nil {
		return err
	}
	if strings.TrimSpace(r.ID) == "" {
		return fmt.Errorf("document id is required: %w", ErrInvalid)
	}
	return nil
}

func (r DocumentRef) Key() string {
	return string(r.Kind) + ":" + r.ID
}

type PatchOperation string

const (
	PatchPut    PatchOperation = "put"
	PatchDelete PatchOperation = "delete"
)

type Patch struct {
	Document  DocumentRef     `json:"document"`
	Operation PatchOperation  `json:"operation"`
	Content   json.RawMessage `json:"content,omitempty"`
}

func (p Patch) Validate() error {
	if err := p.Document.Validate(); err != nil {
		return err
	}
	switch p.Operation {
	case PatchPut:
		if len(p.Content) == 0 || !json.Valid(p.Content) {
			return fmt.Errorf("put patch content must be valid JSON: %w", ErrInvalid)
		}
	case PatchDelete:
		if len(p.Content) != 0 {
			return fmt.Errorf("delete patch cannot contain content: %w", ErrInvalid)
		}
	default:
		return fmt.Errorf("unknown patch operation %q: %w", p.Operation, ErrInvalid)
	}
	return nil
}

type AuthorKind string

const (
	AuthorUser      AuthorKind = "user"
	AuthorAI        AuthorKind = "ai"
	AuthorExtension AuthorKind = "extension"
	AuthorSystem    AuthorKind = "system"
)

type Author struct {
	Kind AuthorKind `json:"kind"`
	ID   string     `json:"id"`
}

// Machine 报告作者是否是机器（AI 或扩展）：它们的提交受创作边界约束，用户由自己背书。
func (a Author) Machine() bool { return a.Kind == AuthorAI || a.Kind == AuthorExtension }

type ApprovalState string

const (
	ApprovalPending  ApprovalState = "pending"
	ApprovalApproved ApprovalState = "approved"
	ApprovalRejected ApprovalState = "rejected"
)

// ImpactReport 是提案的影响：Structural 是 Change Engine 按依赖图算出的结构影响；
// Semantic 是面向用户三选一的语义影响报告；Compliance 是自动批准前对 locked/guided
// 约束的独立合规裁定。各报告在产出处校验，之后按类型读取。
type ImpactReport struct {
	Structural *StructuralImpact         `json:"structural,omitempty"`
	Semantic   *SemanticImpactReport     `json:"semantic,omitempty"`
	Compliance *SemanticComplianceReport `json:"compliance,omitempty"`
}

// StructuralImpact 是一次变更直接改动的文档与沿依赖受牵连的文档。
type StructuralImpact struct {
	Direct   []DocumentRef `json:"direct"`
	Affected []DocumentRef `json:"affected"`
}

// Proposal describes a requested authority change. It is not authoritative until
// the store commits it as a ChangeSet and assigns a new revision.
type Proposal struct {
	ID           string          `json:"id"`
	OperationID  string          `json:"operation_id,omitempty"`
	Target       AuthorityTarget `json:"authority_target"`
	BaseRevision Revision        `json:"base_revision"`
	Author       Author          `json:"author"`
	Reason       string          `json:"reason"`
	// Source 是促成这份提案的用户的话（D77）：主编提出的修改指回对话里那条消息。
	Source        string        `json:"source,omitempty"`
	Patches       []Patch       `json:"patches"`
	Impact        ImpactReport  `json:"impact_report"`
	ApprovalState ApprovalState `json:"approval_state"`
	DecidedBy     *Author       `json:"decided_by,omitempty"`
	DecidedAt     *time.Time    `json:"decided_at,omitempty"`
	// DecisionReason 是裁决附带的反馈（D26）：否决理由入账为该章作用域的 Directive（§4.9）。
	DecisionReason string    `json:"decision_reason,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// ChangeSet is an approved Proposal committed to one authority revision.
type ChangeSet struct {
	Proposal
	NewRevision Revision `json:"new_revision"`
}

type DocumentVersion struct {
	Target      AuthorityTarget `json:"authority_target"`
	Document    DocumentRef     `json:"document"`
	Revision    Revision        `json:"revision"`
	Content     json.RawMessage `json:"content"`
	ChangeSetID string          `json:"change_set_id"`
}

func (p Proposal) Validate() error {
	if strings.TrimSpace(p.ID) == "" {
		return fmt.Errorf("proposal id is required: %w", ErrInvalid)
	}
	if p.OperationID != "" && strings.TrimSpace(p.OperationID) == "" {
		return fmt.Errorf("operation id cannot be blank: %w", ErrInvalid)
	}
	if err := p.Target.Validate(); err != nil {
		return err
	}
	if p.BaseRevision < InitialRevision {
		return fmt.Errorf("base revision cannot be negative: %w", ErrInvalid)
	}
	// 系统只作裁决人（审批策略），不撰写提案。
	switch p.Author.Kind {
	case AuthorUser, AuthorAI, AuthorExtension:
	default:
		return fmt.Errorf("proposal author must be user, ai or extension, got %q: %w", p.Author.Kind, ErrInvalid)
	}
	if strings.TrimSpace(p.Author.ID) == "" {
		return fmt.Errorf("author id is required: %w", ErrInvalid)
	}
	if strings.TrimSpace(p.Reason) == "" {
		return fmt.Errorf("change reason is required: %w", ErrInvalid)
	}
	if _, ok := ParseConversationSource(p.Source); p.Source != "" && !ok {
		return fmt.Errorf("proposal source %q is not a conversation message: %w", p.Source, ErrInvalid)
	}
	if len(p.Patches) == 0 {
		return fmt.Errorf("proposal requires at least one patch: %w", ErrInvalid)
	}
	seen := make(map[string]struct{}, len(p.Patches))
	for i, patch := range p.Patches {
		if err := patch.Validate(); err != nil {
			return fmt.Errorf("patch %d: %w", i, err)
		}
		key := patch.Document.Key()
		if _, ok := seen[key]; ok {
			return fmt.Errorf("duplicate patch document %q: %w", key, ErrInvalid)
		}
		seen[key] = struct{}{}
	}
	switch p.ApprovalState {
	case ApprovalPending:
		if p.DecidedBy != nil || p.DecidedAt != nil || p.DecisionReason != "" {
			return fmt.Errorf("pending proposal cannot have an approval decision: %w", ErrInvalid)
		}
	case ApprovalApproved, ApprovalRejected:
		if p.DecidedBy == nil || p.DecidedAt == nil || p.DecidedAt.IsZero() {
			return fmt.Errorf("approval decision requires decider and time: %w", ErrInvalid)
		}
		switch p.DecidedBy.Kind {
		case AuthorUser, AuthorSystem:
		default:
			return fmt.Errorf("approval decider must be user or system, got %q: %w", p.DecidedBy.Kind, ErrInvalid)
		}
		if strings.TrimSpace(p.DecidedBy.ID) == "" {
			return fmt.Errorf("approval decider id is required: %w", ErrInvalid)
		}
	default:
		return fmt.Errorf("unknown approval state %q: %w", p.ApprovalState, ErrInvalid)
	}
	if p.CreatedAt.IsZero() {
		return fmt.Errorf("created_at is required: %w", ErrInvalid)
	}
	return nil
}

// SameSubmission 报告两份提案是否是同一份提交：只允许基线与结构、合规影响不同。
// 待裁决稿件的重定位与合规证据回写只能改这些，补丁、作者、理由与身份一律不变。
func SameSubmission(a, b Proposal) bool {
	normalize := func(p Proposal) []byte {
		p.BaseRevision, p.Impact.Structural, p.Impact.Compliance = 0, nil, nil
		raw, err := json.Marshal(p)
		if err != nil {
			return nil
		}
		return raw
	}
	left, right := normalize(a), normalize(b)
	return left != nil && bytes.Equal(left, right)
}

func (c ChangeSet) Validate() error {
	if err := c.Proposal.Validate(); err != nil {
		return err
	}
	if c.ApprovalState != ApprovalApproved {
		return fmt.Errorf("change set must be approved: %w", ErrInvalid)
	}
	if c.NewRevision <= InitialRevision || c.NewRevision != c.BaseRevision+1 {
		return fmt.Errorf("change set revision must follow base revision: %w", ErrInvalid)
	}
	return nil
}
