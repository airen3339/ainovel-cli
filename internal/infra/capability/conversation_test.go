package capability

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/voocel/agentcore"
	domainmodel "github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/litellm"
)

// 对话载入：最新摘要在前，随后是它之前带着的消息与之后的全部；摘要不是谁说的话，界面不展示。
func TestConversationHistoryResumesFromTheLatestSummary(t *testing.T) {
	var log []domainmodel.ConversationMessage
	add := func(message agentcore.Message, carried int) {
		payload, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		log = append(log, domainmodel.ConversationMessage{Sequence: len(log) + 1, Payload: payload, Carried: carried})
	}
	reply := func(text string) agentcore.Message {
		return agentcore.Message{Role: litellm.RoleAssistant, Blocks: []litellm.Block{litellm.TextBlock{Text: text}}}
	}
	add(agentcore.UserText("一"), 0)
	add(reply("答一"), 0)
	add(agentcore.UserText("二"), 0)
	add(agentcore.SummaryMessage("旧摘要"), 1)
	add(reply("答二"), 0)
	add(agentcore.UserText("三"), 0)
	add(agentcore.SummaryMessage("新摘要"), 1)
	add(reply("答三"), 0)

	history, err := conversationHistory(log)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, message := range history {
		got = append(got, message.Text())
	}
	got[0] = agentcore.SummaryText(history[0])
	want := []string{"新摘要", "三", "答三"}
	if history[0].Kind != agentcore.KindSummary || !slices.Equal(got, want) {
		t.Fatalf("history = %q, want %q", got, want)
	}

	entries, err := ConversationEntries(log)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 6 || entries[0].Text != "一" || entries[5].Text != "答三" {
		t.Fatalf("entries = %+v", entries)
	}
}

// 有的服务把多个文字块首尾相接成一段：书况与用户的话之间的分界写在文字里，接起来也分得清；
// 界面与引用校验仍只拿到用户的原话。
func TestUserWordsStayApartWhenBlocksAreJoined(t *testing.T) {
	message := UserMessage("【书况】宿主此刻的记录\n生效的要求：0 条", "每章两千字左右")
	joined := ""
	for _, block := range message.Blocks {
		joined += block.(litellm.TextBlock).Text
	}
	if joined != "【书况】宿主此刻的记录\n生效的要求：0 条\n\n【用户的话】\n每章两千字左右" || UserWords(message) != "每章两千字左右" {
		t.Fatalf("joined = %q, words = %q", joined, UserWords(message))
	}
}
