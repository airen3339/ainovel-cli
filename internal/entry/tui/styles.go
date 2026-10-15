package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
)

// spinnerFrames 是进行中步骤与顶栏的转圈，随创作动画的节拍换帧（pulse.go）。
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// newInput 统一文本输入样式：所有页面的输入框长一个样。
func newInput(placeholder string) textinput.Model {
	input := textinput.New()
	input.Placeholder = placeholder
	input.Prompt = "❯ "
	input.PromptStyle = benchTheme.Focus
	input.PlaceholderStyle = benchTheme.Muted
	return input
}

// progressBar 用 ▰▱ 渲染章节进度，写完部分着品牌色。
func progressBar(done, total, width int) string {
	if total <= 0 {
		total = 1
	}
	if done > total {
		done = total
	}
	width = max(0, width)
	done = max(0, done)
	filled := done * width / total
	bar := benchTheme.Focus.Render(strings.Repeat("▰", filled)) +
		benchTheme.Muted.Render(strings.Repeat("▱", width-filled))
	return bar
}
