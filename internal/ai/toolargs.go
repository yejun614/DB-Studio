package ai

import (
	"encoding/json"
	"fmt"
	"strings"
)

// 스트리밍 툴 호출 조립.
//
// 규약(OpenAI)은 이렇게 정한다: 툴 호출은 `index`로 구분되고, `id`와 `name`은 첫
// 조각에만 오며, `arguments`는 문자열 조각으로 이어 붙는다. 그대로 믿고 index만
// 쓰면 대부분의 서버에서 잘 동작한다.
//
// 문제는 그 약속을 지키지 않는 구현이 있다는 것이다. Gemini의 OpenAI 호환 계층은
// 조각에 **index를 넣지 않는다.** Go에서 `index`를 int로 받으면 없는 것과 0번이
// 구분되지 않으므로, 한 턴에 툴을 두 개 부른 응답이 전부 0번 슬롯에 겹친다.
// 그 결과가 `{"connection":"a"}{"connection":"b"}` 같은 문자열이고, 증상은 두 가지로
// 나타난다.
//
//   - 툴이 "인자를 해석할 수 없습니다: invalid character '{' after top-level value"로 실패한다.
//   - 그 망가진 호출이 대화 이력에 남아, 다음 요청부터 프로바이더가 400으로 거부한다
//     ("Request contains an invalid argument").
//
// 그래서 조각을 세 단계로 가른다: ① index가 있으면 그것으로, ② 없으면 id로,
// ③ 둘 다 없으면 직전 슬롯의 이어짐으로 본다. 그리고 어느 방법으로도 갈리지 않는
// 모양을 위해 마지막 방어선을 둔다 — **이미 완성된 JSON 뒤에 새 객체가 붙으면
// 그것은 이어짐이 아니라 다음 호출이다.**

// toolCallBuf는 조립 중인 툴 호출 하나다.
type toolCallBuf struct {
	id   string
	name string
	args strings.Builder
	// sig는 프로바이더가 이 호출에 붙인 사고 서명이다.
	// 어느 조각에 실려 올지 규약에 정해져 있지 않아(구현마다 첫 조각이거나
	// 마지막 조각이다) 값이 올 때마다 덮어써 둔다.
	sig string
}

// completedArgs는 지금까지 모인 인자가 **그 자체로 완결된 JSON**이면 그것을 반환한다.
// 아직 조립 중이면 빈 문자열이다.
//
// nil에서도 안전하다: 아직 슬롯이 없다는 뜻이고, 그때는 완결된 것도 없다.
func (c *toolCallBuf) completedArgs() string {
	if c == nil {
		return ""
	}
	s := strings.TrimSpace(c.args.String())
	if s == "" || !json.Valid([]byte(s)) {
		return ""
	}
	return s
}

// repeats는 들어온 조각이 이 슬롯에 이미 담긴 호출을 통째로 되풀이한 것인지 본다.
//
// 같은 이름 + 같은 인자를 다시 보낸 것이라면 새 호출로 갈라내면 안 된다 —
// 그러면 같은 툴이 두 번 실행된다(변경 툴이면 승인 요청이 두 개 생긴다).
func (c *toolCallBuf) repeats(name, frag string) bool {
	if c == nil {
		return false
	}
	if name != "" && c.name != "" && name != c.name {
		return false
	}
	return strings.TrimSpace(frag) == strings.TrimSpace(c.args.String())
}

// splitsCall은 이 조각이 앞선 호출의 **이어짐이 아니라 새 호출**인지 판단한다.
//
// 판단 근거는 문법 하나다: 완결된 JSON 값 뒤에 다시 `{`가 오는 것은 이어지는 인자일
// 수 없다. 이 규칙은 index가 없는 구현에서도, id를 재사용하는 구현에서도 통한다.
func splitsCall(completed, frag string) bool {
	if completed == "" {
		return false
	}
	frag = strings.TrimSpace(frag)
	return strings.HasPrefix(frag, "{")
}

// slotKey는 이 조각이 속할 슬롯의 키를 만든다.
//
// index → id → 직전 슬롯 순으로 본다. 셋 다 없으면(첫 조각인데 식별자가 하나도 없는
// 경우) 0번으로 시작한다 — 그런 스트림에서는 호출이 하나뿐인 것이 보통이고,
// 둘 이상이면 splitsCall이 갈라낸다.
func slotKey(index *int, id, last string) string {
	if index != nil {
		return fmt.Sprintf("i%d", *index)
	}
	if id != "" {
		return "id:" + id
	}
	if last != "" {
		return last
	}
	return "i0"
}

// NormalizeToolArgs는 툴 인자를 **반드시 유효한 JSON 객체**로 만든다.
//
// 두 번째 반환값은 손을 댔는지 여부다(기록용). 규칙은 세 가지뿐이다.
//
//   - 비어 있으면 `{}` — 인자 없는 툴을 부른 것이다.
//   - 유효한 JSON이면 그대로.
//   - 아니면 **첫 번째 완결된 JSON 값만** 남긴다. 뒤에 붙은 것은 조립이 어긋나
//     섞여 들어온 조각이며, 그것을 그대로 두면 이 호출도 못 쓰고 대화 이력도 오염된다.
//     하나도 건질 수 없으면 `{}`로 둔다 — 툴은 "필수 인자가 없다"고 답할 것이고,
//     그것은 모델이 읽고 고칠 수 있는 오류다.
//
// 유효하지 않은 JSON을 그대로 흘려보내면 안 되는 이유가 이력에 있다. 그 값은
// 대화에 저장되고 다음 요청에 그대로 실려 나가므로, 한 번 오염되면 그 대화는
// 새 질문마다 400을 받는다 — 사용자가 대화를 지우기 전까지 회복되지 않는다.
func NormalizeToolArgs(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "{}", false
	}
	if json.Valid([]byte(s)) {
		return s, false
	}

	dec := json.NewDecoder(strings.NewReader(s))
	var first json.RawMessage
	if err := dec.Decode(&first); err != nil {
		return "{}", true
	}
	repaired := strings.TrimSpace(string(first))
	if repaired == "" || !json.Valid([]byte(repaired)) {
		return "{}", true
	}
	return repaired, true
}
