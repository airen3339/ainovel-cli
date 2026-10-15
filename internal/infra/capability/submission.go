package capability

import (
	"context"

	"github.com/voocel/ainovel-cli/internal/domain/model"
)

// chapterDrafts 取写章任务的本章工作稿：正文只经工作稿提交，一次任务只有一份（D86）。章节
// 是否是任务目标、字数与事实变化等契约由 changes.Validate 统一执行（D64）。
func (r *Runtime) chapterDrafts(ctx context.Context, operation model.Operation) ([]model.ManuscriptChapter, error) {
	if operation.Kind != model.OperationWriteChapter && operation.Kind != model.OperationRewriteChapter {
		return nil, nil
	}
	chapter, err := r.workspace.Chapter(ctx, operation.ID)
	if err != nil {
		return nil, err
	}
	return []model.ManuscriptChapter{chapter}, nil
}
