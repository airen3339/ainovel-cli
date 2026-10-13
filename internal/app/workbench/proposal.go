package workbench

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	projectdoc "github.com/voocel/ainovel-cli/internal/app/project"
	"github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/ainovel-cli/internal/domain/narrative"
)

// ProposalView 是待裁决稿件里正文以外的变更，用故事语言描述（D66）：大纲按提案生效后
// 的序号成树，人物用名称，设定写成种类+主体+内容，不露文档 ID、谓词键与原始内容。正文候选
// 另见 Candidates，罗盘的前后对比由入口按当前罗盘呈现。
type ProposalView struct {
	// Summary 是规模摘要的各段，如「新增 1 卷 · 5 个故事弧 · 7 章」「设定 21 条」。
	Summary []string `json:"summary,omitempty"`
	// Changes 逐处说清改了什么（D77），如「第 2 章《旧》改名《新》」：主编的回执、确认卡
	// 与变更记录都用它，不依赖主编复述。
	Changes  []string               `json:"changes,omitempty"`
	Outline  []narrative.VolumeView `json:"outline,omitempty"`  // 新增或修订的卷弧章，祖先补齐
	Entities []narrative.EntityView `json:"entities,omitempty"` // 新增或修订的人物、地点、物品、组织
	Facts    []Fact                 `json:"facts,omitempty"`    // 新增或修订的设定
	Compass  *model.Compass         `json:"compass,omitempty"`  // 提案后的故事罗盘，未改动为空
	Removed  []string               `json:"removed,omitempty"`  // 删除的内容
	Other    []string               `json:"other,omitempty"`    // 其余种类的改动
}

func proposalView(project projectdoc.Snapshot, patches []model.Patch) (ProposalView, error) {
	base := storyOf(project)
	next, err := base.Apply(patches)
	if err != nil {
		return ProposalView{}, err
	}
	var view ProposalView
	var plans, entities []string
	added := make(map[model.PlanNodeKind]int)
	revisedPlans, addedEntities, revisedEntities := 0, 0, 0
	for _, patch := range patches {
		line, err := changeLine(project, base, next, patch)
		if err != nil {
			return ProposalView{}, err
		}
		view.Changes = append(view.Changes, line)
	}
	for _, patch := range patches {
		ref := patch.Document
		if patch.Operation != model.PatchPut {
			view.Removed = append(view.Removed, describe(base, ref))
			continue
		}
		_, existed := base.Label(ref)
		switch ref.Kind {
		case model.DocumentManuscript:
		case model.DocumentPlan:
			var node model.PlanNode
			if err := json.Unmarshal(patch.Content, &node); err != nil {
				return ProposalView{}, fmt.Errorf("decode proposed plan node: %w", err)
			}
			plans = append(plans, node.ID)
			if existed {
				revisedPlans++
			} else {
				added[node.Kind]++
			}
		case model.DocumentEntity:
			entities = append(entities, ref.ID)
			if existed {
				revisedEntities++
			} else {
				addedEntities++
			}
		case model.DocumentCanon:
			var fact model.CanonFact
			if err := json.Unmarshal(patch.Content, &fact); err != nil {
				return ProposalView{}, fmt.Errorf("decode proposed fact: %w", err)
			}
			view.Facts = append(view.Facts, factOf(next, fact))
		case model.DocumentCompass:
			view.Compass = new(model.Compass)
			if err := json.Unmarshal(patch.Content, view.Compass); err != nil {
				return ProposalView{}, fmt.Errorf("decode proposed compass: %w", err)
			}
		default:
			view.Other = append(view.Other, describe(next, ref))
		}
	}
	view.Outline, view.Entities = next.Outline(plans), next.Entities(entities)

	var nodes []string
	for _, kind := range []model.PlanNodeKind{model.PlanVolume, model.PlanArc, model.PlanChapter} {
		if added[kind] > 0 {
			nodes = append(nodes, fmt.Sprintf("%d %s", added[kind], planNoun[kind]))
		}
	}
	summary := func(format string, count int) {
		if count > 0 {
			view.Summary = append(view.Summary, fmt.Sprintf(format, count))
		}
	}
	if len(nodes) > 0 {
		view.Summary = append(view.Summary, "新增 "+strings.Join(nodes, " · "))
	}
	summary("修订大纲 %d 处", revisedPlans)
	summary("新增人物地点 %d 个", addedEntities)
	summary("修订人物地点 %d 个", revisedEntities)
	summary("设定 %d 条", len(view.Facts))
	summary("删除 %d 项", len(view.Removed))
	summary("其他变更 %d 项", len(view.Other))
	return view, nil
}

var planNoun = map[model.PlanNodeKind]string{model.PlanVolume: "卷", model.PlanArc: "个故事弧", model.PlanChapter: "章"}

// documentNoun 是故事索引之外的文档种类名：它们没有章号或名称可指称。
var documentNoun = map[model.DocumentKind]string{
	model.DocumentIntent: "创作意图", model.DocumentCompass: "故事罗盘", model.DocumentOwnership: "锁定规则", model.DocumentApproval: "审批方式",
	model.DocumentOverlay: "书级创作规则", model.DocumentAssets: "启用的创作包", model.DocumentDirective: "创作要求",
	model.DocumentAdjudication: "裁决记录", model.DocumentAttachment: "附件",
}

// describe 用故事语言指称一份文档：故事里有的用章号、名称；其余用种类名，不露 ID。
func describe(story *narrative.Story, ref model.DocumentRef) string {
	if label, ok := story.Label(ref); ok {
		return label
	}
	if noun, ok := documentNoun[ref.Kind]; ok {
		return noun
	}
	return string(ref.Kind)
}

func storyOf(project projectdoc.Snapshot) *narrative.Story {
	return narrative.New(narrative.Content{Plan: project.Plan, Entities: project.Entities, Canon: project.Canon, Manuscript: project.Manuscript})
}

// changeLine 用一句故事语言说清一处改动：大纲说改名、改梗概或挪位置，设定说新的内容，
// 作品方向逐项说改成了什么。
func changeLine(project projectdoc.Snapshot, base, next *narrative.Story, patch model.Patch) (string, error) {
	ref := patch.Document
	if patch.Operation != model.PatchPut {
		if ref.Kind == model.DocumentOwnership {
			for _, rule := range project.Ownership {
				if rule.Target.Key() == ref.ID {
					return "放开" + describe(base, rule.Target), nil
				}
			}
		}
		return "删去" + describe(base, ref), nil
	}
	switch ref.Kind {
	case model.DocumentManuscript:
		if _, existed := base.Label(ref); existed {
			return "改写" + describe(next, ref), nil
		}
		return "入稿" + describe(next, ref), nil
	case model.DocumentPlan:
		var node model.PlanNode
		if err := json.Unmarshal(patch.Content, &node); err != nil {
			return "", fmt.Errorf("decode proposed plan node: %w", err)
		}
		before, existed := model.IndexPlan(project.Plan)[node.ID]
		if !existed {
			return "新增" + next.PlanLabel(node.ID), nil
		}
		var parts []string
		if node.Title != before.Title {
			parts = append(parts, fmt.Sprintf("改名《%s》", node.Title))
		}
		if node.Summary != before.Summary {
			parts = append(parts, "梗概改为："+node.Summary)
		}
		if node.ParentID != before.ParentID {
			parts = append(parts, "移到"+next.PlanLabel(node.ParentID)+"下")
		}
		return base.PlanLabel(node.ID) + strings.Join(parts, "，"), nil
	case model.DocumentEntity:
		var entity model.Entity
		if err := json.Unmarshal(patch.Content, &entity); err != nil {
			return "", fmt.Errorf("decode proposed entity: %w", err)
		}
		was, existed := base.Label(ref)
		if !existed {
			return fmt.Sprintf("新增%s「%s」", entity.Kind.Noun(), entity.Name), nil
		}
		line := fmt.Sprintf("「%s」是%s", entity.Name, entity.Kind.Noun())
		if was != "「"+entity.Name+"」" {
			line = fmt.Sprintf("%s改名「%s」，是%s", was, entity.Name, entity.Kind.Noun())
		}
		if len(entity.Aliases) > 0 {
			line += "，又名" + strings.Join(entity.Aliases, "、")
		}
		return line, nil
	case model.DocumentCanon:
		var fact model.CanonFact
		if err := json.Unmarshal(patch.Content, &fact); err != nil {
			return "", fmt.Errorf("decode proposed fact: %w", err)
		}
		subject, text := next.FactNote(fact)
		return fmt.Sprintf("设定 · 「%s」%s", subject, text), nil
	case model.DocumentIntent:
		var intent model.Intent
		if err := json.Unmarshal(patch.Content, &intent); err != nil {
			return "", fmt.Errorf("decode proposed intent: %w", err)
		}
		return intentChange(project.Intent, intent), nil
	case model.DocumentApproval:
		var setting model.ApprovalSetting
		if err := json.Unmarshal(patch.Content, &setting); err != nil {
			return "", fmt.Errorf("decode proposed approval: %w", err)
		}
		return "审批改为：" + approvalLabel[setting.Policy], nil
	case model.DocumentOwnership:
		var rule model.OwnershipRule
		if err := json.Unmarshal(patch.Content, &rule); err != nil {
			return "", fmt.Errorf("decode proposed ownership: %w", err)
		}
		if rule.Control == model.ControlGuided {
			return describe(next, rule.Target) + "只按要求改：「" + strings.Join(rule.Guidance, "；") + "」", nil
		}
		return "锁定" + describe(next, rule.Target), nil
	case model.DocumentDirective:
		var directive model.Directive
		if err := json.Unmarshal(patch.Content, &directive); err != nil {
			return "", fmt.Errorf("decode proposed directive: %w", err)
		}
		if directive.Status == model.DirectiveRetired {
			return "退掉要求「" + directive.Text + "」", nil
		}
		return "记下要求「" + directive.Text + "」", nil
	}
	return "改动" + describe(next, ref), nil
}

// approvalLabel 是审批方式的读者用语。
var approvalLabel = map[model.ApprovalPolicy]string{
	model.ApprovalAuto: "自动放行", model.ApprovalMilestone: "重大变化等你确认", model.ApprovalManual: "每一步都等你确认",
}

func intentChange(before, after model.Intent) string {
	var parts []string
	field := func(name, old, value string) {
		if value != old {
			parts = append(parts, name+"改为「"+value+"」")
		}
	}
	list := func(name string, old, value []string) {
		if !slices.Equal(old, value) {
			parts = append(parts, name+"改为「"+strings.Join(value, "、")+"」")
		}
	}
	field("故事前提", before.Premise, after.Premise)
	field("读者", before.Audience, after.Audience)
	field("结局方向", before.EndingDirection, after.EndingDirection)
	list("想给读者的体验", before.DesiredExperience, after.DesiredExperience)
	list("必须有", before.Required, after.Required)
	list("不要", before.Forbidden, after.Forbidden)
	return "作品方向：" + strings.Join(parts, "，")
}
