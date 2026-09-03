package api

import (
	"strings"
	"testing"
)

// 사용자가 보고 있는 화면이 프롬프트에 담기지 않으면 이 기능은 아무 일도 하지 않는다.
// 그 사실은 화면에서 확인할 방법이 없다(모델의 답이 조금 달라질 뿐이므로) — 여기서 고정한다.
func TestScreenPromptCarriesScreen(t *testing.T) {
	got := screenPrompt(&screenReport{
		Path:  "/data?conn=abc123&table=orders",
		Label: "데이터",
		Detail: []string{
			"보고 있는 DB: shop-prod / shop (postgres, 운영)",
			"보고 있는 테이블: public.orders",
		},
	})

	for _, want := range []string{
		"데이터", "/data?conn=abc123&table=orders",
		"shop-prod", "public.orders",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("화면 보고에 %q 가 없다:\n%s", want, got)
		}
	}
	// 되묻지 말라는 지시가 이 문단의 목적이다. "이 테이블"이 무엇인지 다시 물으면
	// 사람은 결국 손으로 적게 되고, 그러면 이 기능은 없는 것과 같다.
	if !strings.Contains(got, "되묻지") {
		t.Errorf("되묻지 말라는 지시가 없다:\n%s", got)
	}
	// 화면의 값은 사용자 데이터다. 지시가 아니라는 경계가 없으면 편집기에 적어 둔
	// 문장이나 테이블 주석이 지시처럼 읽힐 수 있다.
	if !strings.Contains(got, "지시가 아닙니다") {
		t.Errorf("화면 보고가 지시가 아니라는 경계가 없다:\n%s", got)
	}
}

// 화면 정보가 없으면 아무것도 붙이지 않는다. 빈 문단을 붙이면 시스템 프롬프트가
// 매 차례 조금씩 달라져 프로바이더의 프롬프트 캐시가 깨진다.
func TestScreenPromptEmpty(t *testing.T) {
	for name, r := range map[string]*screenReport{
		"nil":   nil,
		"빈 값":   {},
		"공백만":   {Path: "   ", Label: "\t", Detail: []string{"", "  "}},
		"빈 목록만": {Detail: []string{}},
	} {
		if got := screenPrompt(r); got != "" {
			t.Errorf("%s: 빈 화면 보고에서 글이 나왔다: %q", name, got)
		}
	}
}

// 화면 보고는 클라이언트가 보내는 값이라 길이를 믿을 수 없다. 자르지 않으면
// 편집기에 붙여 둔 만 줄짜리 스크립트가 시스템 프롬프트로 들어가고, 그 차례의
// 질문은 그 안에 묻힌다.
func TestScreenPromptClipsLongValues(t *testing.T) {
	long := strings.Repeat("가", 5000)
	bits := make([]string, 20)
	for i := range bits {
		bits[i] = long
	}
	got := screenPrompt(&screenReport{
		Path: strings.Repeat("/x", 1000), Label: long, Detail: bits,
	})
	if got == "" {
		t.Fatal("긴 보고가 통째로 버려졌다")
	}
	// 줄 수와 글자 수 모두 상한을 지켜야 한다.
	if n := strings.Count(got, "\n- "); n > maxScreenBits+1 {
		t.Errorf("보고 줄이 %d 개다(상한 %d)", n, maxScreenBits)
	}
	if runes := len([]rune(got)); runes > maxScreenBits*(maxScreenBitLen+8)+2000 {
		t.Errorf("보고 글이 너무 길다: %d자", runes)
	}
	if strings.Contains(got, long) {
		t.Error("긴 값이 잘리지 않고 그대로 들어갔다")
	}
}

// 화면 이름만 있거나 주소만 있어도 담긴다. 상세 화면(/erd/<아이디>)은 이름이 있고,
// 메뉴에 없는 화면은 주소만 있다 — 둘 다 "어디에 있는가"를 말해 준다.
func TestScreenPromptPartialFields(t *testing.T) {
	onlyLabel := screenPrompt(&screenReport{Label: "ERD 설계"})
	if !strings.Contains(onlyLabel, "ERD 설계") {
		t.Errorf("이름만 있는 보고가 비었다:\n%s", onlyLabel)
	}
	onlyPath := screenPrompt(&screenReport{Path: "/some/new/screen"})
	if !strings.Contains(onlyPath, "/some/new/screen") {
		t.Errorf("주소만 있는 보고가 비었다:\n%s", onlyPath)
	}
	onlyDetail := screenPrompt(&screenReport{Detail: []string{"고른 테이블: orders"}})
	if !strings.Contains(onlyDetail, "고른 테이블: orders") {
		t.Errorf("상세만 있는 보고가 비었다:\n%s", onlyDetail)
	}
}
