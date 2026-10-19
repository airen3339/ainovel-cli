package tui

import "strings"

// 首页与它的表单页（更多设定、导入作品）共用工作台的外框：顶栏两行（品牌与右侧信息、分隔线），
// 底栏两行（嵌着反馈的分隔线、按键），进出工作台时外框不跳。
const (
	pageHeader = 2
	pageFooter = 2
)

// pageTop 是顶栏：品牌（可带页面名）靠左，right 靠右。
func pageTop(title, right string, width int) []string {
	left := " " + benchTheme.Accent.Bold(true).Render("AINOVEL")
	if title != "" {
		left += benchTheme.Muted.Render("  /  " + title)
	}
	return []string{alignRight(left, right+" ", width), benchRule(width)}
}

// pageBottom 是底栏：反馈嵌在分隔线里，下一行是按键与靠右的 aside。
func (m model) pageBottom(feedback, hint, aside string, width int) []string {
	return []string{feedbackRule(feedback, width), " " + alignRight(m.keyHint(benchTheme.Muted.Render(hint)), aside, width-2)}
}

func problemFeedback(problem string) string {
	if problem == "" {
		return ""
	}
	return benchTheme.Error.Render("! " + oneLine(problem))
}

func (m model) entryWidth() int { return min(76, max(1, m.width-4)) }

// entryPage 是表单页：内容在居中的窄栏里自上而下排，字段变化时不重新居中。
func (m model) entryPage(title string, content []string, problem, hint string) string {
	w, h := max(1, m.width), max(1, m.height)
	inner := m.entryWidth()
	pad := strings.Repeat(" ", (w-inner)/2)
	lines := pageTop(title, "", w)
	for _, line := range fitBlock(strings.Join(append([]string{""}, content...), "\n"), inner, h-pageHeader-pageFooter) {
		lines = append(lines, pad+line)
	}
	lines = append(lines, m.pageBottom(problemFeedback(problem), hint, "", w)...)
	return strings.Join(fitBlock(strings.Join(lines, "\n"), w, h), "\n")
}
