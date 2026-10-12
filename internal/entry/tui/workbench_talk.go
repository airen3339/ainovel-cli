package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/voocel/ainovel-cli/internal/app/chief"
)

// sessionMsg 是会话唤醒回执：主编的对话或创作推进有了变化，醒来后整读快照。
type sessionMsg struct{ gen int }

// chiefSentMsg 是一句话交给主编的回执；err 非空时话没送出去，输入框保留原文。
type chiefSentMsg struct {
	gen  int
	text string
	err  error
}

// watchSessionCmd 等会话的下一次变化（合并信号）。
func (m model) watchSessionCmd() tea.Cmd {
	session, gen, ctx := m.bench.session, m.bench.gen, m.ctx
	if session == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case <-session.Changes():
		case <-ctx.Done():
		}
		return sessionMsg{gen: gen}
	}
}

// loadConversationCmd 载入这本书已有的对话。
func (m model) loadConversationCmd() tea.Cmd {
	session, ctx, gen := m.bench.session, m.ctx, m.bench.gen
	return func() tea.Msg {
		if err := session.Load(ctx); err != nil {
			return chiefSentMsg{gen: gen, err: err}
		}
		return nil
	}
}

// sendCmd 把输入框里的话交给主编，切到对话视图看它回话。
func (m model) sendCmd(text string) (tea.Model, tea.Cmd) {
	m.switchView(viewTalk)
	m.bench.talkHeld, m.bench.talkOffset = false, 0
	session, ctx, gen := m.bench.session, m.ctx, m.bench.gen
	view := chief.View{Chapter: m.selectedChapterNumber()}
	return m, func() tea.Msg {
		return chiefSentMsg{gen: gen, text: text, err: session.Send(ctx, text, view)}
	}
}

// applySession 读会话快照：对话照抄；推进开始了就跟上直播，推进结束了就处理落点。
func (m model) applySession() (tea.Model, tea.Cmd) {
	bench := &m.bench
	bench.thread = bench.session.Thread()
	production := bench.session.Production()
	cmds := []tea.Cmd{m.watchSessionCmd()}
	if bench.thread.Busy || production.Writing {
		cmds = append(cmds, bench.pulse.start(bench.gen))
	}
	if production.Ended > bench.driveSeen {
		bench.driveSeen = production.Ended
		next, cmd := m.handleDriveEnd(production.Last)
		m = next.(model)
		cmds = append(cmds, cmd)
	}
	if production.Writing && !m.bench.writing {
		// 主编让创作动了起来（继续、改篇幅、退稿后重写）：与按键开始的一样跟上直播。
		m.startedWriting()
		cmds = append(cmds, pollTick(m.bench.gen))
	}
	return m, tea.Batch(cmds...)
}

// talkLines 对话视图的全部行：用户的话、主编的话、主编做的事，正在生成的回复在最后。
// 定下的条目沿用排好的行；进行中的动作带转圈，每帧重排。
func (m model) talkLines(width int) []string {
	thread := m.bench.thread
	cached := m.bench.talkCache.lines(thread.Entries, width, func(entry chief.Entry) []string { return m.entryLines(entry, width) })
	var lines []string
	for i, entry := range thread.Entries {
		if entry.Kind == chief.EntryUser && i > 0 {
			lines = append(lines, "")
		}
		if entry.Kind == chief.EntryAction && !entry.Done {
			lines = append(lines, m.entryLines(entry, width)...)
			continue
		}
		lines = append(lines, cached[i]...)
	}
	for _, text := range thread.Pending {
		lines = append(lines, "")
		lines = append(lines, styledLines("› "+text, width, benchTheme.Muted)...)
	}
	switch {
	case thread.Reply != "":
		lines = append(lines, styledLines(thread.Reply, width, benchTheme.Text)...)
	case thread.Busy && thread.Thinking:
		lines = append(lines, styleFocus.Render(m.bench.pulse.spinner()+" 主编在想"))
	case thread.Busy:
		lines = append(lines, styleFocus.Render(m.bench.pulse.spinner()+" 主编在看"))
	}
	if thread.Err != "" {
		lines = append(lines, styledLines("! "+oneLine(thread.Err), width, benchTheme.Error)...)
	}
	return lines
}

func (m model) entryLines(entry chief.Entry, width int) []string {
	switch entry.Kind {
	case chief.EntryUser:
		return styledLines("› "+entry.Text, width, benchTheme.Accent)
	case chief.EntryChief:
		return styledLines(entry.Text, width, benchTheme.Text)
	default:
		return []string{fitLine("  "+m.actionLine(entry), width)}
	}
}

func styledLines(text string, width int, style lipgloss.Style) []string {
	lines := readingLines(text, width)
	for i, line := range lines {
		lines[i] = style.Render(line)
	}
	return lines
}

// talkCache 是对话各条排好的行：条目只追加或在原位定下结果，与上次相同、宽度不变就沿用。
type talkCache struct {
	width   int
	entries []chief.Entry
	rows    [][]string
}

func (c *talkCache) lines(entries []chief.Entry, width int, render func(chief.Entry) []string) [][]string {
	if c.width != width || len(c.entries) > len(entries) {
		c.width, c.entries, c.rows = width, nil, nil
	}
	for i, entry := range entries {
		switch {
		case i == len(c.entries):
			c.entries, c.rows = append(c.entries, entry), append(c.rows, render(entry))
		case c.entries[i] != entry:
			c.entries[i], c.rows[i] = entry, render(entry)
		}
	}
	return c.rows
}

// actionLine 主编做的一件事：改动作品的是回执，只读的是一笔带过，进行中的转着。
func (m model) actionLine(entry chief.Entry) string {
	switch {
	case entry.Failed:
		return benchTheme.Error.Render("✗ " + entry.Text)
	case entry.Change:
		return styleNotice.Render("✓ ") + benchTheme.Text.Render(entry.Text)
	case entry.Done:
		return benchTheme.Muted.Render("· " + entry.Text)
	case m.bench.thread.Busy:
		return styleFocus.Render(m.bench.pulse.spinner() + " " + entry.Text)
	default:
		return benchTheme.Muted.Render("◦ " + entry.Text + " · 未完成")
	}
}

// talkView 对话视图：跟随最新；上滚后停在原处，↓ 到底或 Esc 恢复。
func (m model) talkView(width, height int) []string {
	lines := []string{viewHead("对话", "主编", "", width)}
	body := m.talkLines(width)
	if len(body) == 0 {
		return append(lines,
			benchTheme.Muted.Render("在下面的输入框里跟主编说话：提要求、问进度、讨论情节、退回稿件、暂停或继续都行。"),
			benchTheme.Muted.Render("主编不写正文，它把你的意思落到书里，由创作团队去写。"),
		)
	}
	capacity := max(1, height-viewHeadRows)
	last := max(0, len(body)-capacity)
	offset := last
	if m.bench.talkHeld {
		offset = min(m.bench.talkOffset, last)
	}
	return append(lines, body[offset:min(len(body), offset+capacity)]...)
}

func (m model) scrollTalk(delta int) model {
	l := m.benchLayout()
	total := len(m.talkLines(l.inner))
	last := max(0, total-max(1, l.contentRows-viewHeadRows))
	offset := last
	if m.bench.talkHeld {
		offset = m.bench.talkOffset
	}
	offset = min(max(0, offset+delta), last)
	m.bench.talkHeld, m.bench.talkOffset = offset < last, offset
	return m
}
