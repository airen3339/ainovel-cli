package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain/model"
)

// 对话按书只追加、序号连续；用量按书汇总，模型取最近一条回话的。
func TestConversationAppendsPerBookAndSumsUsage(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "ainovel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	for _, message := range []model.ConversationMessage{
		{ProjectID: "a", Payload: json.RawMessage(`{"role":"user"}`), CreatedAt: now},
		{ProjectID: "a", Payload: json.RawMessage(`{"role":"assistant"}`), Model: "m1", Usage: model.Usage{Input: 10, Output: 2}, CreatedAt: now},
		{ProjectID: "b", Payload: json.RawMessage(`{"role":"user"}`), CreatedAt: now},
		{ProjectID: "a", Payload: json.RawMessage(`{"role":"user","kind":"summary"}`), Carried: 1, Usage: model.Usage{Input: 5}, CreatedAt: now},
	} {
		if _, err := s.AppendConversation(ctx, message); err != nil {
			t.Fatal(err)
		}
	}
	messages, err := s.ListConversation(ctx, "a")
	if err != nil || len(messages) != 3 || messages[2].Sequence != 3 || messages[2].Carried != 1 || messages[1].Model != "m1" {
		t.Fatalf("messages = %+v, %v", messages, err)
	}
	usage, last, err := s.ConversationUsage(ctx, "a")
	if err != nil || usage != (model.Usage{Input: 15, Output: 2}) || last != "m1" {
		t.Fatalf("usage = %+v %q %v", usage, last, err)
	}
	if _, err := s.AppendConversation(ctx, model.ConversationMessage{ProjectID: "a", Payload: json.RawMessage(`{`), CreatedAt: now}); err == nil {
		t.Fatal("invalid payload must be rejected")
	}
}
