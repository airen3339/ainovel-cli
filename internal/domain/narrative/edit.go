package narrative

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/voocel/ainovel-cli/internal/domain/model"
)

// Edit 是主编提出的修改（D77）：Worker 提交的同一份故事语言（不含正文、罗盘的终局与篇幅、
// 事实确认），加上作品方向、锁定与审批三段。长线可以改（D91）：用户点名要追的问题。
type Edit struct {
	Threads  []ThreadEdit         `json:"threads,omitempty"`
	Volumes  []VolumeEdit         `json:"volumes,omitempty"`
	Arcs     []ArcEdit            `json:"arcs,omitempty"`
	Chapters []ChapterEdit        `json:"chapters,omitempty"`
	Entities []EntityEdit         `json:"entities,omitempty"`
	Facts    []FactEdit           `json:"facts,omitempty"`
	Remove   []FactRef            `json:"remove_facts,omitempty"`
	Intent   *IntentEdit          `json:"intent,omitempty"`
	Locks    []LockEdit           `json:"locks,omitempty"`
	Approval model.ApprovalPolicy `json:"approval,omitempty"`
}

// IntentEdit 改作品方向：给出的字段整体替换，没给的不变；列表给空即清空。
type IntentEdit struct {
	Premise           string   `json:"premise,omitempty"`
	Audience          string   `json:"audience,omitempty"`
	EndingDirection   string   `json:"ending_direction,omitempty"`
	DesiredExperience []string `json:"desired_experience,omitempty"`
	Required          []string `json:"required,omitempty"`
	Forbidden         []string `json:"forbidden,omitempty"`
}

// LockEdit 设定一章、一个实体或一条设定的控制级别：locked 不许 AI 改，guided 按 guidance
// 改，open 放开。一章管它的大纲，写了正文也管正文。chapter、entity、fact 给一个。
type LockEdit struct {
	Chapter  int                `json:"chapter,omitempty"`
	Entity   string             `json:"entity,omitempty"`
	Fact     *FactRef           `json:"fact,omitempty"`
	Control  model.ControlLevel `json:"control"`
	Guidance []string           `json:"guidance,omitempty"`
}

// Settings 是故事索引之外、修改要对照的作品现状。
type Settings struct {
	Intent    model.Intent
	Ownership []model.OwnershipRule
}

// ResolveEdit 把主编的修改解析成补丁。作品方向返回修改后的完整 Intent，供调用方核对
// 新写入的文字都出自用户（D74 引用即署名）。
func (s *Story) ResolveEdit(edit Edit, current Settings) ([]model.Patch, *model.Intent, error) {
	r := s.resolver(true)
	submission := Submission{
		Threads: edit.Threads, Volumes: edit.Volumes, Arcs: edit.Arcs, Chapters: edit.Chapters, Entities: edit.Entities, Facts: edit.Facts, Remove: edit.Remove,
	}
	if err := r.submission(nil, submission, nil); err != nil {
		return nil, nil, err
	}
	var intent *model.Intent
	if edit.Intent != nil {
		next := edit.Intent.apply(current.Intent)
		if err := r.put(model.DocumentIntent, model.SingletonDocumentID, next); err != nil {
			return nil, nil, err
		}
		intent = &next
	}
	if edit.Approval != "" {
		if err := r.put(model.DocumentApproval, model.SingletonDocumentID, model.ApprovalSetting{Policy: edit.Approval}); err != nil {
			return nil, nil, err
		}
	}
	if len(edit.Locks) > 0 {
		// 锁定按这次修改之后的书定位：同一次修改里新写的设定、新建的人物也能锁。
		book, err := s.Apply(r.patches)
		if err != nil {
			return nil, nil, err
		}
		for _, lock := range edit.Locks {
			if err := r.lock(book, lock, current.Ownership); err != nil {
				return nil, nil, err
			}
		}
	}
	if len(r.patches) == 0 {
		return nil, nil, fmt.Errorf("修改是空的：没有给出任何要改的内容: %w", model.ErrInvalid)
	}
	return r.patches, intent, nil
}

func (e IntentEdit) apply(intent model.Intent) model.Intent {
	replace := func(field *string, value string) {
		if value != "" {
			*field = value
		}
	}
	replace(&intent.Premise, e.Premise)
	replace(&intent.Audience, e.Audience)
	replace(&intent.EndingDirection, e.EndingDirection)
	if e.DesiredExperience != nil {
		intent.DesiredExperience = e.DesiredExperience
	}
	if e.Required != nil {
		intent.Required = e.Required
	}
	if e.Forbidden != nil {
		intent.Forbidden = e.Forbidden
	}
	return intent
}

// lock 写入或移除 book 上某处的控制规则：open 是默认约定，没有规则时什么也不做。
func (r *resolver) lock(book *Story, lock LockEdit, rules []model.OwnershipRule) error {
	var targets []model.DocumentRef
	switch {
	case lock.Chapter > 0 && lock.Entity == "" && lock.Fact == nil:
		plan, err := book.chapterPlan(lock.Chapter)
		if err != nil {
			return err
		}
		targets = append(targets, model.DocumentRef{Kind: model.DocumentPlan, ID: plan.ID})
		if chapter, ok := book.written[plan.ID]; ok {
			targets = append(targets, model.DocumentRef{Kind: model.DocumentManuscript, ID: chapter.ID})
		}
	case lock.Entity != "" && lock.Chapter == 0 && lock.Fact == nil:
		entity, err := book.entity(lock.Entity)
		if err != nil {
			return err
		}
		targets = append(targets, model.DocumentRef{Kind: model.DocumentEntity, ID: entity.ID})
	case lock.Fact != nil && lock.Chapter == 0 && lock.Entity == "":
		fact, err := book.factAt(*lock.Fact)
		if err != nil {
			return err
		}
		targets = append(targets, model.DocumentRef{Kind: model.DocumentCanon, ID: fact.ID})
	default:
		return errors.New("锁定要指明一章（chapter）、一个人物地点物品组织（entity）或一条设定（fact），三选一")
	}
	for _, target := range targets {
		ref := model.DocumentRef{Kind: model.DocumentOwnership, ID: target.Key()}
		if lock.Control == model.ControlOpen {
			if slices.ContainsFunc(rules, func(rule model.OwnershipRule) bool { return rule.Target == target }) {
				r.patches = append(r.patches, model.Patch{Document: ref, Operation: model.PatchDelete})
			}
			continue
		}
		rule := model.OwnershipRule{Target: target, Control: lock.Control, Guidance: lock.Guidance}
		if err := rule.Validate(); err != nil {
			return err
		}
		content, err := json.Marshal(rule)
		if err != nil {
			return fmt.Errorf("encode ownership rule: %w", err)
		}
		r.patches = append(r.patches, model.Patch{Document: ref, Operation: model.PatchPut, Content: content})
	}
	return nil
}
