package bootstrap_test

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/voocel/ainovel-cli/internal/app/chief"
	"github.com/voocel/ainovel-cli/internal/app/workbench"
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
	replay := u.importAt(source, revision)
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

// TestLiveReviewReplay 从一本书的某个版本接着写到第 AINOVEL_REPLAY_UNTIL 章，看审阅与重写在
// 那一段收不收得住（D89）：导入的书没有裁定，已写的章逐窗重审。库会被写入，用副本：
//
//	AINOVEL_LIVE=<库的副本> AINOVEL_REPLAY_PROJECT=<作品> AINOVEL_REPLAY_REVISION=<版本> \
//	AINOVEL_REPLAY_UNTIL=24 go test ./internal/bootstrap -run TestLiveReviewReplay -v
func TestLiveReviewReplay(t *testing.T) {
	source := os.Getenv("AINOVEL_REPLAY_PROJECT")
	if source == "" {
		t.Skip("AINOVEL_REPLAY_PROJECT 未设：不重放")
	}
	revision, err := strconv.Atoi(os.Getenv("AINOVEL_REPLAY_REVISION"))
	if err != nil {
		t.Fatalf("AINOVEL_REPLAY_REVISION: %v", err)
	}
	until, err := strconv.Atoi(os.Getenv("AINOVEL_REPLAY_UNTIL"))
	if err != nil {
		t.Fatalf("AINOVEL_REPLAY_UNTIL: %v", err)
	}
	u := openLive(t)
	origin, err := u.api.Workbench.WorkbenchSnapshot(u.ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	var goal model.NovelGoal // 篇幅照原书，结局不落在重放停下的地方
	if err := json.Unmarshal(origin.Run.Goal.Payload, &goal); err != nil {
		t.Fatal(err)
	}
	replay := u.importAt(source, revision)
	u.project, u.session = replay, u.api.Chief.Open(u.ctx, replay, liveUserID)
	t.Cleanup(u.session.Close)
	u.log("从 %s 第 %d 版接着写到第 %d 章（全书 %d 章）", source, revision, until, goal.TargetChapters)
	// 推进停下时才看得到章数，入稿到第 until 章就按暂停。
	u.steps = []liveStep{{at: until, name: "重放到此为止", do: func() {
		if err := u.session.Pause(u.ctx); err != nil {
			t.Fatal(err)
		}
	}}}
	u.session.Write(chief.WriteParams{Chapters: goal.TargetChapters, Approval: model.ApprovalAuto})
	u.advance(func(snap workbench.WorkbenchSnapshot) bool { return len(snap.Manuscript) >= until })
}

// importAt 把作品在 revision 上的样子导入成一本新书并通过，返回新书。
func (u *liveUser) importAt(source string, revision int) string {
	u.t.Helper()
	now := time.Now().UTC()
	projection, err := u.api.Projects.ExportProject(u.ctx, source, model.Revision(revision))
	if err != nil {
		u.t.Fatal(err)
	}
	replay := "replay-" + now.Format("150405")
	projection.ProjectID, projection.BaseRevision = replay, 0
	imported, err := u.api.Projects.ImportProject(u.ctx, replay+":import", liveUserID, "重放的起点", projection, now)
	if err != nil {
		u.t.Fatal(err)
	}
	if _, err := u.api.Decisions.Approve(u.ctx, imported.ID, liveUserID, now); err != nil {
		u.t.Fatal(err)
	}
	return replay
}
