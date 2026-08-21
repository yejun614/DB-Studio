package erd

import (
	"encoding/json"
	"testing"
)

// 되돌리기는 "적용 전후를 비교해 되돌리는 op를 만든다"로 구현되어 있다.
// 그래서 확인할 것은 하나다: 어떤 op를 적용한 뒤 그 역을 적용하면 문서가
// 원래 모습과 **완전히** 같아지는가. 눈에 잘 띄지 않는 것(컬럼 순서, 남의
// 테이블의 외래키, 좌표)이 조용히 달라지는 것이 이 기능의 실패 방식이다.

func snapshot(t *testing.T, doc *Document) string {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// roundTrip은 op 하나를 적용하고 되돌린 뒤 원래대로인지 본다.
func roundTrip(t *testing.T, doc *Document, kind Kind, payload string) {
	t.Helper()
	before := doc.Clone()
	// 양쪽 모두 사본으로 비교한다. Clone은 빈 슬라이스를 nil로 남기는 자리가 있어
	// (Views) 원본과 사본을 직접 비교하면 이 시험과 무관한 차이가 잡힌다.
	want := snapshot(t, before)

	apply(t, doc, kind, payload)
	if snapshot(t, doc.Clone()) == want {
		t.Fatalf("%s 가 문서를 바꾸지 않았습니다 — 시험이 아무것도 확인하지 못합니다", kind)
	}

	inv := Diff(doc, before)
	if inv == nil {
		t.Fatalf("%s 의 역연산이 만들어지지 않았습니다", kind)
	}
	inv.ID = "undo"
	if err := Apply(doc, inv); err != nil {
		t.Fatalf("%s 되돌리기: %v", kind, err)
	}
	if got := snapshot(t, doc.Clone()); got != want {
		t.Errorf("%s 를 되돌린 결과가 다릅니다\n원래: %s\n결과: %s", kind, want, got)
	}
}

func seeded(t *testing.T) *Document {
	t.Helper()
	doc := NewDocument("d1", "초안", "c1", "postgres")
	apply(t, doc, OpTableAdd, `{"name":"users","withId":true}`)
	apply(t, doc, OpTableAdd, `{"name":"orders","withId":true}`)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"email","type":"text"}`)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"name","type":"text"}`)
	apply(t, doc, OpColumnAdd, `{"table":"orders","name":"user_id","type":"bigint"}`)
	apply(t, doc, OpIndexAdd, `{"table":"users","name":"users_email_idx","columns":["email"],"unique":true}`)
	apply(t, doc, OpFKAdd, `{"table":"orders","name":"orders_user_fk","columns":["user_id"],"refTable":"users","refColumns":["id"]}`)
	apply(t, doc, OpNoteAdd, `{"id":"n1","text":"메모","x":10,"y":20}`)
	apply(t, doc, OpGroupAdd, `{"id":"g1","label":"주문","x":0,"y":0,"w":100,"h":100}`)
	return doc
}

func TestUndoRoundTrip(t *testing.T) {
	cases := []struct {
		kind    Kind
		payload string
	}{
		{OpTableAdd, `{"name":"payments"}`},
		{OpTableUpdate, `{"key":"users","name":"members","comment":"이름 변경"}`},
		{OpTableMove, `{"key":"users","x":500,"y":600}`},
		{OpColumnAdd, `{"table":"users","name":"phone","type":"text"}`},
		{OpColumnUpdate, `{"table":"users","name":"email","type":"varchar(200)","nullable":false}`},
		{OpColumnMove, `{"table":"users","name":"email","to":1}`},
		{OpPKSet, `{"table":"users","columns":["id","email"]}`},
		{OpIndexAdd, `{"table":"orders","name":"orders_user_idx","columns":["user_id"]}`},
		{OpIndexDelete, `{"table":"users","name":"users_email_idx"}`},
		{OpFKDelete, `{"table":"orders","name":"orders_user_fk"}`},
		{OpCheckAdd, `{"table":"users","name":"users_email_ck","expression":"email <> ''"}`},
		{OpEnumAdd, `{"name":"status","values":["a","b"]}`},
		{OpNoteUpdate, `{"id":"n1","text":"바뀐 메모"}`},
		{OpNoteDelete, `{"id":"n1"}`},
		{OpGroupUpdate, `{"id":"g1","label":"바뀐 그룹","color":"#f00"}`},
		{OpGroupDelete, `{"id":"g1"}`},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			roundTrip(t, seeded(t), tc.kind, tc.payload)
		})
	}
}

// 지우기는 되돌리기가 가장 필요한 편집이고, 가장 많은 것을 함께 지운다.
// 테이블을 지우면 **다른 테이블의 외래키까지** 사라지므로, 그것까지 돌아와야 한다.
func TestUndoCascadingDelete(t *testing.T) {
	doc := seeded(t)
	before := doc.Clone()

	apply(t, doc, OpTableDelete, `{"key":"users","cascade":true}`)
	if len(doc.Schema.Tables) != 1 {
		t.Fatalf("테이블 수 = %d", len(doc.Schema.Tables))
	}
	orders := doc.findTable("orders")
	if len(orders.ForeignKeys) != 0 {
		t.Fatalf("cascade가 참조를 지우지 않았습니다: %+v", orders.ForeignKeys)
	}

	inv := Diff(doc, before)
	if inv == nil {
		t.Fatal("역연산이 없습니다")
	}
	inv.ID = "undo"
	if err := Apply(doc, inv); err != nil {
		t.Fatalf("되돌리기: %v", err)
	}
	if snapshot(t, doc.Clone()) != snapshot(t, before) {
		t.Error("cascade 삭제를 되돌린 결과가 원본과 다릅니다")
	}
	// 특히 남의 테이블에 있던 외래키가 살아 돌아왔는지 명시적으로 본다.
	if got := doc.findTable("orders"); len(got.ForeignKeys) != 1 {
		t.Errorf("참조하던 외래키가 돌아오지 않았습니다: %+v", got.ForeignKeys)
	}
}

// 컬럼 삭제도 기본키·인덱스·외래키를 함께 정리한다.
func TestUndoColumnDeleteRestoresConstraints(t *testing.T) {
	doc := seeded(t)
	before := doc.Clone()

	apply(t, doc, OpColumnDelete, `{"table":"users","name":"id"}`)
	if doc.findTable("users").PrimaryKey != nil {
		t.Fatal("기본키가 남아 있습니다")
	}
	if len(doc.findTable("orders").ForeignKeys) != 0 {
		t.Fatal("참조하던 외래키가 남아 있습니다")
	}

	inv := Diff(doc, before)
	inv.ID = "undo"
	if err := Apply(doc, inv); err != nil {
		t.Fatalf("되돌리기: %v", err)
	}
	if snapshot(t, doc.Clone()) != snapshot(t, before) {
		t.Error("컬럼 삭제를 되돌린 결과가 원본과 다릅니다")
	}
}

// 되돌리기의 역은 다시실행이다 — 같은 함수로 만들어지므로 별도 규칙이 없어야 한다.
func TestRedoIsInverseOfUndo(t *testing.T) {
	doc := seeded(t)
	before := doc.Clone()
	apply(t, doc, OpTableDelete, `{"key":"orders"}`)
	afterEdit := doc.Clone()

	undo := Diff(doc, before)
	undo.ID = "undo"
	if err := Apply(doc, undo); err != nil {
		t.Fatalf("되돌리기: %v", err)
	}
	redo := Diff(doc, afterEdit)
	if redo == nil {
		t.Fatal("다시실행 op가 없습니다")
	}
	redo.ID = "redo"
	if err := Apply(doc, redo); err != nil {
		t.Fatalf("다시실행: %v", err)
	}
	if snapshot(t, doc.Clone()) != snapshot(t, afterEdit) {
		t.Error("다시실행 결과가 편집 직후와 다릅니다")
	}
}

// 되돌리기는 다른 사람의 편집 위에서 일어난다. 되살린 테이블이 없는 것을
// 가리키게 되면 거절해야 한다 — 마이그레이션 단계에서 터지면 원인을 찾을 수 없다.
func TestUndoRejectsDanglingReference(t *testing.T) {
	doc := seeded(t)
	before := doc.Clone()

	// 내가 orders를 지웠고,
	apply(t, doc, OpTableDelete, `{"key":"orders"}`)
	undo := Diff(doc, before)
	undo.ID = "undo"

	// 그 사이에 다른 사람이 users를 지웠다.
	apply(t, doc, OpTableDelete, `{"key":"users"}`)

	err := Apply(doc, undo)
	if err == nil {
		t.Fatal("참조가 끊긴 되돌리기가 통과했습니다")
	}
	var opErr *Error
	if !asError(err, &opErr) || opErr.Code != "conflict" {
		t.Errorf("오류 코드 = %v, conflict여야 합니다", err)
	}
}

func asError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}

// 바뀐 것이 없으면 되돌릴 것도 없다. nil을 돌려주지 않으면 빈 op가 로그에 쌓인다.
func TestDiffNilWhenUnchanged(t *testing.T) {
	doc := seeded(t)
	if op := Diff(doc, doc.Clone()); op != nil {
		t.Errorf("같은 문서인데 op가 나왔습니다: %s", op.Payload)
	}
}
