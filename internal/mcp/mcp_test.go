package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// 클라이언트가 보낸 판을 그대로 돌려주는 것이 규약이다.
// 우리 것을 고집하면 옛 클라이언트가 연결을 끊는다.
func TestNegotiateVersion(t *testing.T) {
	for _, v := range []string{"2025-06-18", "2025-03-26", "2024-11-05"} {
		if got := NegotiateVersion(v); got != v {
			t.Errorf("NegotiateVersion(%q) = %q, want %q", v, got, v)
		}
	}
	// 모르는 판이면 우리 것을 알려준다.
	if got := NegotiateVersion("1999-01-01"); got != ProtocolVersion {
		t.Errorf("모르는 판 = %q, want %q", got, ProtocolVersion)
	}
	if got := NegotiateVersion(""); got != ProtocolVersion {
		t.Errorf("빈 판 = %q", got)
	}
}

// 알림(ID 없는 메시지)에 응답하면 규약 위반이다. 그 판정이 이 함수 하나에 달려 있다.
func TestIsNotification(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{`{"jsonrpc":"2.0","method":"notifications/initialized"}`, true},
		{`{"jsonrpc":"2.0","id":null,"method":"ping"}`, true},
		{`{"jsonrpc":"2.0","id":1,"method":"ping"}`, false},
		{`{"jsonrpc":"2.0","id":"abc","method":"ping"}`, false},
		// id가 0인 것은 유효한 ID다. 숫자 0을 "없음"으로 보면 응답이 사라진다.
		{`{"jsonrpc":"2.0","id":0,"method":"ping"}`, false},
	}
	for _, tc := range cases {
		var r Request
		if err := json.Unmarshal([]byte(tc.raw), &r); err != nil {
			t.Fatalf("Unmarshal(%s): %v", tc.raw, err)
		}
		if got := r.IsNotification(); got != tc.want {
			t.Errorf("%s → IsNotification()=%v, want %v", tc.raw, got, tc.want)
		}
	}
}

// 모르는 알림에 "메서드 없음" 오류를 돌려주면 규약 위반이다.
// 앞으로 생길 알림까지 조용히 무시하려면 이름 규칙으로 판단해야 한다.
func TestIsNotificationMethod(t *testing.T) {
	cases := map[string]bool{
		`{"jsonrpc":"2.0","method":"notifications/progress"}`:      true,
		`{"jsonrpc":"2.0","id":1,"method":"notifications/future"}`: true,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`:           false,
		`{"jsonrpc":"2.0","id":1,"method":"unknown/thing"}`:        false,
	}
	for raw, want := range cases {
		var r Request
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if got := r.IsNotificationMethod(); got != want {
			t.Errorf("%s → %v, want %v", raw, got, want)
		}
	}
}

func TestParseMessages(t *testing.T) {
	single, isBatch, err := ParseMessages([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatalf("단일: %v", err)
	}
	if isBatch || len(single) != 1 || single[0].Method != "ping" {
		t.Errorf("단일 = batch:%v len:%d", isBatch, len(single))
	}

	// 배치는 규약상 허용되고 일부 클라이언트가 쓴다. 받지 않으면 연결조차 못 한다.
	batch, isBatch, err := ParseMessages([]byte(
		`[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"}]`))
	if err != nil {
		t.Fatalf("배치: %v", err)
	}
	if !isBatch || len(batch) != 2 {
		t.Errorf("배치 = batch:%v len:%d", isBatch, len(batch))
	}

	// 앞뒤 공백이 있어도 배열인지 알아봐야 한다.
	_, isBatch, err = ParseMessages([]byte("  \n [{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\"}]  "))
	if err != nil || !isBatch {
		t.Errorf("공백 있는 배치 = batch:%v err:%v", isBatch, err)
	}

	if _, _, err := ParseMessages([]byte("")); err == nil {
		t.Error("빈 본문은 오류여야 한다")
	}
	if _, _, err := ParseMessages([]byte("not json")); err == nil {
		t.Error("잘못된 JSON은 오류여야 한다")
	}
}

// 툴 오류는 JSON-RPC 오류가 아니라 isError로 돌려준다.
// 프로토콜 오류로 만들면 클라이언트가 대화를 끊어서 모델이 고쳐 부를 기회를 잃는다.
func TestTextResult(t *testing.T) {
	ok := TextResult("결과", false)
	if ok.IsError || len(ok.Content) != 1 || ok.Content[0].Type != "text" {
		t.Errorf("성공 결과 = %+v", ok)
	}
	bad := TextResult("권한이 없습니다", true)
	if !bad.IsError {
		t.Error("오류 결과에 isError가 없다")
	}

	// isError는 false일 때 JSON에서 빠져야 한다(omitempty). 일부 클라이언트가
	// 존재 여부만 보고 판단한다.
	data, err := json.Marshal(ok)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "isError") {
		t.Errorf("성공 결과에 isError가 남아 있다: %s", data)
	}
}

func TestResponseShape(t *testing.T) {
	id := json.RawMessage(`7`)

	res := NewResult(id, map[string]any{"ok": true})
	data, _ := json.Marshal(res)
	if !strings.Contains(string(data), `"jsonrpc":"2.0"`) || !strings.Contains(string(data), `"id":7`) {
		t.Errorf("결과 응답 = %s", data)
	}
	// 성공 응답에 error 필드가 있으면 안 된다.
	if strings.Contains(string(data), `"error"`) {
		t.Errorf("성공 응답에 error가 있다: %s", data)
	}

	errRes := NewError(id, CodeMethodNotFound, "없음", nil)
	data, _ = json.Marshal(errRes)
	if !strings.Contains(string(data), `"code":-32601`) {
		t.Errorf("오류 응답 = %s", data)
	}
	if strings.Contains(string(data), `"result"`) {
		t.Errorf("오류 응답에 result가 있다: %s", data)
	}
}

// 안내문에 토큰 범위가 들어가야 한다. 읽기 토큰으로 연결한 모델이
// "변경 툴이 없는 이유"를 사용자에게 설명할 수 있어야 한다.
func TestInstructionsMentionScope(t *testing.T) {
	read := Instructions("alice", "read")
	if !strings.Contains(read, "alice") || !strings.Contains(read, "read") {
		t.Errorf("읽기 안내문에 사용자·범위가 없다: %q", read)
	}
	if !strings.Contains(read, "읽기 전용") {
		t.Errorf("읽기 토큰 안내가 없다: %q", read)
	}
	write := Instructions("bob", "write")
	if strings.Contains(write, "읽기 전용이므로") {
		t.Errorf("쓰기 토큰에 읽기 전용 안내가 붙었다: %q", write)
	}
}

// 툴 주석은 클라이언트가 사람에게 확인을 받을지 정하는 근거다.
// readOnlyHint가 빠지면 조회 툴마다 확인 창이 뜨고, 반대면 변경이 조용히 실행된다.
func TestToolAnnotationsSerialized(t *testing.T) {
	tool := Tool{
		Name: "x", Description: "y", InputSchema: map[string]any{"type": "object"},
		Annotations: &ToolAnnotations{ReadOnlyHint: true},
	}
	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"readOnlyHint":true`) {
		t.Errorf("readOnlyHint가 없다: %s", data)
	}
	if !strings.Contains(string(data), `"inputSchema"`) {
		t.Errorf("inputSchema가 없다: %s", data)
	}
}
