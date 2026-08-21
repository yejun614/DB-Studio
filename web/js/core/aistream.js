// AI 대화 스트리밍 (SSE) 읽기.
//
// EventSource가 아니라 fetch + ReadableStream을 쓰는 이유: EventSource는 GET만
// 지원하므로 질문을 URL에 넣어야 하고, 그러면 길이 제한에 걸린다. 또 중단
// (AbortController)도 EventSource로는 깔끔하게 되지 않는다.
//
// 어시스턴트 화면과 ERD 설계 화면이 같은 엔드포인트를 쓰므로 파싱은 여기 한 벌만
// 둔다 — 두 벌이면 서버가 이벤트를 하나 추가할 때 한쪽만 고쳐지고, 그 증상은
// "어떤 화면에서는 툴 실행이 안 보인다"로 나타난다.

// parseSSE는 이벤트 한 덩어리(빈 줄로 끊긴 블록)를 {event, data}로 바꾼다.
export function parseSSE(chunk) {
  let event = '';
  const dataLines = [];
  for (const line of chunk.split('\n')) {
    if (line.startsWith('event:')) event = line.slice(6).trim();
    else if (line.startsWith('data:')) dataLines.push(line.slice(5).trim());
  }
  if (!event) return null;
  let data = {};
  if (dataLines.length) {
    try {
      data = JSON.parse(dataLines.join('\n'));
    } catch {
      data = {};
    }
  }
  return { event, data };
}

// readSSE는 응답 본문을 끝까지 읽으며 이벤트마다 onEvent를 부른다.
export async function readSSE(body, onEvent) {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    // 마지막 조각은 다음 청크와 이어질 수 있으므로 남겨둔다.
    const chunks = buffer.split('\n\n');
    buffer = chunks.pop() ?? '';
    for (const chunk of chunks) {
      const ev = parseSSE(chunk);
      if (ev) onEvent(ev);
    }
  }
}

// streamAIChat은 대화 한 번을 왕복한다. 오류는 던지므로 부르는 쪽에서 잡는다.
//
// api.js의 헬퍼를 쓰지 않는 이유: 그쪽은 JSON 본문을 통째로 기다린다. 여기서는
// 첫 토큰이 도착하는 순간부터 그려야 하므로 응답 본문을 직접 읽어야 한다.
export async function streamAIChat(sessionId, message, onEvent, signal) {
  const res = await fetch(
    `/api/v1/ai/sessions/${encodeURIComponent(sessionId)}/chat`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'dbstudio' },
      credentials: 'same-origin',
      body: JSON.stringify({ message }),
      signal,
    },
  );
  if (!res.ok) {
    let payload = null;
    try {
      payload = await res.json();
    } catch { /* 본문이 JSON이 아니다 */ }
    throw new Error(payload?.detail || payload?.message || `HTTP ${res.status}`);
  }
  await readSSE(res.body, onEvent);
}
