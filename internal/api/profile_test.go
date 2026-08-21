package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func strptr(s string) *string { return &s }

// TestProfileParamsValidation은 자기 프로필 수정 입력 검증을 확인한다.
func TestProfileParamsValidation(t *testing.T) {
	long := strings.Repeat("가", 61)
	cases := []struct {
		name string
		req  updateProfileRequest
		code string
	}{
		{"이름 변경", updateProfileRequest{DisplayName: strptr("김디비")}, ""},
		{"이름 공백만", updateProfileRequest{DisplayName: strptr("   ")}, "invalid_display_name"},
		{"이름 61자", updateProfileRequest{DisplayName: strptr(long)}, "invalid_display_name"},
		{"이름 60자", updateProfileRequest{DisplayName: strptr(long[:len(long)-3])}, ""},
		{"이메일 정상", updateProfileRequest{Email: strptr("a@example.com")}, ""},
		{"이메일 비우기", updateProfileRequest{Email: strptr("")}, ""},
		{"이메일 @ 없음", updateProfileRequest{Email: strptr("nobody")}, "invalid_email"},
		{"이메일 @ 두 개", updateProfileRequest{Email: strptr("a@b@c")}, "invalid_email"},
		{"이메일 공백 포함", updateProfileRequest{Email: strptr("a b@c.com")}, "invalid_email"},
		{"아이콘 선택", updateProfileRequest{Avatar: strptr("role-dba")}, ""},
		{"아이콘 해제", updateProfileRequest{Avatar: strptr("")}, ""},
		{"없는 아이콘", updateProfileRequest{Avatar: strptr("role-nope")}, "invalid_avatar"},
		{"빈 요청", updateProfileRequest{}, "no_changes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, code, msg := profileParams(tc.req)
			if code != tc.code {
				t.Fatalf("code = %q, 기대 %q", code, tc.code)
			}
			if code != "" && msg == "" {
				t.Error("거부했는데 사용자에게 보여줄 이유가 비었습니다")
			}
		})
	}
}

// TestProfileParamsTrims는 앞뒤 공백을 제거해 저장하는지 확인한다.
// 공백이 남으면 목록 정렬과 검색이 사람 눈에 이상하게 보인다.
func TestProfileParamsTrims(t *testing.T) {
	p, code, _ := profileParams(updateProfileRequest{
		DisplayName: strptr("  김디비  "), Email: strptr(" a@example.com "),
	})
	if code != "" {
		t.Fatalf("거부됨: %s", code)
	}
	if *p.DisplayName != "김디비" {
		t.Errorf("displayName = %q", *p.DisplayName)
	}
	if *p.Email != "a@example.com" {
		t.Errorf("email = %q", *p.Email)
	}
}

// TestProfileRequestIgnoresPrivilegeFields는 프로필 API로 권한을 올릴 수 없음을 확인한다.
// 요청 본문에 role·status를 넣어도 구조체에 자리가 없어 파싱 단계에서 사라진다.
// 이 테스트가 깨진다면 누군가 편의를 위해 필드를 추가한 것이며, 그 순간
// 멤버가 스스로 슈퍼 어드민이 될 수 있다.
func TestProfileRequestIgnoresPrivilegeFields(t *testing.T) {
	body := `{"role":"superadmin","status":"disabled","mustChangePassword":false}`
	var req updateProfileRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.DisplayName != nil || req.Email != nil || req.Avatar != nil {
		t.Fatal("권한 필드가 프로필 필드로 흘러들어왔습니다")
	}
	if _, code, _ := profileParams(req); code != "no_changes" {
		t.Errorf("code = %q, 기대 no_changes", code)
	}
}

// TestChangedProfileFields는 감사 로그에 남길 변경 필드 목록을 확인한다.
func TestChangedProfileFields(t *testing.T) {
	p, _, _ := profileParams(updateProfileRequest{
		DisplayName: strptr("김디비"), Avatar: strptr("person-glasses"),
	})
	got := strings.Join(changedProfileFields(p), ",")
	if got != "displayName,avatar" {
		t.Errorf("fields = %q", got)
	}
}
