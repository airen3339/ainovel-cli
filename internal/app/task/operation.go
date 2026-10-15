package task

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/voocel/ainovel-cli/internal/app/profile"
	"github.com/voocel/ainovel-cli/internal/app/project"
	"github.com/voocel/ainovel-cli/internal/app/resource"
	"github.com/voocel/ainovel-cli/internal/domain/model"
	operationengine "github.com/voocel/ainovel-cli/internal/domain/operation"
	"github.com/voocel/ainovel-cli/internal/infra/capability/prompt"
)

type StartOperationCommand struct {
	OperationID string
	ProjectID   string
	Kind        model.OperationKind
	Priority    int
	DependsOn   []string
	Input       json.RawMessage
	// LLM 执行族的编译输入：资产引用（Worker 由任务种类决定）；模型是运行时绑定，不进快照（D57）。
	Packs           []resource.PackRef
	CreatorProfiles []resource.CreatorProfileRef
	// ConfigDigest 是外部执行族的自身配置摘要，冻结进快照（D45）。
	ConfigDigest   string
	ApprovalPolicy model.ApprovalPolicy
	// RunID 非空时 Operation 归属该 CreationRun（§6.3）：血统事件与策略版本
	// 由存储层在创建事务内一并落盘。
	RunID     string
	CreatedAt time.Time
}

type RestartOperationCommand struct {
	FromOperationID string
	ConfigDigest    string
	OperationID     string
	Packs           []resource.PackRef
	CreatorProfiles []resource.CreatorProfileRef
	ApprovalPolicy  model.ApprovalPolicy
	RunID           string
	// Input 非空时替代前任输入：后继按当前快照重算的任务输入（含新命中的要求）。
	Input     json.RawMessage
	CreatedAt time.Time
}

// StartOperation 冻结执行快照并入队（D45）：按种类的执行族取身份与配置——
// LLM 族编译 Execution Profile，外部族透传自身配置摘要并绑定已装配的执行器。
func (s *Manager) StartOperation(ctx context.Context, command StartOperationCommand) (model.Operation, error) {
	operation, err := s.prepareOperation(ctx, command)
	if err != nil {
		return model.Operation{}, err
	}
	return s.store.CreateOperation(ctx, operation)
}

func (s *Manager) prepareOperation(ctx context.Context, command StartOperationCommand) (model.Operation, error) {
	spec, err := model.KindSpec(command.Kind)
	if err != nil {
		return model.Operation{}, err
	}
	target := model.AuthorityTarget{Kind: model.AuthorityProject, ID: command.ProjectID}
	revision, err := s.store.CurrentRevision(ctx, target)
	if err != nil {
		return model.Operation{}, err
	}
	policy, err := s.resolveApprovalPolicy(ctx, target, revision, command.ApprovalPolicy)
	if err != nil {
		return model.Operation{}, err
	}
	snapshot := model.ExecutionSnapshot{
		BaseRevision: revision, InputDigest: model.Digest(command.Input), ApprovalPolicy: policy,
	}
	switch spec.Executor {
	case model.ExecutorLLM:
		compiled, err := s.profiles.Compile(ctx, profile.CompileCommand{
			ProjectID: command.ProjectID, Revision: revision, Kind: command.Kind,
			Input: command.Input, Packs: command.Packs, CreatorProfiles: command.CreatorProfiles, CreatedAt: command.CreatedAt,
		})
		if err != nil {
			return model.Operation{}, err
		}
		snapshot.Executor, snapshot.ConfigDigest = prompt.ExecutorIdentity, compiled.ProfileDigest
	case model.ExecutorExternal:
		if s.executors.External == nil || strings.TrimSpace(command.ConfigDigest) == "" {
			return model.Operation{}, fmt.Errorf("%s requires a configured executor and its config digest: %w", command.Kind, model.ErrInvalid)
		}
		snapshot.Executor, snapshot.ConfigDigest = s.executors.External.Identity(), command.ConfigDigest
	}
	return model.Operation{
		ID: command.OperationID, Kind: command.Kind, Target: target,
		DependsOn: command.DependsOn, Priority: command.Priority, State: model.OperationQueued,
		RunID:    command.RunID,
		Snapshot: snapshot, Input: append(json.RawMessage(nil), command.Input...),
		CreatedAt: command.CreatedAt, UpdatedAt: command.CreatedAt,
	}, nil
}

// resolveApprovalPolicy 解析启动时刻的审批策略：未显式指定时取当前 Revision 的
// 权威设置（§6.3），快照记录它作为历史最低约束；custom 需要显式契约。
func (s *Manager) resolveApprovalPolicy(
	ctx context.Context,
	target model.AuthorityTarget,
	revision model.Revision,
	requested model.ApprovalPolicy,
) (model.ApprovalPolicy, error) {
	policy := requested
	if policy == "" {
		policy = model.ApprovalAuto
		if revision > model.InitialRevision {
			settings, err := project.LoadDocuments[model.ApprovalSetting](ctx, s.store, target, model.DocumentApproval, revision)
			if err != nil {
				return "", err
			}
			if len(settings) > 0 {
				policy = settings[0].Policy
			}
		}
	}
	return policy, nil
}

func (s *Manager) RestartOperation(ctx context.Context, command RestartOperationCommand) (model.Operation, error) {
	if strings.TrimSpace(command.FromOperationID) == "" || strings.TrimSpace(command.OperationID) == "" ||
		command.FromOperationID == command.OperationID || command.CreatedAt.IsZero() {
		return model.Operation{}, fmt.Errorf("restart requires distinct source, target and time: %w", model.ErrInvalid)
	}
	previous, err := s.store.GetOperation(ctx, command.FromOperationID)
	if err != nil {
		return model.Operation{}, err
	}
	switch previous.State {
	case model.OperationFailed, model.OperationCancelled, model.OperationStale:
	default:
		return model.Operation{}, fmt.Errorf(
			"operation %q must be failed, cancelled or stale before restart, got %s: %w",
			previous.ID, previous.State, model.ErrStateConflict,
		)
	}
	runID := command.RunID
	if runID == "" {
		runID = previous.RunID
	}
	if runID != previous.RunID {
		return model.Operation{}, fmt.Errorf(
			"restart cannot move operation %q to another creation run: %w",
			previous.ID, model.ErrInvalid,
		)
	}
	approval := command.ApprovalPolicy
	if approval == "" {
		approval = previous.Snapshot.ApprovalPolicy
	}
	successor := StartOperationCommand{
		OperationID: command.OperationID, ProjectID: previous.Target.ID, Kind: previous.Kind,
		Priority: previous.Priority, DependsOn: previous.DependsOn,
		Input: append(json.RawMessage(nil), previous.Input...), ApprovalPolicy: approval,
		RunID: runID, CreatedAt: command.CreatedAt,
	}
	if len(command.Input) != 0 {
		successor.Input = append(json.RawMessage(nil), command.Input...)
	}
	spec, err := model.KindSpec(previous.Kind)
	if err != nil {
		return model.Operation{}, err
	}
	switch spec.Executor {
	case model.ExecutorLLM:
		// 沿用前任冻结的 Execution Profile 作为默认编译输入；资产引用按记录的
		// 来源恢复，显式传入的才替换。
		compiled, err := s.profiles.Load(ctx, previous.Snapshot.ConfigDigest)
		if err != nil {
			return model.Operation{}, err
		}
		successor.Packs, err = packRefsFromSources(compiled.Sources)
		if err != nil {
			return model.Operation{}, err
		}
		successor.CreatorProfiles, err = creatorProfileRefsFromSources(compiled.Sources)
		if err != nil {
			return model.Operation{}, err
		}
		if command.Packs != nil {
			successor.Packs = command.Packs
		}
		if command.CreatorProfiles != nil {
			successor.CreatorProfiles = command.CreatorProfiles
		}
	case model.ExecutorExternal:
		successor.ConfigDigest = previous.Snapshot.ConfigDigest
		if command.ConfigDigest != "" {
			successor.ConfigDigest = command.ConfigDigest
		}
	}
	if existing, err := s.store.GetOperation(ctx, command.OperationID); err == nil {
		if err := s.validateRestartTarget(ctx, existing, previous, successor); err != nil {
			return model.Operation{}, err
		}
		if err := s.store.CopyWorkspaceArtifacts(ctx, previous.ID, existing.ID, command.CreatedAt); err != nil {
			return model.Operation{}, err
		}
		return existing, nil
	} else if !errors.Is(err, model.ErrNotFound) {
		return model.Operation{}, err
	}
	restarted, err := s.prepareOperation(ctx, successor)
	if err != nil {
		return model.Operation{}, err
	}
	return s.store.CreateSuccessorOperation(ctx, previous.ID, restarted)
}

// validateRestartTarget 校验同 ID 的既有 Operation 就是本次重启请求（幂等）。
func (s *Manager) validateRestartTarget(
	ctx context.Context,
	existing, previous model.Operation,
	successor StartOperationCommand,
) error {
	if existing.State != model.OperationQueued || existing.Target != previous.Target || existing.Kind != previous.Kind ||
		existing.Priority != previous.Priority || !bytes.Equal(existing.Input, successor.Input) ||
		existing.Snapshot.ApprovalPolicy != successor.ApprovalPolicy ||
		existing.RunID != successor.RunID || !slices.Equal(existing.DependsOn, previous.DependsOn) {
		return fmt.Errorf("operation %q is not the requested restart target: %w", existing.ID, model.ErrIdempotencyConflict)
	}
	if successor.ConfigDigest != "" {
		if s.executors.External == nil || existing.Snapshot.Executor != s.executors.External.Identity() || existing.Snapshot.ConfigDigest != successor.ConfigDigest {
			return fmt.Errorf("operation %q is not the requested restart target: %w", existing.ID, model.ErrIdempotencyConflict)
		}
		return nil
	}
	// Recompile against the already frozen source revision, so an idempotent
	// retry checks all execution inputs without adopting newer story content.
	compiled, err := s.profiles.Compile(ctx, profile.CompileCommand{
		ProjectID: existing.Target.ID, Revision: existing.Snapshot.BaseRevision,
		Kind: successor.Kind, Input: successor.Input, Packs: successor.Packs, CreatorProfiles: successor.CreatorProfiles,
		CreatedAt: existing.CreatedAt,
	})
	if err != nil {
		return err
	}
	if compiled.ProfileDigest != existing.Snapshot.ConfigDigest || existing.Snapshot.Executor != prompt.ExecutorIdentity {
		return fmt.Errorf("operation %q uses different execution settings: %w", existing.ID, model.ErrIdempotencyConflict)
	}
	return nil
}

func packRefsFromSources(sources []prompt.Source) ([]resource.PackRef, error) {
	refs := make([]resource.PackRef, 0)
	for _, source := range sources {
		if source.Layer != "pack_defaults" {
			continue
		}
		separator := strings.LastIndex(source.ID, "@")
		if separator <= 0 || source.Revision <= model.InitialRevision {
			return nil, fmt.Errorf("pack source %q is invalid: %w", source.ID, model.ErrInvalid)
		}
		refs = append(refs, resource.PackRef{ID: source.ID[:separator], Revision: source.Revision})
	}
	return refs, nil
}

func creatorProfileRefsFromSources(sources []prompt.Source) ([]resource.CreatorProfileRef, error) {
	refs := make([]resource.CreatorProfileRef, 0)
	for _, source := range sources {
		if source.Layer != "creator_profile" {
			continue
		}
		separator := strings.LastIndex(source.ID, "/")
		if separator <= 0 || separator == len(source.ID)-1 || source.Revision <= model.InitialRevision {
			return nil, fmt.Errorf("creator profile source %q is invalid: %w", source.ID, model.ErrInvalid)
		}
		refs = append(refs, resource.CreatorProfileRef{
			ID: source.ID[:separator], Scope: source.ID[separator+1:], Revision: source.Revision,
		})
	}
	return refs, nil
}

func (s *Manager) RunNextOperation(
	ctx context.Context,
	workerID string,
	leaseDuration time.Duration,
	now time.Time,
) (operationengine.RunResult, error) {
	return s.operations.RunNext(ctx, s.executors.all(), workerID, leaseDuration, now)
}

func (s *Manager) RunOperation(
	ctx context.Context,
	operationID, workerID string,
	leaseDuration time.Duration,
	now time.Time,
) (operationengine.RunResult, error) {
	return s.operations.Run(ctx, s.executors.all(), operationID, workerID, leaseDuration, now)
}

func (s *Manager) PauseOperation(ctx context.Context, id string, at time.Time) (model.Operation, error) {
	operation, err := s.store.GetOperation(ctx, id)
	if err != nil {
		return model.Operation{}, err
	}
	if operation.State == model.OperationPaused {
		return operation, nil
	}
	return s.store.TransitionOperation(ctx, id, operation.State, model.OperationPaused, "paused by user", at)
}

func (s *Manager) ResumeOperation(ctx context.Context, id string, at time.Time) (model.Operation, error) {
	operation, err := s.store.GetOperation(ctx, id)
	if err != nil {
		return model.Operation{}, err
	}
	if operation.State == model.OperationQueued {
		return operation, nil
	}
	if operation.State != model.OperationPaused && operation.State != model.OperationFailed {
		return model.Operation{}, fmt.Errorf("operation %q cannot resume from %s: %w", id, operation.State, model.ErrStateConflict)
	}
	if operation.State == model.OperationFailed {
		proposal, err := s.store.GetProposalByOperation(ctx, operation.ID)
		if err == nil && proposal.ApprovalState == model.ApprovalRejected {
			return model.Operation{}, fmt.Errorf("operation %q has a rejected proposal; create an explicit restart instead: %w", id, model.ErrStateConflict)
		}
		if err != nil && !errors.Is(err, model.ErrNotFound) {
			return model.Operation{}, err
		}
	}
	return s.store.TransitionOperation(ctx, id, operation.State, model.OperationQueued, "resumed by user", at)
}

func (s *Manager) CancelOperation(ctx context.Context, id string, at time.Time) (model.Operation, error) {
	operation, err := s.store.GetOperation(ctx, id)
	if err != nil {
		return model.Operation{}, err
	}
	if operation.State == model.OperationCancelled {
		return operation, nil
	}
	return s.store.TransitionOperation(ctx, id, operation.State, model.OperationCancelled, "cancelled by user; workspace retained", at)
}

func (s *Manager) ReprioritizeOperation(ctx context.Context, id string, priority int, at time.Time) (model.Operation, error) {
	return s.store.SetOperationPriority(ctx, id, priority, at)
}

func (s *Manager) RecoverOperations(ctx context.Context, at time.Time) ([]string, error) {
	return s.store.RecoverExpiredOperations(ctx, at)
}

func (s *Manager) Operation(ctx context.Context, id string) (model.Operation, error) {
	return s.store.GetOperation(ctx, id)
}

func (s *Manager) OperationAttempts(ctx context.Context, id string) ([]model.OperationAttempt, error) {
	return s.store.ListOperationAttempts(ctx, id)
}

func (s *Manager) OperationTranscript(ctx context.Context, id string) ([]model.TranscriptEntry, error) {
	return s.store.ListTranscript(ctx, id)
}

// PruneTranscripts 回收不会再执行的任务的对话记录（D72），返回回收条数。
func (s *Manager) PruneTranscripts(ctx context.Context) (int, error) {
	return s.store.PruneTranscripts(ctx)
}
