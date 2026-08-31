package api

import (
	"testing"
)

// 사전은 누구나 보고, 커넥션 관리자만 고친다.
//
// 팀의 약속이라 아무나 바꾸면 약속이 아니게 되지만, 아무나 볼 수 없으면 지킬 수도
// 없다 — 설계하는 사람이 찾아보는 것이 이 표의 유일한 쓸모다.
func TestGlossaryReadAllWriteManagers(t *testing.T) {
	e := newTestEnv(t)
	member(t, e, "dana", "", "")

	alice := loginAs(t, e, "alice") // 슈퍼 어드민
	if code, body := alice.do("POST", "/api/v1/glossary/",
		map[string]any{"term": "회원", "physical": "member"}); code != 201 {
		t.Fatalf("관리자 추가 = %d: %v", code, body)
	}

	dana := loginAs(t, e, "dana")
	code, body := dana.do("GET", "/api/v1/glossary/", nil)
	if code != 200 {
		t.Fatalf("일반 사용자 읽기 = %d: %v", code, body)
	}
	terms, _ := body["terms"].([]any)
	if len(terms) != 1 {
		t.Errorf("읽은 용어 = %d개, 1개여야 합니다", len(terms))
	}
	// 화면이 단추를 그릴지 판단할 수 있어야 한다. 눌러 보고서야 거부되는 단추는
	// "왜 안 되지"를 남기고, 그 답은 화면 어디에도 없다.
	if body["canManage"] != false {
		t.Errorf("canManage = %v, 일반 사용자에게는 false 여야 합니다", body["canManage"])
	}

	if code, _ := dana.do("POST", "/api/v1/glossary/",
		map[string]any{"term": "주문", "physical": "order"}); code != 403 {
		t.Errorf("일반 사용자 추가 = %d, 403이어야 합니다", code)
	}
	if code, _ := dana.do("POST", "/api/v1/glossary/bulk",
		map[string]any{"text": "주문, order"}); code != 403 {
		t.Errorf("일반 사용자 여러 줄 = %d, 403이어야 합니다", code)
	}
}

// 같은 말을 두 번 올리면 409로 답한다. 조용히 덮어쓰면 앞사람의 약속이 사라진다.
func TestGlossaryRejectsDuplicate(t *testing.T) {
	e := newTestEnv(t)
	alice := loginAs(t, e, "alice")

	if code, _ := alice.do("POST", "/api/v1/glossary/",
		map[string]any{"term": "회원", "physical": "member"}); code != 201 {
		t.Fatal("첫 추가가 실패했습니다")
	}
	code, body := alice.do("POST", "/api/v1/glossary/",
		map[string]any{"term": "  회원  ", "physical": "mbr"})
	if code != 409 {
		t.Errorf("같은 용어 = %d, 409여야 합니다: %v", code, body)
	}
	if body["error"] != "duplicate_term" {
		t.Errorf("사유 = %v", body["error"])
	}
}

// 물리명 없는 용어는 받지 않는다. 그것이 없으면 사전이 답하지 못한다.
func TestGlossaryNeedsBothNames(t *testing.T) {
	e := newTestEnv(t)
	alice := loginAs(t, e, "alice")

	for _, tc := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"용어 없음", map[string]any{"term": " ", "physical": "member"}, "invalid_term"},
		{"물리명 없음", map[string]any{"term": "회원", "physical": " "}, "invalid_physical"},
	} {
		code, body := alice.do("POST", "/api/v1/glossary/", tc.body)
		if code != 400 {
			t.Errorf("%s = %d, 400이어야 합니다", tc.name, code)
		}
		if body["error"] != tc.want {
			t.Errorf("%s 사유 = %v, 기대 %q", tc.name, body["error"], tc.want)
		}
	}
}

// 여러 줄 올리기: 이미 있는 말은 건너뛰고 계속 간다.
//
// 목록의 절반이 이미 들어 있는 것이 보통이다. 거기서 멈추면 사람이 그 줄을 지우고
// 다시 붙여넣는 일을 반복하게 된다.
func TestGlossaryBulkSkipsExisting(t *testing.T) {
	e := newTestEnv(t)
	alice := loginAs(t, e, "alice")

	if code, _ := alice.do("POST", "/api/v1/glossary/",
		map[string]any{"term": "회원", "physical": "member"}); code != 201 {
		t.Fatal("준비 실패")
	}
	code, body := alice.do("POST", "/api/v1/glossary/bulk", map[string]any{
		"text": "# 주석\n주문, order\n회원, mbr\n주문 일시\torder_dttm\t결제 시각 아님\n망가진줄\n",
	})
	if code != 200 {
		t.Fatalf("여러 줄 = %d: %v", code, body)
	}
	added, _ := body["added"].([]any)
	skipped, _ := body["skipped"].([]any)
	invalid, _ := body["invalid"].([]any)
	if len(added) != 2 {
		t.Errorf("더한 수 = %d, 기대 2", len(added))
	}
	if len(skipped) != 1 {
		t.Errorf("건너뛴 수 = %d, 기대 1 (이미 있는 '회원')", len(skipped))
	}
	if len(invalid) != 1 {
		t.Errorf("형식 오류 = %d, 기대 1", len(invalid))
	}

	// 건너뛴 줄이 기존 항목을 덮어쓰지 않아야 한다.
	_, list := alice.do("GET", "/api/v1/glossary/?q=회원", nil)
	terms, _ := list["terms"].([]any)
	if len(terms) != 1 {
		t.Fatalf("회원 검색 = %d건", len(terms))
	}
	first, _ := terms[0].(map[string]any)
	if first["physical"] != "member" {
		t.Errorf("물리명 = %v, 건너뛴 줄이 덮어썼습니다", first["physical"])
	}
}

// 분류는 함께 저장되고, 목록은 쓰인 조합을 함께 돌려준다.
//
// 화면이 "이미 쓰이던 분류"를 제안하려면 조합이 필요하다 — 새 분류를 적을 수는
// 있지만, 있는 것을 다시 적어 "회원"과 "회원관리"를 따로 만들 이유는 없다.
func TestGlossaryCategoriesRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	alice := loginAs(t, e, "alice")

	code, body := alice.do("POST", "/api/v1/glossary/", map[string]any{
		"term": "비밀번호", "physical": "password",
		"cat1": " 회원 ", "cat2": "인증", "cat3": "",
	})
	if code != 201 {
		t.Fatalf("추가 = %d: %v", code, body)
	}
	term, _ := body["term"].(map[string]any)
	if term["cat1"] != "회원" || term["cat2"] != "인증" {
		t.Errorf("분류 = %v / %v", term["cat1"], term["cat2"])
	}
	// 빈 분류는 아예 나가지 않는다(omitempty). 화면이 빈 칸을 그리지 않도록.
	if _, has := term["cat3"]; has {
		t.Errorf("빈 소분류가 응답에 있습니다: %v", term["cat3"])
	}

	// 분류 없는 용어도 그대로 받는다.
	if code, body := alice.do("POST", "/api/v1/glossary/",
		map[string]any{"term": "번호", "physical": "no"}); code != 201 {
		t.Fatalf("분류 없는 추가 = %d: %v", code, body)
	}

	_, list := alice.do("GET", "/api/v1/glossary/", nil)
	cats, _ := list["categories"].([]any)
	if len(cats) != 1 {
		t.Fatalf("분류 조합 = %v, 쓰인 것 하나여야 합니다", list["categories"])
	}
	first, _ := cats[0].([]any)
	if len(first) != 3 || first[0] != "회원" || first[1] != "인증" {
		t.Errorf("조합 = %v", first)
	}

	// 분류로도 찾힌다.
	_, found := alice.do("GET", "/api/v1/glossary/?q=인증", nil)
	terms, _ := found["terms"].([]any)
	if len(terms) != 1 {
		t.Errorf("분류로 찾기 = %d건", len(terms))
	}
}

// 여러 줄 올리기도 분류까지 받는다.
//
// 사전을 처음 들이는 팀의 엑셀에는 대개 분류 칸이 이미 있다. 그것을 버리게 하면
// 분류를 손으로 다시 넣어야 하고, 그러면 아무도 넣지 않는다.
func TestGlossaryBulkTakesCategories(t *testing.T) {
	e := newTestEnv(t)
	alice := loginAs(t, e, "alice")

	code, body := alice.do("POST", "/api/v1/glossary/bulk", map[string]any{
		"text": "비밀번호\tpassword\t로그인 자격\t회원\t인증\t자격\n주문, order, , 주문\n번호, no\n",
	})
	if code != 200 {
		t.Fatalf("여러 줄 = %d: %v", code, body)
	}
	if added, _ := body["added"].([]any); len(added) != 3 {
		t.Fatalf("더한 수 = %d, 기대 3: %v", len(added), body)
	}

	_, list := alice.do("GET", "/api/v1/glossary/?q=password", nil)
	terms, _ := list["terms"].([]any)
	if len(terms) != 1 {
		t.Fatalf("찾기 = %d건", len(terms))
	}
	got, _ := terms[0].(map[string]any)
	if got["cat1"] != "회원" || got["cat2"] != "인증" || got["cat3"] != "자격" {
		t.Errorf("탭 구분 분류 = %v", got)
	}
	if got["note"] != "로그인 자격" {
		t.Errorf("설명 = %v", got["note"])
	}

	_, one := alice.do("GET", "/api/v1/glossary/?q=order", nil)
	rows, _ := one["terms"].([]any)
	row, _ := rows[0].(map[string]any)
	if row["cat1"] != "주문" {
		t.Errorf("쉼표 구분 분류 = %v", row)
	}
	// 빈 설명 칸은 빈 채로 남는다 — 자리를 비워 둔 것이지 건너뛴 것이 아니다.
	if _, has := row["note"]; has {
		t.Errorf("빈 설명이 값으로 들어갔습니다: %v", row["note"])
	}
}
