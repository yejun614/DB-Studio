package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// 논리명은 툴로 붙일 수 있어야 한다.
//
// 이 테스트가 생긴 이유: "모든 테이블에 논리명을 붙여줘"라는 부탁을 받은 모델이
// 그럴 툴이 없어서 스키마만 반복해 읽다가 툴 왕복 상한에 걸려 끝났다. 사람에게는
// 그냥 "답이 중간에서 멈췄다"로 보였다. 없는 기능은 모델이 아무리 똑똑해도 못 한다.
func TestSetLogicalNames(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	docID := createStandalone(t, e, c, "논리명 초안", "postgres")

	registry := aiTools()
	tc := &toolContext{ctx: context.Background(), srv: e.srv, user: e.user}
	run := func(name, args string) (string, error) {
		tool := registry[name]
		if tool == nil {
			t.Fatalf("툴 %s 이(가) 상자에 없습니다", name)
		}
		return tool.Run(tc, json.RawMessage(args))
	}

	if _, err := run("erd_add_table", `{"document":"논리명 초안","name":"members"}`); err != nil {
		t.Fatalf("add_table: %v", err)
	}
	if _, err := run("erd_add_column",
		`{"document":"논리명 초안","table":"members","name":"member_id","type":"bigint","nullable":false}`); err != nil {
		t.Fatalf("add_column: %v", err)
	}

	// 카드를 옮겨 둔다. 논리명을 붙이는 동안 자리가 흐트러지면 안 된다 —
	// 논리명은 배치(table.move)로 저장되는데, 그 op는 좌표도 함께 받는다.
	doc, err := e.st.GetERDDocument(context.Background(), docID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	key := doc.Schema.Tables[0].Key()
	if box := doc.Layout[key]; box != nil {
		box.X, box.Y = 640, 480
	}
	if _, err := run("erd_update_table",
		`{"document":"논리명 초안","table":"members","comment":"회원"}`); err != nil {
		t.Fatalf("update_table: %v", err)
	}

	out, err := run("erd_set_logical_names", `{"document":"논리명 초안","tables":[
		{"table":"members","logical":"회원","columns":{"member_id":"회원 번호"}}]}`)
	if err != nil {
		t.Fatalf("set_logical_names: %v", err)
	}
	if !strings.Contains(out, "회원") {
		t.Errorf("결과에 붙인 이름이 없습니다: %s", out)
	}

	doc, err = e.st.GetERDDocument(context.Background(), docID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	box := doc.Layout[key]
	if box == nil {
		t.Fatal("배치가 없습니다")
	}
	if box.Logical != "회원" {
		t.Errorf("테이블 논리명 = %q, 기대 %q", box.Logical, "회원")
	}
	if got := box.ColumnLogical["member_id"]; got != "회원 번호" {
		t.Errorf("컬럼 논리명 = %q, 기대 %q", got, "회원 번호")
	}

	// read_schema가 붙은 이름을 되돌려줘야 한다. 안 보이면 모델은 다 붙었는지
	// 알 수 없어 매번 처음부터 다시 붙인다.
	got, err := run("erd_read_schema", `{"document":"논리명 초안"}`)
	if err != nil {
		t.Fatalf("read_schema: %v", err)
	}
	if !strings.Contains(got, "회원 번호") {
		t.Errorf("read_schema가 논리명을 빠뜨렸습니다: %s", got)
	}

	// 없는 컬럼은 조용히 넘기지 않는다. 넘기면 모델은 붙였다고 믿는다.
	if _, err := run("erd_set_logical_names", `{"document":"논리명 초안","tables":[
		{"table":"members","columns":{"nope":"없는 것"}}]}`); err == nil {
		t.Error("없는 컬럼에 이름을 붙였는데 통과했습니다")
	}
}

// 테이블이 많으면 read_schema가 스스로 나눠 준다.
//
// 예전에는 통째로 만들어 마지막에 바이트로 잘랐다. 잘린 자리가 JSON 한가운데라
// 모델은 아무것도 못 읽고, 결과에 붙은 "조건을 좁혀 다시 조회하세요"를 따를 방법도
// 없었다(그 툴은 인자를 받지 않았다). 그래서 같은 호출을 반복하다 끝났다.
func TestReadSchemaPages(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	createStandalone(t, e, c, "큰 초안", "postgres")

	registry := aiTools()
	tc := &toolContext{ctx: context.Background(), srv: e.srv, user: e.user}
	run := func(name, args string) (string, error) {
		return registry[name].Run(tc, json.RawMessage(args))
	}

	// 컬럼이 많은 표를 여러 개 만들어 한 번에 다 담기지 않게 한다.
	for i := 0; i < 12; i++ {
		name := "table_with_a_fairly_long_name_" + string(rune('a'+i))
		if _, err := run("erd_add_table", `{"document":"큰 초안","name":"`+name+`"}`); err != nil {
			t.Fatalf("add_table: %v", err)
		}
		for j := 0; j < 14; j++ {
			col := "column_with_a_fairly_long_name_" + string(rune('a'+j))
			if _, err := run("erd_add_column", `{"document":"큰 초안","table":"`+name+
				`","name":"`+col+`","type":"character varying(255)",`+
				`"comment":"이 컬럼은 설명이 제법 길어서 결과 크기를 키운다"}`); err != nil {
				t.Fatalf("add_column: %v", err)
			}
		}
	}

	first, err := run("erd_read_schema", `{"document":"큰 초안"}`)
	if err != nil {
		t.Fatalf("read_schema: %v", err)
	}
	// 잘리지 않은 온전한 JSON이어야 한다.
	var page struct {
		TableCount int `json:"tableCount"`
		NextOffset int `json:"nextOffset"`
		Tables     []struct {
			Name string `json:"name"`
		} `json:"tables"`
	}
	if err := json.Unmarshal([]byte(first), &page); err != nil {
		t.Fatalf("결과가 온전한 JSON이 아닙니다: %v\n%s", err, first[:200])
	}
	if strings.Contains(first, "잘렸습니다") {
		t.Error("스스로 멈추지 않고 바이트 상한에 걸렸습니다")
	}
	if page.TableCount != 12 {
		t.Errorf("전체 개수 = %d, 기대 12", page.TableCount)
	}
	if page.NextOffset == 0 {
		t.Fatalf("한 번에 다 담겼습니다(%d개). 나눠 담기를 검사할 수 없습니다", len(page.Tables))
	}
	if len(page.Tables) != page.NextOffset {
		t.Errorf("담은 개수 %d 와 nextOffset %d 이 어긋납니다", len(page.Tables), page.NextOffset)
	}

	// 이어서 부르면 나머지가 온다.
	rest, err := run("erd_read_schema",
		fmt.Sprintf(`{"document":"큰 초안","offset":%d}`, page.NextOffset))
	if err != nil {
		t.Fatalf("이어 읽기: %v", err)
	}
	var page2 struct {
		Offset int `json:"offset"`
		Tables []struct {
			Name string `json:"name"`
		} `json:"tables"`
	}
	if err := json.Unmarshal([]byte(rest), &page2); err != nil {
		t.Fatalf("두 번째 쪽이 온전한 JSON이 아닙니다: %v", err)
	}
	if page2.Offset != page.NextOffset {
		t.Errorf("이어 읽은 자리 = %d, 기대 %d", page2.Offset, page.NextOffset)
	}
	if len(page2.Tables) == 0 {
		t.Error("이어 읽었는데 아무것도 오지 않았습니다")
	}

	// 표 하나만 짚어 읽을 수도 있어야 한다.
	one, err := run("erd_read_schema", `{"document":"큰 초안","table":"table_with_a_fairly_long_name_a"}`)
	if err != nil {
		t.Fatalf("한 표 읽기: %v", err)
	}
	var single struct {
		Tables []struct {
			Name string `json:"name"`
		} `json:"tables"`
	}
	if err := json.Unmarshal([]byte(one), &single); err != nil {
		t.Fatalf("한 표 결과가 JSON이 아닙니다: %v", err)
	}
	if len(single.Tables) != 1 {
		t.Errorf("한 표를 달라고 했는데 %d개가 왔습니다", len(single.Tables))
	}
}

// 잘린 답은 잘렸다고 말해야 한다.
func TestStopNoticeExplainsCutOff(t *testing.T) {
	if stopNotice("stop") != "" || stopNotice("tool_calls") != "" || stopNotice("") != "" {
		t.Error("정상 종료에 안내가 붙었습니다")
	}
	if !strings.Contains(stopNotice("length"), "끊겼습니다") {
		t.Errorf("길이 한계 안내 = %q", stopNotice("length"))
	}
	if !strings.Contains(stopNotice("incomplete"), "연결이 끊겼습니다") {
		t.Errorf("연결 끊김 안내 = %q", stopNotice("incomplete"))
	}
}
