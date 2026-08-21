package model

// 매크로 접근 제어.
//
// 커넥션 권한(Level/Capability)과 **다른 축**이다. 커넥션 권한은 관리자가 위에서
// 내려주는 것이고, 매크로 접근은 만든 사람이 스스로 정한다. 둘을 한 사다리에 합치면
// "내 매크로를 동료에게 보여주려면 관리자에게 요청해야 하는" 상태가 된다.
//
// 두 권한은 곱해진다. 매크로를 수정할 수 있다고 해서 그 안의 노드가 실행되는 것은
// 아니다 — 실행은 언제나 실행자의 커넥션 권한으로 노드마다 다시 판정된다
// (macro.Engine.Blockers). 여기서 정하는 것은 "이 매크로에 손댈 수 있는가"뿐이다.

// MacroVisibility는 매크로(또는 전역 커스텀 노드)의 공개 범위다.
type MacroVisibility string

const (
	// MacroPrivate는 작성자와 협업자만 볼 수 있다. 새로 만드는 것의 기본값이다.
	MacroPrivate MacroVisibility = "private"
	// MacroPublic은 매크로 권한이 있는 모든 사용자가 볼 수 있다.
	// 매크로 권한 자체가 없는 사람에게는 여전히 보이지 않는다 — 공개는 앱 안에서의
	// 공개이지 익명 공개가 아니다.
	MacroPublic MacroVisibility = "public"
)

func (v MacroVisibility) Valid() bool { return v == MacroPrivate || v == MacroPublic }

// MacroPublicAccess는 공개했을 때 남들이 무엇까지 할 수 있는지다.
//
// 공개가 곧 수정 허용이 아닌 이유: 공유하는 이유의 대부분은 "돌려 쓰라"이지
// "고쳐도 된다"가 아니다. 둘을 한 스위치로 묶으면 남에게 보여주기 위해 수정까지
// 열어야 한다.
type MacroPublicAccess string

const (
	MacroPublicView MacroPublicAccess = "view" // 조회 + 실행
	MacroPublicEdit MacroPublicAccess = "edit" // + 수정(새 버전 저장, 이름/설명 변경)
)

func (p MacroPublicAccess) Valid() bool { return p == MacroPublicView || p == MacroPublicEdit }

// MacroAccess는 어떤 사용자가 어떤 매크로에 대해 가지는 권한이다.
//
// 포함 관계를 가지는 한 줄 사다리로 둔 이유: 판정이 필요한 곳마다 "무엇을 할 수
// 있는가"를 조합으로 따지면 어딘가는 반드시 빠뜨린다. 비교 한 번으로 끝나야 한다.
type MacroAccess int

const (
	MacroAccessNone   MacroAccess = iota // 존재조차 보이지 않는다
	MacroAccessView                      // 조회 + 실행 + 실행 이력 열람
	MacroAccessEdit                      // + 새 버전 저장, 되돌리기, 이름/설명 변경
	MacroAccessManage                    // + 공개 설정, 협업자, 자동 실행 트리거
	MacroAccessOwn                       // + 삭제
)

func (a MacroAccess) CanView() bool   { return a >= MacroAccessView }
func (a MacroAccess) CanRun() bool    { return a >= MacroAccessView }
func (a MacroAccess) CanEdit() bool   { return a >= MacroAccessEdit }
func (a MacroAccess) CanManage() bool { return a >= MacroAccessManage }
func (a MacroAccess) CanDelete() bool { return a >= MacroAccessOwn }

// String은 화면과 감사 로그가 쓰는 이름이다. JSON으로도 이 값이 나간다 —
// 정수를 내보내면 프런트에서 4가 무엇인지 알아야 한다.
func (a MacroAccess) String() string {
	switch {
	case a >= MacroAccessOwn:
		return "owner"
	case a >= MacroAccessManage:
		return "manage"
	case a >= MacroAccessEdit:
		return "edit"
	case a >= MacroAccessView:
		return "view"
	}
	return "none"
}

func (a MacroAccess) MarshalJSON() ([]byte, error) {
	return []byte(`"` + a.String() + `"`), nil
}

// MacroOwnership은 판정에 필요한 매크로 쪽 정보다.
// 매크로와 커스텀 노드가 같은 규칙을 쓰므로 둘 다 이것으로 환산해 넘긴다.
type MacroOwnership struct {
	CreatedBy    string
	Visibility   MacroVisibility
	PublicAccess MacroPublicAccess
	// IsCollaborator는 판정 대상 사용자가 협업자 목록에 있는지다.
	IsCollaborator bool
}

// ResolveMacroAccess는 사용자 한 명의 권한을 계산한다.
//
// 여러 자격이 겹칠 때는 **가장 높은 것**을 준다. 작성자가 자기 매크로를 공개+조회로
// 두었다고 자기 수정 권한이 사라지면 안 되고, 협업자로 지정된 사람이 공개 설정을
// 바꿨다고 자기가 밀려나서도 안 된다.
func ResolveMacroAccess(u *User, o MacroOwnership) MacroAccess {
	// 매크로 메뉴 권한이 먼저다. 이것이 없으면 공개 매크로도 보이지 않는다.
	// (HasPerm은 비활성 계정과 nil을 함께 걸러낸다.)
	if !u.HasPerm(PermMacro) {
		return MacroAccessNone
	}
	// 슈퍼어드민은 모든 매크로를 조회·수정·관리·삭제한다.
	if u.Role == RoleSuperadmin {
		return MacroAccessOwn
	}

	access := MacroAccessNone
	// 작성자가 비어 있는 매크로(계정이 삭제되어 created_by가 NULL이 된 경우)는
	// 주인이 없다. u.ID와의 우연한 일치로 소유권이 넘어가지 않도록 막는다.
	if o.CreatedBy != "" && o.CreatedBy == u.ID {
		access = MacroAccessOwn
	}
	if o.IsCollaborator && access < MacroAccessManage {
		access = MacroAccessManage
	}
	if o.Visibility == MacroPublic {
		public := MacroAccessView
		if o.PublicAccess == MacroPublicEdit {
			public = MacroAccessEdit
		}
		if access < public {
			access = public
		}
	}
	return access
}
