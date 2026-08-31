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
