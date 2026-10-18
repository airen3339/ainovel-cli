package bootstrap_test

import (
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	projectdoc "github.com/voocel/ainovel-cli/internal/app/project"
	"github.com/voocel/ainovel-cli/internal/infra/store"
)

// 定版时冻结的存储结构（D93）：每个冻结过的版本留真实的库作夹具（testdata/schema/v<版本>-<场景>.db.gz）。
// 当前程序要能打开它、读出书架与工作台，并在上面接着提交——每次提交都校验整个投影状态。存储格式
// 一变这里先失败：版本加一并附迁移，让这些库升上来。
func TestFrozenSchemaFixturesStayUsable(t *testing.T) {
	paths, err := filepath.Glob("testdata/schema/*.db.gz")
	if err != nil || len(paths) == 0 {
		t.Fatalf("fixtures = %v, %v", paths, err)
	}
	ctx := context.Background()
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			s, err := store.Open(ctx, unpackFixture(t, path))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			api := newTestApp(s)
			library, err := api.Workbench.Library(ctx)
			if err != nil || len(library) != 1 || library[0].Written == 0 {
				t.Fatalf("library = %+v, %v", library, err)
			}
			project := library[0].ID
			if _, err := api.Workbench.WorkbenchSnapshot(ctx, project); err != nil {
				t.Fatal(err)
			}
			if _, err := api.Projects.AddDirective(ctx, projectdoc.AddDirectiveCommand{
				ProjectID: project, ChangeID: "frozen-directive", UserID: "user-1", DirectiveID: "frozen",
				Scope: "from_chapter:1", Text: "每章结尾留悬念", Reason: "在冻结的库上接着提交", CreatedAt: time.Now().UTC(),
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func unpackFixture(t *testing.T, path string) string {
	t.Helper()
	packed, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer packed.Close()
	reader, err := gzip.NewReader(packed)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "ainovel.db")
	unpacked, err := os.Create(target)
	if err != nil {
		t.Fatal(err)
	}
	defer unpacked.Close()
	if _, err := io.Copy(unpacked, reader); err != nil {
		t.Fatal(err)
	}
	return target
}
