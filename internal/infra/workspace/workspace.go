// Package workspace 提供 Operation 工作区的章节级读写。
//
// 工作区是持久化但非权威的草稿区：Worker 在其中反复编辑，只有最终候选才经
// Proposal 进入 Change Engine。它的消费者是 capability（Worker 工具层），
// 因此独立成包——放在 operation 下会让 capability 反向依赖 operation。
package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/ainovel-cli/internal/infra/store"
)

const ChapterMediaType = "application/vnd.ainovel.chapter-draft+json"

// ChapterKey 是章节工作稿的键：一次任务只写一章、只有一份工作稿（D86），键由宿主定。
const ChapterKey = "chapter"

var ErrBlockNotFound = errors.New("workspace block not found")

// Draft 是章节工作稿：正文与写它时照的作品版本（D87）。后继任务接手前任的工作稿时，自己照的
// 版本更新，说明这之间书里改过，写手要照现在的书核对着改。按段落改是在原稿上改，版本不变。
type Draft struct {
	Revision model.Revision          `json:"revision"`
	Chapter  model.ManuscriptChapter `json:"chapter"`
}

type Service struct {
	store *store.Store
}

func New(authorityStore *store.Store) *Service {
	return &Service{store: authorityStore}
}

// PutChapter 整篇覆盖本任务的章节工作稿。
func (s *Service) PutChapter(ctx context.Context, operationID string, draft Draft, attempt int, updatedAt time.Time) (model.WorkspaceArtifact, error) {
	return s.put(ctx, operationID, draft, nil, attempt, updatedAt)
}

// Chapter 取本任务章节工作稿的当前版本：正文只经工作稿提交。
func (s *Service) Chapter(ctx context.Context, operationID string) (Draft, error) {
	_, draft, err := s.draft(ctx, operationID)
	return draft, err
}

// ReplaceChapterBlock 按稳定段落编号改一段。同一轮里并行改几段都基于同一次读取，段落编号不变，
// 各自落在当前版本上；跨执行实例由 attempt 围栏挡住（D42），读改写之间的前提用这里刚读到的版本。
func (s *Service) ReplaceChapterBlock(
	ctx context.Context,
	operationID, blockID, text string,
	attempt int,
	updatedAt time.Time,
) (model.WorkspaceArtifact, error) {
	if strings.TrimSpace(blockID) == "" || strings.TrimSpace(text) == "" {
		return model.WorkspaceArtifact{}, fmt.Errorf("block id and text are required: %w", model.ErrInvalid)
	}
	artifact, draft, err := s.draft(ctx, operationID)
	if err != nil {
		return model.WorkspaceArtifact{}, err
	}
	index := slices.IndexFunc(draft.Chapter.Blocks, func(block model.ManuscriptBlock) bool { return block.ID == blockID })
	if index < 0 {
		return model.WorkspaceArtifact{}, fmt.Errorf("chapter block %q: %w", blockID, ErrBlockNotFound)
	}
	draft.Chapter.Blocks[index].Text = text
	return s.put(ctx, operationID, draft, &artifact.Version, attempt, updatedAt)
}

func (s *Service) put(
	ctx context.Context,
	operationID string,
	draft Draft,
	expectedVersion *int64, // nil = 整篇覆盖；按块编辑才带版本前提
	attempt int,
	updatedAt time.Time,
) (model.WorkspaceArtifact, error) {
	chapter, err := json.Marshal(draft.Chapter)
	if err != nil {
		return model.WorkspaceArtifact{}, fmt.Errorf("encode workspace chapter: %w", err)
	}
	if err := model.ValidateDocumentContent(model.DocumentRef{Kind: model.DocumentManuscript, ID: draft.Chapter.ID}, chapter); err != nil {
		return model.WorkspaceArtifact{}, err
	}
	payload, err := json.Marshal(draft)
	if err != nil {
		return model.WorkspaceArtifact{}, fmt.Errorf("encode workspace draft: %w", err)
	}
	return s.store.PutWorkspaceArtifact(ctx, model.WorkspaceArtifact{
		OperationID: operationID, Key: ChapterKey, MediaType: ChapterMediaType, Content: payload, UpdatedAt: updatedAt,
	}, expectedVersion, attempt)
}

func (s *Service) draft(ctx context.Context, operationID string) (model.WorkspaceArtifact, Draft, error) {
	artifact, err := s.store.GetWorkspaceArtifact(ctx, operationID, ChapterKey)
	if errors.Is(err, model.ErrNotFound) {
		return model.WorkspaceArtifact{}, Draft{}, fmt.Errorf("本章还没有工作稿，先用 workspace_put_chapter 写入: %w", err)
	}
	if err != nil {
		return model.WorkspaceArtifact{}, Draft{}, err
	}
	var draft Draft
	if err := model.DecodeStrict(artifact.Content, &draft); err != nil {
		return model.WorkspaceArtifact{}, Draft{}, fmt.Errorf("decode workspace draft: %w", err)
	}
	return artifact, draft, nil
}
