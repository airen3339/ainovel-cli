package prompt

import (
	"encoding/json"
	"slices"
	"testing"
)

// TestSubmissionSchemaExposesOnlyUsableParameters 钉住实跑教训：写新章也公开确认参数，模型
// 每章都去确认前文全部事实；工作稿版本可选，模型就把整章正文抄进提交。每个 Worker 只公开
// 有合法用途的参数，正文由宿主取本章工作稿装配（D86），参数里没有任何文档 ID（D66）。
func TestSubmissionSchemaExposesOnlyUsableParameters(t *testing.T) {
	planning := []string{"compass", "volumes", "arcs", "chapters"}
	redeclare := []string{"confirm_facts", "remove_facts"}
	drafts := []string{"workspace_key", "workspace_version", "workspace_versions"}
	cases := map[string]struct {
		required, present, absent []string
	}{
		"architect.design": {
			required: []string{"reason"}, present: append(append([]string{"entities", "facts"}, planning...), redeclare...),
			// 创作意图只装用户的原话（D70），任何 Worker 都不能提交它。
			absent: append([]string{"intent", "outline"}, drafts...),
		},
		"writer.compose": {
			required: []string{"reason", "facts"}, present: []string{"entities"},
			absent: append(append(append(slices.Clone(drafts), planning...), redeclare...), "outline"),
		},
		// 审阅修订照章纲改正文，改不了章纲：否则写手能改大纲躲开“与章纲不符”的意见（D87）。
		"writer.revise": {
			required: []string{"reason"}, present: append([]string{"entities", "facts"}, redeclare...),
			absent: append(append(slices.Clone(drafts), planning...), "outline"),
		},
		"writer.rework": {
			required: []string{"reason", "outline"}, present: append([]string{"entities", "facts"}, redeclare...),
			absent: append(slices.Clone(drafts), planning...),
		},
	}
	for id, want := range cases {
		t.Run(id, func(t *testing.T) {
			worker, err := BuiltinWorkerProfile(id)
			if err != nil {
				t.Fatal(err)
			}
			index := slices.IndexFunc(worker.Tools, func(tool ToolSchema) bool { return tool.Name == ToolProposalSubmit })
			if index < 0 {
				t.Fatal("missing proposal_submit")
			}
			var schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
				Required   []string                   `json:"required"`
			}
			if err := json.Unmarshal(worker.Tools[index].InputSchema, &schema); err != nil {
				t.Fatal(err)
			}
			slices.Sort(schema.Required)
			slices.Sort(want.required)
			if !slices.Equal(schema.Required, want.required) {
				t.Fatalf("required = %v, want %v", schema.Required, want.required)
			}
			for _, name := range want.present {
				if schema.Properties[name] == nil {
					t.Fatalf("%s lacks parameter %s", id, name)
				}
			}
			for _, name := range want.absent {
				if schema.Properties[name] != nil {
					t.Fatalf("%s exposes unusable parameter %s", id, name)
				}
			}
			var facts struct {
				Items struct {
					Required []string `json:"required"`
				} `json:"items"`
			}
			if err := json.Unmarshal(schema.Properties["facts"], &facts); err != nil {
				t.Fatal(err)
			}
			// 来源章由宿主按任务填。
			if slices.Contains(facts.Items.Required, "chapter") {
				t.Fatal("fact chapter must not be required")
			}
		})
	}
}
