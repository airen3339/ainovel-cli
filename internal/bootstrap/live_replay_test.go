package bootstrap_test

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/ainovel-cli/internal/domain/narrative"
)

// TestLiveChangeReplay 把主编的一次修改原样重放给语义影响分析，看结论稳不稳（D87）：从验收
// 库里取作品在修改基线上的样子，导入成一本新书，再把当时 propose_change 的参数逐次提交。
// 库会被写入，用副本：
//
//	AINOVEL_LIVE=<库的副本> AINOVEL_REPLAY_PROJECT=<作品> AINOVEL_REPLAY_REVISION=<修改的基线> \
//	AINOVEL_REPLAY_EDIT=<propose_change 参数文件> AINOVEL_REPLAY_TIMES=5 \
//	go test ./internal/bootstrap -run TestLiveChangeReplay -v
func TestLiveChangeReplay(t *testing.T) {
	source, file := os.Getenv("AINOVEL_REPLAY_PROJECT"), os.Getenv("AINOVEL_REPLAY_EDIT")
	if source == "" || file == "" {
		t.Skip("AINOVEL_REPLAY_PROJECT、AINOVEL_REPLAY_EDIT 未设：不重放")
	}
	revision, err := strconv.Atoi(os.Getenv("AINOVEL_REPLAY_REVISION"))
	if err != nil {
		t.Fatalf("AINOVEL_REPLAY_REVISION: %v", err)
	}
	times, err := strconv.Atoi(os.Getenv("AINOVEL_REPLAY_TIMES"))
	if err != nil {
		t.Fatalf("AINOVEL_REPLAY_TIMES: %v", err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var args struct {
		Reason string `json:"reason"`
		narrative.Edit
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatal(err)
	}
	u := openLive(t)
	now := time.Now().UTC()
	projection, err := u.api.Projects.ExportProject(u.ctx, source, model.Revision(revision))
	if err != nil {
		t.Fatal(err)
	}
	replay := "replay-" + now.Format("150405")
	projection.ProjectID, projection.BaseRevision = replay, 0
	imported, err := u.api.Projects.ImportProject(u.ctx, replay+":import", liveUserID, "重放的起点", projection, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.api.Decisions.Approve(u.ctx, imported.ID, liveUserID, now); err != nil {
		t.Fatal(err)
	}
	project, err := u.api.Projects.Project(u.ctx, replay, 0)
	if err != nil {
		t.Fatal(err)
	}
	story := narrative.New(narrative.Content{Plan: project.Plan, Entities: project.Entities, Canon: project.Canon, Manuscript: project.Manuscript})
	patches, _, err := story.ResolveEdit(args.Edit, narrative.Settings{Intent: project.Intent, Ownership: project.Ownership})
	if err != nil {
		t.Fatal(err)
	}
	tally := make(map[string]int)
	for i := range times {
		start := time.Now()
		submitted, err := u.api.Decisions.Submit(u.ctx, model.Proposal{
			ID: fmt.Sprintf("%s:%d", replay, i+1), Target: model.AuthorityTarget{Kind: model.AuthorityProject, ID: replay},
			BaseRevision: project.Revision, Author: model.ChiefAuthor, Reason: args.Reason, Source: model.ConversationSource(1),
			Patches: patches, ApprovalState: model.ApprovalPending, CreatedAt: time.Now().UTC(),
		})
		var outcome string
		switch {
		case err != nil && strings.Contains(err.Error(), "contradicts this planning"):
			outcome = "退回：规划有出入"
		case err != nil:
			outcome = "出错"
		case submitted.Conflicted:
			outcome = "确认卡：已写章节有出入"
		default:
			outcome = "直接放行"
		}
		tally[outcome]++
		u.log("第 %d 次（%s）：%s", i+1, time.Since(start).Round(time.Second), outcome)
		if err != nil {
			u.log("  %v", story.Humanize(err))
			continue
		}
		if report := submitted.Proposal.Impact.Semantic; report != nil {
			for _, finding := range report.Findings {
				u.log("  出入：%s", finding.Explanation)
			}
		}
	}
	u.log("重放 %d 次：%v", times, tally)
}
