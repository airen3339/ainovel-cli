package profile

import (
	"context"

	"github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/ainovel-cli/internal/infra/capability/prompt"
)

// CompileChief 按这本书此刻固定的 Pack 与 Creator Profile 编译主编的稳定前缀（D74）。
// 前缀在会话内冻结：之后换包或改偏好，下次打开这本书时才生效（§7.4）。书级创作规则
// （Project Overlay）是写作方法，主编不写正文，不叠加。
func (s *Compiler) CompileChief(ctx context.Context, projectID string, tools []prompt.ToolSchema) (prompt.ChiefPrompt, error) {
	target := model.AuthorityTarget{Kind: model.AuthorityProject, ID: projectID}
	revision, err := s.store.CurrentRevision(ctx, target)
	if err != nil {
		return prompt.ChiefPrompt{}, err
	}
	packRefs, profileRefs, err := s.pinnedAssets(ctx, target, revision)
	if err != nil {
		return prompt.ChiefPrompt{}, err
	}
	packs, profiles, err := s.loadAssets(ctx, packRefs, profileRefs)
	if err != nil {
		return prompt.ChiefPrompt{}, err
	}
	return prompt.CompileChief(prompt.ChiefRequest{ProjectID: projectID, Tools: tools, Packs: packs, CreatorProfiles: profiles})
}
