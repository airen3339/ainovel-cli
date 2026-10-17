package model

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// 复审（D68）：已审且正文未变的章只作上下文，阻塞发现必须链接被违反的要求；
// 输入里的已审章与上一轮意见必须落在审阅范围内。
func TestReviewedChaptersOnlyBlockThroughRequirements(t *testing.T) {
	basis := EvidenceBasis{Documents: []DocumentBasis{{Ref: DocumentRef{Kind: DocumentManuscript, ID: "chapter-1"}, Revision: 2}}}
	input := ReviewRangeInput{
		ChapterIDs: []string{"chapter-1", "chapter-2"}, Reviewed: []string{"chapter-1"}, Basis: basis,
		Requirements: []Requirement{{ID: "directive:rain", Text: "要下雨"}},
	}
	raw, _ := json.Marshal(input)
	operation := Operation{Kind: OperationReviewRange, Snapshot: ExecutionSnapshot{BaseRevision: 3}, Input: raw}
	verdict := func(check string, findings ...ReviewFinding) ReviewVerdict {
		return ReviewVerdict{
			Revision: 3, ChapterIDs: input.ChapterIDs, ReviewKey: "review", Basis: basis,
			Checks: []RequirementCheck{{ID: "directive:rain", Status: check}}, Findings: append([]ReviewFinding{}, findings...),
		}
	}
	cases := []struct {
		name    string
		verdict ReviewVerdict
		wantErr bool
	}{
		{"blocking on reviewed chapter", verdict(CheckSatisfied, ReviewFinding{ChapterID: "chapter-1", Severity: FindingBlocking, Note: "旧问题"}), true},
		{"violated requirement on reviewed chapter", verdict(CheckViolated, ReviewFinding{ChapterID: "chapter-1", Severity: FindingBlocking, Note: "没下雨", Requirement: "directive:rain"}), false},
		{"note on reviewed chapter", verdict(CheckSatisfied, ReviewFinding{ChapterID: "chapter-1", Severity: FindingNote, Note: "旧问题"}), false},
		{"blocking on changed chapter", verdict(CheckSatisfied, ReviewFinding{ChapterID: "chapter-2", Severity: FindingBlocking, Note: "新问题"}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateReviewVerdictForOperation(operation, tc.verdict)
			if tc.wantErr != (err != nil) || tc.wantErr && (!errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), `"chapter-1"`)) {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
		})
	}

	blocking := ReviewFinding{ChapterID: "chapter-2", Severity: FindingBlocking, Note: "时间线冲突"}
	for name, bad := range map[string]ReviewRangeInput{
		"reviewed outside range":     {ChapterIDs: []string{"chapter-2"}, Reviewed: []string{"chapter-1"}, Basis: basis},
		"duplicated reviewed":        {ChapterIDs: input.ChapterIDs, Reviewed: []string{"chapter-1", "chapter-1"}, Basis: basis},
		"prior on reviewed chapter":  {ChapterIDs: input.ChapterIDs, Reviewed: []string{"chapter-2"}, PriorFindings: []ReviewFinding{blocking}, Basis: basis},
		"prior note":                 {ChapterIDs: input.ChapterIDs, PriorFindings: []ReviewFinding{{ChapterID: "chapter-2", Severity: FindingNote, Note: "节奏"}}, Basis: basis},
		"prior outside review range": {ChapterIDs: []string{"chapter-1"}, PriorFindings: []ReviewFinding{blocking}, Basis: basis},
	} {
		if err := bad.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	good := ReviewRangeInput{ChapterIDs: input.ChapterIDs, Reviewed: []string{"chapter-1"}, PriorFindings: []ReviewFinding{blocking}, Basis: basis}
	if err := good.Validate(); err != nil {
		t.Fatalf("recheck input: %v", err)
	}
}

// 复审时没改过的章与上一份裁定已判满足的要求都没变，那条结论不重开：不能借它阻塞没改过的
// 章，矛盾记在改动章上；上一份裁定没判满足的要求照旧能阻塞（如新增的伏笔让收官要求变了）。
func TestSatisfiedRequirementsStayClosedOnReviewedChapters(t *testing.T) {
	basis := EvidenceBasis{Documents: []DocumentBasis{{Ref: DocumentRef{Kind: DocumentManuscript, ID: "chapter-1"}, Revision: 2}}}
	input := ReviewRangeInput{
		ChapterIDs: []string{"chapter-1", "chapter-2"}, Reviewed: []string{"chapter-1"}, Satisfied: []string{"directive:rain"}, Basis: basis,
		Requirements: []Requirement{{ID: "directive:rain", Text: "要下雨"}, {ID: "book:finale:new", Text: "结局回收伏笔"}},
	}
	block := func(chapter, requirement string) ReviewFinding {
		return ReviewFinding{ChapterID: chapter, Severity: FindingBlocking, Note: "不合要求", Requirement: requirement}
	}
	if err := input.AdmitsFinding(block("chapter-1", "directive:rain")); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), `"directive:rain"`) {
		t.Fatalf("settled requirement reopened on an unchanged chapter: %v", err)
	}
	for _, finding := range []ReviewFinding{block("chapter-1", "book:finale:new"), block("chapter-2", "directive:rain")} {
		if err := input.AdmitsFinding(finding); err != nil {
			t.Fatalf("%+v: %v", finding, err)
		}
	}
	if err := input.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]ReviewRangeInput{
		"satisfied outside requirements": {ChapterIDs: input.ChapterIDs, Reviewed: input.Reviewed, Requirements: input.Requirements, Satisfied: []string{"directive:gone"}, Basis: basis},
		"satisfied without reviewed":     {ChapterIDs: input.ChapterIDs, Requirements: input.Requirements, Satisfied: []string{"directive:rain"}, Basis: basis},
	} {
		if err := bad.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestReviewVerdictRequirementCoverage(t *testing.T) {
	// D62：任务输入携带的每项要求都必须被恰好声明一次，不得漏项、越界或重复。
	operation := Operation{
		Kind: OperationReviewRange, Snapshot: ExecutionSnapshot{BaseRevision: 3},
		Input: json.RawMessage(`{"chapter_ids":["chapter-1"],"requirements":[{"id":"directive:hook","text":"结尾留钩子"},{"id":"directive:rain","text":"要下雨"}],"basis":{"documents":[{"ref":{"kind":"manuscript","id":"chapter-1"},"revision":2}]}}`),
	}
	basis := EvidenceBasis{Documents: []DocumentBasis{{Ref: DocumentRef{Kind: DocumentManuscript, ID: "chapter-1"}, Revision: 2}}}
	base := func(checks ...RequirementCheck) ReviewVerdict {
		return ReviewVerdict{
			Revision: 3, ChapterIDs: []string{"chapter-1"}, ReviewKey: "review", Basis: basis,
			Checks: checks, Findings: []ReviewFinding{},
		}
	}
	hook := RequirementCheck{ID: "directive:hook", Status: CheckSatisfied}
	rain := RequirementCheck{ID: "directive:rain", Status: CheckPending}
	cases := []struct {
		name    string
		verdict ReviewVerdict
		wantErr bool
	}{
		{"missing all", base(), true},
		{"missing one", base(hook), true},
		{"outside requested", base(hook, RequirementCheck{ID: "directive:other", Status: CheckSatisfied}), true},
		{"duplicated", base(hook, hook), true},
		{"all declared", base(rain, hook), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateReviewVerdictForOperation(operation, tc.verdict)
			if tc.wantErr && !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected err = %v", err)
			}
		})
	}

	// 任务未携带要求时，裁定不得凭空声明。
	plain := Operation{
		Kind: OperationReviewRange, Snapshot: ExecutionSnapshot{BaseRevision: 3},
		Input: json.RawMessage(`{"chapter_ids":["chapter-1"],"basis":{"documents":[{"ref":{"kind":"manuscript","id":"chapter-1"},"revision":2}]}}`),
	}
	if err := ValidateReviewVerdictForOperation(plain, base(hook)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unrequested check err = %v", err)
	}
	if err := ValidateReviewVerdictForOperation(plain, base()); err != nil {
		t.Fatalf("plain pass err = %v", err)
	}
}

func TestOperationOutcomeCombinations(t *testing.T) {
	proposal := &Proposal{ID: "p"}
	verdict := json.RawMessage(`{"status":"pass"}`)
	artifacts := []Artifact{{
		ID: "op/cover", ProjectID: "book-1", Digest: Digest([]byte("d")), MediaType: "image/png",
		OperationID: "op", Attempt: 1, CreatedAt: time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC),
	}}
	cases := []struct {
		name    string
		outcome OperationOutcome
		valid   bool
	}{
		{"empty", OperationOutcome{}, false},
		{"proposal", OperationOutcome{Proposal: proposal}, true},
		{"verdict", OperationOutcome{Verdict: verdict}, true},
		{"artifacts", OperationOutcome{Artifacts: artifacts}, true},
		{"proposal+verdict", OperationOutcome{Proposal: proposal, Verdict: verdict}, false},
		{"proposal+artifacts", OperationOutcome{Proposal: proposal, Artifacts: artifacts}, true},
		{"verdict+artifacts", OperationOutcome{Verdict: verdict, Artifacts: artifacts}, true},
		{"all", OperationOutcome{Proposal: proposal, Verdict: verdict, Artifacts: artifacts}, false},
	}
	for _, testCase := range cases {
		if err := testCase.outcome.Validate(); (err == nil) != testCase.valid {
			t.Fatalf("%s: err = %v, want valid=%v", testCase.name, err, testCase.valid)
		}
	}
	duplicate := OperationOutcome{Artifacts: append(append([]Artifact(nil), artifacts...), artifacts...)}
	if err := duplicate.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate artifacts err = %v", err)
	}
}

// D81：结论由发现推出，审阅者不另报——有 blocking 即未通过，只有 note 或没有发现即通过。
func TestReviewStatusFollowsFindings(t *testing.T) {
	note := ReviewFinding{ChapterID: "chapter-1", Severity: FindingNote, Note: "节奏偏慢"}
	blocking := ReviewFinding{ChapterID: "chapter-2", Severity: FindingBlocking, Note: "同一封信既没寄出又被退回"}
	for _, c := range []struct {
		findings []ReviewFinding
		want     int
	}{{nil, 0}, {[]ReviewFinding{note}, 0}, {[]ReviewFinding{note, blocking}, 1}} {
		if got := (ReviewVerdict{Findings: c.findings}).Blocking(); got != c.want {
			t.Fatalf("blocking of %+v = %d, want %d", c.findings, got, c.want)
		}
	}
}
