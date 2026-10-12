package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/voocel/ainovel-cli/internal/app/decision"
	"github.com/voocel/ainovel-cli/internal/app/workbench"
	domainmodel "github.com/voocel/ainovel-cli/internal/domain/model"
)

// 主编提出、等你确认的修改（D77）：确认卡与稿件的决定卡共用正文下方的位置，修改在前——
// 它等着时创作也停着。y 确认；与已写章节有出入时输入序号选处置（S4）。

// changeCard 是修改的确认卡：标题线、为什么改、改了什么（有出入时是出入）、怎么确认。
func (m model) changeCard(width int) []string {
	c := m.bench.snap.Change
	bar := benchTheme.Warning.Render("▎ ")
	summary := strings.Join(c.View.Changes, "；")
	switch {
	case c.Stale:
		summary = benchTheme.Warning.Render("提出之后书又有了新变化，不能照它确认")
	case len(c.Findings) > 0:
		summary = "与已写章节有出入：" + strings.Join(c.Findings, "；")
	}
	return []string{
		sectionTitle(benchTheme.Warning.Bold(true).Render("◇ 主编提出的修改 · 等你确认"), width),
		bar + fitLine(oneLine(c.Proposal.Reason), width-2),
		bar + fitLine(oneLine(summary), width-2),
		bar + benchTheme.Muted.Render(fitLine(m.changeActions()+" · 回车查看全部", width-2)),
	}
}

func (m model) changeActions() string {
	c := m.bench.snap.Change
	switch {
	case c.Stale:
		return "跟主编说，让它按最新内容重提"
	case len(c.Options) > 0:
		var choices []string
		for i, option := range c.Options {
			choices = append(choices, fmt.Sprintf("%d %s", i+1, optionLabel(option)))
		}
		return "输入序号选怎么处理：" + strings.Join(choices, " · ")
	}
	return "y 确认 · 不要了跟主编说"
}

func optionLabel(option workbench.ChangeOption) string {
	switch option.Strategy {
	case domainmodel.ResolutionRewriteAffected:
		numbers := make([]string, len(option.Chapters))
		for i, number := range option.Chapters {
			numbers[i] = strconv.Itoa(number)
		}
		return "重写第 " + strings.Join(numbers, "、") + " 章"
	case domainmodel.ResolutionReinterpretFuture:
		return "只改设定，后文按新的写"
	}
	return "不改了"
}

// changeContent 是修改的全部内容：逐处改动、与已写章节的出入、各种处置的说明。
func (m model) changeContent() string {
	c := m.bench.snap.Change
	var body strings.Builder
	section := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		body.WriteString("\n" + styleTitle.Render(title) + "\n")
		for _, item := range items {
			body.WriteString(styleHint.Render("· ") + item + "\n")
		}
	}
	body.WriteString(styleTitle.Render("主编提出的修改") + "\n\n" + c.Proposal.Reason + "\n")
	if c.Stale {
		body.WriteString("\n" + styleWarn.Render("提出之后书又有了新变化，不能照它确认；跟主编说，让它按最新内容重提。") + "\n")
	}
	section("改动", c.View.Changes)
	section("与已写章节的出入", c.Findings)
	var options []string
	for i, option := range c.Options {
		options = append(options, fmt.Sprintf("%d %s：%s", i+1, optionLabel(option), option.Explanation))
	}
	section("怎么处理", options)
	body.WriteString("\n" + styleHint.Render(m.changeActions()))
	return body.String()
}

func (m model) openChange() model {
	return m.openBody(m.changeContent())
}

// confirmChange 是 y：没有出入的修改直接确认；有出入的要选处置，过时的要主编重提。
func (m model) confirmChange() (tea.Model, tea.Cmd) {
	c := m.bench.snap.Change
	switch {
	case c.Stale:
		m.bench.notice = "这份修改提出之后书又变了，不能照它确认；跟主编说，让它按最新内容重提"
		return m, nil
	case len(c.Options) > 0:
		m.bench.notice = "这份修改与已写章节有出入：输入序号选怎么处理"
		return m, nil
	}
	api, ctx, user, gen := m.api, m.ctx, m.deps.UserID, m.bench.gen
	id, continueAfter := c.Proposal.ID, m.waitingRun()
	return m, func() tea.Msg {
		_, err := api.Decisions.Approve(ctx, id, user, time.Now().UTC())
		return decisionDoneMsg{gen: gen, continueAfter: continueAfter, err: err, echo: "你确认了主编的修改"}
	}
}

// resolveChange 按序号选处置（S4）：重写受影响的章要归进进行中的创作，由它接着写。
func (m model) resolveChange(choice int) (tea.Model, tea.Cmd) {
	c := m.bench.snap.Change
	if c.Stale {
		m.bench.notice = "这份修改提出之后书又变了，不能照它确认；跟主编说，让它按最新内容重提"
		return m, nil
	}
	option := c.Options[choice-1]
	runID := ""
	if run := m.bench.snap.Run; run != nil && activeRun(run.State) {
		runID = run.ID
	}
	if option.Strategy == domainmodel.ResolutionRewriteAffected && runID == "" {
		m.bench.notice = "现在没有进行中的创作，重写已写章节要等创作开始后再选"
		return m, nil
	}
	api, ctx, user, gen := m.api, m.ctx, m.deps.UserID, m.bench.gen
	id, continueAfter := c.Proposal.ID, m.waitingRun()
	echo := "你选了：" + optionLabel(option)
	return m, func() tea.Msg {
		_, err := api.Decisions.ResolveProposal(ctx, decision.ResolveProposalCommand{
			ProposalID: id, UserID: user, Strategy: string(option.Strategy), RunID: runID, CreatedAt: time.Now().UTC(),
		})
		return decisionDoneMsg{gen: gen, continueAfter: continueAfter, err: err, echo: echo}
	}
}

// changeChoice 是输入框里的处置序号；不是在选处置时为 0。
func (m model) changeChoice(text string) int {
	c := m.bench.snap.Change
	if c == nil {
		return 0
	}
	choice, err := strconv.Atoi(text)
	if err != nil || choice < 1 || choice > len(c.Options) {
		return 0
	}
	return choice
}

// waitingRun 报告创作停着等你：确认之后接着写。用户暂停的、已完本的不动。
func (m model) waitingRun() bool {
	return m.bench.hasRun() && m.bench.run().State == domainmodel.RunWaitingUser
}

func activeRun(state domainmodel.CreationRunState) bool {
	switch state {
	case domainmodel.RunRunning, domainmodel.RunWaitingUser, domainmodel.RunPaused:
		return true
	}
	return false
}
