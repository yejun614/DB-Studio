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
import { navigate, currentPath, setLeaveGuard } from '../core/router.js';
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
  // askClose는 지금 이 창 안의 대화에게 "닫아도 되나"를 묻는 함수다.
  // 대화를 바꿀 때마다 새로 받는다(아래 guard).
  let askClose = null;
  const panel = openFloatPanel({
    id: ASSISTANT_PANEL,
    title: 'AI 어시스턴트',
    iconName: 'sparkles',
    width: 620,
    height: 680,
    onClose: () => cleanup?.(),
    beforeClose: () => (askClose ? askClose() : true),
    render: (body, handle) => {
      const show = async (id) => {
        cleanup?.();
        askClose = null;
        handle.sessionId = id;
        cleanup = await mountAssistant(body, {
          sessionId: id,
          // 팝업의 X 는 라우터를 지나지 않는다. 그래서 닫기를 막는 길을 따로 받는다.
          guard: (fn) => { askClose = fn; },
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
  sessionId: wanted = '', nav, compact = false, panel = null, guard = null,
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

  // 대화가 없어도, 프로바이더가 없어도 **키 관리와 툴 목록에는 닿을 수 있어야 한다.**
  //
  // 팝업(compact)에는 사이드바가 늘 보이지 않고 머리글의 "대화" 단추로만 열리는데,
  // 그 머리글은 대화가 있을 때만 그려졌다. 그래서 처음 켠 사람 — 대화도 키도 없는
  // 바로 그 사람 — 이 키를 넣을 자리도 툴 목록도 찾을 수 없었다. 넓은 화면에서는
  // 사이드바가 그대로 있으므로 이 머리글은 팝업에서만 붙인다.
  const emptyMain = (...body) => {
    mount(ui.main, compact ? [emptyHead(ui), ...body] : body);
  };

  if (data.providers.length === 0) {
    emptyMain(noProviderView(canManageKeys, nav));
    return () => {};
  }
  if (!sessionId) {
    emptyMain(emptyState('대화를 시작하려면 새 대화를 만드세요.',
      h('button.btn.btn-primary', {
        type: 'button', onclick: () => createSession(data, conns, nav),
      }, icon('plus'), '새 대화')));
    return () => {};
  }
  if (!data.sessions.some((s) => s.id === sessionId)) {
    // 목록은 본인 세션만 담고 있다. 없다면 남의 세션이거나 이미 삭제된 것이다.
    emptyMain(errorPanel({
      message: '이 대화를 찾을 수 없습니다',
      detail: '삭제되었거나 다른 사용자의 대화입니다.',
    }));
    return () => {};
  }

  const chat = new ChatView(sessionId, data, conns, ui, nav);
  chat.compact = compact;
  await chat.load();
  // 답변을 받는 중에 나가려 하면 물어본다.
  //
  // 세 갈래를 각각 막아야 한다. (1) 팝업의 X — 라우터를 지나지 않으므로 부르는
  // 쪽이 준 guard 로 막는다. (2) 앱 안에서 다른 화면으로 이동 — 라우터의
  // leaveGuard 다. 팝업에는 걸지 않는다(팝업은 화면이 바뀌어도 남으므로 이동이
  // 스트림을 끊지 않는다). (3) 새로고침·창 닫기 — beforeunload 다.
  guard?.(() => chat.confirmAbort());
  if (!panel) setLeaveGuard(() => chat.confirmAbort());
  const onBeforeUnload = (e) => {
    if (!chat.streamingNow()) return;
    e.preventDefault();
    // 브라우저는 우리 문장을 보여주지 않는다(자기 문구를 쓴다). 그래도 값을
    // 채워야 물어보는 브라우저가 있다.
    e.returnValue = '';
  };
  window.addEventListener('beforeunload', onBeforeUnload);

  return () => {
    window.removeEventListener('beforeunload', onBeforeUnload);
    if (!panel) setLeaveGuard(null);
    chat.stop();
  };
}

// emptyHead는 대화가 없을 때의 머리글이다(팝업 전용).
//
// 여기 있는 "대화" 단추 하나가 사이드바를 겹쳐 연다 — 그 안에 새 대화·AI 키 관리·
// 툴 목록이 모두 있다. 세 가지를 여기에 다시 늘어놓지 않는 이유: 같은 것을 두 곳에
// 그리면 하나가 늘거나 이름이 바뀔 때 한쪽만 낡는다.
function emptyHead(ui) {
  return h('header.ai-head.is-compact', {},
    h('div.ai-head-main', {},
      h('button.btn.btn-small', {
        type: 'button', title: '대화 목록 · AI 키 · 툴',
        onclick: () => ui.openSessions?.(),
      }, icon('list'), '대화'),
      h('h1.ai-title', {}, 'AI 어시스턴트'),
    ));
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
      // 키 관리는 커넥션 관리자만 보인다. 그 창에서 할 수 있는 일(추가·수정·삭제·
      // 연결 확인)이 전부 그 권한을 요구해서, 없는 사람에게는 열어 봐야 읽을 것만
      // 남는다 — 게다가 그 읽을 것은 남의 API 키 설정(주소·키 유무)이다.
      //
      // 툴 목록은 그대로 둔다. 그것은 "이 어시스턴트가 무엇을 할 수 있는가"이고,
      // 쓰는 사람 모두가 알아야 하는 것이다.
      canManageKeys
        ? h('button.btn.btn-small.btn-block', {
          type: 'button', onclick: () => openProviderDialog(canManageKeys, nav),
        }, icon('key'), 'AI 키 관리')
        : null,
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
    //
    // once 로 두지 않는다: 이 창 안에는 대화 말고도 누를 것이 있다(새 대화·AI 키
    // 관리·툴 목록). 그중 하나를 먼저 누르면 그 한 번으로 이 감시가 사라져서,
    // 그다음에 대화를 골라도 창이 열린 채 남는다.
    const onPick = (e) => {
      if (!e.target.closest('.ai-session')) return;
      sidebar.removeEventListener('click', onPick);
      close();
    };
    sidebar.addEventListener('click', onPick);
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
    // follow는 "새 내용을 따라 맨 아래로 내릴까"다.
    //
    // 예전에는 이벤트가 올 때마다 무조건 내렸다. 그러면 위로 올려 읽던 대화가
    // 토큰마다 끌려 내려와, 생각이 길어지는 동안에는 기록을 읽을 수가 없다.
    // 지금은 사람이 자기 말을 보낼 때 켜고(그때는 맨 아래를 보고 싶은 것이
    // 분명하다), 위로 올리는 순간 끈다.
    this.follow = true;
    // logTop은 다시 그릴 때 되돌릴 스크롤 자리다. 대화 전체를 다시 만들면
    // 스크롤이 맨 위로 돌아가는데, 따라가지 않는 사람에게 그것은 읽던 자리를
    // 잃는 일이다.
    this.logTop = 0;
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

  // streamingNow는 지금 답변을 받고 있는지다.
  streamingNow() {
    return Boolean(this.controller);
  }

  // confirmAbort는 답변을 받는 중에 나가려 할 때 묻는다.
  //
  // true 를 돌려주면 나간다(스트림은 정리 함수가 끊는다). false 면 그대로 남는다.
  //
  // 왜 묻는가: 툴을 쓰는 답변은 몇 분이 걸린다. 그 동안 창을 정리하려고 X 를
  // 누르거나 다른 화면을 열면 답변이 조용히 중단되었고, 무엇을 잃었는지도
  // 화면에 남지 않았다.
  async confirmAbort() {
    if (!this.streamingNow()) return true;
    return new Promise((resolve) => {
      // 답을 한 번만 낸다. openModal 의 onClose 는 footer 단추가 close() 를 부를
      // 때도 함께 울리므로, 표시가 없으면 "계속 받기"가 언제나 먼저 결정된다.
      let answered = false;
      const done = (value, close) => {
        answered = true;
        close();
        resolve(value);
      };
      openModal({
        title: '답변을 받는 중입니다',
        width: 460,
        body: () => h('p.modal-message', {},
          // 여기까지 온 것은 남는다 — 서버는 클라이언트가 사라지면 그때까지의
          // 답변을 저장한다(agentRun 의 gone 처리). 남은 답변은 만들어지지 않는다.
          '지금 나가면 답변 생성이 중단됩니다. 여기까지 받은 내용은 대화에 남고, '
          + '나머지는 만들어지지 않습니다.'),
        footer: (close) => [
          h('button.btn', {
            type: 'button',
            onclick: () => done(false, close),
          }, '계속 받기'),
          h('button.btn.btn-danger', {
            type: 'button',
            onclick: () => done(true, close),
          }, '중단하고 나가기'),
        ],
        // X 나 Esc 로 이 창을 닫는 것은 "아직 결정하지 않았다"다. 그때 나가 버리면
        // 물어본 의미가 없다.
        onClose: () => { if (!answered) resolve(false); },
      });
    });
  }

  render() {
    const conn = this.conns.items.find((i) => i.connection.id === this.session.connectionId);
    // 지금 보고 있던 자리를 기억해 둔다(아래 mount 가 이 요소를 버린다).
    if (this.log) this.logTop = this.log.scrollTop;
    this.log = h('div.ai-log');
    // 위로 올리면 따라가기를 멈추고, 바닥까지 내리면 다시 켠다.
    //
    // **방향**으로 판단하는 이유: 내용이 자라도 스크롤 이벤트가 온다. 그때의
    // 자리만 보고 판단하면, 바닥에 있는데도 방금 늘어난 만큼(여유보다 큰 간격)을
    // 보고 따라가기가 꺼진다. 반대로 위로 움직이는 것은 사람만 한다 — 아래에
    // 글이 붙는 것으로는 스크롤 위치가 위로 가지 않는다.
    //
    // 휠·손가락·스크롤바 끌기·키보드를 따로 듣지 않아도 되는 것이 이 방식의
    // 이점이다. 무엇으로 올렸든 위치가 줄어든다.
    let lastTop = 0;
    this.log.addEventListener('scroll', () => {
      const top = this.log.scrollTop;
      // 2px 여유: 부드러운 스크롤과 소수점 때문에 1px씩 오르내리는 일이 있다.
      if (top < lastTop - 2) this.follow = false;
      else if (atLogBottom(this.log)) this.follow = true;
      lastTop = top;
    }, { passive: true });
    this.inputBox = h('textarea.input.ai-input', {
      rows: 3,
      placeholder: '무엇을 도와드릴까요? (Enter 전송, Shift+Enter 줄바꿈)',
    });
    // 입력 줄은 상태에 따라 달라진다(보내기 ↔ 중단). 그 자리를 따로 들고 있으면
     // 스트림이 시작·끝날 때 그 줄만 다시 그릴 수 있다 — 대화 전체를 다시 그리면
     // 스크롤이 움직이고, 답변이 오는 동안 그것이 계속 일어난다.
    this.composer = h('div.ai-composer');

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

    mount(this.ui.main, head, this.log, this.composer);

    this.renderComposer();
    this.renderLog();
    this.inputBox.focus();
  }

  // renderComposer는 입력 줄을 지금 상태에 맞춘다.
  //
  // 답변을 받는 동안 단추가 **중단**이 되는 이유: 예전에는 보내기가 회색으로
  // 죽어 있을 뿐이어서, 잘못 물어본 것을 알아차려도 끝까지 기다리거나 창을
  // 닫아야 했다. 툴을 쓰는 답변은 몇 분이 걸린다.
  //
  // 한 단추가 두 일을 하는 것이 맞는 자리다 — 글자가 바뀌는 것이 상태가 아니라
  // **지금 할 수 있는 일**이고, 그 둘은 동시에 성립하지 않는다(보내는 중에는
  // 보낼 수 없다).
  renderComposer() {
    if (!this.composer) return;
    this.sendBtn = this.streamingNow()
      ? h('button.btn.btn-danger', {
        type: 'button', title: '지금까지 받은 내용은 대화에 남습니다',
        onclick: () => this.abort(),
      }, icon('stop'), '중단')
      : h('button.btn.btn-primary', {
        type: 'button', onclick: () => this.send(),
      }, icon('play'), '보내기');
    mount(this.composer, this.inputBox, this.sendBtn);
  }

  // abort는 받고 있는 답변을 끊는다.
  //
  // 서버는 상대가 사라진 것을 다음 쓰기에서 알고 그때까지의 답변을 저장한다
  // (agentRun 의 gone 처리). 그래서 여기서는 끊기만 하면 된다 — send() 의
  // finally 가 저장된 상태로 다시 읽어 온다.
  abort() {
    if (!this.controller) return;
    this.controller.abort();
    toast('답변을 중단했습니다. 여기까지 받은 내용은 대화에 남습니다', 'info');
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
    // 고를 수 있는 것을 프로바이더별로 모은다.
    //
    // 예전에는 **지금 프로바이더의 모델이 둘 이상일 때만** 고르개가 떴다. 그래서
    // 모델을 하나만 등록한 사람이나 목록을 비워 둔 사람(제한 없음)에게는 대화
    // 도중에 모델을 바꿀 길이 아예 없었다 — 하필 아무 모델이나 쓸 수 있는 쪽이다.
    //
    // 프로바이더까지 함께 고르는 이유: 모델을 바꾸는 까닭은 대개 "이 대목은 더
    // 좋은 모델로"인데, 그 좋은 모델은 다른 키(다른 프로바이더)에 있는 경우가
    // 많다. 세션은 프로바이더와 모델을 함께 들고 있고 서버도 그 조합으로 판정하므로
    // (checkSessionModel), 화면에서 둘을 나눠 고르게 할 이유가 없다.
    const providers = this.data.providers ?? [];
    const provider = providers.find((p) => p.id === this.session.providerId) ?? providers[0];
    const current = this.session.model || provider?.defaultModel || '';
    const currentKey = `${provider?.id ?? ''}|${current}`;

    const options = [];
    for (const p of providers) {
      const models = p.models?.length ? p.models : [p.defaultModel].filter(Boolean);
      for (const m of models) {
        options.push({
          value: `${p.id}|${m}`,
          label: providers.length > 1 ? `${p.name} · ${m}` : m,
        });
      }
      // 목록을 비워 둔 프로바이더는 제한이 없다(modelAllowed). 그때는 이름을
      // 직접 적을 수 있어야 한다 — 로컬 LLM 은 모델이 수십 개다.
      if (!p.models?.length) {
        options.push({
          value: `${p.id}|`,
          label: providers.length > 1 ? `${p.name} · 직접 입력…` : '직접 입력…',
        });
      }
    }
    // 지금 쓰는 조합이 목록에 없으면(모델을 지운 뒤) 그것도 넣어 준다. 없으면
    // 고르개가 엉뚱한 값을 가리키고, 건드리지도 않았는데 모델이 바뀐다.
    if (current && !options.some((o) => o.value === currentKey)) {
      options.unshift({ value: currentKey, label: `${current} (지금)` });
    }
    if (options.length === 0) return null;
    if (options.length === 1 && !options[0].value.endsWith('|')) {
      return current ? h('code.muted', {}, current) : null;
    }

    const sel = select(options, { value: currentKey });
    sel.classList.add('ai-model-switch');
    sel.title = '이 대화에 쓸 모델';
    sel.addEventListener('change', async () => {
      const [providerId, picked] = sel.value.split('|');
      const model = picked || await this.askModelName(providerId);
      if (!model) {
        sel.value = currentKey;
        return;
      }
      try {
        const res = await api.patch(
          `/ai/sessions/${encodeURIComponent(this.session.id)}`, { providerId, model });
        this.session = res.session;
        toast(`모델을 ${model} 로 바꿨습니다`, 'success');
        // 머리글의 프로바이더 배지와 고르개를 지금 세션에 맞춘다. 답변을 받는
        // 중이어도 안전하다 — 그 말풍선 노드는 다시 쓰인다(renderLog).
        this.render();
      } catch (err) {
        sel.value = currentKey;
        toastError(err);
      }
    });
    return sel;
  }

  // askModelName은 모델 이름을 직접 받는다(제한을 두지 않은 프로바이더).
  askModelName(providerId) {
    const p = (this.data.providers ?? []).find((x) => x.id === providerId);
    const box = input({ value: '', placeholder: p?.defaultModel || '예: llama3.1:70b' });
    return new Promise((resolve) => {
      let answered = false;
      const done = (value, close) => {
        answered = true;
        close();
        resolve(value);
      };
      openModal({
        title: '모델 이름',
        width: 420,
        body: () => [
          h('label.field', {}, h('span.field-label', {}, `${p?.name ?? '프로바이더'} 의 모델`), box),
          h('p.field-help', {},
            '이 프로바이더에는 허용 모델 목록이 없어 이름을 직접 적습니다. '
            + '틀린 이름은 다음 질문에서 프로바이더가 거부합니다.'),
        ],
        footer: (close) => [
          h('button.btn', { type: 'button', onclick: () => done('', close) }, '취소'),
          h('button.btn.btn-primary', {
            type: 'button', onclick: () => done(box.value.trim(), close),
          }, '적용'),
        ],
        onClose: () => { if (!answered) resolve(''); },
      });
    });
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
    // 접어 둔 옛 대화가 있으면 맨 위에 알린다.
    //
    // 보여 주는 이유: 접기는 되돌릴 수 없다. 무엇이 남았는지 사람이 볼 수 있어야
    // "그 이야기는 왜 잊었지?"에 답할 수 있고, 요약이 중요한 것을 빠뜨렸다면
    // 다시 말해 줄 수 있다.
    if (this.session?.summary) {
      items.push(h('details.ai-summary', {},
        h('summary', {}, icon('list', 13), '이전 대화 요약 (문맥이 차서 접었습니다)'),
        h('div.ai-summary-body', {}, renderMarkdown(this.session.summary))));
    }
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
    // 따라가는 중이면 맨 아래로, 아니면 읽던 자리로.
    if (this.follow) toLogEnd(this.log);
    else if (this.logTop) this.log.scrollTop = this.logTop;
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
    this.streaming = createStreamingNode();
    // 내 말을 보낸 순간은 맨 아래를 보고 싶은 것이 분명하다. 답변이 자라는
    // 동안에는 위로 올리는 순간 멈춘다(위 follow 주석).
    this.follow = true;
    this.renderLog();
    // 생각을 글로 보내지 않는 모델을 위해, 잠깐 기다려도 소식이 없으면
    // 그때 "생각 중"을 띄운다. 바로 띄우면 빠른 답에서도 깜박여 어수선하다.
    const waitTimer = setTimeout(() => this.streaming?.waiting(), 1500);

    this.controller = new AbortController();
    // 여기서부터 단추는 **중단**이다.
    this.renderComposer();
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
      this.renderComposer();
      // 시계를 멈춘다. 끊겼든 끝났든, 안 멈추면 화면을 떠난 뒤에도 계속 돈다.
      this.streaming?.stop();
      clearTimeout(waitTimer);
      this.streaming = null;
      // 스트림이 끝나면 서버에 저장된 상태로 다시 그린다 — 토큰 사용량과
      // 보류 제안이 정확히 반영된다.
      await this.load();
    }
  }

  handleEvent(ev) {
    if (!ev || !ev.event) return;
    switch (ev.event) {
      case 'thinking':
        this.streaming?.appendThinking(ev.data.text ?? '');
        break;
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
    this.followDown();
  }

  // followDown은 따라가는 중이면 맨 아래로 내린다.
  //
  // 한 프레임 뒤에 내리는 이유: 말풍선은 토큰이 올 때마다 requestAnimationFrame 으로
  // 다시 그린다(createStreamingNode). 지금 당장 내리면 그 그리기 **전의** 높이로
  // 내려서, 새로 늘어난 만큼이 늘 화면 밖에 남는다 — 따라간다면서 조금씩 뒤처진다.
  followDown() {
    // 바닥에 닿아 있으면 따라가기를 다시 켠다.
    //
    // 스크롤 이벤트만으로 켜지 않는 이유: 사람이 바닥까지 내려 놓고 다음 글자를
    // 기다리는 동안에는 스크롤 이벤트가 오지 않는다. 그 사이에 내용이 자라면
    // "바닥에 있는데 따라오지 않는" 상태로 남는다.
    if (!this.follow && atLogBottom(this.log)) this.follow = true;
    if (!this.follow || this.scrollFrame) return;
    this.scrollFrame = requestAnimationFrame(() => {
      this.scrollFrame = 0;
      if (this.follow && this.log) toLogEnd(this.log);
    });
  }
}

// atLogBottom은 대화가 맨 아래에 닿아 있는지다.
//
// 여유를 두는 이유: 줄 높이와 소수점 때문에 정확히 0이 되는 일은 드물고, 몇 px
// 차이로 따라가기가 꺼지면 "왜 어떤 때는 따라가고 어떤 때는 안 따라가나"가 된다.
const LOG_BOTTOM_SLACK = 60;

function atLogBottom(el) {
  if (!el) return true;
  return el.scrollHeight - el.scrollTop - el.clientHeight <= LOG_BOTTOM_SLACK;
}

// toLogEnd는 맨 아래로 **즉시** 내린다.
//
// .ai-log 에는 scroll-behavior: smooth 가 걸려 있다(사람이 어딘가로 뛸 때 부드럽게
// 가는 것이 좋다). 그런데 따라가기는 토큰마다 일어나므로, 부드럽게 두면 애니메이션이
// 끝나기 전에 다음 글자가 와서 계속 뒤처진다 — 따라가는데 바닥에 닿지 않는다.
function toLogEnd(el) {
  if (!el) return;
  el.scrollTo({ top: el.scrollHeight, behavior: 'instant' });
}

// createStreamingNode는 응답 중인 메시지 노드와 갱신 함수를 만든다.
//
// 전체를 다시 그리지 않고 이 노드만 갱신하는 이유: 토큰이 도착할 때마다 대화 전체를
// 재생성하면 스크롤 위치가 튀고 긴 대화에서 눈에 보이게 느려진다.
function createStreamingNode() {
  const bubble = h('div.ai-bubble.is-markdown.is-streaming', {}, h('span.ai-cursor', {}, '▌'));
  const toolBox = h('div.ai-stream-tools');

  // 생각 표시.
  //
  // 왜 필요한가: 생각이 긴 모델은 첫 글자가 나오기까지 수십 초가 걸린다. 그동안
  // 화면에는 깜박이는 커서 하나뿐이라, 생각하는 중인지 멈춘 것인지 알 수 없었다.
  // 흐른 시간을 함께 보이는 이유도 같다 — "3초"와 "40초"는 기다릴지 그만둘지를
  // 가르는 차이인데, 점 세 개는 그 둘을 똑같이 보이게 한다.
  const thinkTime = h('span.ai-think-time', {}, '0초');
  const thinkBody = h('pre.ai-think-body');
  const thinkBox = h('details.ai-think', { hidden: true },
    h('summary', {}, icon('activity', 13),
      h('span.ai-think-label', {}, '생각 중'), thinkTime),
    thinkBody);

  const node = h('div.ai-msg.is-assistant', {}, thinkBox, bubble, toolBox);
  let text = '';
  let thinking = '';
  let frame = 0;
  let thinkFrame = 0;
  const toolNodes = new Map();

  const startedAt = Date.now();
  const tick = () => {
    thinkTime.textContent = `${Math.round((Date.now() - startedAt) / 1000)}초`;
  };
  const timer = setInterval(tick, 1000);
  // 답이 오기 시작하면 시계를 멈추고 "생각 12초"로 굳힌다. 계속 세면 다 끝난
  // 뒤에도 숫자가 올라가 아직 무언가 하고 있는 것처럼 보인다.
  const settle = () => {
    if (!timer) return;
    clearInterval(timer);
    thinkBox.open = false;
    const label = thinkBox.querySelector('.ai-think-label');
    if (label) label.textContent = '생각함';
  };

  // 도착한 조각마다 마크다운을 다시 그리되 프레임당 한 번으로 묶는다.
  // 토큰은 프레임보다 훨씬 빠르게 오므로 그대로 두면 같은 화면을 수십 번 그린다.
  const paint = () => {
    frame = 0;
    mount(bubble, renderMarkdown(text), h('span.ai-cursor', {}, '▌'));
  };

  return {
    node,
    appendThinking(chunk) {
      thinking += chunk;
      thinkBox.hidden = false;
      if (!thinkFrame) {
        thinkFrame = requestAnimationFrame(() => {
          thinkFrame = 0;
          // 생각은 길다. 뒤쪽(지금 하고 있는 생각)이 보이는 것이 앞쪽보다 쓸모 있다.
          thinkBody.textContent = thinking.length > 4000
            ? `…${thinking.slice(-4000)}` : thinking;
          thinkBody.scrollTop = thinkBody.scrollHeight;
        });
      }
    },
    // waiting은 아직 아무것도 오지 않았을 때 "생각 중"을 띄운다.
    //
    // 생각을 글로 보내지 않는 모델도 있다. 그때도 오래 걸리는 것은 마찬가지라,
    // 잠깐 기다려도 소식이 없으면 같은 표시를 띄운다.
    waiting() {
      if (text || thinking) return;
      thinkBox.hidden = false;
    },
    appendText(chunk) {
      if (chunk) settle();
      text += chunk;
      if (!frame) frame = requestAnimationFrame(paint);
    },
    stop() {
      settle();
    },
    addToolCall(data) {
      settle();
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
          h('div.meta-row', {}, h('dt', {}, '컨텍스트'),
            h('dd', {}, p.contextTokens
              ? `${p.contextTokens.toLocaleString('ko-KR')} 토큰`
              : '모름 (기본값 사용)')),
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
  // 컨텍스트 크기.
  //
  // 이 값이 왜 필요한가: 대화 이력을 얼마나 남길지가 여기서 정해진다. 하나의
  // 상수로 두면 어느 쪽으로든 틀린다 — Claude(20만 토큰)에서는 멀쩡한 이력을
  // 이유 없이 버리고, 로컬 Ollama(기본 4~8천 토큰)에서는 넘치는 줄도 모르고
  // 보낸다. Ollama는 넘치면 오류를 내지 않고 **앞을 말없이 잘라낸다.**
  const ctxInput = input({
    type: 'number', min: '0', step: '1024',
    value: existing?.contextTokens ? String(existing.contextTokens) : '',
    placeholder: '모르면 비워 두세요',
  });
  const ctxNote = h('span.field-help');
  // 모델별 컨텍스트 크기(Ollama가 알려준다). 모델을 고르면 이것으로 칸을 채운다.
  let ctxByModel = {};

  const fillContext = (model) => {
    const known = ctxByModel[model];
    if (!known) return false;
    ctxInput.value = String(known);
    mount(ctxNote, `${model} 의 컨텍스트 ${known.toLocaleString('ko-KR')} 토큰을 넣었습니다`);
    return true;
  };

  // askContext는 모르는 모델의 크기를 서버에 물어본다.
  //
  // 목록에 크기가 함께 오는 것은 로컬 Ollama뿐이다. Ollama Cloud는 목록에 넣어
  // 주지 않는데, 모델마다 크기가 크게 다르다(gpt-oss:120b 131,072 · glm-5.3
  // 1,048,576). 그래서 고른 모델 하나만 따로 묻는다.
  let asking = '';
  const askContext = async (model) => {
    if (!model || ctxByModel[model] || asking === model) return;
    asking = model;
    mount(ctxNote, `${model} 의 컨텍스트를 알아보는 중…`);
    try {
      const res = await api.post('/ai/providers/models', {
        id: existing?.id ?? '',
        provider: kindSelect.value,
        baseUrl: baseInput.value,
        apiKey: keyInput.value.trim(),
        model,
      });
      ctxByModel = { ...ctxByModel, ...(res.contextTokens ?? {}) };
      // 묻는 동안 사람이 직접 적었을 수 있다. 그러면 덮지 않는다.
      if (ctxInput.value.trim() === '' && fillContext(model)) return;
      mount(ctxNote, '');
    } catch {
      // 못 물어봐도 그만이다. 사람이 적으면 된다 — 여기서 오류를 띄우면
      // 모델을 고를 때마다 붉은 토스트가 뜬다.
      mount(ctxNote, '');
    } finally {
      asking = '';
    }
  };

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
    sel.addEventListener('change', () => {
      defaultModel = sel.value;
      // 사람이 손으로 적은 값은 덮지 않는다. 비어 있을 때만 채운다.
      if (ctxInput.value.trim() !== '') return;
      if (!fillContext(defaultModel)) askContext(defaultModel);
    });
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
    if (ctxInput.value.trim() === '' && !fillContext(name)) askContext(name);
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
      ctxByModel = res.contextTokens ?? {};
      // 아직 아무것도 안 적었으면 기본 모델의 크기로 채워 준다. 사람이 모델
      // 카드를 찾아 옮겨 적는 것보다 서버가 아는 값이 정확하다.
      if (ctxInput.value.trim() === '' && defaultModel
        && !fillContext(defaultModel)) askContext(defaultModel);
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
        h('label.field', {}, h('span.field-label', {}, '컨텍스트 크기 (토큰)'), ctxInput,
          ctxNote,
          h('span.field-help', {},
            '대화 이력을 얼마나 남길지 정합니다. 비워 두면 보수적인 기본값을 씁니다. '
            + 'Ollama에서는 이 값이 num_ctx로 그대로 전달됩니다 — '
            + 'Ollama는 넘치는 프롬프트를 오류 없이 앞에서 잘라내므로, '
            + '모델의 실제 크기를 적어 두는 편이 안전합니다.')),
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
            contextTokens: Number(ctxInput.value.trim()) || 0,
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
