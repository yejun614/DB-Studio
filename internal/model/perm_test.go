package model

import (
	"slices"
	"testing"
)

// 저장 형식은 왕복해야 한다. 문자열 한 칸에 담기로 한 이상,
// 읽고 쓰는 과정에서 능력이 늘거나 줄면 권한이 조용히 바뀐다.
func TestCapsRoundTrip(t *testing.T) {
	in := []Capability{CapSQLRun, CapDataRead}
	out := CapsFromString(CapsToString(in))
	if len(out) != 2 || !slices.Contains(out, CapDataRead) || !slices.Contains(out, CapSQLRun) {
		t.Fatalf("왕복 실패: %v", out)
	}
	// 순서를 고정하므로 같은 집합은 같은 문자열이 된다.
	// 그러지 않으면 감사 로그의 두 줄을 눈으로 비교할 수 없다.
	if CapsToString(in) != CapsToString([]Capability{CapDataRead, CapSQLRun}) {
		t.Error("같은 집합은 같은 문자열이어야 한다")
	}
}

// 능력을 없앤 뒤에도 DB에 남아 있는 문자열이 판정에 끼어들면 안 된다.
func TestCapsFromStringDropsUnknown(t *testing.T) {
	got := CapsFromString("data.read,legacy.superpower,,data.write")
	if len(got) != 2 {
		t.Fatalf("모르는 값은 버려야 한다: %v", got)
	}
	if !slices.Contains(got, CapDataRead) || !slices.Contains(got, CapDataWrite) {
		t.Errorf("아는 값은 살아야 한다: %v", got)
	}
}

func TestCapsFromEmptyString(t *testing.T) {
	got := CapsFromString("")
	if len(got) != 0 {
		t.Errorf("빈 문자열은 빈 집합이어야 한다: %v", got)
	}
	// nil이 아니어야 JSON이 [] 로 나간다. null이면 화면의 .includes()가 터진다.
	if got == nil {
		t.Error("빈 슬라이스여야 한다(nil 아님)")
	}
}

func TestPermsRoundTrip(t *testing.T) {
	in := []Perm{PermScriptRun, PermMacro}
	out := PermsFromString(PermsToString(in))
	if len(out) != 2 {
		t.Fatalf("왕복 실패: %v", out)
	}
	if len(PermsFromString("nope")) != 0 {
		t.Error("모르는 권한은 버려야 한다")
	}
}

func TestHasPerm(t *testing.T) {
	member := &User{Role: RoleMember, Status: UserActive, Perms: []Perm{PermMacro}}
	if !member.HasPerm(PermMacro) {
		t.Error("부여된 권한을 가져야 한다")
	}
	if member.HasPerm(PermScriptRun) {
		t.Error("부여되지 않은 권한을 가지면 안 된다")
	}

	// 슈퍼 어드민은 스스로 부여할 수 있으므로 목록과 무관하게 전부 가진다.
	super := &User{Role: RoleSuperadmin, Status: UserActive}
	for _, p := range AllPerms() {
		if !super.HasPerm(p) {
			t.Errorf("슈퍼 어드민은 %s 를 가져야 한다", p)
		}
	}

	// 비활성 계정은 아무 권한도 없다. 계정을 잠갔는데 매크로가 도는 일이 있어서는 안 된다.
	disabled := &User{Role: RoleSuperadmin, Status: UserDisabled}
	if disabled.HasPerm(PermMacro) {
		t.Error("비활성 계정은 권한이 없어야 한다")
	}

	var nilUser *User
	if nilUser.HasPerm(PermMacro) {
		t.Error("nil 사용자는 권한이 없어야 한다")
	}
}

func TestCapabilityLabelsExistForAll(t *testing.T) {
	for _, c := range AllCapabilities() {
		if c.Label() == string(c) {
			t.Errorf("%s 에 한국어 이름이 없다", c)
		}
		if !c.Valid() {
			t.Errorf("%s 가 유효하지 않다고 나온다", c)
		}
	}
	for _, p := range AllPerms() {
		if p.Label() == string(p) {
			t.Errorf("%s 에 한국어 이름이 없다", p)
		}
	}
	if Capability("data.delete").Valid() {
		t.Error("정의되지 않은 능력은 거부해야 한다")
	}
}

// 업로드 아바타는 프로필 수정 API로 설정할 수 없어야 한다.
// 이미지가 없는 상태에서 avatar='upload'가 저장되면 화면이 깨진 이미지를 그린다.
func TestValidAvatarRejectsUploadSentinel(t *testing.T) {
	if ValidAvatar(AvatarUpload) {
		t.Error("upload는 프로필 수정으로 설정할 수 없어야 한다")
	}
	if !ValidAvatar("") {
		t.Error("빈 값(아이콘 없음)은 허용해야 한다")
	}
	if !ValidAvatar("role-dba") {
		t.Error("목록에 있는 키는 허용해야 한다")
	}
	if ValidAvatar("role-nonexistent") {
		t.Error("목록에 없는 키는 거부해야 한다")
	}
}

func TestValidAvatarMime(t *testing.T) {
	for _, m := range AvatarMimes() {
		if !ValidAvatarMime(m) {
			t.Errorf("%s 는 허용되어야 한다", m)
		}
	}
	// SVG는 스크립트를 품을 수 있는 문서 형식이므로 이미지로 받지 않는다.
	if ValidAvatarMime("image/svg+xml") {
		t.Error("SVG는 거부해야 한다")
	}
	if ValidAvatarMime("text/html") {
		t.Error("HTML은 거부해야 한다")
	}
}
