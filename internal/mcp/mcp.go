// Package mcp는 Model Context Protocol 서버를 구현한다.
//
// 외부 MCP 클라이언트(Claude Code, Claude Desktop 등)가 DB Studio의 툴을 쓰게 한다.
// 앱 안의 AI 어시스턴트와 **같은 툴 레지스트리**를 쓴다 — 두 벌을 만들면 어느 한쪽에만
// 있는 툴이 생기고, 더 나쁘게는 권한 검사가 한쪽에만 들어간다.
//
// 프로토콜은 JSON-RPC 2.0이고 전송은 Streamable HTTP다. stdio를 지원하지 않는 이유:
// 이 앱은 이미 서버로 떠 있고, stdio 모드는 같은 SQLite 파일을 여는 두 번째 프로세스를
// 뜻한다(단일 인스턴스 전제와 어긋난다). stdio만 받는 클라이언트는 mcp-remote 같은
// 프록시를 쓰면 된다.
package mcp

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion은 이 서버가 기본으로 답하는 프로토콜 판이다.
const ProtocolVersion = "2025-06-18"

// supportedVersions는 클라이언트가 요청했을 때 그대로 받아들일 판들이다.
//
// 클라이언트가 보낸 판을 그대로 돌려주는 것이 규약이다. 모르는 판이면 우리 것을
// 알려주고, 클라이언트가 감당할 수 없으면 연결을 끊는다.
var supportedVersions = map[string]bool{
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

// NegotiateVersion은 클라이언트가 요청한 판을 그대로 쓰거나 우리 것을 알려준다.
func NegotiateVersion(requested string) string {
	if supportedVersions[requested] {
		return requested
	}
	return ProtocolVersion
}

// ---------- JSON-RPC ----------

// Request는 들어오는 JSON-RPC 메시지다.
//
// ID가 없으면 알림(notification)이며 응답을 보내지 않는다. 그것을 구분하려고
// json.RawMessage로 받는다 — ID는 문자열일 수도 숫자일 수도 있고, 우리는 그것을
// 해석할 필요 없이 그대로 돌려주기만 하면 된다.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification은 응답을 보내지 않아야 하는 메시지인지 반환한다.
func (r *Request) IsNotification() bool { return len(r.ID) == 0 || string(r.ID) == "null" }

// IsNotificationMethod는 이름만으로 알림임을 알 수 있는 메서드인지 본다.
//
// 모르는 알림에 "메서드 없음" 오류를 돌려주면 규약 위반이다(알림에는 응답이 없다).
// 앞으로 생길 알림까지 조용히 무시하려면 이름 규칙으로 판단하는 수밖에 없다.
func (r *Request) IsNotificationMethod() bool {
	return r.IsNotification() || len(r.Method) > 14 && r.Method[:14] == "notifications/"
}

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// JSON-RPC 표준 오류 코드.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

func NewResult(id json.RawMessage, result any) *Response {
	return &Response{JSONRPC: "2.0", ID: id, Result: result}
}

func NewError(id json.RawMessage, code int, message string, data any) *Response {
	return &Response{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: message, Data: data}}
}

// ---------- MCP 타입 ----------

// Tool은 tools/list가 돌려주는 툴 하나다.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	// Annotations는 클라이언트가 사용자에게 위험도를 알리는 데 쓴다.
	// readOnlyHint가 false면 대부분의 클라이언트가 호출 전에 사람에게 확인을 받는다 —
	// 우리 화면의 "승인" 단계가 그쪽에서 하는 일이다.
	Annotations *ToolAnnotations `json:"annotations,omitempty"`
}

type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint"`
	DestructiveHint bool   `json:"destructiveHint,omitempty"`
	IdempotentHint  bool   `json:"idempotentHint,omitempty"`
	OpenWorldHint   bool   `json:"openWorldHint,omitempty"`
}

// Content는 툴 결과의 한 조각이다. 지금은 텍스트만 쓴다 —
// 툴 결과는 JSON 문자열이고, 그것을 이미지나 리소스로 감쌀 이유가 없다.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// CallToolResult는 tools/call의 결과다.
//
// 오류를 JSON-RPC 오류가 아니라 isError로 돌려주는 것이 MCP의 규약이다.
// 그래야 모델이 그 오류를 읽고 스스로 고쳐 다시 부를 수 있다 — 프로토콜 수준
// 오류로 만들면 클라이언트가 대화를 끊는다.
type CallToolResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

func TextResult(text string, isError bool) *CallToolResult {
	return &CallToolResult{Content: []Content{{Type: "text", Text: text}}, IsError: isError}
}

// ServerInfo는 initialize 응답에 담기는 서버 정보다.
type ServerInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

type InitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      ServerInfo     `json:"serverInfo"`
	Instructions    string         `json:"instructions,omitempty"`
}

// instructions는 클라이언트가 모델에게 전달하는 서버 사용 안내다.
//
// 여기에 권한 모델을 적어 두는 이유: 모델이 "권한이 없다"는 오류를 받았을 때
// 그것이 버그가 아니라 설계라는 것을 알아야 엉뚱한 우회를 시도하지 않는다.
const instructions = `DB Studio는 여러 데이터베이스를 등록해 접근 권한·모니터링·스키마 설계·
마이그레이션·백업을 관리하는 도구다.

먼저 list_connections를 불러 어떤 DB에 접근할 수 있는지 확인한다. 다른 툴에는 그 목록의
이름을 그대로 넘기면 된다.

모든 툴은 이 토큰을 발급한 사용자의 권한으로 실행된다. 권한이 없어 거부되는 것은 정상 동작이며,
우회할 방법은 없다. 데이터 조회/수정/SQL 실행은 등급과 별개인 능력으로 커넥션마다 따로 부여된다.

되돌릴 수 없는 동작(운영 DB 복구, 파괴적 마이그레이션 실행)은 사람이 웹 화면에서 확인 문구를
입력해야 하므로 이 경로로는 실행되지 않는다. 그런 경우에는 사용자에게 화면에서 진행하라고 안내한다.`

// Instructions는 서버 사용 안내를 만든다.
//
// 토큰 범위를 함께 알려주는 이유: 읽기 토큰으로 연결한 모델이 "변경해 줘"라는 요청을
// 받았을 때, 툴이 없는 이유를 알아야 사용자에게 정확히 설명할 수 있다.
func Instructions(username, scope string) string {
	scopeLine := fmt.Sprintf("\n\n현재 연결은 사용자 %q 의 권한을 쓰며 토큰 범위는 %q 다.", username, scope)
	if scope == "read" {
		scopeLine += " 읽기 전용이므로 변경 툴은 아예 제공되지 않는다 — " +
			"변경이 필요하면 사용자가 쓰기 범위의 토큰을 새로 발급해야 한다."
	}
	return instructions + scopeLine
}

// ParseMessages는 요청 본문을 JSON-RPC 메시지 목록으로 읽는다.
//
// 배열(배치)과 단일 객체를 모두 받는다. 배치는 규약상 허용되고 일부 클라이언트가
// 초기화 직후에 쓴다 — 받지 않으면 그 클라이언트는 연결조차 하지 못한다.
func ParseMessages(body []byte) ([]*Request, bool, error) {
	trimmed := trimSpace(body)
	if len(trimmed) == 0 {
		return nil, false, fmt.Errorf("빈 요청")
	}
	if trimmed[0] == '[' {
		var batch []*Request
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			return nil, true, err
		}
		return batch, true, nil
	}
	var one Request
	if err := json.Unmarshal(trimmed, &one); err != nil {
		return nil, false, err
	}
	return []*Request{&one}, false, nil
}

func trimSpace(b []byte) []byte {
	start := 0
	for start < len(b) && isSpace(b[start]) {
		start++
	}
	end := len(b)
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
