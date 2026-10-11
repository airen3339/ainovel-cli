package store

import (
	"context"
	"fmt"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain/model"
)

// AttemptUsage 是一本书里一次执行尝试的用量与起止（工作台「创作团队」），直接取自尝试摘要。
type AttemptUsage struct {
	OperationID string
	Attempt     int
	State       model.OperationState // 任务此刻的状态
	Live        bool                 // 正在执行：任务此刻的尝试，未收尾且租约未过期（进程中断留下的不算）
	Role        string               // 任务的角色：Worker 的模型角色，同一任务各次尝试相同
	Model       string               // 这次尝试 Agent 实际用的模型；没有 Agent 会话的尝试为空
	Usage       model.Usage
	StartedAt   time.Time
	ActiveAt    time.Time
}

// ProjectAttempts 按开始先后列出一本书里团队成员出过力的全部尝试。角色属于任务：只做收尾
// （如崩溃恢复后的合规核对）的尝试没有 Agent 会话，按同一任务其他尝试的角色计；从没有
// Agent 会话的任务（外部执行器）不是团队成员的工作，不列入。
func (s *Store) ProjectAttempts(ctx context.Context, projectID string, now time.Time) ([]AttemptUsage, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH roles AS (
			SELECT operation_id, MAX(agent_role) AS role FROM operation_attempts
			WHERE agent_role != '' GROUP BY operation_id
		)
		SELECT a.operation_id, a.attempt, o.state,
			a.outcome = '' AND a.attempt = o.attempt AND o.state = ? AND o.lease_until_unix_ms > ?,
			r.role, a.agent_model, a.input_tokens, a.output_tokens, a.cache_read_tokens, a.cost,
			a.started_at_unix_ms, a.active_at_unix_ms
		FROM operation_attempts a
		JOIN operations o ON o.id = a.operation_id
		JOIN roles r ON r.operation_id = a.operation_id
		WHERE o.target_kind = ? AND o.target_id = ?
		ORDER BY a.started_at_unix_ms, a.operation_id, a.attempt`,
		model.OperationRunning, now.UnixMilli(), model.AuthorityProject, projectID)
	if err != nil {
		return nil, fmt.Errorf("list project attempts: %w", err)
	}
	defer rows.Close()
	var attempts []AttemptUsage
	for rows.Next() {
		var a AttemptUsage
		var started, active int64
		if err := rows.Scan(&a.OperationID, &a.Attempt, &a.State, &a.Live, &a.Role, &a.Model,
			&a.Usage.Input, &a.Usage.Output, &a.Usage.CacheRead, &a.Usage.Cost, &started, &active); err != nil {
			return nil, fmt.Errorf("scan project attempt: %w", err)
		}
		a.StartedAt, a.ActiveAt = time.UnixMilli(started).UTC(), time.UnixMilli(active).UTC()
		attempts = append(attempts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate project attempts: %w", err)
	}
	return attempts, nil
}
