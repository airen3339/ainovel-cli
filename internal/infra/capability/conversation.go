package capability

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/agentcore/compact"
	"github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/ainovel-cli/internal/infra/capability/prompt"
	"github.com/voocel/ainovel-cli/internal/infra/llm/models"
	"github.com/voocel/litellm"
)

// conversationCompactAt 是主编会话压缩的阈值（估算 token）：对话长期累积，超过就把早先的
// 部分写成摘要，成本不随聊天时长无限增长。
const conversationCompactAt = 100_000

type conversationStore interface {
	AppendConversation(ctx context.Context, message model.ConversationMessage) (model.ConversationMessage, error)
}

// ConversationConfig 构造主编会话：Prompt 是编译好的稳定前缀，Tools 与 Prompt.Tools 同名
// 对应；History 是这本书已落盘的对话，Observe 接收会话里发生的事（在回合的 goroutine 上）。
type ConversationConfig struct {
	ProjectID string
	Prompt    prompt.ChiefPrompt
	Tools     map[string]func(ctx context.Context, args json.RawMessage) (string, error)
	History   []model.ConversationMessage
	Store     conversationStore
	Observe   func(ConversationEvent)
}

// Conversation 是主编的常驻会话（D74）：持有对话历史，进入历史的每条消息先落盘再生效，
// 压缩写成摘要；回合中途到达的话 Steer 进当前回合。一次只跑一个回合由调用方保证。它不
// 理解工具语义。
type Conversation struct {
	projectID string
	digest    string
	agent     *agentcore.Agent
	store     conversationStore
	observe   func(ConversationEvent)
}

// ConversationEvent 是会话里发生的一件事：Entries 是刚落盘的消息展开的条目；Delta 是正在
// 生成的回复文字增量；Thinking 表示模型在思考。
type ConversationEvent struct {
	Entries  []ConversationEntry
	Delta    string
	Thinking bool
}

// ConversationEntry 是对话里给人看的一条：用户的话、主编的话、一次工具调用或它的结果。
type ConversationEntry struct {
	Sequence int
	Kind     EntryKind
	Text     string          // 用户的话 / 主编的话 / 工具结果
	Tool     string          // 工具调用与结果
	CallID   string          // 调用与结果的配对键
	Args     json.RawMessage // 工具调用参数
	Failed   bool            // 工具结果是错误
	At       time.Time
}

type EntryKind string

const (
	EntryUser   EntryKind = "user"
	EntryChief  EntryKind = "chief"
	EntryCall   EntryKind = "call"
	EntryResult EntryKind = "result"
)

func NewConversation(config ConversationConfig) (*Conversation, error) {
	tools := make([]agentcore.Tool, 0, len(config.Prompt.Tools))
	for _, definition := range config.Prompt.Tools {
		run, ok := config.Tools[definition.Name]
		if !ok {
			return nil, fmt.Errorf("chief tool %q has no handler: %w", definition.Name, model.ErrInvalid)
		}
		var schema map[string]any
		if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
			return nil, fmt.Errorf("decode tool schema %q: %w", definition.Name, err)
		}
		tools = append(tools, agentcore.Tool{
			Name: definition.Name, Description: definition.Description, Schema: schema,
			Run: func(ctx context.Context, args json.RawMessage) (agentcore.Result, error) {
				text, err := run(ctx, args)
				if err != nil {
					return agentcore.Result{}, err
				}
				return agentcore.TextResult(text), nil
			},
		})
	}
	history, err := conversationHistory(config.History)
	if err != nil {
		return nil, err
	}
	c := &Conversation{projectID: config.ProjectID, digest: config.Prompt.Digest, store: config.Store, observe: config.Observe}
	c.agent = agentcore.NewAgent(agentcore.Config{
		System:        []litellm.Block{litellm.TextBlock{Text: config.Prompt.System, Cache: &litellm.CacheControl{}}},
		Tools:         tools,
		Cache:         &litellm.CacheControl{},
		Compactor:     compact.Summarizer{},
		CompactAt:     conversationCompactAt,
		MaxToolErrors: 3,
	}, history)
	c.agent.Subscribe(c.record)
	return c, nil
}

// Message 是会话里的一条消息。
type Message = agentcore.Message

// UserMessage 是用户的一句话：前一块是宿主渲染的书况，后一块是用户的原话。界面与引用
// 校验只认后一块。
func UserMessage(brief, words string) Message {
	return agentcore.User(litellm.Text(brief), litellm.Text(words))
}

// Send 以 binding 跑一次，直到主编停下、ctx 取消或出错。
func (c *Conversation) Send(ctx context.Context, binding models.Binding, messages ...Message) error {
	cacheKey, err := prompt.CacheKey(c.projectID, prompt.ChiefRole, c.digest, binding.Digest)
	if err != nil {
		return err
	}
	config := c.agent.Config()
	config.Model = binding.Routed(cacheKey, "")
	c.agent.SetConfig(config)
	return c.agent.Prompt(ctx, messages...)
}

// Steer 把话送进正在进行的回合，在下一次模型调用前读到。
func (c *Conversation) Steer(message Message) { c.agent.Steer(message) }

// Unanswered 取回送进回合、却在回合结束前没读到的话：调用方接着回答，或在打断时丢弃。
func (c *Conversation) Unanswered() []Message {
	steering, followUp := c.agent.ClearQueues()
	return append(steering, followUp...)
}

// record 先落盘再让事件生效：落盘失败即停止回合，历史与记录不分叉。
func (c *Conversation) record(event agentcore.Event) error {
	out := ConversationEvent{}
	switch e := event.(type) {
	case agentcore.MessageEnd:
		stored, err := c.append(e.Message, 0, e.Message.Usage)
		if err != nil {
			return err
		}
		out.Entries = conversationEntries(stored.Sequence, e.Message)
	case agentcore.CompactionEnd:
		if e.Compaction == nil {
			return nil
		}
		_, err := c.append(e.Compaction.Messages[0], len(e.Compaction.Messages)-1, e.Compaction.Usage)
		return err
	case agentcore.MessageDelta:
		switch d := e.Event.(type) {
		case litellm.TextDelta:
			out.Delta = d.Text
		case litellm.ReasoningDelta:
			out.Thinking = true
		default:
			return nil
		}
	default:
		return nil
	}
	if c.observe != nil {
		c.observe(out)
	}
	return nil
}

func (c *Conversation) append(message agentcore.Message, carried int, usage *agentcore.Usage) (model.ConversationMessage, error) {
	payload, err := json.Marshal(message)
	if err != nil {
		return model.ConversationMessage{}, fmt.Errorf("encode conversation message: %w", err)
	}
	// 落盘不随回合取消：被打断的回合里已经进入历史的消息同样要记下。消息进入历史时由
	// agentcore 计时，压缩摘要生成时计时。
	return c.store.AppendConversation(context.Background(), model.ConversationMessage{
		ProjectID: c.projectID, Payload: payload, Carried: carried, Model: message.Model,
		Usage: messageUsage(usage), CreatedAt: message.Time.UTC(),
	})
}

// conversationHistory 从落盘记录还原模型看到的会话：最近一次压缩的摘要、它带过来的消息，
// 以及其后的全部消息。
func conversationHistory(log []model.ConversationMessage) ([]agentcore.Message, error) {
	messages := make([]agentcore.Message, len(log))
	start := -1
	for i, stored := range log {
		if err := json.Unmarshal(stored.Payload, &messages[i]); err != nil {
			return nil, fmt.Errorf("decode conversation message %d: %w", stored.Sequence, err)
		}
		if messages[i].Kind == agentcore.KindSummary {
			start = i
		}
	}
	if start < 0 {
		return messages, nil
	}
	carried := log[start].Carried
	history := append([]agentcore.Message{messages[start]}, messages[start-carried:start]...)
	return append(history, messages[start+1:]...), nil
}

// ConversationEntries 把落盘的对话展开成给人看的条目；压缩摘要不是谁说的话，不展示。
func ConversationEntries(log []model.ConversationMessage) ([]ConversationEntry, error) {
	var entries []ConversationEntry
	for _, stored := range log {
		var message agentcore.Message
		if err := json.Unmarshal(stored.Payload, &message); err != nil {
			return nil, fmt.Errorf("decode conversation message %d: %w", stored.Sequence, err)
		}
		entries = append(entries, conversationEntries(stored.Sequence, message)...)
	}
	return entries, nil
}

func conversationEntries(sequence int, message agentcore.Message) []ConversationEntry {
	if message.Kind != "" {
		return nil // 摘要与续写提示是会话机制，不是谁说的话
	}
	at := message.Time
	if result, ok := message.ToolResult(); ok {
		return []ConversationEntry{{Sequence: sequence, Kind: EntryResult, CallID: result.ToolUseID,
			Text: agentcore.Message{Blocks: result.Content}.Text(), Failed: result.IsError, At: at}}
	}
	if message.Role == litellm.RoleUser {
		return []ConversationEntry{{Sequence: sequence, Kind: EntryUser, Text: UserWords(message), At: at}}
	}
	var entries []ConversationEntry
	if text := strings.TrimSpace(message.Text()); text != "" {
		entries = append(entries, ConversationEntry{Sequence: sequence, Kind: EntryChief, Text: text, At: at})
	}
	for _, call := range message.ToolCalls() {
		entries = append(entries, ConversationEntry{Sequence: sequence, Kind: EntryCall, Tool: call.Name, CallID: call.ID, Args: json.RawMessage(call.Arguments), At: at})
	}
	return entries
}

// UserWords 是用户消息里用户自己的话：书况之后的最后一个文字块。
func UserWords(message agentcore.Message) string {
	for i := len(message.Blocks) - 1; i >= 0; i-- {
		if text, ok := message.Blocks[i].(litellm.TextBlock); ok {
			return text.Text
		}
	}
	return ""
}
