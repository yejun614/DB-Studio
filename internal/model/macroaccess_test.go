package model

import "testing"

func member(id string, perms ...Perm) *User {
	return &User{ID: id, Username: id, Role: RoleMember, Status: UserActive, Perms: perms}
}

// 판정 사다리 전체를 한 표로 확인한다.
//
// 이 규칙은 저장 계층·API·엔진·AI 툴이 모두 참조하는 유일한 정의다.
// 한 줄이라도 조용히 바뀌면 어딘가에서 남의 비공개 매크로가 보이게 된다.
func TestResolveMacroAccess(t *testing.T) {
	owner := member("owner", PermMacro)
	other := member("other", PermMacro)
	collab := member("collab", PermMacro)
	noPerm := member("nomacro")
	admin := &User{ID: "root", Role: RoleSuperadmin, Status: UserActive}
	disabled := &User{ID: "owner", Role: RoleMember, Status: UserDisabled, Perms: []Perm{PermMacro}}

	privateOwned := MacroOwnership{CreatedBy: "owner", Visibility: MacroPrivate, PublicAccess: MacroPublicView}
	publicView := MacroOwnership{CreatedBy: "owner", Visibility: MacroPublic, PublicAccess: MacroPublicView}
	publicEdit := MacroOwnership{CreatedBy: "owner", Visibility: MacroPublic, PublicAccess: MacroPublicEdit}

	cases := []struct {
		name  string
		user  *User
		owned MacroOwnership
		want  MacroAccess
	}{
		{"만든 사람은 비공개여도 전부 할 수 있다", owner, privateOwned, MacroAccessOwn},
		{"남은 비공개 매크로를 볼 수 없다", other, privateOwned, MacroAccessNone},
		{"공개(조회)는 조회·실행까지", other, publicView, MacroAccessView},
		{"공개(수정)는 수정까지, 관리는 아니다", other, publicEdit, MacroAccessEdit},
		{"슈퍼어드민은 비공개도 전부", admin, privateOwned, MacroAccessOwn},
		{"매크로 권한이 없으면 공개도 안 보인다", noPerm, publicEdit, MacroAccessNone},
		{"비활성 계정은 자기 매크로도 못 본다", disabled, privateOwned, MacroAccessNone},
		{"nil 사용자", nil, publicEdit, MacroAccessNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveMacroAccess(tc.user, tc.owned); got != tc.want {
				t.Errorf("= %v, want %v", got, tc.want)
			}
		})
	}

	// 협업자는 관리까지 하고 삭제만 못 한다.
	c := privateOwned
	c.IsCollaborator = true
	got := ResolveMacroAccess(collab, c)
	if got != MacroAccessManage {
		t.Errorf("협업자 = %v, want manage", got)
	}
	if got.CanDelete() {
		t.Error("협업자는 삭제할 수 없어야 한다")
	}
	if !got.CanManage() || !got.CanEdit() || !got.CanRun() {
		t.Error("협업자는 관리·수정·실행을 할 수 있어야 한다")
	}
}

// 여러 자격이 겹치면 가장 높은 것을 준다.
//
// 이것이 깨지는 방식은 조용하다: 자기 매크로를 공개+조회로 열어 둔 사람이
// 자기 매크로를 못 고치게 되고, 아무도 그것을 정책이 아니라 버그로 신고하지 않는다.
func TestResolveMacroAccessTakesHighest(t *testing.T) {
	owner := member("owner", PermMacro)
	o := MacroOwnership{CreatedBy: "owner", Visibility: MacroPublic, PublicAccess: MacroPublicView}
	if got := ResolveMacroAccess(owner, o); got != MacroAccessOwn {
		t.Errorf("공개(조회)로 열어도 만든 사람은 owner여야 한다: %v", got)
	}

	// 협업자로도 지정된 사람이 공개(수정)에 밀려 내려가면 안 된다.
	collab := member("collab", PermMacro)
	o2 := MacroOwnership{
		CreatedBy: "owner", Visibility: MacroPublic, PublicAccess: MacroPublicEdit,
		IsCollaborator: true,
	}
	if got := ResolveMacroAccess(collab, o2); got != MacroAccessManage {
		t.Errorf("협업자 = %v, want manage", got)
	}
}

// 작성자 계정이 삭제되면 created_by가 NULL이 된다. 그 빈 값이 빈 ID와 우연히
// 일치해 소유권이 넘어가서는 안 된다.
func TestOrphanMacroHasNoOwner(t *testing.T) {
	ghost := &User{ID: "", Role: RoleMember, Status: UserActive, Perms: []Perm{PermMacro}}
	o := MacroOwnership{CreatedBy: "", Visibility: MacroPrivate}
	if got := ResolveMacroAccess(ghost, o); got != MacroAccessNone {
		t.Errorf("주인 없는 비공개 매크로 = %v, want none", got)
	}
}

// Access는 JSON으로 문자열이 되어야 한다. 정수로 나가면 프런트가 4가 무엇인지
// 알아야 하고, 사다리 순서가 두 곳에 존재하게 된다.
func TestMacroAccessJSON(t *testing.T) {
	cases := map[MacroAccess]string{
		MacroAccessNone:   `"none"`,
		MacroAccessView:   `"view"`,
		MacroAccessEdit:   `"edit"`,
		MacroAccessManage: `"manage"`,
		MacroAccessOwn:    `"owner"`,
	}
	for access, want := range cases {
		b, err := access.MarshalJSON()
		if err != nil {
			t.Fatalf("MarshalJSON: %v", err)
		}
		if string(b) != want {
			t.Errorf("%d → %s, want %s", access, b, want)
		}
	}
}
