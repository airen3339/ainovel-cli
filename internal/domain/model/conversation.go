package model

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// ConversationMessage 是用户与主编对话里的一条消息（D74）：只追加的交互记录，不是权威，
// 不进 Revision、导出投影与任何任务上下文。Payload 是 Agent 消息原文；Carried 只在压缩
// 摘要上非零，是摘要之前仍要随它带进会话的消息条数。
type ConversationMessage struct {
	ProjectID string          `json:"project_id"`
	Sequence  int             `json:"sequence"`
	Payload   json.RawMessage `json:"payload"`
	Carried   int             `json:"carried,omitempty"`
	Model     string          `json:"model,omitempty"`
	Usage     Usage           `json:"usage"`
	CreatedAt time.Time       `json:"created_at"`
}

// ChiefRole 是主编的模型角色：绑定模型与团队用量都按它归属。
const ChiefRole = "chief"

// ChiefAuthor 是主编提出的修改的作者（D77）：AI 作者，按审批策略放行或等用户确认。
var ChiefAuthor = Author{Kind: AuthorAI, ID: ChiefRole}

// conversationSource 是对话消息引用的前缀：要求的 Source 指回用户说这句话的那条消息。
const conversationSource = "conversation:"

// ConversationSource 构造指向对话第 sequence 条消息的来源引用。
func ConversationSource(sequence int) string {
	return conversationSource + strconv.Itoa(sequence)
}

// ParseConversationSource 解析来源引用，不是对话消息引用时 ok=false。
func ParseConversationSource(source string) (int, bool) {
	value, found := strings.CutPrefix(source, conversationSource)
	if !found {
		return 0, false
	}
	sequence, err := strconv.Atoi(value)
	return sequence, err == nil && sequence > 0
}
