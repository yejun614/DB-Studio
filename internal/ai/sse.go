package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// maxErrorBody는 오류 응답을 읽을 상한이다.
const maxErrorBody = 64 << 10

// sseLineLimit은 SSE 한 줄의 상한이다.
//
// 기본 bufio.Scanner 버퍼(64KB)로는 부족하다: 툴 인자 JSON이 한 줄에 담겨 오는
// 경우가 있고, 큰 스키마를 다루는 툴에서는 그것이 수백 KB가 될 수 있다.
// 넘치면 스캐너가 조용히 멈춰 응답이 중간에 끊긴 것처럼 보인다.
const sseLineLimit = 4 << 20

// postSSE는 SSE 스트림을 요청하고 data 줄을 콜백에 넘긴다.
//
// 콜백이 false를 반환하면 읽기를 멈춘다.
func postSSE(ctx context.Context, endpoint string, headers map[string]string, body any,
	onData func(raw []byte) bool) error {

	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("요청 실패: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, maxErrorBody))
		return &APIError{
			Status:  res.StatusCode,
			Message: extractMessage(raw),
			Body:    string(raw),
		}
	}

	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), sseLineLimit)
	for scanner.Scan() {
		line := scanner.Bytes()
		// SSE는 빈 줄로 이벤트를 구분하고 event:/id:/retry: 줄도 올 수 있다.
		// 우리에게 필요한 것은 data: 줄뿐이다 — 이벤트 종류는 payload의 type 필드에
		// 들어 있고(Anthropic), OpenAI는 event 줄을 아예 쓰지 않는다.
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 {
			continue
		}
		if !onData(payload) {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		// 컨텍스트 취소로 끊긴 것은 오류가 아니다 (사용자가 화면을 떠난 경우).
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("스트림 읽기 실패: %w", err)
	}
	return nil
}

// getJSON은 단순 GET 요청을 수행한다 (모델 목록 조회 등).
func getJSON(ctx context.Context, endpoint string, headers map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("요청 실패: %w", err)
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(res.Body, maxErrorBody))
	if err != nil {
		return fmt.Errorf("응답을 읽지 못했습니다: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return &APIError{Status: res.StatusCode, Message: extractMessage(raw), Body: string(raw)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("응답을 해석하지 못했습니다: %w", err)
	}
	return nil
}

// extractMessage는 프로바이더별 오류 메시지 필드를 찾는다.
//
// Anthropic: {"type":"error","error":{"type":"...","message":"..."}}
// OpenAI:    {"error":{"message":"...","type":"...","code":"..."}}
// 로컬 LLM:  {"error":"..."} 또는 {"message":"..."} 등 제각각
func extractMessage(raw []byte) string {
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	if m, ok := probe["error"].(map[string]any); ok {
		if s, ok := m["message"].(string); ok && s != "" {
			return s
		}
		if s, ok := m["type"].(string); ok && s != "" {
			return s
		}
	}
	if s, ok := probe["error"].(string); ok && s != "" {
		return s
	}
	if s, ok := probe["message"].(string); ok && s != "" {
		return s
	}
	if s, ok := probe["detail"].(string); ok && s != "" {
		return s
	}
	return ""
}

func isPrivateHost(host string) bool {
	host = strings.ToLower(host)
	if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// postNDJSON은 줄 단위 JSON 스트림을 읽는다 (Ollama 네이티브 API).
//
// SSE와 따로 두는 이유: Ollama는 `data:` 접두사도 빈 줄 구분도 쓰지 않고 한 줄에
// JSON 하나를 그대로 흘린다. postSSE에 섞으면 두 규약을 하나의 함수가 눈치껏
// 가르게 되고, 그 눈치가 틀리는 날 증상은 "가끔 응답이 비어 있다"가 된다.
func postNDJSON(ctx context.Context, endpoint string, headers map[string]string, body any,
	onLine func(raw []byte) bool) error {

	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("요청 실패: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, maxErrorBody))
		return &APIError{
			Status:  res.StatusCode,
			Message: extractMessage(raw),
			Body:    string(raw),
		}
	}

	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), sseLineLimit)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if !onLine(line) {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		// 컨텍스트 취소로 끊긴 것은 오류가 아니다 (사용자가 화면을 떠난 경우).
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("스트림 읽기 실패: %w", err)
	}
	return nil
}
