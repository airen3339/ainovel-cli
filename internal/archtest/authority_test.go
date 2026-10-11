package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// authorityWriters 是唯一可以调用权威写方法的包：change 引擎与兑现它的 store。
var authorityWriters = []string{"domain/change", "infra/store"}

// 权威数据只经 change 引擎写入（§11 模块纪律第 3 条、§12.1 硬规则第 1 条）。调用方大多
// 持有具体 store 或宽接口，依赖白名单挡不住绕过引擎的写入，因此按方法名钉住：change.Store
// 契约里的写方法只能出现在 authorityWriters 的生产代码里。
func TestAuthorityWritesOnlyThroughChangeEngine(t *testing.T) {
	writes := authorityWriteMethods(t)
	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			rel, err := filepath.Rel("..", path)
			if err != nil {
				return err
			}
			if rel := filepath.ToSlash(rel); rel == "archtest" || slices.Contains(authorityWriters, rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && slices.Contains(writes, selector.Sel.Name) {
					t.Errorf("%s 调用了 %s：权威数据只能经 change 引擎写入", path, selector.Sel.Name)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// authorityWriteMethods 从 change.Store 契约推导写方法：读方法按约定以 Get、List 或
// Current 开头，其余都是写。新增写方法自动纳入；不守约定的新读方法会被当成写而报错，
// 不会静默放行。
func authorityWriteMethods(t *testing.T) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "domain", "change", "ports.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse change ports: %v", err)
	}
	spec, ok := file.Scope.Lookup("Store").Decl.(*ast.TypeSpec)
	if !ok {
		t.Fatal("change.Store 契约不存在：推导失效")
	}
	contract, ok := spec.Type.(*ast.InterfaceType)
	if !ok {
		t.Fatal("change.Store 不是接口：推导失效")
	}
	var writes []string
	for _, method := range contract.Methods.List {
		for _, name := range method.Names {
			if !strings.HasPrefix(name.Name, "Get") && !strings.HasPrefix(name.Name, "List") && !strings.HasPrefix(name.Name, "Current") {
				writes = append(writes, name.Name)
			}
		}
	}
	if len(writes) == 0 {
		t.Fatal("change.Store 推导不出任何写方法：推导失效")
	}
	return writes
}
