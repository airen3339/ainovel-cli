package chief

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/voocel/ainovel-cli/internal/infra/capability"
)

// Entry 是对话里给人看的一条：用户的话、主编的话，或主编做的一件事。
type Entry struct {
	Kind EntryKind
	// Text 是话本身；动作是它的说明，有结果后改写的动作换成回执，失败带上原因。
	Text string
	Tool string
	Call string
	// Done 表示动作有了结果；Failed 是结果为失败；Change 表示它改动了作品或创作。
	Done   bool
	Failed bool
	Change bool
	At     time.Time
}

type EntryKind string

const (
	EntryUser   EntryKind = "user"
	EntryChief  EntryKind = "chief"
	EntryAction EntryKind = "action"
)

// actionLabel 是动作还没有结果时的说明，按工具与参数写成故事语言。
func actionLabel(tool string, args json.RawMessage) string {
	var a struct {
		Chapter int    `json:"chapter"`
		Arc     int    `json:"arc"`
		Volume  int    `json:"volume"`
		Entity  string `json:"entity"`
	}
	_ = json.Unmarshal(args, &a) // 只用于说明；参数不合法时由工具本身报错
	switch tool {
	case toolOutline:
		return "看大纲"
	case toolRead:
		switch {
		case a.Chapter > 0:
			return fmt.Sprintf("读第 %d 章", a.Chapter)
		case a.Arc > 0:
			return fmt.Sprintf("看第 %d 个故事弧", a.Arc)
		case a.Volume > 0:
			return fmt.Sprintf("看第 %d 卷", a.Volume)
		case a.Entity != "":
			return "查「" + a.Entity + "」"
		}
		return "查书"
	case toolRequirements:
		return "看要求"
	case toolReviews:
		if a.Chapter > 0 {
			return fmt.Sprintf("看第 %d 章的审阅意见", a.Chapter)
		}
		return "看审阅意见"
	case toolDraft:
		return "看待定稿件"
	case toolNote:
		return "记要求"
	case toolRetire:
		return "退要求"
	case toolReject:
		return "退稿"
	case toolLength:
		return "调整篇幅"
	case toolPause:
		return "暂停创作"
	case toolResume:
		return "继续创作"
	}
	return tool
}

// settle 把结果并回动作：改动类工具的结果就是回执；只读的保持原说明；失败带上原因。
func settle(action Entry, result capability.ConversationEntry) Entry {
	action.Done, action.Failed = true, result.Failed
	switch {
	case result.Failed:
		action.Text += "：" + result.Text
	case changes[action.Tool]:
		action.Text, action.Change = result.Text, true
	}
	return action
}
