// 방 대화(전체 대화) 뷰.
//
// ERD 설계 화면과 구조 화면이 같은 방을 쓰므로 대화도 같은 모양이어야 한다.
// 두 벌로 두면 한쪽만 고쳐지고, 같은 일을 하는 두 화면이 서로 다르게 읽힌다 —
// 실제로 구조 화면의 대화가 먼저 그렇게 갈라졌다(입력칸 모양도, 줄의 구조도 달랐다).
import { h } from './dom.js';
import { input, badge, relativeTime } from './ui.js';
import { renderMarkdown } from './markdown.js';

/**
 * roomChatView는 대화 목록과 입력칸을 만들어 돌려준다.
 * @param {object} opts
 * @param {Array} opts.messages  {userName, body, kind, targetKey, createdAt}
 * @param {number} opts.participants  지금 방에 있는 사람 수
 * @param {(body: string) => void} opts.onSend  보내기
 * @param {string} [opts.placeholder]  입력칸 안내
 * @param {string} [opts.emptyText]  대화가 없을 때 문구
 * @param {string} [opts.disabledNote]  보낼 수 없을 때 그 이유(있으면 입력칸 대신 뜬다)
 * @returns {Node[]} 패널 본문에 그대로 넣을 노드들
 */
export function roomChatView({
  messages = [], participants = 0, onSend,
  placeholder = '메시지를 입력하세요', emptyText = '아직 대화가 없습니다.',
  disabledNote = '',
}) {
  const list = h('div.erd-chat-list', {}, messages.length === 0
    ? h('p.muted.erd-chat-empty', {}, emptyText)
    : messages.map((m) => h(`div.erd-chat-msg${m.kind === 'system' ? '.is-system' : ''}`, {},
      h('div.erd-chat-meta', {},
        h('strong', {}, m.userName || '알 수 없음'),
        m.targetKey ? badge(m.targetKey, 'neutral') : null,
        h('span.muted', {}, relativeTime(m.createdAt)),
      ),
      // "[AI] "로 시작하는 줄은 AI 세션에서 공유된 답변이다. 그 본문은 모델이 쓴
      // 마크다운이라 그대로 두면 `**표**` 같은 별표가 화면에 남는다. 사람이 친
      // 메시지는 마크다운으로 그리지 않는다 — 저기서의 `*`는 별표다.
      m.body?.startsWith('[AI] ')
        ? h('div.erd-chat-md', {}, renderMarkdown(m.body.slice(5)))
        : h('p.erd-chat-body', {}, m.body),
    )));

  const box = input({ placeholder });
  const send = () => {
    const body = box.value.trim();
    if (!body) return;
    onSend?.(body);
    box.value = '';
  };
  // Enter로 보낸다. 짧은 한 줄이 대부분이라 버튼까지 손을 옮기는 편이 느리다.
  box.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      send();
    }
  });

  return [
    h('p.muted.small.erd-chat-note', {}, `${participants}명 참여 중`),
    list,
    disabledNote
      ? h('p.field-help.erd-chat-note', {}, disabledNote)
      : h('div.erd-chat-input', {}, box,
        h('button.btn.btn-small.btn-primary', { type: 'button', onclick: send }, '보내기')),
  ];
}

// scrollChatToBottom은 새 줄이 아래에 쌓이므로 열 때마다 맨 아래를 보여준다.
export function scrollChatToBottom(root) {
  const list = root?.querySelector('.erd-chat-list');
  if (list) list.scrollTop = list.scrollHeight;
}
