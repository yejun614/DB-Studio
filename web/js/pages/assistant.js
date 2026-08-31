// AI 어시스턴트: 세션 목록, 스트리밍 대화, 툴 호출 표시, 쓰기 제안 승인.
//
// SSE 읽기는 core/aistream.js에 있다 — ERD 설계 화면의 AI 세션도 같은
// 엔드포인트를 쓰므로 파싱이 두 벌이면 한쪽만 낡는다.
import { api } from '../core/api.js';
import { withProject } from '../core/project.js';
import { streamAIChat } from '../core/aistream.js';
import { state } from '../core/store.js';
import {
  h, mount, icon, select, input, textarea, spinner, emptyState,
  badge, envBadge, toast, toastError, relativeTime, openModal, confirmDialog,
  copyToClipboard,
} from '../core/ui.js';
import { navigate, currentPath } from '../core/router.js';
import { openFloatPanel, panelModal, isPanelOpen, closeFloatPanel } from '../core/floatpanel.js';
import { renderMarkdown } from '../core/markdown.js';
import { errorPanel } from './users.js';
import { serverDbPicker, groupedSelect } from '../core/connpick.js';

// assistantBubble은 모델 응답을 마크다운으로 그린다.
//
// 모델은 제목·목록·표·코드 블록을 섞어 답한다. 그것을 글자 그대로 보여주면
// `**테이블 수**: 3개` 같은 줄이 그대로 남아 읽기 어렵고, 표는 아예 형태를 잃는다.
// 사용자 메시지에는 쓰지 않는다 — 사람이 친 `*`는 강조가 아니라 그냥 별표다.
function assistantBubble(text) {
  const el = h('div.ai-bubble.is-markdown');
  el.appendChild(renderMarkdown(text));
  return el;
}

// nav는 "다른 대화로 옮겨 가는 방법"이다.
//
// 이 화면은 두 가지 틀에 담긴다: 주소를 가진 페이지(/assistant)와 떠 있는 팝업.
// 페이지에서는 대화를 고르면 주소가 바뀌어야 하고(뒤로 가기·새로고침·링크 공유가
// 성립한다), 팝업에서는 그 자리에서 내용만 바뀌어야 한다 — 팝업이 주소를 바꾸면
// 뒤에서 보고 있던 화면이 사라진다.
function pageNav() {
  return {
    href: (id) => `/assistant?s=${encodeURIComponent(id)}`,
    open: (id) => navigate(id ? `/assistant?s=${encodeURIComponent(id)}` : '/assistant'),
    refresh: () => navigate(currentPath()),
  };
}

export async function renderAssistant(outlet, params, query) {
  return mountAssistant(outlet, { sessionId: query.get('s') ?? '', nav: pageNav() });
}

// ASSISTANT_PANEL은 떠 있는 어시스턴트 창의 이름이다. 하나만 열린다.
export const ASSISTANT_PANEL = 'assistant';

// toggleAssistantPopup은 떠 있으면 닫고 없으면 연다.
//
// 사이드바 메뉴가 이것을 쓴다. 열려 있는데 같은 메뉴를 눌러도 아무 일이 없으면,
// 사람은 "안 눌렸나" 하고 다시 누른다 — 그러다 창을 옮겨 둔 것을 잊고 화면 밖에서
// 찾게 된다. 누르는 것으로 닫을 수 있어야 그 자리에서 확인이 끝난다.
export function toggleAssistantPopup(sessionId = '') {
  if (isPanelOpen(ASSISTANT_PANEL)) {
    closeFloatPanel(ASSISTANT_PANEL);
    return null;
  }
  return openAssistantPopup(sessionId);
}

// openAssistantPopup은 어시스턴트를 팝업으로 연다.
//
// 화면을 옮기지 않는 것이 요점이다. 스키마를 보다가, 데이터를 보다가 물어보는
// 도구이므로 뒤에 있던 화면이 그대로 남아야 한다. 페이지(/assistant)도 그대로
// 둔다 — 긴 조사를 할 때는 넓은 화면이 낫고, 주소로 공유할 수도 있어야 한다.
export function openAssistantPopup(sessionId = '') {
  let cleanup = null;
  const panel = openFloatPanel({
    id: ASSISTANT_PANEL,
    title: 'AI 어시스턴트',
    iconName: 'sparkles',
    width: 620,
    height: 680,
    onClose: () => cleanup?.(),
    render: (body, handle) => {
      const show = async (id) => {
        cleanup?.();
        handle.sessionId = id;
        cleanup = await mountAssistant(body, {
          sessionId: id,
          // 팝업은 좁다. 대화 목록과 설정을 늘 펼쳐 두면 정작 대화가 보이는
          // 높이가 남지 않으므로, 둘 다 필요할 때만 겹쳐 연다(패널 안 모달).
          compact: true,
          panel: handle.panel,
          nav: {
            // href가 없으므로 목록은 버튼으로 그려진다(buildLayout).
            href: null,
            open: (next) => show(next),
            refresh: () => show(handle.sessionId ?? ''),
            resolved: (resolvedId) => { handle.sessionId = resolvedId; },
          },
        });
      };
      show(sessionId);
    },
  });
  return panel;
}

// mountAssistant는 어시스턴트 화면 한 벌을 이 요소 안에 그린다.
// 반환값은 정리 함수다 — 진행 중인 스트림을 끊는다.
export async function mountAssistant(outlet, {
  sessionId: wanted = '', nav, compact = false, panel = null,
}) {
  mount(outlet, spinner('어시스턴트를 준비하는 중…'));

  let data;
  let conns;
  try {
    [data, conns] = await Promise.all([
      api.get('/ai/sessions'),
      api.get(withProject('/connections/')),
    ]);
  } catch (err) {
    mount(outlet, errorPanel(err));
    return () => {};
  }

  const canManageKeys = Boolean(state.permissions?.manageConnections);
  // 무엇을 열지 정하지 않았으면 가장 최근 대화를 연다. 그 결정을 부르는 쪽에도
  // 알린다 — 팝업의 "전체 화면으로 열기"는 지금 보고 있는 대화를 그대로 이어야 한다.
  const sessionId = wanted || data.sessions[0]?.id || '';
  nav.resolved?.(sessionId);

  const ui = buildLayout(data, conns, canManageKeys, sessionId, nav, { compact, panel });
  mount(outlet, ui.root);

  if (data.providers.length === 0) {
    mount(ui.main, noProviderView(canManageKeys, nav));
    return () => {};
  }
  if (!sessionId) {
    mount(ui.main, emptyState('대화를 시작하려면 새 대화를 만드세요.',
      h('button.btn.btn-primary', {
        type: 'button', onclick: () => createSession(data, conns, nav),
      }, icon('plus'), '새 대화')));
    return () => {};
  }
  if (!data.sessions.some((s) => s.id === sessionId)) {
    // 목록은 본인 세션만 담고 있다. 없다면 남의 세션이거나 이미 삭제된 것이다.
    mount(ui.main, errorPanel({
      message: '이 대화를 찾을 수 없습니다',
      detail: '삭제되었거나 다른 사용자의 대화입니다.',
    }));
    return () => {};
  }

  const chat = new ChatView(sessionId, data, conns, ui, nav);
  chat.compact = compact;
  await chat.load();
  return () => chat.stop();
}

function buildLayout(data, conns, canManageKeys, activeId, nav, opts = {}) {
  const { compact = false, panel = null } = opts;
  const main = h('div.ai-main');
  // 팝업에는 주소가 없다. 그때는 링크가 아니라 버튼이어야 한다 —
  // href가 있으면 새 탭으로 열거나 가운데 버튼으로 눌렀을 때 페이지가 통째로 바뀐다.
  const sessionItem = (s, children) => (nav.href
    ? h('a', {
      href: nav.href(s.id),
      class: s.id === activeId ? 'ai-session is-active' : 'ai-session',
    }, children)
    : h('button', {
      type: 'button',
      class: s.id === activeId ? 'ai-session is-active' : 'ai-session',
      onclick: () => nav.open(s.id),
    }, children));

  // 지우기 단추는 항목 **바깥**에 둔다(형제).
  //
  // 항목 안에 넣을 수 없다: 항목 자체가 <a> 또는 <button>이고, 그 안에 버튼을
  // 넣는 것은 잘못된 마크업이라 브라우저가 알아서 밖으로 끄집어낸다. 그러면 클릭이
  // 어디로 가는지 종류마다 달라진다.
  const sessionRow = (s) => {
    const row = h('div.ai-session-row', {},
      sessionItem(s, [
        h('div.ai-session-title', {}, s.title || '새 대화'),
        h('div.ai-session-meta', {},
          h('span.muted', {}, relativeTime(s.updatedAt)),
          s.pendingCount > 0 ? badge(`승인 ${s.pendingCount}`, 'warn') : null,
          s.messageCount ? h('span.muted', {}, `${s.messageCount}개`) : null,
        ),
      ]),
      h('button.icon-btn.ai-session-del', {
        type: 'button', title: '이 대화 삭제', 'aria-label': `${s.title || '새 대화'} 삭제`,
        onclick: (e) => {
          // 목록의 클릭이 항목으로 번지면 지우려다 그 대화를 열게 된다.
          e.preventDefault();
          e.stopPropagation();
          removeSessionFromList(s, s.id === activeId, nav, row);
        },
      }, icon('trash', 13)),
    );
    return row;
  };

  const list = h('div.ai-session-list', {}, data.sessions.length === 0
    ? h('p.muted.ai-empty-list', {}, '대화가 없습니다')
    : data.sessions.map(sessionRow));

  const sidebar = h('aside.ai-sidebar', {},
      h('div.ai-sidebar-head', {},
        h('strong', {}, '대화'),
        h('button.btn.btn-small.btn-primary', {
          type: 'button', onclick: () => createSession(data, conns, nav),
        }, icon('plus'), '새 대화'),
      ),
      list,
    h('div.ai-sidebar-foot', {},
      h('button.btn.btn-small.btn-block', {
        type: 'button', onclick: () => openProviderDialog(canManageKeys, nav),
      }, icon('key'), 'AI 키 관리'),
      h('button.btn.btn-small.btn-block', {
        type: 'button', onclick: () => openToolsDialog(data),
      }, icon('list'), `툴 ${data.tools.length}개`),
    ),
  );

  // 좁은 팝업에서는 목록을 늘 펼쳐 두지 않는다. 폭을 나눠 쓰면 대화도 목록도
  // 읽을 수 없어, 둘 다 있는 것이 오히려 없는 것보다 나쁘다.
  const root = compact
    ? h('div.ai-shell.is-compact', {}, main)
    : h('div.ai-shell', {}, sidebar, main);

  // openSessions는 목록을 겹쳐 연다(팝업 안 모달).
  const openSessions = () => {
    const close = panel
      ? panelModal(panel, { title: '대화', body: () => sidebar })
      : openModal({ title: '대화', width: 420, body: () => sidebar });
    // 목록에서 고르면 화면이 통째로 다시 그려지므로 겹친 창은 닫아 둔다.
    sidebar.addEventListener('click', (e) => {
      if (e.target.closest('.ai-session')) close();
    }, { once: true });
  };

  return { root, main, list, sidebar, openSessions, panel };
}

// removeSessionFromList는 목록에서 대화를 지운다.
//
// 승인 대기 중인 제안이 있으면 그 수를 문장에 넣는다. 그것은 "아직 사람이 결정하지
// 않은 것"이고, 대화를 지우면 결정할 자리도 함께 사라진다 — 개수를 보여주지 않으면
// 그 사실을 지운 뒤에 알게 된다.
async function removeSessionFromList(session, isActive, nav, row) {
  const name = session.title || '새 대화';
  const pending = session.pendingCount > 0
    ? ` 승인을 기다리는 제안 ${session.pendingCount}건도 함께 사라집니다.`
    : '';
  const ok = await confirmDialog({
    title: '대화 삭제',
    message: `"${name}" 과 그 툴 실행 기록을 삭제합니다.${pending} `
      + '이미 실행된 작업은 되돌아가지 않습니다.',
    confirmLabel: '삭제', danger: true,
  });
  if (!ok) return;
  try {
    await api.del(`/ai/sessions/${encodeURIComponent(session.id)}`);
    toast('대화를 삭제했습니다', 'success');
    // 그 줄을 그 자리에서 지운다.
    //
    // 다시 그리는 것에만 맡길 수 없다. 팝업에서는 이 목록이 겹쳐 뜬 창에 담겨
    // 있어서, 뒤의 화면을 새로 그려도 사람이 보고 있는 그 목록은 옛 것 그대로다 —
    // "지웠는데 그대로 있다"가 그래서 생긴다.
    const list = row?.parentElement;
    row?.remove();
    if (list && !list.querySelector('.ai-session-row')) {
      mount(list, h('p.muted.ai-empty-list', {}, '대화가 없습니다'));
    }
    // 보고 있던 대화를 지웠으면 빈 자리를 보여주지 않고 다른 대화를 연다.
    if (isActive) nav.open('');
    else nav.refresh();
  } catch (err) {
    toastError(err);
  }
}

function noProviderView(canManageKeys, nav) {
  return h('div.card', {},
    h('h2', {}, 'AI 프로바이더가 없습니다'),
    h('p', {}, 'API 키를 등록하면 어시스턴트를 쓸 수 있습니다. ' +
      'Anthropic 또는 OpenAI 호환 엔드포인트(로컬 LLM 포함)를 지원합니다.'),
    canManageKeys
      ? h('button.btn.btn-primary', {
        type: 'button', onclick: () => openProviderDialog(true, nav),
      }, icon('key'), 'API 키 등록')
      : h('p.notice.notice-info', {}, icon('alert'),
        'API 키 등록은 커넥션 관리 권한(어드민 이상)이 필요합니다.'),
  );
}

async function createSession(data, conns, nav) {
  const titleInput = input({ placeholder: '예: 운영 DB 성능 조사' });
  const providerSelect = select(
    (data.providers ?? []).map((p) => ({
      value: p.id, label: `${p.name} (${p.defaultModel || '모델 미지정'})`,
    })),
    { value: data.providers?.[0]?.id ?? '' },
  );
  const usable = conns.items.filter((i) => i.accessible);
  const connSelect = serverDbPicker({
    usable,
    currentId: '',
    onPick: () => {},
    serverLabel: '대상 서버 (선택)',
    allLabel: '특정 DB 지정 안 함',
    serverHelp: '어시스턴트가 기본으로 참고할 커넥션입니다',
    inline: false,
  });

  // 모델은 프로바이더에 딸린 선택이므로 프로바이더가 바뀌면 다시 그린다.
  // 허용 목록이 없는 프로바이더는 고를 것이 없으므로 칸 자체를 숨긴다 —
  // 고를 수 없는 빈 선택 상자는 "설정이 덜 됐나"라는 오해만 만든다.
  const modelField = h('div');
  const modelBox = { value: '' };
  const renderModel = () => {
    const p = (data.providers ?? []).find((x) => x.id === providerSelect.value);
    const models = p?.models ?? [];
    if (models.length === 0) {
      modelBox.value = '';
      mount(modelField, p?.defaultModel
        ? h('p.field-help', {}, `모델: ${p.defaultModel}`)
        : null);
      return;
    }
    modelBox.value = models.includes(p.defaultModel) ? p.defaultModel : models[0];
    const sel = select(models.map((m) => ({ value: m, label: m })), { value: modelBox.value });
    sel.addEventListener('change', () => { modelBox.value = sel.value; });
    mount(modelField, h('label.field', {}, h('span.field-label', {}, '모델'), sel));
  };
  providerSelect.addEventListener('change', renderModel);

  openModal({
    title: '새 대화',
    width: 520,
    body: () => {
      setTimeout(renderModel, 0);
      return [
        h('label.field', {}, h('span.field-label', {}, '제목 (비우면 첫 질문으로 정합니다)'), titleInput),
        h('label.field', {}, h('span.field-label', {}, '프로바이더'), providerSelect),
        modelField,
        ...connSelect.nodes,
      ];
    },
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: async () => {
          try {
            const res = await api.post('/ai/sessions', {
              title: titleInput.value,
              providerId: providerSelect.value,
              model: modelBox.value,
              connectionId: connSelect.value,
            });
            close();
            nav.open(res.session.id);
          } catch (err) {
            toastError(err);
          }
        },
      }, '시작'),
    ],
  });
}

// ---------- 대화 화면 ----------

class ChatView {
  constructor(sessionId, data, conns, ui, nav) {
    this.sessionId = sessionId;
    this.data = data;
    this.conns = conns;
    this.ui = ui;
    this.nav = nav;
    this.session = null;
    this.messages = [];
    this.pending = [];
    // controller는 진행 중인 스트림을 끊기 위한 것이다.
    this.controller = null;
    // streaming은 현재 응답 중인 assistant 메시지의 임시 상태다.
    this.streaming = null;
  }

  async load() {
    mount(this.ui.main, spinner('대화를 불러오는 중…'));
    try {
      const res = await api.get(`/ai/sessions/${encodeURIComponent(this.sessionId)}`);
      this.session = res.session;
      this.messages = res.messages ?? [];
      this.pending = res.pendingActions ?? [];
    } catch (err) {
      mount(this.ui.main, errorPanel(err));
      return;
    }
    this.render();
    this.syncSidebar();
  }

  // syncSidebar는 사이드바의 현재 항목만 갱신한다.
  // 첫 메시지로 제목이 정해지거나 승인 대기가 생겨도 목록 전체를 다시 받지 않는다.
  syncSidebar() {
    const link = this.ui.list.querySelector('.ai-session.is-active');
    if (!link) return;
    const title = link.querySelector('.ai-session-title');
    if (title) mount(title, this.session.title || '새 대화');
    const meta = link.querySelector('.ai-session-meta');
    const count = this.pending.filter((p) => p.status === 'pending').length;
    if (meta) {
      mount(meta,
        h('span.muted', {}, relativeTime(this.session.updatedAt)),
        count > 0 ? badge(`승인 ${count}`, 'warn') : null,
        h('span.muted', {}, `${this.messages.length}개`));
    }
  }

  stop() {
    if (this.controller) {
      this.controller.abort();
      this.controller = null;
    }
  }

  render() {
    const conn = this.conns.items.find((i) => i.connection.id === this.session.connectionId);
    this.log = h('div.ai-log');
    this.inputBox = h('textarea.input.ai-input', {
      rows: 3,
      placeholder: '무엇을 도와드릴까요? (Enter 전송, Shift+Enter 줄바꿈)',
    });
    this.sendBtn = h('button.btn.btn-primary', {
      type: 'button', onclick: () => this.send(),
    }, icon('play'), '보내기');

    this.inputBox.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.shiftKey) {
        e.preventDefault();
        this.send();
      }
    });

    // 좁은 팝업에서는 머리글에 이름만 남긴다. 모델·대상 DB·삭제까지 늘어놓으면
    // 두 줄이 되고, 그만큼 대화가 보이는 높이가 줄어든다.
    const head = this.compact
      ? h('header.ai-head.is-compact', {},
        h('div.ai-head-main', {},
          h('button.btn.btn-small', {
            type: 'button', title: '대화 목록',
            onclick: () => this.ui.openSessions?.(),
          }, icon('list'), '대화'),
          h('h1.ai-title', {}, this.session.title || '새 대화'),
        ),
        h('div.ai-head-side', {},
          h('button.icon-btn', {
            type: 'button', title: '이 대화 설정',
            onclick: () => this.openSettings(),
          }, icon('settings')),
        ),
      )
      : h('header.ai-head', {},
        h('div.ai-head-main', {},
          h('h1.ai-title', {}, this.session.title || '새 대화'),
          h('button.icon-btn', {
            type: 'button', title: '대화 이름 변경',
            onclick: () => this.renameSession(),
          }, icon('edit')),
          this.session.providerName ? badge(this.session.providerName, 'accent') : null,
          this.modelControl(),
          this.connControl(),
          conn ? envBadge(conn.connection.environment) : null,
        ),
        h('div.ai-head-side', {},
          h('span.muted', {},
            `토큰 ${this.session.inputTokens.toLocaleString('ko-KR')} / ${this.session.outputTokens.toLocaleString('ko-KR')}`),
          h('button.icon-btn', {
            type: 'button', title: '대화 삭제',
            onclick: () => this.deleteSession(),
          }, icon('trash')),
        ),
      );

    mount(this.ui.main,
      head,
      this.log,
      h('div.ai-composer', {}, this.inputBox, this.sendBtn),
    );

    this.renderLog();
    this.inputBox.focus();
  }

  // openSettings는 좁은 팝업에서 감춰 둔 것들을 겹쳐 보여준다.
  //
  // 이름·모델·대상 DB·삭제는 자주 쓰지 않지만 없으면 안 되는 것들이다.
  // 머리글에서 빼되 한 곳에 모아 둔다 — 흩어 놓으면 어디 있었는지 찾게 된다.
  openSettings() {
    const conn = this.conns.items.find((i) => i.connection.id === this.session.connectionId);
    const body = () => [
      h('div.ai-settings-row', {},
        h('span.field-label', {}, '이름'),
        h('div.ai-settings-value', {},
          h('span', {}, this.session.title || '새 대화'),
          h('button.icon-btn', {
            type: 'button', title: '대화 이름 변경',
            onclick: () => { close(); this.renameSession(); },
          }, icon('edit'))),
      ),
      h('div.ai-settings-row', {},
        h('span.field-label', {}, '모델'),
        h('div.ai-settings-value', {},
          this.session.providerName ? badge(this.session.providerName, 'accent') : null,
          this.modelControl()),
      ),
      h('div.ai-settings-row', {},
        h('span.field-label', {}, '대상 DB'),
        h('div.ai-settings-value', {},
          this.connControl(),
          conn ? envBadge(conn.connection.environment) : null),
      ),
      h('div.ai-settings-row', {},
        h('span.field-label', {}, '토큰'),
        h('span.muted', {},
          `${this.session.inputTokens.toLocaleString('ko-KR')} / ${this.session.outputTokens.toLocaleString('ko-KR')}`),
      ),
      h('button.btn.btn-danger-ghost.btn-block', {
        type: 'button',
        onclick: () => { close(); this.deleteSession(); },
      }, icon('trash'), '대화 삭제'),
    ];

    const close = this.ui.panel
      ? panelModal(this.ui.panel, { title: '대화 설정', body })
      : openModal({ title: '대화 설정', width: 460, body });
  }

  // modelControl은 대화 중에 모델을 바꾸는 칸이다.
  //
  // 대화 도중에 바꿀 수 있어야 하는 이유: 조사는 싼 모델로 시작해도 되지만
  // 판단이 필요한 대목에서는 좋은 모델이 필요하다. 그때 대화를 새로 시작하게 하면
  // 지금까지 모은 문맥을 버리게 된다.
  //
  // 고를 것이 없으면(허용 목록이 비었으면) 지금 쓰는 모델 이름만 보여준다.
  modelControl() {
    // 프로바이더를 지정하지 않은 세션은 기본 프로바이더를 쓴다.
    // 목록은 is_default 순이므로 첫 항목이 그것이다.
    const provider = (this.data.providers ?? [])
      .find((p) => p.id === this.session.providerId) ?? (this.data.providers ?? [])[0];
    const models = provider?.models ?? [];
    const current = this.session.model || provider?.defaultModel || '';
    if (models.length < 2) {
      return current ? h('code.muted', {}, current) : null;
    }
    const sel = select(models.map((m) => ({ value: m, label: m })),
      { value: models.includes(current) ? current : models[0] });
    sel.classList.add('ai-model-switch');
    sel.title = '이 대화에 쓸 모델';
    sel.addEventListener('change', async () => {
      try {
        const res = await api.patch(
          `/ai/sessions/${encodeURIComponent(this.session.id)}`, { model: sel.value });
        this.session = res.session;
        toast(`모델을 ${sel.value} 로 바꿨습니다`, 'success');
      } catch (err) {
        sel.value = current;
        toastError(err);
      }
    });
    return sel;
  }

  // connControl은 이 대화가 참고할 DB를 바꾸는 칸이다.
  //
  // 대화 도중에 바꿀 수 있어야 하는 이유: 대상 DB는 새 대화를 만들 때 고르는데,
  // 이야기를 하다 보면 옆 DB를 같이 봐야 하는 일이 잦다. 그때 대화를 새로 시작하면
  // 지금까지의 문맥을 버리게 된다.
  connControl() {
    const usable = this.conns.items.filter((i) => i.accessible);
    const current = this.session.connectionId ?? '';
    const has = usable.some((i) => i.connection.id === current);
    const sel = groupedSelect({
      usable,
      currentId: has ? current : '',
      allLabel: '대상 DB 없음',
    });
    sel.classList.add('ai-conn-switch');
    sel.title = '이 대화가 기본으로 참고할 DB';
    sel.addEventListener('change', () => {
      this.applySessionChange({ connectionId: sel.value }, sel.value
        ? '대상 DB를 바꿨습니다'
        : '대상 DB를 지정하지 않도록 바꿨습니다');
    });
    return sel;
  }

  renameSession() {
    const nameInput = input({ value: this.session.title ?? '' });
    openModal({
      title: '대화 이름 변경',
      width: 460,
      body: () => [
        h('label.field', {}, h('span.field-label', {}, '이름'), nameInput,
          h('span.field-help', {}, '첫 질문으로 자동으로 정해진 이름을 바꿀 수 있습니다')),
      ],
      footer: (close) => [
        h('button.btn', { type: 'button', onclick: close }, '취소'),
        h('button.btn.btn-primary', {
          type: 'button',
          onclick: async () => {
            if (nameInput.value.trim() === '') {
              toast('이름을 입력하세요', 'error');
              return;
            }
            if (await this.applySessionChange({ title: nameInput.value }, '이름을 바꿨습니다')) close();
          },
        }, '저장'),
      ],
    });
  }

  // applySessionChange는 세션 설정을 바꾸고 화면을 다시 그린다.
  //
  // 쓰던 질문을 지키는 것이 요점이다: 헤더의 설정을 바꿨다고 입력 중이던 문장이
  // 사라지면 사용자는 그 칸을 건드리지 않게 된다.
  async applySessionChange(patch, message) {
    const draft = this.inputBox?.value ?? '';
    try {
      const res = await api.patch(
        `/ai/sessions/${encodeURIComponent(this.session.id)}`, patch);
      this.session = res.session;
    } catch (err) {
      toastError(err);
      return false;
    }
    this.render();
    this.inputBox.value = draft;
    this.syncSidebar();
    if (message) toast(message, 'success');
    return true;
  }

  renderLog() {
    const items = [];
    for (const m of this.messages) {
      const node = this.messageNode(m);
      if (!node) continue;
      // 고치기가 그 자리를 찾아야 한다. 대화 전체를 다시 그리지 않고 그 말풍선만
      // 칸으로 바꾸기 위해서다 — 다시 그리면 스크롤이 튀고 스트림이 끊긴다.
      if (m.id) node.dataset.msgId = String(m.id);
      items.push(node);
    }
    // 승인 대기 중인 제안은 항상 마지막에 눈에 띄게 둔다.
    for (const p of this.pending) {
      if (p.status === 'pending') items.push(this.pendingNode(p));
    }
    if (this.streaming) items.push(this.streaming.node);
    if (items.length === 0) {
      items.push(this.suggestions());
    }
    mount(this.log, items);
    this.log.scrollTop = this.log.scrollHeight;
  }

  suggestions() {
    const ideas = [
      '지금 문제가 있는 DB가 있나요?',
      '개발 MySQL에서 가장 느린 쿼리를 알려주세요',
      '스키마가 앱 밖에서 바뀐 커넥션이 있나요?',
      '대기 중인 마이그레이션을 정리해 주세요',
    ];
    return h('div.ai-suggestions', {},
      h('p.muted', {}, '이렇게 물어볼 수 있습니다:'),
      h('div.ai-chips', {}, ideas.map((text) => h('button.btn.btn-small', {
        type: 'button',
        onclick: () => {
          this.inputBox.value = text;
          this.send();
        },
      }, text))),
    );
  }

  messageNode(m) {
    if (m.role === 'user') {
      // 고치기는 아이디가 있을 때만이다. 방금 보낸 말은 서버에 저장되기 전이라
      // 지울 자리를 가리킬 수 없다 — 스트림이 끝나면 다시 그려지며 생긴다.
      return h('div.ai-msg.is-user', {},
        h('div.ai-bubble', {}, m.text),
        this.msgActions(m.text, m.id ? () => this.startEdit(m) : null));
    }
    if (m.role === 'assistant') {
      const parts = [];
      if (m.text) parts.push(assistantBubble(m.text));
      for (const call of m.toolCalls ?? []) {
        parts.push(toolCallNode(call));
      }
      if (m.error) {
        parts.push(h('div.notice.notice-danger', {}, icon('alert'), m.error));
      }
      if (parts.length === 0) return null;
      // 답에는 복사만 둔다. 남의 말을 고치는 것은 대화를 고치는 것이 아니라
      // 없었던 일을 만드는 것이다.
      if (m.text) parts.push(this.msgActions(m.text, null));
      return h('div.ai-msg.is-assistant', {}, parts);
    }
    if (m.role === 'tool') {
      return h('div.ai-msg.is-tool', {}, (m.toolResults ?? []).map((r) => {
        // 실패는 펼치지 않아도 보여야 한다. 접어 두면 모델이 왜 "할 수 없다"고
        // 답했는지 알 수 없고, 실패 이유는 대개 한두 줄로 짧다.
        if (r.isError) {
          return h('div.notice.notice-danger.ai-tool-error', {}, icon('alert'),
            h('span', {}, truncate(r.content, 400)));
        }
        // 성공 결과는 기본적으로 접어 둔다. 대화 흐름을 읽는 데 방해가 되고,
        // 필요한 사람은 펼쳐서 원본을 확인할 수 있어야 한다.
        return h('details.ai-tool-result', {},
          h('summary', {}, icon('check', 13), '툴 결과',
            h('span.muted', {}, ` ${r.content.length.toLocaleString('ko-KR')}자`)),
          h('pre.ai-tool-body', {}, r.content));
      }));
    }
    return null;
  }

  // msgActions는 말풍선에 붙는 동작 줄이다(복사, 그리고 내 말이면 고치기).
  //
  // 늘 보이게 두지 않는다 — 대화 열 줄에 단추 스무 개가 함께 서 있으면 읽는 것이
  // 방해된다. CSS가 그 말풍선에 손이 올라갈 때만 보여준다. 키보드로도 닿아야 하므로
  // 포커스에서도 보인다(:focus-within).
  msgActions(text, onEdit) {
    return h('div.ai-msg-actions', {},
      h('button.icon-btn', {
        type: 'button', title: '복사',
        onclick: () => copyToClipboard(text),
      }, icon('copy', 13)),
      onEdit
        ? h('button.icon-btn', {
          type: 'button', title: '고쳐서 다시 보내기',
          onclick: onEdit,
        }, icon('edit', 13))
        : null);
  }

  // startEdit은 그 말풍선을 고치는 칸으로 바꾼다.
  //
  // 입력 칸으로 옮겨 적게 하지 않는 이유: 그러면 "고친 것"과 "새로 물은 것"이
  // 구분되지 않고, 옛 문답이 위에 그대로 남는다. 이 자리에서 고쳐야 그 자리부터
  // 다시 시작한다는 뜻이 화면에 드러난다.
  startEdit(m) {
    if (this.controller) {
      toast('답변이 끝난 뒤에 고칠 수 있습니다', 'info');
      return;
    }
    const box = textarea({ rows: 3, value: m.text });
    const node = [...this.log.children].find((el) => el.dataset.msgId === String(m.id));
    if (!node) return;

    const cancel = () => this.renderLog();
    const submit = () => {
      const next = box.value.trim();
      if (!next) {
        toast('내용을 비울 수는 없습니다. 지우려면 대화를 새로 시작하세요', 'error');
        return;
      }
      if (next === m.text) {
        cancel();
        return;
      }
      this.send(next, m.id);
    };

    mount(node,
      h('div.ai-bubble.ai-edit', {},
        box,
        h('p.field-help', {},
          '다시 보내면 이 말부터 아래의 답까지 사라지고, 고친 말로 다시 시작합니다.'),
        h('div.ai-edit-actions', {},
          h('button.btn.btn-small', { type: 'button', onclick: cancel }, '취소'),
          h('button.btn.btn-small.btn-primary', {
            type: 'button', onclick: submit,
          }, icon('play', 13), '다시 보내기'))));
    box.focus();
    // 끝으로 커서를 둔다. 고치려는 사람은 대개 뒤에 덧붙이거나 지운다.
    box.setSelectionRange(box.value.length, box.value.length);
    box.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.shiftKey) {
        e.preventDefault();
        submit();
      }
      if (e.key === 'Escape') {
        e.preventDefault();
        cancel();
      }
    });
    node.scrollIntoView({ block: 'nearest' });
  }

  pendingNode(p) {
    let preview = null;
    try {
      preview = typeof p.preview === 'string' ? JSON.parse(p.preview) : p.preview;
    } catch {
      preview = null;
    }
    return h('div.ai-msg.is-pending', {},
      h('div.card.ai-pending', {},
        h('div.ai-pending-head', {},
          icon('alert'),
          h('strong', {}, '승인이 필요한 작업'),
          badge(p.toolName, 'accent'),
        ),
        h('p.ai-pending-summary', {}, p.summary),
        preview ? pendingPreview(preview) : null,
        h('div.ai-pending-actions', {},
          h('button.btn.btn-danger', {
            type: 'button',
            onclick: (e) => this.decide(p, 'approve', e.currentTarget),
          }, icon('check'), '승인하고 실행'),
          h('button.btn', {
            type: 'button',
            onclick: (e) => this.decide(p, 'reject', e.currentTarget),
          }, icon('x'), '거부'),
        ),
        h('p.field-help', {},
          '승인해도 앱의 기존 안전장치(검토자 승인, 사전 검사, 운영 DB 확인)는 그대로 적용됩니다.'),
      ));
  }

  async decide(p, decision, btn) {
    btn.disabled = true;
    const card = btn.closest('.ai-pending');
    try {
      const res = await api.post(
        `/ai/sessions/${encodeURIComponent(this.sessionId)}/actions/${encodeURIComponent(p.id)}`,
        { decision },
      );
      toast(decision === 'approve' ? '실행했습니다' : '거부했습니다',
        decision === 'approve' ? 'success' : 'info');
      await this.load();
      if (decision === 'approve' && res.result) {
        // 실행 결과를 이어서 모델에게 물어 대화가 끊기지 않게 한다.
        this.send('방금 승인한 작업의 결과를 확인하고 다음 단계를 알려주세요.');
      }
    } catch (err) {
      btn.disabled = false;
      // 실패 원인(예: 검토자 승인 부족, 사전 검사 실패)은 길 수 있으므로
      // 토스트가 아니라 카드 안에 그대로 남긴다.
      const detail = err.detail ?? err.payload?.detail;
      card?.appendChild(h('div.notice.notice-danger', {}, icon('alert'),
        h('div', {}, err.message ?? '실행하지 못했습니다',
          detail ? h('pre.sql-block', {}, detail) : null)));
      // 서버가 상태를 바꿨을 수 있으니(failed) 목록만 조용히 갱신한다.
      if (err.status === 409) await this.load();
    }
  }

  async deleteSession() {
    const ok = await confirmDialog({
      title: '대화 삭제',
      message: '이 대화와 툴 실행 기록을 삭제합니다. 이미 실행된 작업은 되돌아가지 않습니다.',
      confirmLabel: '삭제', danger: true,
    });
    if (!ok) return;
    try {
      await api.del(`/ai/sessions/${encodeURIComponent(this.sessionId)}`);
      toast('대화를 삭제했습니다', 'success');
      this.nav.open('');
    } catch (err) {
      toastError(err);
    }
  }

  // send는 메시지를 보내고 응답을 스트리밍으로 받는다.
  async send(overrideText, replaceFrom = 0) {
    const text = (overrideText ?? this.inputBox.value).trim();
    if (!text || this.controller) return;

    if (replaceFrom) {
      // 고친 말부터 아래를 화면에서도 먼저 걷어낸다. 서버가 지우는 것과 같은
      // 자리다 — 남겨 두면 답이 오는 동안 옛 문답이 함께 보인다.
      const cut = this.messages.findIndex((x) => x.id === replaceFrom);
      if (cut >= 0) this.messages = this.messages.slice(0, cut);
      this.pending = [];
    }
    this.messages.push({ role: 'user', text });
    if (!overrideText) this.inputBox.value = '';
    this.sendBtn.disabled = true;
    this.streaming = createStreamingNode();
    this.renderLog();

    this.controller = new AbortController();
    try {
      await streamAIChat(this.sessionId, text, (ev) => this.handleEvent(ev),
        this.controller.signal, replaceFrom);
    } catch (err) {
      if (err.name !== 'AbortError') {
        this.streaming?.setError(err.message);
        toast(err.message, 'error', 6000);
      }
    } finally {
      this.controller = null;
      this.sendBtn.disabled = false;
      this.streaming = null;
      // 스트림이 끝나면 서버에 저장된 상태로 다시 그린다 — 토큰 사용량과
      // 보류 제안이 정확히 반영된다.
      await this.load();
    }
  }

  handleEvent(ev) {
    if (!ev || !ev.event) return;
    switch (ev.event) {
      case 'text':
        this.streaming?.appendText(ev.data.text ?? '');
        break;
      case 'tool_call':
        this.streaming?.addToolCall(ev.data);
        break;
      case 'tool_result':
        this.streaming?.setToolResult(ev.data);
        break;
      case 'pending_action':
        this.streaming?.addPending(ev.data);
        break;
      case 'notice':
        this.streaming?.addNotice(ev.data.message ?? '');
        break;
      case 'error':
        this.streaming?.setError(ev.data.message ?? '오류가 발생했습니다');
        break;
      default:
        break;
    }
    this.log.scrollTop = this.log.scrollHeight;
  }
}

// createStreamingNode는 응답 중인 메시지 노드와 갱신 함수를 만든다.
//
// 전체를 다시 그리지 않고 이 노드만 갱신하는 이유: 토큰이 도착할 때마다 대화 전체를
// 재생성하면 스크롤 위치가 튀고 긴 대화에서 눈에 보이게 느려진다.
function createStreamingNode() {
  const bubble = h('div.ai-bubble.is-markdown.is-streaming', {}, h('span.ai-cursor', {}, '▌'));
  const toolBox = h('div.ai-stream-tools');
  const node = h('div.ai-msg.is-assistant', {}, bubble, toolBox);
  let text = '';
  let frame = 0;
  const toolNodes = new Map();

  // 도착한 조각마다 마크다운을 다시 그리되 프레임당 한 번으로 묶는다.
  // 토큰은 프레임보다 훨씬 빠르게 오므로 그대로 두면 같은 화면을 수십 번 그린다.
  const paint = () => {
    frame = 0;
    mount(bubble, renderMarkdown(text), h('span.ai-cursor', {}, '▌'));
  };

  return {
    node,
    appendText(chunk) {
      text += chunk;
      if (!frame) frame = requestAnimationFrame(paint);
    },
    addToolCall(data) {
      const el = toolCallNode(data, true);
      toolNodes.set(data.id, el);
      toolBox.appendChild(el);
    },
    setToolResult(data) {
      const el = toolNodes.get(data.id);
      if (!el) return;
      el.classList.remove('is-running');
      el.classList.add(data.error ? 'is-error' : 'is-done');
      const status = el.querySelector('.ai-tool-status');
      if (status) {
        mount(status, data.error
          ? h('span.text-danger', {}, data.error)
          : h('span.muted', {}, `${(data.size ?? 0).toLocaleString('ko-KR')}자`));
      }
      // 서버는 결과 전체가 아니라 앞부분만 보낸다. 스트림 중에는 그것으로 충분하고,
      // 전체는 스트림이 끝난 뒤 다시 불러온 대화에서 볼 수 있다.
      if (data.preview) {
        el.after(h('details.ai-tool-result', {},
          h('summary', {}, icon('list', 13), '결과 미리보기'),
          h('pre.ai-tool-body', {}, data.preview)));
      }
    },
    addPending(action) {
      toolBox.appendChild(h('p.notice.notice-warn', {}, icon('alert'),
        `승인 대기: ${action.summary}`));
    },
    addNotice(message) {
      toolBox.appendChild(h('p.notice.notice-info', {}, icon('alert'), message));
    },
    setError(message) {
      toolBox.appendChild(h('div.notice.notice-danger', {}, icon('alert'), message));
    },
  };
}

// toolCallNode는 툴 호출 한 건을 표시한다.
function toolCallNode(call, running = false) {
  let args = call.arguments ?? call.input;
  if (typeof args !== 'string') {
    try {
      args = JSON.stringify(args);
    } catch {
      args = '';
    }
  }
  if (args === '{}') args = '';
  return h(`div.ai-tool-call${running ? '.is-running' : ''}`, {},
    icon('settings', 13),
    h('code', {}, call.name),
    args ? h('span.ai-tool-args', {}, truncate(args, 120)) : null,
    call.mutating ? badge('승인 필요', 'warn') : null,
    h('span.ai-tool-status', {}, running ? h('span.muted', {}, '실행 중…') : null),
  );
}

function pendingPreview(preview) {
  const rows = [];
  const push = (label, value) => rows.push(h('div.meta-row', {},
    h('dt', {}, label), h('dd', {}, value)));

  for (const [key, value] of Object.entries(preview)) {
    if (value === null || value === undefined) continue;
    if (key === 'changes' && Array.isArray(value)) {
      rows.push(h('div.ai-preview-changes', {},
        h('dt', {}, `변경 ${value.length}건`),
        h('dd', {}, h('ul.note-list', {}, value.slice(0, 20).map((c) =>
          h('li', { class: c.destructive ? 'text-danger' : '' },
            c.summary ?? JSON.stringify(c)))))));
      continue;
    }
    if (key === 'upSql' && typeof value === 'string') {
      rows.push(h('div.ai-preview-sql', {},
        h('dt', {}, '실행될 SQL'),
        h('dd', {}, h('pre.sql-block', {}, value))));
      continue;
    }
    if (key === 'precheck' && typeof value === 'object') {
      const blockers = value.blockers ?? [];
      const warnings = value.warnings ?? [];
      rows.push(h('div', {},
        h('dt', {}, '사전 검사'),
        h('dd', {},
          value.ok ? badge('실행 가능', 'success') : badge('실행 불가', 'danger'),
          blockers.length
            ? h('ul.note-list', {}, blockers.map((b) => h('li.text-danger', {}, b)))
            : null,
          warnings.length
            ? h('ul.note-list', {}, warnings.map((w) => h('li', {}, w)))
            : null)));
      continue;
    }
    if (typeof value === 'object') {
      push(key, h('code', {}, truncate(JSON.stringify(value), 200)));
      continue;
    }
    push(key, String(value));
  }
  return h('dl.ai-preview', {}, rows);
}

// ---------- 프로바이더 / 툴 대화상자 ----------

async function openProviderDialog(canManage, nav) {
  const box = h('div', {}, spinner('불러오는 중…'));
  let changed = false;
  openModal({
    title: 'AI 프로바이더',
    width: 680,
    body: () => box,
    // 프로바이더가 생기면 어시스턴트 화면 자체가 달라진다("키 없음" → 대화 가능).
    // 대화상자를 닫을 때 한 번만 다시 그린다.
    onClose: () => {
      if (changed) nav.refresh();
    },
  });

  const reload = async ({ mutated = false } = {}) => {
    if (mutated) changed = true;
    try {
      const res = await api.get('/ai/providers');
      mount(box, providerList(res, canManage, reload));
    } catch (err) {
      mount(box, errorPanel(err));
    }
  };
  await reload();
}

function providerList(res, canManage, reload) {
  return h('div', {},
    res.items.length === 0
      ? h('p.muted', {}, '등록된 프로바이더가 없습니다.')
      : h('div.ai-provider-list', {}, res.items.map((p) => h('div.card.ai-provider', {},
        h('div.ai-provider-head', {},
          h('strong', {}, p.name),
          badge(p.provider === 'anthropic' ? 'Anthropic' : 'OpenAI 호환', 'accent'),
          p.isDefault ? badge('기본', 'info') : null,
          p.enabled ? null : badge('비활성', 'neutral'),
          p.hasKey ? badge('키 설정됨', 'success') : badge('키 없음', 'danger'),
          p.lastCheckAt
            ? (p.lastCheckOk ? badge('확인됨', 'success') : badge('확인 실패', 'danger'))
            : null,
        ),
        h('dl.mig-meta', {},
          h('div.meta-row', {}, h('dt', {}, '기본 모델'), h('dd', {}, p.defaultModel || '—')),
          h('div.meta-row', {}, h('dt', {}, '허용 모델'),
            h('dd', {}, (p.models ?? []).length === 0
              ? h('span.muted', {}, '제한 없음')
              : (p.models ?? []).map((m) => h('code.ai-model-chip', {}, m)))),
          h('div.meta-row', {}, h('dt', {}, '주소'),
            h('dd', {}, p.baseUrl ? h('code', {}, p.baseUrl) : h('span.muted', {}, '기본'))),
        ),
        p.lastCheckMsg ? h('p.muted', {}, p.lastCheckMsg) : null,
        canManage
          ? h('div.row-actions', {},
            h('button.btn.btn-small', {
              type: 'button',
              onclick: (e) => testProvider(p, e.currentTarget, reload),
            }, icon('play'), '연결 확인'),
            h('button.btn.btn-small', {
              type: 'button', onclick: () => openProviderForm(res, p, reload),
            }, icon('edit'), '수정'),
            h('button.btn.btn-small.btn-danger-ghost', {
              type: 'button',
              onclick: async () => {
                const ok = await confirmDialog({
                  title: '프로바이더 삭제',
                  message: `"${p.name}" 을 삭제합니다. 저장된 API 키도 함께 지워집니다.`,
                  confirmLabel: '삭제', danger: true,
                });
                if (!ok) return;
                try {
                  await api.del(`/ai/providers/${encodeURIComponent(p.id)}`);
                  toast('삭제했습니다', 'success');
                  reload({ mutated: true });
                } catch (err) {
                  toastError(err);
                }
              },
            }, icon('trash'), '삭제'),
          )
          : null,
      ))),
    canManage
      ? h('button.btn.btn-primary', {
        type: 'button', onclick: () => openProviderForm(res, null, reload),
      }, icon('plus'), '프로바이더 추가')
      : h('p.notice.notice-info', {}, icon('alert'),
        '추가·수정은 커넥션 관리 권한(어드민 이상)이 필요합니다.'),
  );
}

async function testProvider(p, btn, reload) {
  btn.disabled = true;
  const original = btn.textContent;
  btn.textContent = '확인 중…';
  try {
    const res = await api.post(`/ai/providers/${encodeURIComponent(p.id)}/test`);
    toast(`연결 확인 성공 — 모델 ${res.models.length}개`, 'success');
    if (res.warnings?.length) {
      toast(res.warnings.join(' / '), 'error', 6000);
    }
  } catch (err) {
    toast(err.detail ? `${err.message}: ${err.detail}` : (err.message ?? '실패'), 'error', 6000);
  } finally {
    btn.disabled = false;
    btn.textContent = original;
    reload();
  }
}

function openProviderForm(meta, existing, reload) {
  const isEdit = Boolean(existing);
  const nameInput = input({ value: existing?.name ?? '' });
  const kindSelect = select((meta.kinds ?? []).map((k) => ({ value: k.value, label: k.label })),
    { value: existing?.provider ?? 'anthropic' });
  const baseInput = input({ value: existing?.baseUrl ?? '' });
  const keyInput = input({
    type: 'password', autocomplete: 'new-password',
    placeholder: isEdit ? '(변경하지 않으려면 비워 두세요)' : 'API 키',
  });
  const enabledBox = h('input', { type: 'checkbox', checked: existing ? existing.enabled : true });
  const defaultBox = h('input', { type: 'checkbox', checked: existing ? existing.isDefault : true });
  const hints = h('div.vcs-hints');

  // 주소 프리셋. 특히 Google의 호환 엔드포인트는 경로가 /v1beta/openai 라서
  // 손으로 적으면 틀리기 쉽고, 틀리면 404만 보여 원인을 찾기 어렵다.
  const presets = meta.presets ?? [];
  const presetSelect = select(
    [{ value: '', label: '직접 입력' },
      ...presets.map((p, i) => ({ value: String(i), label: p.label }))],
    { value: '' },
  );
  presetSelect.addEventListener('change', () => {
    const p = presets[Number(presetSelect.value)];
    if (!p) return;
    kindSelect.value = p.provider;
    baseInput.value = p.baseUrl;
    refresh();
  });

  // ---------- 허용 모델 ----------
  //
  // 목록이 비어 있으면 제한이 없다. 채우면 그 안에서만 고를 수 있고, 기본 모델도
  // 그중 하나여야 한다 — 아무도 고르지 않은 모델이 모든 새 대화에 쓰이면
  // 목록을 정한 의미가 없어진다.
  let allowed = [...(existing?.models ?? [])];
  let fetched = [];
  let filter = '';
  let defaultModel = existing?.defaultModel ?? '';

  // 조작 칸은 한 번만 만든다. 목록을 다시 그릴 때마다 새로 만들면 사용자가
  // 입력하던 글자와 포커스가 사라진다 — 거르기 상자에서 특히 티가 난다.
  const listBox = h('div');
  const countLine = h('p.field-help');
  const filterInput = input({ placeholder: '모델 이름으로 거르기' });
  const filterRow = h('div.ai-model-filter', {}, filterInput);
  const manualInput = input({ placeholder: '모델 이름을 직접 입력' });
  const defaultModelField = h('div');

  const renderDefault = () => {
    if (allowed.length === 0) {
      const free = input({ value: defaultModel, placeholder: '예: gpt-4o-mini' });
      free.addEventListener('input', () => { defaultModel = free.value; });
      mount(defaultModelField,
        free,
        h('span.field-help', {}, '허용 모델을 고르지 않으면 이 모델 하나만 쓰입니다'));
      return;
    }
    if (!allowed.includes(defaultModel)) defaultModel = allowed[0];
    const sel = select(allowed.map((m) => ({ value: m, label: m })), { value: defaultModel });
    sel.addEventListener('change', () => { defaultModel = sel.value; });
    mount(defaultModelField, sel,
      h('span.field-help', {}, '새 대화가 기본으로 쓰는 모델입니다 (대화마다 바꿀 수 있습니다)'));
  };

  const renderList = () => {
    // 고른 것과 불러온 것을 합쳐 보여준다. 고른 모델이 목록에 없을 수도 있다
    // (직접 입력했거나, 프로바이더가 모델을 정리했거나).
    const all = [...new Set([...allowed, ...fetched])];
    const shown = filter
      ? all.filter((m) => m.toLowerCase().includes(filter.toLowerCase()))
      : all;

    filterRow.hidden = all.length <= 12;
    mount(listBox, all.length === 0
      ? h('p.muted.small', {},
        '아직 목록이 없습니다. "모델 불러오기"로 이 키가 쓸 수 있는 모델을 확인하거나 직접 입력하세요. ' +
        '비워 두면 모델을 제한하지 않습니다.')
      : h('div.ai-model-list', {}, shown.map((m) => {
        const box = h('input', { type: 'checkbox', checked: allowed.includes(m) });
        box.addEventListener('change', () => {
          if (box.checked) {
            if (!allowed.includes(m)) allowed.push(m);
          } else {
            allowed = allowed.filter((x) => x !== m);
          }
          renderCount();
          renderDefault();
        });
        return h('label.checkbox', {}, box, h('code', {}, m));
      })));
    renderCount();
  };

  const renderCount = () => {
    mount(countLine, `선택 ${allowed.length}개` + (allowed.length === 0 ? ' — 제한 없음' : ''));
  };

  filterInput.addEventListener('input', () => {
    filter = filterInput.value;
    renderList();
  });

  const addManual = () => {
    const name = manualInput.value.trim();
    if (name === '') return;
    if (!allowed.includes(name)) allowed.push(name);
    manualInput.value = '';
    renderList();
    renderDefault();
  };
  manualInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') { e.preventDefault(); addManual(); }
  });

  const modelsBox = h('div.ai-model-picker', {},
    h('div.row-actions', {},
      h('button.btn.btn-small', {
        type: 'button',
        onclick: (e) => loadModels(e.currentTarget),
      }, icon('play'), '모델 불러오기'),
      manualInput,
      h('button.btn.btn-small', { type: 'button', onclick: addManual }, icon('plus'), '추가'),
    ),
    filterRow,
    listBox,
    countLine,
  );

  const loadModels = async (btn) => {
    btn.disabled = true;
    const original = btn.textContent;
    btn.textContent = '불러오는 중…';
    try {
      const res = await api.post('/ai/providers/models', {
        id: existing?.id ?? '',
        provider: kindSelect.value,
        baseUrl: baseInput.value,
        apiKey: keyInput.value.trim(),
      });
      fetched = res.models ?? [];
      if (!res.supported) {
        toast('이 엔드포인트는 모델 목록을 제공하지 않습니다. 직접 입력하세요', 'error', 6000);
      } else {
        toast(`모델 ${fetched.length}개를 불러왔습니다`, 'success');
      }
      renderList();
    } catch (err) {
      toast(err.detail ? `${err.message}: ${err.detail}` : (err.message ?? '실패'), 'error', 6000);
    } finally {
      btn.disabled = false;
      btn.textContent = original;
    }
  };

  const refresh = () => {
    const k = (meta.kinds ?? []).find((x) => x.value === kindSelect.value);
    mount(hints, k ? h('ul.note-list', {},
      h('li', {}, `주소: ${k.baseHint}`),
      h('li', {}, `키: ${k.keyHint}`),
      h('li', {}, `모델: ${k.modelHint}`)) : null);
  };
  kindSelect.addEventListener('change', refresh);

  openModal({
    title: isEdit ? '프로바이더 수정' : '프로바이더 추가',
    width: 620,
    body: () => {
      setTimeout(() => { refresh(); renderList(); renderDefault(); }, 0);
      return [
        h('label.field', {}, h('span.field-label', {}, '이름'), nameInput),
        h('label.field', {}, h('span.field-label', {}, '종류'), kindSelect),
        h('label.field', {}, h('span.field-label', {}, '서비스 프리셋'), presetSelect,
          h('span.field-help', {}, '고르면 종류와 주소가 채워집니다')),
        hints,
        h('label.field', {}, h('span.field-label', {}, '주소 (선택)'), baseInput,
          h('span.field-help', {},
            'http는 사설망 주소에만 허용됩니다 — API 키가 평문으로 전송되기 때문입니다')),
        h('label.field', {}, h('span.field-label', {}, 'API 키'), keyInput,
          h('span.field-help', {}, '암호화되어 저장되며 다시 표시되지 않습니다')),
        h('div.field', {}, h('span.field-label', {}, '허용 모델'), modelsBox),
        h('div.field', {}, h('span.field-label', {}, '기본 모델'), defaultModelField),
        h('label.checkbox', {}, enabledBox, h('span', {}, '활성')),
        h('label.checkbox', {}, defaultBox, h('span', {}, '새 대화의 기본 프로바이더로 사용')),
      ];
    },
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: async (e) => {
          // currentTarget 은 이벤트가 끝나면 null 이 된다. await 뒤에서 다시 만지면
          // 그 자리에서 예외가 나고, 그러면 실패를 알리는 토스트조차 뜨지 않는다 —
          // 눌렀는데 아무 일도 일어나지 않은 것처럼 보인다.
          const pressed = e.currentTarget;
          pressed.disabled = true;
          const payload = {
            name: nameInput.value, provider: kindSelect.value,
            baseUrl: baseInput.value, defaultModel: defaultModel,
            models: allowed,
            enabled: enabledBox.checked, isDefault: defaultBox.checked,
          };
          if (keyInput.value.trim() !== '') payload.apiKey = keyInput.value;
          try {
            if (isEdit) {
              await api.put(`/ai/providers/${encodeURIComponent(existing.id)}`, payload);
            } else {
              await api.post('/ai/providers', payload);
            }
            close();
            toast(isEdit ? '수정했습니다' : '추가했습니다', 'success');
            reload({ mutated: true });
          } catch (err) {
            pressed.disabled = false;
            toastError(err);
          }
        },
      }, isEdit ? '저장' : '추가'),
    ],
  });
}

function openToolsDialog(data) {
  const readTools = data.tools.filter((t) => !t.mutating);
  const writeTools = data.tools.filter((t) => t.mutating);
  openModal({
    title: `어시스턴트가 쓸 수 있는 툴 ${data.tools.length}개`,
    width: 680,
    body: () => [
      h('p.notice.notice-info', {}, icon('alert'),
        '툴은 지금 로그인한 사용자의 권한으로 실행됩니다. ' +
        '권한이 없는 DB의 정보는 어시스턴트도 볼 수 없습니다.'),
      h('h3.erd-sub', {}, `조회 ${readTools.length}개`),
      h('ul.ai-tool-list', {}, readTools.map((t) =>
        h('li', {}, h('code', {}, t.name), ' — ', t.description))),
      h('h3.erd-sub', {}, `변경 ${writeTools.length}개 (승인 필요)`),
      h('p.field-help', {},
        '아래 툴은 어시스턴트가 직접 실행할 수 없습니다. 제안을 만들고 사용자가 승인해야 실행됩니다.'),
      h('ul.ai-tool-list', {}, writeTools.map((t) =>
        h('li', {}, h('code', {}, t.name), ' ', badge('승인 필요', 'warn'), ' — ', t.description))),
    ],
  });
}

// ---------- 유틸 ----------

function truncate(s, max) {
  const str = String(s ?? '');
  return str.length > max ? `${str.slice(0, max - 1)}…` : str;
}
