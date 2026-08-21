package model

import "slices"

// 프로필 아이콘.
//
// 기본은 **미리 그려둔 라인 아이콘 중 하나를 고르는 것**이다. 저장되는 것은 아이콘 키
// 문자열 하나뿐이고, 그림은 프론트엔드가 인라인 SVG로 그린다. 서버는 목록과
// 검증만 갖는다 — 목록이 서버에 있어야 화면과 API가 같은 판단을 한다.
//
// 여기에 더해 **직접 올린 이미지**를 쓸 수 있다(avatar = AvatarUpload). 처음에는
// 업로드를 넣지 않았는데, 그 이유였던 "저장소·용량 제한·디코딩·삭제 정책이 딸려 온다"는
// 사실 자체는 지금도 맞다. 달라진 것은 그 비용을 어디에 두느냐다:
//
//   - 바이트는 메타 DB에 넣는다. 파일로 떨어뜨리면 단일 바이너리 원칙이 깨지지만,
//     BLOB 한 행은 백업·삭제(FK CASCADE)·권한이 나머지 데이터와 같은 규칙을 따른다.
//   - 크기 상한(-avatar-max-kb)과 MIME 화이트리스트를 두고, 확장자나 Content-Type을
//     믿지 않고 **실제 바이트를 디코딩해** 확인한다.
//   - SVG는 받지 않는다. SVG는 스크립트를 품을 수 있는 문서 형식이라 이미지가 아니라
//     실행 가능한 마크업이고, 그것을 사용자 입력으로 받아 다른 사람에게 보여주는 것은
//     저장형 XSS와 같은 말이다.
type AvatarOption struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Group string `json:"group"`
}

// AvatarUpload는 "업로드한 이미지를 쓴다"는 표시다.
// 실제 바이트는 user_avatars 테이블에 있고, 화면은 /api/v1/users/:id/avatar 를 그린다.
const AvatarUpload = "upload"

// AvatarMimes는 받아들이는 이미지 형식이다. 화면의 accept 속성과 서버 검증이 같은
// 목록을 쓰도록 /meta로 함께 내보낸다.
func AvatarMimes() []string {
	return []string{"image/png", "image/jpeg", "image/gif", "image/webp"}
}

func ValidAvatarMime(mime string) bool { return slices.Contains(AvatarMimes(), mime) }

// 아바타 그룹. 직무는 "무슨 일을 하는 사람인지", 사람은 "누구인지"를 고른다.
// 두 축을 섞지 않는 이유: 목록이 20개를 넘어가면 고르는 일이 일이 되고,
// 같은 성격의 것끼리 모아 두면 훑어보는 것으로 끝난다.
const (
	AvatarGroupRole   = "role"
	AvatarGroupPerson = "person"
)

// AvatarGroups는 그룹의 표시 순서와 라벨이다.
func AvatarGroups() []AvatarOption {
	return []AvatarOption{
		{Key: AvatarGroupRole, Label: "직무"},
		{Key: AvatarGroupPerson, Label: "사람"},
	}
}

var avatars = []AvatarOption{
	{Key: "role-dba", Label: "DBA", Group: AvatarGroupRole},
	{Key: "role-dev", Label: "개발자", Group: AvatarGroupRole},
	{Key: "role-analyst", Label: "분석가", Group: AvatarGroupRole},
	{Key: "role-ops", Label: "운영·SRE", Group: AvatarGroupRole},
	{Key: "role-architect", Label: "아키텍트", Group: AvatarGroupRole},
	{Key: "role-security", Label: "보안", Group: AvatarGroupRole},
	{Key: "role-support", Label: "지원", Group: AvatarGroupRole},
	{Key: "role-manager", Label: "기획·관리", Group: AvatarGroupRole},

	{Key: "person-plain", Label: "기본", Group: AvatarGroupPerson},
	{Key: "person-glasses", Label: "안경", Group: AvatarGroupPerson},
	{Key: "person-beard", Label: "수염", Group: AvatarGroupPerson},
	{Key: "person-cap", Label: "모자", Group: AvatarGroupPerson},
	{Key: "person-long-hair", Label: "긴 머리", Group: AvatarGroupPerson},
	{Key: "person-curly", Label: "곱슬머리", Group: AvatarGroupPerson},
	{Key: "person-bun", Label: "묶은 머리", Group: AvatarGroupPerson},
	{Key: "person-smile", Label: "웃는 얼굴", Group: AvatarGroupPerson},
}

// Avatars는 선택 가능한 아이콘 목록을 표시 순서대로 반환한다.
func Avatars() []AvatarOption {
	out := make([]AvatarOption, len(avatars))
	copy(out, avatars)
	return out
}

// ValidAvatar는 저장 가능한 값인지 판단한다. 빈 문자열은 "아이콘 없음"(이니셜 표시)이며
// 항상 허용한다 — 고르지 않을 자유가 없으면 되돌릴 방법도 없다.
//
// AvatarUpload는 여기서 허용하지 않는다. 이미지가 없는 상태에서 avatar='upload'가
// 저장되면 화면이 깨진 이미지를 그리게 되므로, 이 값은 업로드 API가 바이트를 저장한
// 뒤에만 직접 설정한다(프로필 수정 API로는 설정할 수 없다).
func ValidAvatar(key string) bool {
	if key == "" {
		return true
	}
	for _, a := range avatars {
		if a.Key == key {
			return true
		}
	}
	return false
}
