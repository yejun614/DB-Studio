package macro

import "testing"

// "전체 DB"는 그 값을 처리할 줄 아는 노드에만 있어야 한다.
//
// 고를 수 있는데 실행이 거부되면 사용자는 권한 문제라고 오해하고, 반대로 SQL 실행이나
// 데이터 수정에까지 열어 두면 문장 하나가 모든 DB에 도는 길이 생긴다.
func TestOnlyBackupAllowsAllConnections(t *testing.T) {
	found := false
	for _, spec := range Specs() {
		for _, f := range spec.Fields {
			if f.Type != "connection" || !f.AllowAll {
				continue
			}
			if spec.Type != TypeBackup {
				t.Errorf("%s 노드가 전체 DB를 고를 수 있게 되어 있다 — 처리 경로가 있는지 확인하세요", spec.Type)
			}
			found = true
		}
	}
	if !found {
		t.Error("전체 DB를 고를 수 있는 노드가 하나도 없다 (백업 노드에 있어야 한다)")
	}
}

// 백업 노드의 커넥션 칸은 여전히 필수다. 전체 DB를 더했다고 빈 값이 허용되면
// 실수로 아무것도 고르지 않은 노드가 저장된다.
func TestBackupConnectionStaysRequired(t *testing.T) {
	for _, spec := range Specs() {
		if spec.Type != TypeBackup {
			continue
		}
		for _, f := range spec.Fields {
			if f.Key != "connection" {
				continue
			}
			if !f.Required {
				t.Error("백업 노드의 커넥션 칸이 선택 사항이 되었다")
			}
			if f.Help == "" {
				t.Error("전체 DB가 무엇을 하는지 설명이 없다 — 고르기 전에는 알 수 없다")
			}
			return
		}
	}
	t.Fatal("백업 노드를 찾지 못했다")
}
