package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"dbstudio/internal/crypto"
)

func glossaryFixture(t *testing.T) (context.Context, *Store) {
	t.Helper()
	ctx := context.Background()
	box, err := crypto.NewSecretBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	st, err := Open(ctx, filepath.Join(t.TempDir(), "glossary.db"), box)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return ctx, st
}

// 같은 말이 두 번 오르면 사전이 아니다.
//
// 대소문자도 구분하지 않는다 — "Member"와 "member"가 따로 오르면, 찾는 사람은 둘 중
// 어느 쪽이 약속인지 알 수 없다.
func TestGlossaryTermIsUnique(t *testing.T) {
	ctx, st := glossaryFixture(t)

	if _, err := st.CreateGlossaryTerm(ctx, SaveGlossaryParams{
		Term: "회원", Physical: "member",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.CreateGlossaryTerm(ctx, SaveGlossaryParams{
		Term: "회원", Physical: "mbr",
	}); !errors.Is(err, ErrDuplicateTerm) {
		t.Errorf("같은 용어 = %v, ErrDuplicateTerm 이어야 합니다", err)
	}
	if _, err := st.CreateGlossaryTerm(ctx, SaveGlossaryParams{
		Term: "Member", Physical: "member",
	}); err != nil {
		t.Fatalf("영문 용어: %v", err)
	}
	if _, err := st.CreateGlossaryTerm(ctx, SaveGlossaryParams{
		Term: "MEMBER", Physical: "mbr",
	}); !errors.Is(err, ErrDuplicateTerm) {
		t.Errorf("대소문자만 다른 용어 = %v, 같은 것으로 봐야 합니다", err)
	}
}

// 물리명은 겹쳐도 된다.
//
// 뜻이 다른 두 말이 같은 약어를 쓰는 일은 실제로 있고, 그것을 정하는 것은 팀의
// 일이다. 저장 계층에서 막으면 사전에 적지 못한 채로 쓰게 된다 — 사전 밖의 약속이
// 생기는 것이 가장 나쁘다.
func TestGlossaryPhysicalMayRepeat(t *testing.T) {
	ctx, st := glossaryFixture(t)

	if _, err := st.CreateGlossaryTerm(ctx, SaveGlossaryParams{Term: "번호", Physical: "no"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.CreateGlossaryTerm(ctx, SaveGlossaryParams{Term: "아니오", Physical: "no"}); err != nil {
		t.Errorf("같은 물리명을 막았습니다: %v", err)
	}
}

// 찾기는 용어·물리명·설명 세 곳을 모두 본다.
//
// 사람은 "회원"으로도, "member"로도, "가입"으로도 찾는다. 어느 칸에서 찾을지 고르게
// 하면 그 고르개가 한 걸음이 되고, 사전은 찾기 귀찮은 것이 되면 아무도 보지 않는다.
func TestGlossarySearchLooksEverywhere(t *testing.T) {
	ctx, st := glossaryFixture(t)
	for _, p := range []SaveGlossaryParams{
		{Term: "회원", Physical: "member", Note: "가입한 사람"},
		{Term: "주문 일시", Physical: "order_dttm", Note: "결제 완료 시각이 아니다"},
		{Term: "번호", Physical: "no"},
	} {
		if _, err := st.CreateGlossaryTerm(ctx, p); err != nil {
			t.Fatalf("create %s: %v", p.Term, err)
		}
	}

	for _, tc := range []struct{ q, want string }{
		{"회원", "회원"},       // 용어로
		{"order", "주문 일시"}, // 물리명으로
		{"가입", "회원"},       // 설명으로
		{"MEMBER", "회원"},   // 대소문자 무시
	} {
		got, err := st.ListGlossary(ctx, tc.q, 0)
		if err != nil {
			t.Fatalf("list %q: %v", tc.q, err)
		}
		if len(got) != 1 || got[0].Term != tc.want {
			t.Errorf("%q 로 찾기 = %v, 기대 %q", tc.q, termNames(got), tc.want)
		}
	}

	// 사전은 찾아보는 것이라 가나다순이어야 한다. 최근 순이면 같은 말을 찾을 때마다
	// 자리가 달라진다.
	all, err := st.ListGlossary(ctx, "", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"번호", "주문 일시", "회원"}
	got := termNames(all)
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("차례 = %v, 기대 %v", got, want)
		}
	}
}

func TestGlossaryUpdateAndDelete(t *testing.T) {
	ctx, st := glossaryFixture(t)
	made, err := st.CreateGlossaryTerm(ctx, SaveGlossaryParams{Term: "회원", Physical: "member"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := st.UpdateGlossaryTerm(ctx, made.ID, SaveGlossaryParams{
		Term: "회원", Physical: "mbr", Note: "약어로 바꿈",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.Physical != "mbr" || got.Note != "약어로 바꿈" {
		t.Errorf("고친 뒤 = %+v", got)
	}

	if err := st.DeleteGlossaryTerm(ctx, made.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetGlossaryTerm(ctx, made.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("지운 뒤 조회 = %v", err)
	}
	if err := st.DeleteGlossaryTerm(ctx, made.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("두 번 지우기 = %v, 없다고 답해야 합니다", err)
	}
}

func termNames(terms []*GlossaryTerm) []string {
	out := make([]string, 0, len(terms))
	for _, t := range terms {
		out = append(out, t.Term)
	}
	return out
}

// 분류는 셋 다 비워 둘 수 있고, 찾기는 분류도 본다.
//
// 처음부터 분류 체계를 세우고 시작하는 팀은 없다. 필수로 만들면 아무 말이나 넣게
// 되고, 그렇게 들어간 분류는 없느니만 못하다.
func TestGlossaryCategoriesAreOptionalAndSearchable(t *testing.T) {
	ctx, st := glossaryFixture(t)
	for _, p := range []SaveGlossaryParams{
		{Term: "회원", Physical: "member", Cat1: "회원", Cat2: "기본"},
		{Term: "비밀번호", Physical: "password", Cat1: "회원", Cat2: "인증", Cat3: "자격"},
		{Term: "주문", Physical: "order", Cat1: "주문"},
		{Term: "번호", Physical: "no"}, // 분류 없음
	} {
		if _, err := st.CreateGlossaryTerm(ctx, p); err != nil {
			t.Fatalf("create %s: %v", p.Term, err)
		}
	}

	// 분류로도 찾힌다. "회원"으로 찾는 사람은 그 말 자체를 찾을 수도, 그 덩어리를
	// 찾을 수도 있다 — 어느 쪽인지 되묻지 않고 둘 다 보여준다.
	got, err := st.ListGlossary(ctx, "인증", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Term != "비밀번호" {
		t.Errorf("분류로 찾기 = %v", termNames(got))
	}

	// 분류가 있는 것이 먼저, 없는 것이 뒤로 간다. 뒤에 있는 것은 아직 자리를 못
	// 정한 것들이라 그 자체로 "정리할 것" 목록이 된다.
	all, err := st.ListGlossary(ctx, "", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// 분류 → 용어 순. 분류 없는 "번호"만 뒤로 간다.
	want := []string{"주문", "회원", "비밀번호", "번호"}
	if names := termNames(all); !equalNames(names, want) {
		t.Errorf("차례 = %v, 기대 %v", names, want)
	}
}

// 분류 목록은 실제로 쓰인 조합에서 모은다.
//
// 분류를 따로 표로 두지 않기 때문이다. 조합 그대로 돌려주어야 화면이 "이 대분류
// 아래에서 쓰인 중분류"를 제안할 수 있다.
func TestGlossaryCategoryListComesFromUse(t *testing.T) {
	ctx, st := glossaryFixture(t)
	for _, p := range []SaveGlossaryParams{
		{Term: "회원", Physical: "member", Cat1: "회원", Cat2: "기본"},
		{Term: "회원명", Physical: "member_nm", Cat1: "회원", Cat2: "기본"},
		{Term: "비밀번호", Physical: "password", Cat1: "회원", Cat2: "인증"},
		{Term: "번호", Physical: "no"},
	} {
		if _, err := st.CreateGlossaryTerm(ctx, p); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	cats, err := st.GlossaryCategories(ctx)
	if err != nil {
		t.Fatalf("categories: %v", err)
	}
	// 같은 조합은 한 번만, 분류가 아예 없는 용어는 목록에 없다.
	want := [][3]string{{"회원", "기본", ""}, {"회원", "인증", ""}}
	if len(cats) != len(want) {
		t.Fatalf("분류 조합 = %v, 기대 %v", cats, want)
	}
	for i := range want {
		if cats[i] != want[i] {
			t.Errorf("조합[%d] = %v, 기대 %v", i, cats[i], want[i])
		}
	}

	// 마지막 용어가 분류를 잃으면 그 조합도 목록에서 사라진다. 쓰이지 않는 분류를
	// 누가 치우는가는 물을 필요조차 없어야 한다.
	terms, _ := st.ListGlossary(ctx, "비밀번호", 0)
	if _, err := st.UpdateGlossaryTerm(ctx, terms[0].ID, SaveGlossaryParams{
		Term: "비밀번호", Physical: "password", Cat1: "회원", Cat2: "기본",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	cats, _ = st.GlossaryCategories(ctx)
	if len(cats) != 1 || cats[0] != [3]string{"회원", "기본", ""} {
		t.Errorf("고친 뒤 분류 = %v, 쓰이지 않는 조합이 남았습니다", cats)
	}
}

func equalNames(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
