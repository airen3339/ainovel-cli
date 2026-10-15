package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type OperationKind string

const (
	OperationDevelopPlan    OperationKind = "develop_plan"
	OperationWriteChapter   OperationKind = "write_chapter"
	OperationRevisePlan     OperationKind = "revise_plan"
	OperationReviseCanon    OperationKind = "revise_canon"
	OperationRewriteChapter OperationKind = "rewrite_chapter"
	OperationReviewRange    OperationKind = "review_range"
	OperationGenerateAsset  OperationKind = "generate_asset"
	OperationInspectAsset   OperationKind = "inspect_asset"
)

type OperationState string

const (
	OperationQueued           OperationState = "queued"
	OperationRunning          OperationState = "running"
	OperationPaused           OperationState = "paused"
	OperationAwaitingApproval OperationState = "awaiting_approval"
	OperationSucceeded        OperationState = "succeeded"
	OperationFailed           OperationState = "failed"
	OperationCancelled        OperationState = "cancelled"
	OperationStale            OperationState = "stale"
)

// FailureCode 给失败一个机器可判读的原因（D46）。result_unknown 表示外部结果不可知：
// 任务已提交但拿不到结论，不自动重试；用户对账（Resume）或重提（Restart）都是显式决定。
// submission_blocked 表示提交自纠已受阻，保留草稿，等待显式续跑。model_* 是模型服务一侧
// 的失败（D76）：连不上、繁忙在重试用尽后才落到这里；拒绝（密钥、额度、模型名）不自动重开。
type FailureCode string

const (
	FailureResultUnknown     FailureCode = "result_unknown"
	FailureSubmissionBlocked FailureCode = "submission_blocked"
	FailureModelUnreachable  FailureCode = "model_unreachable"
	FailureModelBusy         FailureCode = "model_busy"
	FailureModelRefused      FailureCode = "model_refused"
)

var ErrResultUnknown = errors.New("external result is unknown")
var ErrSubmissionBlocked = errors.New("提交受阻")

// Failure 给外部错误标上失败类别而不改它的文字：原始错误照原样进诊断，用户看到的原因
// 按类别说（D76）。
type Failure struct {
	Code FailureCode
	Err  error
}

func (f *Failure) Error() string { return f.Err.Error() }
func (f *Failure) Unwrap() error { return f.Err }

// ModelService 报告失败出在模型服务一侧：与写作内容无关，模型无从修正。
func (c FailureCode) ModelService() bool {
	return c == FailureModelUnreachable || c == FailureModelBusy || c == FailureModelRefused
}

// Explain 是失败给用户看的原因：模型服务的失败按类别说清楚，其余就是原始错误（多是本
// 程序自己写的说明）。
func (c FailureCode) Explain(raw string) string {
	switch c {
	case FailureModelUnreachable:
		return "连不上模型服务（网络中断或连接超时）"
	case FailureModelBusy:
		return "模型服务繁忙或临时故障"
	case FailureModelRefused:
		return "模型服务拒绝了请求（密钥、额度或模型名有问题）"
	}
	return raw
}

// FailureCodeFor 保留跨执行层传递的失败类别，供协调器决定是否自动续跑。
func FailureCodeFor(cause error) FailureCode {
	if failure, ok := errors.AsType[*Failure](cause); ok {
		return failure.Code
	}
	if errors.Is(cause, ErrResultUnknown) {
		return FailureResultUnknown
	}
	if errors.Is(cause, ErrSubmissionBlocked) {
		return FailureSubmissionBlocked
	}
	return ""
}

// 外部请求记录是工作区工件（D46）：提交前先落 RequestID，恢复按 ID 查询而不是重提；
// 重启后继承前任记录，OperationID 不等于自身即视为用户已决定重提。
const (
	ExternalRequestKey       = "external.request"
	ExternalRequestMediaType = "application/vnd.ainovel.external-request+json"
)

type ExternalRequestRecord struct {
	OperationID string                  `json:"operation_id"`
	Attempt     int                     `json:"attempt"`
	RequestID   string                  `json:"request_id"`
	SubmittedAt time.Time               `json:"submitted_at"`
	Identity    ExternalRequestIdentity `json:"identity"`
}

// ExternalRequestIdentity binds a remote submission to the frozen inputs that produced it.
// InputDigest includes the typed input's evidence basis; BaseRevision also protects inputs
// read directly from the source snapshot. An inherited result may only be reused on equality.
type ExternalRequestIdentity struct {
	Target       AuthorityTarget `json:"target"`
	Executor     string          `json:"executor"`
	ConfigDigest string          `json:"config_digest"`
	InputDigest  string          `json:"input_digest"`
	BaseRevision Revision        `json:"base_revision"`
}

func ExternalIdentityFor(operation Operation) ExternalRequestIdentity {
	return ExternalRequestIdentity{
		Target: operation.Target, Executor: operation.Snapshot.Executor,
		ConfigDigest: operation.Snapshot.ConfigDigest, InputDigest: operation.Snapshot.InputDigest,
		BaseRevision: operation.Snapshot.BaseRevision,
	}
}

func (r ExternalRequestRecord) Validate() error {
	if strings.TrimSpace(r.OperationID) == "" || r.Attempt <= 0 || strings.TrimSpace(r.RequestID) == "" || r.SubmittedAt.IsZero() {
		return fmt.Errorf("external request record requires operation, attempt, request id and time: %w", ErrInvalid)
	}
	if err := r.Identity.Target.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(r.Identity.Executor) == "" || strings.TrimSpace(r.Identity.ConfigDigest) == "" || strings.TrimSpace(r.Identity.InputDigest) == "" || r.Identity.BaseRevision < InitialRevision {
		return fmt.Errorf("external request requires its frozen execution identity: %w", ErrInvalid)
	}
	return nil
}

type ApprovalPolicy string

const (
	ApprovalAuto      ApprovalPolicy = "auto"
	ApprovalMilestone ApprovalPolicy = "milestone"
	ApprovalManual    ApprovalPolicy = "manual"
)

// StricterApproval 取两份策略中更严格的一方（D23）：`auto < milestone < manual`。
func StricterApproval(left, right ApprovalPolicy) ApprovalPolicy {
	rank := func(policy ApprovalPolicy) int {
		switch policy {
		case ApprovalMilestone:
			return 1
		case ApprovalManual:
			return 2
		default:
			return 0
		}
	}
	if rank(right) > rank(left) {
		return right
	}
	return left
}

// ExecutionSnapshot 是任务创建时冻结的执行边界（D45）：谁执行、基于哪个
// Revision、什么输入、什么配置、什么审批策略。字段对执行族中立：LLM 的
// ConfigDigest 是 Execution Profile 摘要，外部执行器是自身配置摘要。
type ExecutionSnapshot struct {
	// Executor 是执行器身份 `族@版本[/路由]`：创建冻结、领取按相等过滤、执行前核对。
	Executor       string         `json:"executor"`
	BaseRevision   Revision       `json:"base_revision"`
	InputDigest    string         `json:"input_digest"`
	ConfigDigest   string         `json:"config_digest"`
	ApprovalPolicy ApprovalPolicy `json:"approval_policy"`
}

func (s ExecutionSnapshot) Validate() error {
	if strings.TrimSpace(s.Executor) == "" || strings.TrimSpace(s.InputDigest) == "" || strings.TrimSpace(s.ConfigDigest) == "" {
		return fmt.Errorf("snapshot executor, input digest and config digest are required: %w", ErrInvalid)
	}
	if s.BaseRevision < InitialRevision {
		return fmt.Errorf("snapshot base revision cannot be negative: %w", ErrInvalid)
	}
	switch s.ApprovalPolicy {
	case ApprovalAuto, ApprovalMilestone, ApprovalManual:
	default:
		return fmt.Errorf("unknown approval policy %q: %w", s.ApprovalPolicy, ErrInvalid)
	}
	return nil
}

// ExecutionProfileRecord 是 LLM 执行族的不可变配置：Digest 是记录自身的内容
// 摘要（Identity），Operation 以 Snapshot.ConfigDigest 引用它。
type ExecutionProfileRecord struct {
	Digest              string          `json:"digest"`
	ProjectID           string          `json:"project_id"`
	WorkerProfile       string          `json:"worker_profile"`
	CoreProtocolVersion string          `json:"core_protocol_version"`
	PromptDigest        string          `json:"prompt_digest"`
	ToolSchemaDigest    string          `json:"tool_schema_digest"`
	StablePrefix        string          `json:"stable_prefix"`
	DynamicTail         string          `json:"dynamic_tail"`
	Tools               json.RawMessage `json:"tools"`
	Sources             json.RawMessage `json:"sources"`
	CreatedAt           time.Time       `json:"created_at"`
}

// Identity 是记录除 Digest 与 CreatedAt 外全部字段的内容摘要。
func (r ExecutionProfileRecord) Identity() (string, error) {
	r.Digest, r.CreatedAt = "", time.Time{}
	return DigestJSON(r)
}

func (r ExecutionProfileRecord) Validate() error {
	for name, value := range map[string]string{
		"project": r.ProjectID, "worker profile": r.WorkerProfile, "core protocol version": r.CoreProtocolVersion,
		"prompt digest": r.PromptDigest, "tool schema digest": r.ToolSchemaDigest,
		"stable prefix": r.StablePrefix, "dynamic tail": r.DynamicTail,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("execution profile %s is required: %w", name, ErrInvalid)
		}
	}
	if len(r.Tools) == 0 || !json.Valid(r.Tools) || len(r.Sources) == 0 || !json.Valid(r.Sources) {
		return fmt.Errorf("execution profile tools and sources must be valid JSON: %w", ErrInvalid)
	}
	if r.CreatedAt.IsZero() {
		return fmt.Errorf("execution profile creation time is required: %w", ErrInvalid)
	}
	identity, err := r.Identity()
	if err != nil {
		return err
	}
	if r.Digest != identity {
		return fmt.Errorf("execution profile digest is not its content identity: %w", ErrInvalid)
	}
	return nil
}

type Operation struct {
	ID        string          `json:"id"`
	Kind      OperationKind   `json:"kind"`
	Target    AuthorityTarget `json:"target"`
	DependsOn []string        `json:"depends_on,omitempty"`
	Priority  int             `json:"priority"`
	State     OperationState  `json:"state"`
	Attempt   int             `json:"attempt"`
	// RunID 是创作血统归属（§6.3）：每个 Operation 都影响故事内容，必须归属唯一
	// CreationRun（D27）。RunPolicyVersion 由存储层在创建
	// 事务内解析为当时最近一次策略事件的序号，调用方不填。
	RunID            string            `json:"run_id,omitempty"`
	RunPolicyVersion int64             `json:"run_policy_version,omitempty"`
	Snapshot         ExecutionSnapshot `json:"execution_snapshot"`
	Input            json.RawMessage   `json:"input"`
	Error            string            `json:"error,omitempty"`
	FailureCode      FailureCode       `json:"failure_code,omitempty"`
	LeaseOwner       string            `json:"lease_owner,omitempty"`
	LeaseUntil       *time.Time        `json:"lease_until,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

func (o Operation) Validate() error {
	if strings.TrimSpace(o.ID) == "" {
		return fmt.Errorf("operation id is required: %w", ErrInvalid)
	}
	if _, err := DecodeTaskInput(o.Kind, o.Input); err != nil {
		return err
	}
	if err := o.Target.Validate(); err != nil {
		return err
	}
	seenDependencies := make(map[string]struct{}, len(o.DependsOn))
	for i, dependency := range o.DependsOn {
		if strings.TrimSpace(dependency) == "" || dependency == o.ID {
			return fmt.Errorf("operation dependency %d is invalid: %w", i, ErrInvalid)
		}
		if _, ok := seenDependencies[dependency]; ok {
			return fmt.Errorf("duplicate operation dependency %q: %w", dependency, ErrInvalid)
		}
		seenDependencies[dependency] = struct{}{}
	}
	switch o.State {
	case OperationQueued, OperationRunning, OperationPaused, OperationAwaitingApproval,
		OperationSucceeded, OperationFailed, OperationCancelled, OperationStale:
	default:
		return fmt.Errorf("unknown operation state %q: %w", o.State, ErrInvalid)
	}
	if o.Attempt < 0 {
		return fmt.Errorf("operation attempt cannot be negative: %w", ErrInvalid)
	}
	switch o.FailureCode {
	case "":
	case FailureResultUnknown, FailureSubmissionBlocked, FailureModelUnreachable, FailureModelBusy, FailureModelRefused:
		if o.State != OperationFailed {
			return fmt.Errorf("failure code %q requires the failed state: %w", o.FailureCode, ErrInvalid)
		}
	default:
		return fmt.Errorf("unknown failure code %q: %w", o.FailureCode, ErrInvalid)
	}
	if err := o.Snapshot.Validate(); err != nil {
		return err
	}
	if o.Snapshot.InputDigest != Digest(o.Input) {
		return fmt.Errorf("snapshot input digest does not match operation input: %w", ErrInvalid)
	}
	if o.CreatedAt.IsZero() || o.UpdatedAt.IsZero() || o.UpdatedAt.Before(o.CreatedAt) {
		return fmt.Errorf("operation timestamps are invalid: %w", ErrInvalid)
	}
	if o.State == OperationRunning {
		if strings.TrimSpace(o.LeaseOwner) == "" || o.LeaseUntil == nil || o.LeaseUntil.IsZero() {
			return fmt.Errorf("running operation requires lease: %w", ErrInvalid)
		}
	} else if o.LeaseOwner != "" || o.LeaseUntil != nil {
		return fmt.Errorf("non-running operation cannot hold lease: %w", ErrInvalid)
	}
	return nil
}

func CanTransitionOperation(from, to OperationState) bool {
	switch from {
	case OperationQueued:
		return to == OperationRunning || to == OperationPaused || to == OperationCancelled
	case OperationRunning:
		return to == OperationPaused || to == OperationAwaitingApproval || to == OperationSucceeded ||
			to == OperationFailed || to == OperationCancelled || to == OperationStale || to == OperationQueued
	case OperationPaused:
		return to == OperationQueued || to == OperationCancelled
	case OperationAwaitingApproval:
		// 批准、否决、取消，或被新的推导取代（D64）。
		return to == OperationSucceeded || to == OperationFailed || to == OperationCancelled || to == OperationStale
	case OperationFailed:
		return to == OperationQueued || to == OperationCancelled
	default:
		return false
	}
}

type WorkspaceArtifact struct {
	OperationID string    `json:"operation_id"`
	Key         string    `json:"key"`
	Version     int64     `json:"version"`
	MediaType   string    `json:"media_type"`
	Content     []byte    `json:"content"`
	Digest      string    `json:"digest"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// OperationAttempt 是一次执行尝试的摘要（D72）：领取时建立，任务离开 running 时在同一
// 事务里收尾。失败预算、用量、实际模型与工具报错都以它为准，与任务同寿；对话记录可以
// 回收，摘要不受影响。
type OperationAttempt struct {
	OperationID string `json:"operation_id"`
	Attempt     int    `json:"attempt"`
	Worker      string `json:"worker"`
	// Agent 是这次尝试里 Agent 会话实际用的模型（D57）；没有会话的尝试为零值。
	Agent      AgentBinding `json:"agent,omitzero"`
	Usage      Usage        `json:"usage,omitzero"`
	ToolErrors int          `json:"tool_errors,omitempty"`
	// Outcome 是尝试结束时任务进入的状态，进行中为空。LeaseExpired 表示由租约回收收尾：
	// 执行者已失联，Error 是回收的说明。
	Outcome      OperationState `json:"outcome,omitempty"`
	Error        string         `json:"error,omitempty"`
	FailureCode  FailureCode    `json:"failure_code,omitempty"`
	LeaseExpired bool           `json:"lease_expired,omitempty"`
	StartedAt    time.Time      `json:"started_at"`
	// ActiveAt 是最后一次有进展的时刻：领取、对话记录、收尾。租约回收不算进展。
	ActiveAt time.Time `json:"active_at"`
}

// AgentBinding 是 Agent 会话实际用的角色与模型绑定（D57）：Thinking 记生效值，Digest 是
// 模型配置摘要。
type AgentBinding struct {
	Role     string `json:"role"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Thinking string `json:"thinking,omitempty"`
	Digest   string `json:"digest"`
}

// TranscriptEntry 是一次尝试里与模型的一次往来（D72）：Agent 会话的一条消息，或会话之外
// 的一次单独判断。它只服务续跑恢复与诊断，任务不会再执行后可以回收；用量与工具报错在
// 写入时并入尝试摘要，不从这里读。
type TranscriptEntry struct {
	OperationID string          `json:"operation_id"`
	Sequence    int64           `json:"sequence"`
	Attempt     int             `json:"attempt"`
	Kind        TranscriptKind  `json:"kind"`
	Payload     json.RawMessage `json:"payload"`
	CreatedAt   time.Time       `json:"created_at"`
}

type TranscriptKind string

const (
	TranscriptMessage  TranscriptKind = "message"
	TranscriptJudgment TranscriptKind = "judgment"
)

// Usage 是模型调用的 token 与花费；Cost 只计有定价的部分。
type Usage struct {
	Input     int     `json:"input"`
	Output    int     `json:"output"`
	CacheRead int     `json:"cache_read"`
	Cost      float64 `json:"cost,omitempty"`
}

func (u *Usage) Add(other Usage) {
	u.Input += other.Input
	u.Output += other.Output
	u.CacheRead += other.CacheRead
	u.Cost += other.Cost
}
