package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain/model"
)

// AppendConversation 在一本书的主编对话末尾追加一条消息，分配序号。
func (s *Store) AppendConversation(ctx context.Context, message model.ConversationMessage) (model.ConversationMessage, error) {
	if strings.TrimSpace(message.ProjectID) == "" || message.CreatedAt.IsZero() || message.Carried < 0 || !json.Valid(message.Payload) {
		return model.ConversationMessage{}, fmt.Errorf("conversation message fields are invalid: %w", model.ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.ConversationMessage{}, fmt.Errorf("begin conversation message: %w", err)
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM conversation_messages WHERE project_id = ?`,
		message.ProjectID).Scan(&message.Sequence); err != nil {
		return model.ConversationMessage{}, fmt.Errorf("allocate conversation sequence: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO conversation_messages (project_id, sequence, payload, carried, model,
			input_tokens, output_tokens, cache_read_tokens, cost, created_at_unix_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		message.ProjectID, message.Sequence, []byte(message.Payload), message.Carried, message.Model,
		message.Usage.Input, message.Usage.Output, message.Usage.CacheRead, message.Usage.Cost, message.CreatedAt.UnixMilli()); err != nil {
		return model.ConversationMessage{}, fmt.Errorf("insert conversation message: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.ConversationMessage{}, fmt.Errorf("commit conversation message: %w", err)
	}
	return message, nil
}

// ListConversation 按序列出一本书的全部对话消息。
func (s *Store) ListConversation(ctx context.Context, projectID string) ([]model.ConversationMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT sequence, payload, carried, model, input_tokens, output_tokens, cache_read_tokens, cost, created_at_unix_ms
		FROM conversation_messages WHERE project_id = ? ORDER BY sequence`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list conversation: %w", err)
	}
	defer rows.Close()
	var messages []model.ConversationMessage
	for rows.Next() {
		message := model.ConversationMessage{ProjectID: projectID}
		var created int64
		if err := rows.Scan(&message.Sequence, &message.Payload, &message.Carried, &message.Model,
			&message.Usage.Input, &message.Usage.Output, &message.Usage.CacheRead, &message.Usage.Cost, &created); err != nil {
			return nil, fmt.Errorf("scan conversation message: %w", err)
		}
		message.CreatedAt = time.UnixMilli(created).UTC()
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate conversation: %w", err)
	}
	return messages, nil
}

// ConversationUsage 是主编在这本书上的累计用量，以及它最近一次回话用的模型。
func (s *Store) ConversationUsage(ctx context.Context, projectID string) (model.Usage, string, error) {
	var usage model.Usage
	var last string
	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cache_read_tokens), 0), COALESCE(SUM(cost), 0),
			COALESCE((SELECT model FROM conversation_messages WHERE project_id = ? AND model != '' ORDER BY sequence DESC LIMIT 1), '')
		FROM conversation_messages WHERE project_id = ?`, projectID, projectID).
		Scan(&usage.Input, &usage.Output, &usage.CacheRead, &usage.Cost, &last); err != nil {
		return model.Usage{}, "", fmt.Errorf("sum conversation usage: %w", err)
	}
	return usage, last, nil
}
