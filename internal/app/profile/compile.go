package profile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	projectdoc "github.com/voocel/ainovel-cli/internal/app/project"
	"github.com/voocel/ainovel-cli/internal/app/resource"
	"github.com/voocel/ainovel-cli/internal/domain/derive"
	"github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/ainovel-cli/internal/infra/capability/prompt"
)

type CompileCommand struct {
	ProjectID       string
	Revision        model.Revision
	Kind            model.OperationKind
	Input           json.RawMessage
	Packs           []resource.PackRef
	CreatorProfiles []resource.CreatorProfileRef
	CreatedAt       time.Time
}

func (s *Compiler) Compile(ctx context.Context, command CompileCommand) (prompt.Compiled, error) {
	target := model.AuthorityTarget{Kind: model.AuthorityProject, ID: command.ProjectID}
	revision := command.Revision
	viewKey, err := derive.ViewKey(command.Kind, command.Input)
	if err != nil {
		return prompt.Compiled{}, err
	}
	var intent model.Intent
	var view derive.ModelView
	cached, err := s.store.GetDerivedDocument(ctx, command.ProjectID, revision, derive.ViewKind, viewKey)
	switch {
	case err == nil:
		intentDocument, err := s.store.GetDocument(ctx, target, model.DocumentRef{Kind: model.DocumentIntent, ID: model.SingletonDocumentID}, revision)
		if err != nil {
			return prompt.Compiled{}, err
		}
		if err := json.Unmarshal(intentDocument.Content, &intent); err != nil {
			return prompt.Compiled{}, fmt.Errorf("decode project intent: %w", err)
		}
		if err := json.Unmarshal(cached.Content, &view); err != nil {
			return prompt.Compiled{}, fmt.Errorf("decode cached model view: %w", err)
		}
	case errors.Is(err, model.ErrNotFound):
		project, err := s.projects.Project(ctx, command.ProjectID, revision)
		if err != nil {
			return prompt.Compiled{}, err
		}
		intent = project.Intent
		view, err = derive.BuildModelView(derive.ProjectContent{
			ID: project.ID, Revision: project.Revision, Compass: project.Compass, Plan: project.Plan, Entities: project.Entities, Canon: project.Canon,
			Manuscript: project.Manuscript, Ownership: project.Ownership,
		}, command.Kind, command.Input)
		if err != nil {
			return prompt.Compiled{}, err
		}
		content, err := json.Marshal(view)
		if err != nil {
			return prompt.Compiled{}, fmt.Errorf("encode model view: %w", err)
		}
		// 以落库的那份为准：并发编译同一任务时，所有人拿到同一份视图。
		stored, err := s.store.SaveDerivedDocument(ctx, model.DerivedDocument{
			ProjectID: command.ProjectID, Revision: revision, Kind: derive.ViewKind,
			Key: viewKey, Content: content, CreatedAt: command.CreatedAt,
		})
		if err != nil {
			return prompt.Compiled{}, err
		}
		if err := json.Unmarshal(stored.Content, &view); err != nil {
			return prompt.Compiled{}, fmt.Errorf("decode model view: %w", err)
		}
	default:
		return prompt.Compiled{}, err
	}
	capability, err := prompt.BuiltinCapability(command.Kind)
	if err != nil {
		return prompt.Compiled{}, err
	}
	// 资产自动装配（D31）：命令未显式指定时，按 Project 的固定引用加载已启用的
	// Pack 与 Creator Profile——给这本书固定一套风格与方法，此后自动沿用。
	var overlayRules []string
	if revision > model.InitialRevision {
		packs, profiles, err := s.pinnedAssets(ctx, target, revision)
		if err != nil {
			return prompt.Compiled{}, err
		}
		if len(command.Packs) == 0 {
			command.Packs = packs
		}
		if len(command.CreatorProfiles) == 0 {
			command.CreatorProfiles = profiles
		}
		overlays, err := projectdoc.LoadDocuments[model.ProjectOverlay](ctx, s.store, target, model.DocumentOverlay, revision)
		if err != nil {
			return prompt.Compiled{}, err
		}
		if len(overlays) > 0 {
			overlayRules = overlays[0].Rules
		}
	}
	packs, creatorProfiles, err := s.loadAssets(ctx, command.Packs, command.CreatorProfiles)
	if err != nil {
		return prompt.Compiled{}, err
	}
	ownership, err := json.Marshal(view.Ownership)
	if err != nil {
		return prompt.Compiled{}, fmt.Errorf("encode ownership view: %w", err)
	}
	storyContext, err := json.Marshal(view.Context)
	if err != nil {
		return prompt.Compiled{}, fmt.Errorf("encode story context: %w", err)
	}
	return s.prompts.Reload(ctx, prompt.CompileRequest{
		ProjectID: command.ProjectID,
		Worker:    capability.Worker, Packs: packs, CreatorProfiles: creatorProfiles,
		Intent: intent, Ownership: ownership, OverlayRules: overlayRules,
		StoryContext: storyContext, Task: view.Task,
		BaseRevision: revision, ProjectOverlayRevision: revision,
	}, command.CreatedAt)
}

// pinnedAssets 是作品在 revision 上固定的 Pack 与 Creator Profile 引用（D31）。
func (s *Compiler) pinnedAssets(ctx context.Context, target model.AuthorityTarget, revision model.Revision) ([]resource.PackRef, []resource.CreatorProfileRef, error) {
	assets, err := projectdoc.LoadDocuments[model.ProjectAssetRefs](ctx, s.store, target, model.DocumentAssets, revision)
	if err != nil || len(assets) == 0 {
		return nil, nil, err
	}
	var packs []resource.PackRef
	for _, ref := range assets[0].Packs {
		packs = append(packs, resource.PackRef{ID: ref.ID, Revision: ref.Revision})
	}
	var profiles []resource.CreatorProfileRef
	for _, ref := range assets[0].CreatorProfiles {
		profiles = append(profiles, resource.CreatorProfileRef{ID: ref.ID, Scope: ref.Scope, Revision: ref.Revision})
	}
	return packs, profiles, nil
}

// loadAssets 按引用读出 Pack 与 Creator Profile 的内容。
func (s *Compiler) loadAssets(ctx context.Context, packRefs []resource.PackRef, profileRefs []resource.CreatorProfileRef) ([]prompt.VersionedPack, []prompt.VersionedCreatorProfile, error) {
	packs := make([]prompt.VersionedPack, len(packRefs))
	for i, ref := range packRefs {
		pack, err := resource.LoadPack(ctx, s.store, ref)
		if err != nil {
			return nil, nil, err
		}
		packs[i] = pack
	}
	profiles := make([]prompt.VersionedCreatorProfile, len(profileRefs))
	for i, ref := range profileRefs {
		profile, err := resource.LoadCreatorProfile(ctx, s.store, ref)
		if err != nil {
			return nil, nil, err
		}
		profiles[i] = profile
	}
	return packs, profiles, nil
}
