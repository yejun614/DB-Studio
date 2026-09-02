package api

import (
	"context"
	"fmt"
	"testing"

	"dbstudio/internal/crypto"
	"dbstudio/internal/erd"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 복제는 **그림을 읽게 만드는 것들**까지 함께 베껴야 한다.
//
// 예전에는 SQL 내보내기 → 새 초안 → 불러오기가 유일한 길이었고, 그 길로는 배치·색·
// 아이콘·논리명·도메인·메모가 모두 사라졌다. 그러면 사본은 같은 설계가 아니다.
func TestDuplicateERDDocumentCopiesEverything(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	docID := createStandalone(t, e, c, "주문 개편", "postgres")

	status, body := c.do("POST", "/api/v1/erd/documents/"+docID+"/import",
		map[string]any{"sql": `
CREATE TABLE members (id bigint PRIMARY KEY, email text NOT NULL);
CREATE TABLE orders (
  id bigint PRIMARY KEY,
  member_id bigint NOT NULL REFERENCES members (id)
);
`})
	if status != 200 {
		t.Fatalf("불러오기 = %d: %v", status, body)
	}

	// 도면에만 있는 것들을 심는다: 논리명·색·아이콘·폭, 도메인, 메모, 묶음.
	//
	// op 를 저장 계층으로 바로 넣는 이유: 편집은 WebSocket 으로만 들어온다(HTTP 의
	// /ops 는 이력 조회다). 여기서 확인하려는 것은 편집 경로가 아니라 "무엇이
	// 사본에 따라오는가"이므로, 문서에 값이 들어 있기만 하면 된다.
	ctx := context.Background()
	seed, err := e.st.GetERDDocument(ctx, docID)
	if err != nil {
		t.Fatalf("초안 읽기: %v", err)
	}
	opNo := 0
	apply := func(kind erd.Kind, payload string) {
		t.Helper()
		// op 마다 다른 id 여야 한다. 같은 id 는 "이미 적용된 op"로 거부된다
		// (AppendERDOp 의 unique 제약이 재전송을 걸러 낸다).
		opNo += 1
		op := &erd.Op{ID: fmt.Sprintf("seed-%d", opNo), Kind: kind, Payload: []byte(payload)}
		if aerr := erd.Apply(seed, op); aerr != nil {
			t.Fatalf("op %s: %v", kind, aerr)
		}
		if serr := e.st.AppendERDOp(ctx, seed, op); serr != nil {
			t.Fatalf("op %s 저장: %v", kind, serr)
		}
	}
	apply(erd.OpTableMove, `{"key":"members","x":120,"y":240,"width":320,`+
		`"logical":"회원","color":"#ff8800","icon":"users"}`)
	apply(erd.OpDomainAdd, `{"name":"이메일","type":"varchar(320)","comment":"소문자로 저장"}`)
	apply(erd.OpNoteAdd, `{"id":"note-1","text":"1차 리뷰 반영","x":40,"y":40}`)
	apply(erd.OpGroupAdd, `{"id":"group-1","label":"회원 영역","x":10,"y":10,"w":400,"h":300}`)

	before, err := e.st.GetERDDocument(ctx, docID)
	if err != nil {
		t.Fatalf("원본 읽기: %v", err)
	}

	status, body = c.do("POST", "/api/v1/erd/documents/"+docID+"/duplicate",
		map[string]any{"name": "주문 개편 B안"})
	if status != 201 {
		t.Fatalf("복제 = %d: %v", status, body)
	}
	dup, _ := body["document"].(map[string]any)
	dupID, _ := dup["id"].(string)
	if dupID == "" || dupID == docID {
		t.Fatalf("사본 id 가 이상합니다: %q (원본 %q)", dupID, docID)
	}
	if dup["name"] != "주문 개편 B안" {
		t.Errorf("이름이 %v 입니다", dup["name"])
	}

	after, err := e.st.GetERDDocument(ctx, dupID)
	if err != nil {
		t.Fatalf("사본 읽기: %v", err)
	}

	if len(after.Schema.Tables) != len(before.Schema.Tables) {
		t.Fatalf("표가 %d개입니다 (원본 %d개)", len(after.Schema.Tables), len(before.Schema.Tables))
	}
	// 관계도 함께 와야 한다. 표만 오면 그것은 도면이 아니라 목록이다.
	orders := after.Schema.Table("orders")
	if orders == nil || len(orders.ForeignKeys) != 1 {
		t.Fatalf("외래키가 따라오지 않았습니다: %+v", orders)
	}
	box := after.Layout["members"]
	if box == nil {
		t.Fatal("배치가 따라오지 않았습니다")
	}
	if box.X != 120 || box.Y != 240 || box.W != 320 {
		t.Errorf("자리·폭이 다릅니다: %+v", box)
	}
	if box.Logical != "회원" || box.Color != "#ff8800" || box.Icon != "users" {
		t.Errorf("논리명·색·아이콘이 따라오지 않았습니다: %+v", box)
	}
	if len(after.Domains) != 1 || after.Domains[0].Name != "이메일" {
		t.Errorf("도메인이 따라오지 않았습니다: %+v", after.Domains)
	}
	if len(after.Notes) != 1 || after.Notes[0].Text != "1차 리뷰 반영" {
		t.Errorf("메모가 따라오지 않았습니다: %+v", after.Notes)
	}
	if len(after.Groups) != 1 || after.Groups[0].Label != "회원 영역" {
		t.Errorf("묶음이 따라오지 않았습니다: %+v", after.Groups)
	}

	// 사본은 초안에서 시작하고, 편집 순번은 0이다. 원본의 순번을 물려받으면
	// 이력이 비어 있는데 "편집 여러 번"으로 보인다.
	if after.Status != erd.StatusDraft {
		t.Errorf("사본 상태가 %q 입니다", after.Status)
	}
	if after.Seq != 0 {
		t.Errorf("사본 편집 순번이 %d 입니다", after.Seq)
	}
	if before.Seq == 0 {
		t.Fatal("원본 순번이 0입니다 — 이 검사가 아무것도 확인하지 못합니다")
	}

	// 원본은 그대로다.
	orig, err := e.st.GetERDDocument(ctx, docID)
	if err != nil {
		t.Fatalf("원본 재확인: %v", err)
	}
	if orig.Name != "주문 개편" || orig.Seq != before.Seq {
		t.Errorf("원본이 바뀌었습니다: 이름 %q, 순번 %d", orig.Name, orig.Seq)
	}

	// 이름을 주지 않으면 "<원본> 사본"이다.
	status, body = c.do("POST", "/api/v1/erd/documents/"+docID+"/duplicate", map[string]any{})
	if status != 201 {
		t.Fatalf("이름 없는 복제 = %d: %v", status, body)
	}
	dup2, _ := body["document"].(map[string]any)
	if name, _ := dup2["name"].(string); name != "주문 개편 사본" {
		t.Errorf("기본 이름이 %q 입니다", name)
	}
}

// 볼 수 없는 문서는 베낄 수도 없다.
//
// 복제는 읽기 권한만 요구한다(사본의 내용은 이미 볼 수 있는 것이다). 그 "읽기"가
// 실제로 판정되는지 확인한다 — 새 경로를 열 때 권한 확인을 빼먹는 것이 가장 흔한
// 실수이고, 그 실수는 아무 오류도 내지 않는다.
func TestDuplicateERDDocumentNeedsAccess(t *testing.T) {
	e := newTestEnv(t)
	alice := login(t, e, "alice")
	docID := createStandalone(t, e, alice, "비밀 초안", "postgres")

	// 프로젝트에 들어 있지 않은 멤버. addMember 는 프로젝트에 넣으므로 쓰지 않는다.
	hash, err := crypto.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, err := e.st.CreateUser(context.Background(), store.CreateUserParams{
		Username: "carol", DisplayName: "carol", Role: model.RoleMember, PasswordHash: hash,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	carol := login(t, e, "carol")
	status, body := carol.do("POST", "/api/v1/erd/documents/"+docID+"/duplicate",
		map[string]any{"name": "몰래 사본"})
	if status != 403 && status != 404 {
		t.Fatalf("남의 초안을 베꼈습니다: %d %v", status, body)
	}
	if _, ok := body["document"]; ok {
		t.Errorf("거부됐는데 문서가 돌아왔습니다: %v", body)
	}
}
