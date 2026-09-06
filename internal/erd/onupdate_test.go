package erd

import "testing"

// 자동 갱신 식은 DDL 에 따옴표 없이 그대로 들어간다. 자유 문자열로 두면 그 자리가
// 곧 문장 구조를 바꾸는 통로가 된다 — ERD 를 그리는 사람과 마이그레이션을 실행하는
// 사람이 다를 수 있으므로 여기서 막는다.
func TestOnUpdateAcceptsOnlyTimeFunctions(t *testing.T) {
	doc := NewDocument("doc1", "테스트", "conn1", "mysql")
	apply(t, doc, OpTableAdd, `{"name":"posts"}`)
	apply(t, doc, OpColumnAdd, `{"table":"posts","name":"updated_at","type":"datetime(3)"}`)

	ok := []string{"CURRENT_TIMESTAMP", "current_timestamp(3)", "NOW()", "LOCALTIME(6)"}
	for _, expr := range ok {
		apply(t, doc, OpColumnUpdate,
			`{"table":"posts","name":"updated_at","onUpdate":"`+expr+`"}`)
		if got := doc.findTable("posts").Column("updated_at").OnUpdate; got != expr {
			t.Errorf("%q 를 넣었는데 %q 가 됐습니다", expr, got)
		}
	}

	bad := map[string]string{
		`{"table":"posts","name":"updated_at","onUpdate":"CURRENT_TIMESTAMP; DROP TABLE x"}`: "계열만",
		`{"table":"posts","name":"updated_at","onUpdate":"my_func()"}`:                       "계열만",
		`{"table":"posts","name":"updated_at","onUpdate":"CURRENT_TIMESTAMP(9)"}`:            "0에서 6",
		`{"table":"posts","name":"updated_at","onUpdate":"CURRENT_TIMESTAMP(3"}`:             "괄호",
	}
	for payload, want := range bad {
		applyErr(t, doc, OpColumnUpdate, payload, "invalid", want)
	}

	// 빈 문자열은 없애라는 뜻이다(기본값과 같은 규칙).
	apply(t, doc, OpColumnUpdate, `{"table":"posts","name":"updated_at","onUpdate":""}`)
	if got := doc.findTable("posts").Column("updated_at").OnUpdate; got != "" {
		t.Errorf("비웠는데 %q 가 남았습니다", got)
	}
}

// 사본에 적용한 op 가 원본을 바꾸면 안 된다(적용은 늘 사본에서 시작한다).
func TestOnUpdateSurvivesClone(t *testing.T) {
	doc := NewDocument("doc1", "테스트", "conn1", "mysql")
	apply(t, doc, OpTableAdd, `{"name":"posts"}`)
	apply(t, doc, OpColumnAdd, `{"table":"posts","name":"updated_at","type":"datetime"}`)
	apply(t, doc, OpColumnUpdate,
		`{"table":"posts","name":"updated_at","onUpdate":"CURRENT_TIMESTAMP"}`)

	clone := doc.Clone()
	if got := clone.findTable("posts").Column("updated_at").OnUpdate; got != "CURRENT_TIMESTAMP" {
		t.Errorf("사본에 자동 갱신이 따라오지 않았습니다: %q", got)
	}
	apply(t, clone, OpColumnUpdate, `{"table":"posts","name":"updated_at","onUpdate":""}`)
	if got := doc.findTable("posts").Column("updated_at").OnUpdate; got != "CURRENT_TIMESTAMP" {
		t.Errorf("사본을 고쳤는데 원본이 바뀌었습니다: %q", got)
	}
}
