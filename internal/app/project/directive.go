package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain/model"
)

type AddDirectiveCommand struct {
	ProjectID string
	ChangeID  string
	UserID    string
	// DirectiveID 为空时按创建时间生成。
	DirectiveID string
	Scope       string
	Text        string
	Constraints *model.DirectiveConstraints
	// Source 是主编代录时用户说这句话的对话消息（D74），用户直接录入为空。
	Source string
	// Rework 声明作用域内已写的正文不满足这条要求，要按它重写（D78）。
	Rework    bool
	Reason    string
	CreatedAt time.Time
}

type RetireDirectiveCommand struct {
	ProjectID   string
	ChangeID    string
	UserID      string
	DirectiveID string
	Reason      string
	CreatedAt   time.Time
}

// ReworkChaptersCommand 是用户在语义影响处置里选的“重写受影响的章”（S4，D86）：ChapterIDs
// 是要重写的正文章节，Text 是分析给出的处置说明。
type ReworkChaptersCommand struct {
	ProjectID  string
	ChangeID   string
	UserID     string
	ChapterIDs []string
	Text       string
	Reason     string
	CreatedAt  time.Time
}

type DirectiveResult struct {
	ProjectID string          `json:"project_id"`
	Revision  model.Revision  `json:"revision"`
	Directive model.Directive `json:"directive"`
}

// AddDirective 记录一条用户创作要求（§4.9）：原话保留、带作用域入账，作者只能是
// 用户，走唯一写路径；同 ID 同内容重复提交幂等，不产生新 Revision。
func (s *Repository) AddDirective(ctx context.Context, command AddDirectiveCommand) (DirectiveResult, error) {
	if err := ValidateDirectiveCommand(command.ProjectID, command.ChangeID, command.UserID, command.Reason, command.CreatedAt); err != nil {
		return DirectiveResult{}, err
	}
	id := command.DirectiveID
	if strings.TrimSpace(id) == "" {
		id = NewID("directive", command.CreatedAt)
	}
	directive := model.Directive{
		ID: id, Scope: strings.TrimSpace(command.Scope), Text: strings.TrimSpace(command.Text),
		Constraints: command.Constraints, Status: model.DirectiveActive, Source: command.Source, Rework: command.Rework,
	}
	if err := directive.Validate(); err != nil {
		return DirectiveResult{}, err
	}
	revision, err := s.putDirectives(ctx, command.ProjectID, command.ChangeID, command.UserID, command.Reason, []model.Directive{directive}, command.CreatedAt)
	return DirectiveResult{ProjectID: command.ProjectID, Revision: revision, Directive: directive}, err
}

// ReworkChapters 把要重写的章逐章记为返工要求（D78）：作用域是各章的计划节点，ID 为
// <ChangeID>:<序号>，一次决定一次提交。之后由推导逐章返工、审阅按要求核验。
func (s *Repository) ReworkChapters(ctx context.Context, command ReworkChaptersCommand) ([]model.Directive, error) {
	if err := ValidateDirectiveCommand(command.ProjectID, command.ChangeID, command.UserID, command.Reason, command.CreatedAt); err != nil {
		return nil, err
	}
	project, err := s.Project(ctx, command.ProjectID, 0)
	if err != nil {
		return nil, err
	}
	directives := make([]model.Directive, len(command.ChapterIDs))
	for i, id := range command.ChapterIDs {
		index := slices.IndexFunc(project.Manuscript, func(chapter model.ManuscriptChapter) bool { return chapter.ID == id })
		if index < 0 {
			return nil, fmt.Errorf("chapter %q is not written: %w", id, model.ErrInvalid)
		}
		directives[i] = model.Directive{
			ID: fmt.Sprintf("%s:%d", command.ChangeID, i+1), Scope: model.DirectiveScopePlanNode(project.Manuscript[index].PlanNodeID),
			Text: strings.TrimSpace(command.Text), Status: model.DirectiveActive, Rework: true,
		}
		if err := directives[i].Validate(); err != nil {
			return nil, err
		}
	}
	_, err = s.putDirectives(ctx, command.ProjectID, command.ChangeID, command.UserID, command.Reason, directives, command.CreatedAt)
	return directives, err
}

// RetireDirective 退役一条要求：不删除文档，历史 Revision 仍可查它曾对哪些章生效。
func (s *Repository) RetireDirective(ctx context.Context, command RetireDirectiveCommand) (DirectiveResult, error) {
	if err := ValidateDirectiveCommand(command.ProjectID, command.ChangeID, command.UserID, command.Reason, command.CreatedAt); err != nil {
		return DirectiveResult{}, err
	}
	target := model.AuthorityTarget{Kind: model.AuthorityProject, ID: command.ProjectID}
	revision, err := s.store.CurrentRevision(ctx, target)
	if err != nil {
		return DirectiveResult{}, err
	}
	existing, err := s.store.GetDocument(ctx, target, model.DocumentRef{Kind: model.DocumentDirective, ID: command.DirectiveID}, revision)
	if err != nil {
		return DirectiveResult{}, err
	}
	var directive model.Directive
	if err := json.Unmarshal(existing.Content, &directive); err != nil {
		return DirectiveResult{}, fmt.Errorf("decode directive: %w", err)
	}
	directive.Status = model.DirectiveRetired
	revision, err = s.putDirectives(ctx, command.ProjectID, command.ChangeID, command.UserID, command.Reason, []model.Directive{directive}, command.CreatedAt)
	return DirectiveResult{ProjectID: command.ProjectID, Revision: revision, Directive: directive}, err
}

// putDirectives 把一次决定的要求作为一次提交写入；内容都已入账即是重试，不产生新 Revision。
func (s *Repository) putDirectives(
	ctx context.Context,
	projectID, changeID, userID, reason string,
	directives []model.Directive,
	at time.Time,
) (model.Revision, error) {
	target := model.AuthorityTarget{Kind: model.AuthorityProject, ID: projectID}
	revision, err := s.store.CurrentRevision(ctx, target)
	if err != nil {
		return 0, err
	}
	var patches []model.Patch
	for _, directive := range directives {
		content, err := json.Marshal(directive)
		if err != nil {
			return 0, fmt.Errorf("encode directive: %w", err)
		}
		ref := model.DocumentRef{Kind: model.DocumentDirective, ID: directive.ID}
		existing, err := s.store.GetDocument(ctx, target, ref, revision)
		if err != nil && !errors.Is(err, model.ErrNotFound) {
			return 0, err
		}
		if err != nil || string(existing.Content) != string(content) {
			patches = append(patches, model.Patch{Document: ref, Operation: model.PatchPut, Content: content})
		}
	}
	if len(patches) == 0 {
		return revision, nil
	}
	committed, err := s.changes.CommitUser(ctx, model.Proposal{
		ID: changeID, Target: target, BaseRevision: revision,
		Author: model.Author{Kind: model.AuthorUser, ID: userID},
		Reason: reason, Patches: patches, ApprovalState: model.ApprovalPending, CreatedAt: at,
	})
	if err != nil {
		return 0, err
	}
	return committed.NewRevision, nil
}

func ValidateDirectiveCommand(projectID, changeID, userID, reason string, at time.Time) error {
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(changeID) == "" ||
		strings.TrimSpace(userID) == "" || strings.TrimSpace(reason) == "" || at.IsZero() {
		return fmt.Errorf("directive change project, change, user, reason and time are required: %w", model.ErrInvalid)
	}
	return nil
}
