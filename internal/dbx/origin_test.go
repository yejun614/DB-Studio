package dbx

import (
	"strings"
	"testing"
)

// 이 시험이 고정하는 것: **실패 문구에 어느 노드가 어느 주소로 시도했는지가 남는가.**
//
// 이번 장애에서 3시간을 쓴 이유가 그 한 줄이 없어서였다. MySQL 원문에는 출발 호스트가
// 들어 있지만 어느 노드가 그 접속을 열었는지는 없고, 그 IP는 계정을 만들 때 넣은 값이라
// 설정 파일에도 화면에도 없다. 그래서 문구가 유일한 단서다 — 그 단서를 잃지 않게 고정한다.

// TestParseOriginReadsMySQLDenial은 1045 원문에서 사용자와 출발 호스트를 뽑는지 본다.
func TestParseOriginReadsMySQLDenial(t *testing.T) {
	// 운영에서 실제로 나온 문구 그대로(마스킹 없이 형태가 같다).
	msg := `접속 실패: Error 1045 (28000): Access denied for user 'appuser'@'172.24.0.4' (using password: YES)`

	o := ParseOrigin(msg)
	if o.Host != "172.24.0.4" {
		t.Errorf("출발 호스트 = %q (기대 172.24.0.4)", o.Host)
	}
	if o.User != "appuser" {
		t.Errorf("사용자 = %q (기대 appuser)", o.User)
	}
}

// TestParseOriginIgnoresNonAuthFailures는 인증까지 가지 않은 실패를 건드리지 않는지 본다.
//
// 출발 호스트는 "인증 단계까지 갔을 때만" MySQL이 알려 주는 값이다. 타임아웃이나
// 호스트 미해석에서 무엇인가를 뽑아 붙이면, 읽는 사람은 계정을 의심하게 되고
// 진짜 원인(망·주소)에서 멀어진다.
func TestParseOriginIgnoresNonAuthFailures(t *testing.T) {
	for _, msg := range []string{
		"dial tcp 10.0.0.5:3306: i/o timeout",
		"host를 찾을 수 없습니다",
		"Error 1049 (42000): Unknown database 'x'",
		"",
	} {
		if o := ParseOrigin(msg); o.Host != "" || o.User != "" {
			t.Errorf("%q 에서 무언가를 뽑았다: %+v", msg, o)
		}
	}
}

// TestAnnotateAddsNodeAndHost는 문구가 "무엇을 어디에 넣어야 하는지"까지 말하는지 본다.
func TestAnnotateAddsNodeAndHost(t *testing.T) {
	msg := `Error 1045 (28000): Access denied for user 'appuser'@'172.24.0.4' (using password: YES)`
	got := ParseOrigin(msg).WithNode("replica-b").Annotate(msg)

	for _, want := range []string{"replica-b", "172.24.0.4", "'appuser'@'172.24.0.4'"} {
		if !contains(got, want) {
			t.Errorf("문구에 %q 가 없다:\n%s", want, got)
		}
	}
	if !contains(got, "실행 노드: replica-b") {
		t.Errorf("실행 노드가 표식 형태로 없습니다:\n%s", got)
	}
}

// TestAnnotateDoesNotDoubleStamp는 두 노드가 차례로 붙여도 문구가 자라지 않는지 본다.
//
// 담당 노드가 자기 이름을 붙여 돌려준 문구를 받은 마스터가 다시 붙이면, 문구에는
// 노드 이름이 둘이 되고 정작 **어느 노드가 시도했는지**는 알 수 없게 된다.
func TestAnnotateDoesNotDoubleStamp(t *testing.T) {
	msg := `Error 1045 (28000): Access denied for user 'appuser'@'172.24.0.4' (using password: YES)`
	once := ParseOrigin(msg).WithNode("replica-b").Annotate(msg)
	twice := ParseOrigin(once).WithNode("master").Annotate(once)

	if twice != once {
		t.Errorf("문구가 다시 가공되었습니다:\n한 번: %s\n두 번: %s", once, twice)
	}
}

// TestAnnotateLeavesMessageAloneWhenNothingToAdd는 붙일 것이 없으면 원문 그대로인지 본다.
func TestAnnotateLeavesMessageAloneWhenNothingToAdd(t *testing.T) {
	msg := "i/o timeout"
	if got := ParseOrigin(msg).Annotate(msg); got != msg {
		t.Errorf("원문이 바뀌었습니다: %q → %q", msg, got)
	}
}

// TestAnnotateWorksWithoutNode은 노드가 없어도 출발 주소는 남기는지 본다.
//
// 독립 실행 모드(클러스터 아님)에서는 노드 이름이 없다. 그때도 출발 주소는
// 계정을 대조할 유일한 값이라 반드시 남아야 한다.
func TestAnnotateWorksWithoutNode(t *testing.T) {
	msg := `Error 1045 (28000): Access denied for user 'appuser'@'10.1.2.3' (using password: YES)`
	got := ParseOrigin(msg).Annotate(msg)

	if !contains(got, "10.1.2.3") {
		t.Errorf("출발 주소가 없다:\n%s", got)
	}
	if contains(got, "실행 노드") {
		t.Errorf("노드가 없는데 실행 노드를 적었다:\n%s", got)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
