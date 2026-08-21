package model

import "testing"

// TestAvatarsWellFormed는 목록 자체의 무결성을 확인한다.
// 키가 겹치거나 그룹이 잘못되면 선택 화면에서 항목이 사라지거나 두 번 나온다.
func TestAvatarsWellFormed(t *testing.T) {
	groups := map[string]bool{}
	for _, g := range AvatarGroups() {
		groups[g.Key] = true
	}

	seen := map[string]bool{}
	for _, a := range Avatars() {
		if a.Key == "" {
			t.Error("키가 빈 항목이 있습니다. 빈 키는 \"아이콘 없음\"과 구분되지 않습니다")
		}
		if seen[a.Key] {
			t.Errorf("키 중복: %s", a.Key)
		}
		seen[a.Key] = true
		if a.Label == "" {
			t.Errorf("%s: 라벨이 없습니다", a.Key)
		}
		if !groups[a.Group] {
			t.Errorf("%s: 알 수 없는 그룹 %q", a.Key, a.Group)
		}
	}
	for _, g := range AvatarGroups() {
		found := false
		for _, a := range Avatars() {
			if a.Group == g.Key {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("그룹 %q에 항목이 없습니다", g.Key)
		}
	}
}

// TestValidAvatar는 저장 가능한 값 판정을 확인한다.
func TestValidAvatar(t *testing.T) {
	if !ValidAvatar("") {
		t.Error("빈 값(아이콘 없음)을 거부했습니다")
	}
	if !ValidAvatar("role-dba") {
		t.Error("목록에 있는 키를 거부했습니다")
	}
	for _, bad := range []string{"role-nope", "ROLE-DBA", "role dba", "../../etc/passwd", "<svg>"} {
		if ValidAvatar(bad) {
			t.Errorf("%q를 허용했습니다", bad)
		}
	}
}

// TestAvatarsIsCopy는 반환된 슬라이스를 고쳐도 원본이 변하지 않는지 확인한다.
func TestAvatarsIsCopy(t *testing.T) {
	list := Avatars()
	if len(list) == 0 {
		t.Fatal("목록이 비었습니다")
	}
	key := list[0].Key
	list[0].Key = "tampered"
	if Avatars()[0].Key != key {
		t.Error("호출자가 목록을 바꿀 수 있습니다")
	}
}
