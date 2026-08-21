package model

import (
	"slices"
	"strings"
)

// 권한의 두 번째 축.
//
// 기존 Level(none < monitor < erd < migrate)은 **설계·운영 작업**의 깊이를 나타내는
// 한 줄짜리 사다리다. 그 사다리에 "데이터 조회"를 끼워 넣을 자리는 없다. 스키마를
// 읽는 것과 그 안의 값을 읽는 것은 위험의 종류가 다르고(개인정보는 구조가 아니라
// 값에 있다), "마이그레이션은 할 수 있지만 고객 데이터는 못 본다"나 그 반대가 모두
// 현실적인 요구이기 때문이다. 그래서 데이터 능력은 사다리가 아니라 **독립적인 깃발**이다.
//
// Level과 마찬가지로 커넥션 단위로 부여한다 — 개발 DB의 데이터는 마음대로 고쳐도
// 되지만 운영 DB는 조회만 되어야 하는 것이 보통이다.
type Capability string

const (
	CapDataRead  Capability = "data.read"  // 테이블/문서/키 값 조회와 검색
	CapDataWrite Capability = "data.write" // 행·문서·키 값의 추가/수정/삭제
	CapSQLRun    Capability = "sql.run"    // 임의 SQL(또는 Mongo/Redis 명령) 실행
)

// AllCapabilities는 부여 가능한 능력을 화면 표시 순서대로 반환한다.
func AllCapabilities() []Capability {
	return []Capability{CapDataRead, CapDataWrite, CapSQLRun}
}

func (c Capability) Valid() bool { return slices.Contains(AllCapabilities(), c) }

// CapabilityLabels는 화면과 감사 로그가 함께 쓰는 한국어 이름이다.
var CapabilityLabels = map[Capability]string{
	CapDataRead:  "데이터 조회",
	CapDataWrite: "데이터 수정",
	CapSQLRun:    "SQL 실행",
}

func (c Capability) Label() string {
	if l, ok := CapabilityLabels[c]; ok {
		return l
	}
	return string(c)
}

// 전역 권한.
//
// 커넥션에 매이지 않는 것들이다. 매크로는 여러 커넥션에 걸쳐 동작하고, 셸 스크립트는
// 애초에 DB와 무관하게 앱이 도는 기계에서 실행된다. 이런 것을 커넥션별로 부여하면
// "어느 커넥션의 권한으로 셸을 실행하는가" 같은 답할 수 없는 질문이 생긴다.
type Perm string

const (
	// PermMacro는 매크로 메뉴 접근 권한이다. 이 권한을 가진 사람은 모든 매크로를
	// 보고 수정할 수 있다(요구사항: 작성자에 관계없이 공유). 버전 관리가 있으므로
	// 잘못된 수정은 되돌릴 수 있고, 그래서 편집을 작성자로 제한하지 않는다.
	PermMacro Perm = "macro"
	// PermScriptRun은 매크로에서 bash/powershell 스크립트를 실행할 권한이다.
	// 서버가 -allow-shell로 켜져 있을 때만 의미가 있다(이중 게이트).
	PermScriptRun Perm = "script.run"
	// PermHTTPCall은 매크로에서 외부 HTTP를 호출할 권한이다.
	//
	// 셸보다 약하지만 별도 권한을 둔 이유: 이것이 있으면 DB에서 읽은 값을 임의의
	// 주소로 보낼 수 있다. 조회 권한과 결합하면 데이터 반출 통로가 되므로,
	// "매크로를 쓸 수 있다"와 "외부로 내보낼 수 있다"는 따로 판단해야 한다.
	PermHTTPCall Perm = "http.call"
)

func AllPerms() []Perm { return []Perm{PermMacro, PermScriptRun, PermHTTPCall} }

func (p Perm) Valid() bool { return slices.Contains(AllPerms(), p) }

var PermLabels = map[Perm]string{
	PermMacro:     "매크로 사용",
	PermScriptRun: "셸 스크립트 실행",
	PermHTTPCall:  "외부 API 호출",
}

func (p Perm) Label() string {
	if l, ok := PermLabels[p]; ok {
		return l
	}
	return string(p)
}

// ---------- 저장 형식 ----------
//
// 능력 집합은 콤마 구분 문자열 한 칸에 담는다. 능력마다 열을 만들면 능력을 추가할
// 때마다 마이그레이션이 필요하고, 별도 테이블로 빼면 정책을 통째로 교체하는 지금의
// 저장 방식(SetAccessPolicy)에 테이블이 하나 더 늘 뿐 얻는 것이 없다.

// CapsToString은 저장용 문자열을 만든다. 순서를 고정해 같은 집합이 같은 문자열이 되게 한다.
func CapsToString(caps []Capability) string {
	ordered := make([]string, 0, len(caps))
	for _, known := range AllCapabilities() {
		if slices.Contains(caps, known) {
			ordered = append(ordered, string(known))
		}
	}
	return strings.Join(ordered, ",")
}

// CapsFromString은 저장된 문자열을 집합으로 되돌린다. 모르는 값은 버린다 —
// 능력을 없앤 뒤에도 남아 있는 문자열이 판정에 끼어들면 안 된다.
func CapsFromString(s string) []Capability {
	out := []Capability{}
	for part := range strings.SplitSeq(s, ",") {
		c := Capability(strings.TrimSpace(part))
		if c.Valid() && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

func PermsToString(perms []Perm) string {
	ordered := make([]string, 0, len(perms))
	for _, known := range AllPerms() {
		if slices.Contains(perms, known) {
			ordered = append(ordered, string(known))
		}
	}
	return strings.Join(ordered, ",")
}

func PermsFromString(s string) []Perm {
	out := []Perm{}
	for part := range strings.SplitSeq(s, ",") {
		p := Perm(strings.TrimSpace(part))
		if p.Valid() && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// HasPerm은 사용자가 전역 권한을 가지는지 반환한다.
// 슈퍼 어드민은 언제나 전부 가진다 — 자기 권한을 스스로 부여할 수 있으므로
// 목록에서 빠져 있다고 막아 봐야 한 번 더 클릭하게 만들 뿐이다.
func (u *User) HasPerm(p Perm) bool {
	if u == nil || UserStatus(u.Status) == UserDisabled {
		return false
	}
	if u.Role == RoleSuperadmin {
		return true
	}
	return slices.Contains(u.Perms, p)
}
