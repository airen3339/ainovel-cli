// Package chief 是主编（D74）：用户在一本书里唯一的对话对象，也是这本书此刻在不在写的
// 唯一持有者。主编把用户的话落成作品里的一等对象（要求、退稿、运行控制），生产线按作品
// 对账推进；它不写正文、不派任务、不批准稿件。
package chief

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/voocel/ainovel-cli/internal/app/decision"
	"github.com/voocel/ainovel-cli/internal/app/novel"
	"github.com/voocel/ainovel-cli/internal/app/profile"
	projectdoc "github.com/voocel/ainovel-cli/internal/app/project"
	"github.com/voocel/ainovel-cli/internal/app/workbench"
	"github.com/voocel/ainovel-cli/internal/domain/creation"
	"github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/ainovel-cli/internal/infra/capability"
	"github.com/voocel/ainovel-cli/internal/infra/capability/prompt"
	"github.com/voocel/ainovel-cli/internal/infra/llm/models"
	"github.com/voocel/ainovel-cli/internal/infra/store"
)

// Models 给出角色此刻生效的模型；还没有可用模型时 ok=false。
type Models interface {
	Binding(role string) (models.Binding, bool)
}

type Service struct {
	store     *store.Store
	projects  *projectdoc.Repository
	decisions *decision.Review
	novels    *novel.Application
	runs      *creation.Coordinator
	bench     *workbench.Query
	prompts   *profile.Compiler
	models    Models
	// work 是全部会话的后台工作（回合与推进）：退出时等它们把任务放回队列。
	work sync.WaitGroup
}

func New(
	s *store.Store, projects *projectdoc.Repository, decisions *decision.Review, novels *novel.Application,
	runs *creation.Coordinator, bench *workbench.Query, prompts *profile.Compiler, models Models,
) *Service {
	return &Service{store: s, projects: projects, decisions: decisions, novels: novels, runs: runs, bench: bench, prompts: prompts, models: models}
}

// Wait 等全部会话的后台工作收尾，最多 grace；超时返回 false，剩下的交给租约到期回收。
func (s *Service) Wait(grace time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.work.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(grace):
		return false
	}
}

// Open 打开一本书的会话；ctx 结束即打断它的回合与推进。不读库，对话由 Load 载入。
func (s *Service) Open(ctx context.Context, projectID, userID string) *Session {
	ctx, cancel := context.WithCancel(ctx)
	return &Session{service: s, ctx: ctx, close: cancel, projectID: projectID, userID: userID, changes: make(chan struct{}, 1)}
}

// Session 是打开着的一本书：主编的对话与创作推进。界面只读它的快照、调它的方法。
type Session struct {
	service   *Service
	ctx       context.Context
	close     context.CancelFunc
	projectID string
	userID    string
	changes   chan struct{}

	// sending 让送出的话按先后进入会话，也守着对话的载入与会话的建立：书况渲染要读库，
	// 不能在 mu 里做。
	sending sync.Mutex
	loaded  bool
	log     []model.ConversationMessage // 载入的对话，交给第一次建立的会话
	conv    *capability.Conversation

	mu      sync.Mutex
	entries []Entry
	pending []string // 已送出、还没进入会话的话
	said    []said   // 用户说过的话，引用校验的依据
	reply   strings.Builder
	thought bool
	turn    context.CancelFunc
	turnErr string
	// paused 表示用户按键暂停了创作、还没说话：说完一句话的回合结束后接着写。主编应用户
	// 要求暂停时清掉它，之后只有回车或主编继续才写。
	paused bool

	drive   context.CancelFunc
	driven  chan struct{}
	writing bool
	ended   int
	last    DriveEnd
}

type said struct {
	sequence int
	words    string
}

// View 是用户此刻在看什么：书况据此告诉主编“这章”指哪一章。
type View struct {
	Chapter int // 正在看的章；0 表示全书
}

// Changes 在会话有新内容时醒来（合并信号），醒来后整读快照。
func (s *Session) Changes() <-chan struct{} { return s.changes }

func (s *Session) notify() {
	select {
	case s.changes <- struct{}{}:
	default:
	}
}

// Close 打断回合与推进，任务由引擎放回队列；最后唤醒一次，等着的人不再空等。
func (s *Session) Close() {
	s.close()
	s.notify()
}

// Load 载入这本书已有的对话；只载一次，之后的消息随回合进来。
func (s *Session) Load(ctx context.Context) error {
	s.sending.Lock()
	defer s.sending.Unlock()
	return s.load(ctx)
}

// load 读落盘的对话：条目给界面，用户的话给引用校验，记录留给会话接上。调用方持有
// sending，所以载入不会与回合交错。
func (s *Session) load(ctx context.Context) error {
	if s.loaded {
		return nil
	}
	log, err := s.service.store.ListConversation(ctx, s.projectID)
	if err != nil {
		return err
	}
	entries, err := capability.ConversationEntries(log)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.apply(entries)
	s.mu.Unlock()
	s.log, s.loaded = log, true
	s.notify()
	return nil
}

// Thread 是对话此刻的样子：落盘的条目、送出未达的话、正在生成的回复。
type Thread struct {
	Entries  []Entry
	Pending  []string
	Reply    string
	Thinking bool
	Busy     bool
	Err      string
}

func (s *Session) Thread() Thread {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Thread{
		Entries: append([]Entry(nil), s.entries...), Pending: append([]string(nil), s.pending...),
		Reply: s.reply.String(), Thinking: s.thought, Busy: s.turn != nil, Err: s.turnErr,
	}
}

// Busy 报告主编是否正在回话。
func (s *Session) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turn != nil
}

// Send 把用户的一句话交给主编：正在回话时送进当前回合，否则开始新回合。回合在后台进行，
// 进展经 Changes 通知。
func (s *Session) Send(ctx context.Context, words string, view View) error {
	words = strings.TrimSpace(words)
	if words == "" {
		return fmt.Errorf("message is empty: %w", model.ErrInvalid)
	}
	binding, ok := s.service.models.Binding(prompt.ChiefRole)
	if !ok {
		return errors.New("还没有可用的模型：先用 /model 配置")
	}
	s.sending.Lock()
	defer s.sending.Unlock()
	conv, err := s.conversation(ctx)
	if err != nil {
		return err
	}
	brief, err := s.brief(ctx, view)
	if err != nil {
		return err
	}
	message := capability.UserMessage(brief, words)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, words)
	s.turnErr = ""
	if s.turn != nil {
		conv.Steer(message)
		s.notify()
		return nil
	}
	turn, cancel := context.WithCancel(s.ctx)
	s.turn = cancel
	s.service.work.Add(1)
	go s.run(turn, cancel, conv, binding, []capability.Message{message})
	s.notify()
	return nil
}

// Interrupt 打断正在进行的回合，报告是否有回合可打断。
func (s *Session) Interrupt() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn == nil {
		return false
	}
	s.turn()
	return true
}

// run 跑一个回合。回合收尾与 Send 的送话都在 mu 里，所以送出的话一定有人答：收尾时取回
// 没读到的话接着答；打断只停下主编正在说的，没读到的话开新回合答。按键暂停后说完话，先
// 接着写再收回合。
func (s *Session) run(ctx context.Context, cancel context.CancelFunc, conv *capability.Conversation, binding models.Binding, messages []capability.Message) {
	defer s.service.work.Done()
	defer func() { cancel() }()
	for {
		err := conv.Send(ctx, binding, messages...)
		interrupted := ctx.Err() != nil
		s.mu.Lock()
		resume := err == nil && !interrupted && s.paused
		s.mu.Unlock()
		if resume {
			s.Write(WriteParams{Chapters: novel.KeepChapters})
		}
		s.mu.Lock()
		messages = conv.Unanswered()
		if (err == nil || interrupted) && len(messages) > 0 {
			if interrupted {
				cancel()
				ctx, cancel = context.WithCancel(s.ctx)
				s.turn = cancel
				s.reply.Reset()
				s.thought = false
			}
			s.mu.Unlock()
			continue
		}
		s.turn, s.pending = nil, nil
		s.reply.Reset()
		s.thought = false
		if err != nil && !interrupted {
			s.turnErr = err.Error()
		}
		s.mu.Unlock()
		s.notify()
		return
	}
}

// conversation 第一次用到时编译主编并接上已有对话；之后整个会话沿用（§7.4 稳定前缀）。
// 调用方持有 sending。
func (s *Session) conversation(ctx context.Context) (*capability.Conversation, error) {
	if s.conv != nil {
		return s.conv, nil
	}
	if err := s.load(ctx); err != nil {
		return nil, err
	}
	tools := s.tools()
	schemas := make([]prompt.ToolSchema, 0, len(tools))
	handlers := make(map[string]func(context.Context, json.RawMessage) (string, error), len(tools))
	for _, tool := range tools {
		schemas = append(schemas, tool.schema)
		handlers[tool.schema.Name] = tool.run
	}
	compiled, err := s.service.prompts.CompileChief(ctx, s.projectID, schemas)
	if projectdoc.IsNotFound(err) {
		return nil, errors.New("这本书还在建立，稍等一下再说")
	}
	if err != nil {
		return nil, err
	}
	conv, err := capability.NewConversation(capability.ConversationConfig{
		ProjectID: s.projectID, Prompt: compiled, Tools: handlers, History: s.log,
		Store: s.service.store, Observe: s.observe,
	})
	if err != nil {
		return nil, err
	}
	s.conv, s.log = conv, nil
	return conv, nil
}

// observe 在回合的 goroutine 上接收会话里发生的事。
func (s *Session) observe(event capability.ConversationEvent) {
	s.mu.Lock()
	switch {
	case len(event.Entries) > 0:
		s.reply.Reset()
		s.thought = false
		s.apply(event.Entries)
	case event.Delta != "":
		s.reply.WriteString(event.Delta)
		s.thought = false
	case event.Thinking:
		s.thought = true
	}
	s.mu.Unlock()
	s.notify()
}

// apply 把落盘的条目并进对话：用户的话记进引用依据并消掉对应的待达，工具结果并回它的调用。
func (s *Session) apply(entries []capability.ConversationEntry) {
	for _, entry := range entries {
		switch entry.Kind {
		case capability.EntryUser:
			s.said = append(s.said, said{sequence: entry.Sequence, words: entry.Text})
			if len(s.pending) > 0 && s.pending[0] == entry.Text {
				s.pending = s.pending[1:]
			}
			s.entries = append(s.entries, Entry{Kind: EntryUser, Text: entry.Text, At: entry.At})
		case capability.EntryChief:
			s.entries = append(s.entries, Entry{Kind: EntryChief, Text: entry.Text, At: entry.At})
		case capability.EntryCall:
			s.entries = append(s.entries, Entry{Kind: EntryAction, Tool: entry.Tool, Call: entry.CallID, Text: actionLabel(entry.Tool, entry.Args), At: entry.At})
		case capability.EntryResult:
			for i := len(s.entries) - 1; i >= 0; i-- {
				if s.entries[i].Kind == EntryAction && s.entries[i].Call == entry.CallID {
					s.entries[i] = settle(s.entries[i], entry)
					break
				}
			}
		}
	}
}
