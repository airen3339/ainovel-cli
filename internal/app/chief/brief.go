package chief

import (
	"context"
	"fmt"
	"strings"

	"github.com/voocel/ainovel-cli/internal/app/workbench"
	"github.com/voocel/ainovel-cli/internal/domain/model"
)

// brief 是附在用户每句话前的书况（D74）：宿主此刻的记录，确定性渲染，故事语言。它在
// 会话的动态尾部，不破坏稳定前缀的缓存。
func (s *Session) brief(ctx context.Context, view View) (string, error) {
	snap, err := s.service.bench.WorkbenchSnapshot(ctx, s.projectID)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	writing := s.writing
	s.mu.Unlock()
	lines := []string{
		"【书况】宿主此刻的记录，不是用户说的话。",
		"作品：" + oneLine(snap.Intent.Premise, 80),
		"进度：" + progress(snap),
		"创作：" + situation(snap, writing),
	}
	if decision := pendingDecision(snap); decision != "" {
		lines = append(lines, "待用户决定："+decision)
	}
	looking := "全书"
	if view.Chapter > 0 {
		looking = chapterLabel(snap, view.Chapter)
	}
	lines = append(lines, "用户正在看："+looking, fmt.Sprintf("生效的要求：%d 条", len(snap.Directives)))
	return strings.Join(lines, "\n"), nil
}

func progress(snap workbench.WorkbenchSnapshot) string {
	written, planned := 0, 0
	for _, node := range snap.Outline {
		if node.Node.Kind != model.PlanChapter {
			continue
		}
		planned++
		if node.State == workbench.ChapterConfirmed {
			written++
		}
	}
	length := snap.Length
	text := fmt.Sprintf("已入稿 %d 章，已规划 %d 章", written, planned)
	switch {
	case length.Fixed > 0:
		return text + fmt.Sprintf("；篇幅由用户固定为 %d 章", length.Fixed)
	case length.Final > 0:
		return text + fmt.Sprintf("；篇幅交给 AI，已承诺 %d 章收官", length.Final)
	case length.Compass != nil && length.Compass.ScaleMax > 0:
		return text + fmt.Sprintf("；篇幅交给 AI，预计 %d 章", length.Compass.ScaleMax)
	default:
		return text + "；篇幅交给 AI"
	}
}

func situation(snap workbench.WorkbenchSnapshot, writing bool) string {
	if writing {
		if snap.CurrentPhase != "" {
			return "正在推进（" + snap.CurrentPhase + "）"
		}
		return "正在推进"
	}
	if snap.Run == nil {
		return "还没开始"
	}
	switch snap.Run.State {
	case model.RunPaused:
		return "已暂停"
	case model.RunWaitingUser:
		return "停下等用户决定"
	case model.RunCompleted:
		return "全书已完成"
	case model.RunFailed:
		return "出了问题停下：" + snap.Run.StateReason
	case model.RunCancelled:
		return "已停止"
	default:
		return "停着，可以继续"
	}
}

// pendingDecision 是等用户决定的事与用户能怎么做；没有为空。
func pendingDecision(snap workbench.WorkbenchSnapshot) string {
	decision := snap.Decision
	switch {
	case decision == nil:
		return ""
	case decision.Held != "":
		return oneLine(decision.Reason, 120) + "。用户按 y 接受现状，或让它再改一次（resume_creation）"
	case decision.HasProposal:
		return oneLine(decision.Reason, 120) + "。稿件用 book_draft 看；用户按 y 通过，不满意就说理由，用 reject_draft 退回"
	default:
		return oneLine(decision.Reason, 120)
	}
}

func chapterLabel(snap workbench.WorkbenchSnapshot, number int) string {
	for _, node := range snap.Outline {
		if node.Node.Kind == model.PlanChapter && node.Number == number {
			return fmt.Sprintf("第 %d 章《%s》", number, node.Node.Title)
		}
	}
	return fmt.Sprintf("第 %d 章", number)
}

func oneLine(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return text
}
