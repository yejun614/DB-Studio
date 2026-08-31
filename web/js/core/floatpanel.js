// 떠 있는 패널: 옮기고 크기를 바꿀 수 있는 창.
//
// 모달과 다른 물건이다. 모달은 "지금 이것만 하라"는 뜻이라 뒤를 덮고 배경을 막는데,
// 여기 담기는 것(어시스턴트)은 **다른 화면을 보면서** 쓰는 도구다. 스키마를 보며
// 물어보고, 답을 보며 화면을 조작하는 것이 목적이므로 뒤가 가려지면 안 된다.
//
// 위치와 크기는 사람마다 다르게 잡는다(넓은 모니터에서는 옆에 세워 두고, 좁은
// 화면에서는 크게 편다). 그래서 마지막 배치를 기억한다 — 매번 같은 자리로 끌어다
// 놓게 하면 그 도구는 결국 쓰이지 않는다.
import { h, mount, icon } from './dom.js';

const MIN_W = 360;
const MIN_H = 300;
// 패널 둘레에 남기는 여백. 화면에 딱 붙으면 크기 손잡이를 잡기 어렵다.
const EDGE = 8;
// 화면 밖으로 밀어낼 때 반드시 남겨 둘 폭·높이.
//
// 전체를 화면 안에 가두면 "지금은 옆으로 치워 두고 뒤를 보겠다"가 불가능하다.
// 대신 제목 줄이 손에 닿을 만큼은 남긴다 — 완전히 나가면 다시 잡을 수 없다.
const KEEP_X = 140;
const KEEP_Y = 34;

// storageKey는 배치를 기억하는 자리다. localStorage를 쓰는 이유: 이것은 서버가
// 알아야 할 설정이 아니라 이 브라우저에서의 배치이고, 창 크기가 기기마다 다르다.
function geometryKey(id) {
  return `dbstudio.panel.${id}`;
}

function loadGeometry(id, fallback) {
  try {
    const raw = localStorage.getItem(geometryKey(id));
    if (!raw) return fallback;
    const saved = JSON.parse(raw);
    if (typeof saved?.w !== 'number' || typeof saved?.h !== 'number') return fallback;
    return saved;
  } catch {
    // 저장소를 못 읽는 것(사생활 보호 모드 등)은 오류가 아니라 "기억이 없다"이다.
    return fallback;
  }
}

function saveGeometry(id, geo) {
  try {
    localStorage.setItem(geometryKey(id), JSON.stringify(geo));
  } catch { /* 저장 못 해도 이번 세션은 그대로 쓴다 */ }
}

// openFloatPanel은 패널을 열고 조작 손잡이를 돌려준다.
//
// 같은 id로 이미 열려 있으면 새로 만들지 않고 앞으로 가져온다 — 메뉴를 두 번 눌러
// 같은 창이 두 개 뜨면 어느 쪽이 진짜인지 알 수 없다.
const open = new Map();

export function openFloatPanel({
  id, title, render, onClose, width = 560, height = 640, actions = [], iconName = 'menu',
}) {
  const existing = open.get(id);
  if (existing) {
    existing.focus();
    return existing;
  }

  const geo = loadGeometry(id, null) ?? defaultGeometry(width, height);
  const body = h('div.float-body');
  const titleEl = h('strong.float-title', {}, title);
  const head = h('div.float-head', {},
    // 손잡이는 제목 줄 전체다. 작은 손잡이를 따로 두면 어디를 잡아야 하는지
    // 알 수 없고, 창을 옮기는 것은 자주 하는 동작이다.
    h('div.float-grip', {}, icon(iconName, 14), titleEl),
    h('div.float-actions'),
  );
  const panel = h('section.float-panel', {
    role: 'dialog', 'aria-label': title,
    style: { left: `${geo.x}px`, top: `${geo.y}px`, width: `${geo.w}px`, height: `${geo.h}px` },
  }, head, body, h('div.float-resize', { 'aria-hidden': 'true' }));

  const handle = {
    panel,
    body,
    focus() {
      // 여러 패널이 겹치면 마지막에 만진 것이 위로 온다.
      for (const other of open.values()) other.panel.classList.remove('is-front');
      panel.classList.add('is-front');
    },
    setTitle(text) {
      mount(titleEl, text);
      panel.setAttribute('aria-label', text);
    },
    close() {
      cleanup();
      panel.remove();
      open.delete(id);
      onClose?.();
    },
  };

  const actionBar = head.querySelector('.float-actions');
  for (const action of actions) {
    actionBar.appendChild(h('button.icon-btn.float-btn', {
      type: 'button', title: action.label, 'aria-label': action.label,
      onclick: () => action.onClick(handle),
    }, icon(action.icon, 14)));
  }
  actionBar.appendChild(h('button.icon-btn.float-btn', {
    type: 'button', title: '닫기', 'aria-label': '닫기',
    onclick: () => handle.close(),
  }, icon('x', 14)));

  document.body.appendChild(panel);
  open.set(id, handle);
  handle.focus();
  panel.addEventListener('pointerdown', () => handle.focus());

  const cleanup = bindGeometry(id, panel, head, geo);
  render(body, handle);
  return handle;
}

// defaultGeometry는 처음 열 때의 자리다. 오른쪽 아래에 붙인다 —
// 왼쪽에는 사이드바가, 위에는 화면 제목이 있다.
function defaultGeometry(width, height) {
  const w = Math.min(width, window.innerWidth - 40);
  const h2 = Math.min(height, window.innerHeight - 40);
  return { x: window.innerWidth - w - 24, y: window.innerHeight - h2 - 24, w, h: h2 };
}

// bindGeometry는 끌기·크기 조절·화면 크기 변화를 다룬다.
function bindGeometry(id, panel, head, geo) {
  const state = { ...geo };

  // 그리기와 저장을 나눈다. 끌고 있는 동안에는 화면만 따라오면 되고,
  // 포인터가 움직일 때마다 localStorage에 쓰면 긴 드래그가 눈에 띄게 끊긴다.
  const apply = () => {
    panel.style.left = `${state.x}px`;
    panel.style.top = `${state.y}px`;
    panel.style.width = `${state.w}px`;
    panel.style.height = `${state.h}px`;
  };
  const save = () => saveGeometry(id, state);

  // 좁아지면 내용이 스스로 다시 배치되도록 표시만 남긴다. 실제 배치는 CSS가
  // 정한다 — 자바스크립트로 폭을 재서 요소를 옮기면 두 벌의 규칙이 생긴다.
  const observer = new ResizeObserver(() => {
    panel.classList.toggle('is-narrow', panel.clientWidth < 620);
    panel.classList.toggle('is-short', panel.clientHeight < 420);
  });
  observer.observe(panel);

  const drag = (e, mode) => {
    // 왼쪽 버튼만. 오른쪽 버튼으로 끌리면 상황에 맞는 메뉴가 열리지 않는다.
    if (e.button !== 0) return;
    e.preventDefault();

    // 포인터의 시작점과 패널의 시작 배치를 **따로** 담는다.
    //
    // 한 객체에 합치면 x/y가 두 가지 뜻을 갖는다(포인터의 x인가 패널의 x인가).
    // 실제로 그렇게 섞여 있었고, 그래서 끌기 시작하는 순간 패널이 포인터와
    // 패널 왼쪽 끝의 거리만큼 튀었다 — 크기 조절도 같은 이유로 어긋났다.
    const from = {
      px: e.clientX, py: e.clientY,
      x: state.x, y: state.y, w: state.w, h: state.h,
    };
    const target = e.currentTarget;
    target.setPointerCapture(e.pointerId);

    const move = (ev) => {
      const dx = ev.clientX - from.px;
      const dy = ev.clientY - from.py;
      if (mode === 'move') {
        state.x = from.x + dx;
        state.y = from.y + dy;
      } else {
        // 크기는 화면 오른쪽·아래를 넘지 않는다. 넘겨서 커지면 보내기 버튼처럼
        // 오른쪽 끝에 있는 것이 잘려 나가 아무것도 할 수 없다.
        state.w = Math.max(MIN_W, Math.min(from.w + dx, window.innerWidth - from.x - EDGE));
        state.h = Math.max(MIN_H, Math.min(from.h + dy, window.innerHeight - from.y - EDGE));
        // 크기는 화면 안에서만 늘린다. 왼쪽으로 밀어 둔 패널(음수 x)은 그만큼
        // 더 늘어날 수 있어야 하므로 x가 음수면 그 몫을 더해 준다.
        if (from.x < 0) state.w = Math.max(MIN_W, Math.min(from.w + dx, window.innerWidth - EDGE));
      }
      clamp(state);
      apply();
    };
    const up = () => {
      save();
      target.releasePointerCapture(e.pointerId);
      target.removeEventListener('pointermove', move);
      target.removeEventListener('pointerup', up);
      target.removeEventListener('pointercancel', up);
      panel.classList.remove('is-dragging');
    };
    panel.classList.add('is-dragging');
    target.addEventListener('pointermove', move);
    target.addEventListener('pointerup', up);
    // 창 밖으로 나가거나 다른 제스처가 끼어들면 pointerup이 오지 않는다.
    // 그러면 손을 뗐는데도 패널이 계속 따라다닌다.
    target.addEventListener('pointercancel', up);
  };

  const grip = head.querySelector('.float-grip');
  const onHeadDown = (e) => drag(e, 'move');
  const resizer = panel.querySelector('.float-resize');
  const onResizeDown = (e) => drag(e, 'resize');
  grip.addEventListener('pointerdown', onHeadDown);
  resizer.addEventListener('pointerdown', onResizeDown);

  // 창을 줄이면 패널이 화면 밖에 남을 수 있다. 그러면 다시 잡을 수 없다.
  const onWindowResize = () => {
    clamp(state);
    apply();
    save();
  };
  window.addEventListener('resize', onWindowResize);

  clamp(state);
  apply();
  save();

  return () => {
    observer.disconnect();
    grip.removeEventListener('pointerdown', onHeadDown);
    resizer.removeEventListener('pointerdown', onResizeDown);
    window.removeEventListener('resize', onWindowResize);
  };
}

// clamp는 패널을 화면 안으로 되돌린다.
//
// 일부러 걸쳐 두는 것도 허용하지 않는다. 반쯤 나간 패널에서는 오른쪽 끝의
// 보내기 버튼이나 크기 손잡이에 닿을 수 없고, 창을 줄였다 키우는 사이에
// 그 상태가 저장되어 다음에 열 때도 그대로 나온다.
function clamp(state) {
  state.w = Math.max(MIN_W, Math.min(state.w, window.innerWidth - EDGE * 2));
  state.h = Math.max(MIN_H, Math.min(state.h, window.innerHeight - EDGE * 2));
  // 옆으로 치워 두는 것은 허용하고, 다시 잡을 수 있을 만큼만 남긴다.
  state.x = Math.max(KEEP_X - state.w, Math.min(state.x, window.innerWidth - KEEP_X));
  // 위로는 넘기지 않는다. 제목 줄이 화면 위로 나가면 잡을 곳이 사라진다.
  state.y = Math.max(0, Math.min(state.y, window.innerHeight - KEEP_Y));
}

// isPanelOpen은 메뉴가 자기 상태를 표시할 때 쓴다.
export function isPanelOpen(id) {
  return open.has(id);
}

export function closeFloatPanel(id) {
  open.get(id)?.close();
}

// closeAllFloatPanels는 떠 있는 창을 모두 닫는다.
//
// 이 창들은 document.body 에 붙어 있어서 앱 셸(#app)을 갈아 끼워도 그대로 남는다.
// 로그아웃 화면에 어시스턴트가 떠 있던 것이 그 때문이다 — 로그인 화면 위에 남의
// 대화가 그대로 보이고, 그 안의 요청은 401로 죽는다.
//
// 세션이 끊기는 길은 여럿이다(로그아웃·만료·비밀번호 강제 변경·2단계 인증 등록).
// 그 모두가 셸을 내리는 한 곳을 지나므로, 닫는 일도 그 한 곳에서 한다.
export function closeAllFloatPanels() {
  // close()가 registry를 지우므로 복사해 돌린다.
  for (const handle of [...open.values()]) handle.close();
}

// panelModal은 **패널 안에서만** 덮는 작은 대화상자다.
//
// 화면 전체를 덮는 모달(core/ui.js의 openModal)을 쓰지 않는 이유: 이 패널은
// 뒤 화면을 보면서 쓰는 도구다. 대화를 고르려고 화면 전체가 어두워지면 그 전제가
// 무너지고, 패널을 옆으로 치워 둔 사람에게는 대화상자가 엉뚱한 자리에 뜬다.
//
// 좁은 팝업에서 목록·설정을 겹쳐 보여주기 위한 것이므로 스스로 스크롤한다.
export function panelModal(panel, { title, body, footer }) {
  const overlay = h('div.float-modal');
  const close = () => overlay.remove();

  const content = typeof body === 'function' ? body(close) : body;
  const foot = typeof footer === 'function' ? footer(close) : footer;

  overlay.append(h('div.float-modal-box', {},
    h('div.float-modal-head', {},
      h('strong', {}, title),
      h('button.icon-btn', {
        type: 'button', title: '닫기', 'aria-label': '닫기', onclick: close,
      }, icon('x', 14)),
    ),
    h('div.float-modal-body', {}, content),
    foot ? h('div.float-modal-foot', {}, foot) : null,
  ));

  // 바깥을 눌러도 닫힌다. 여기 담기는 것은 고르기·설정처럼 가벼운 일이라
  // 실수로 닫혀도 잃는 것이 없다(값을 고치는 모달은 앱 모달을 쓴다).
  overlay.addEventListener('pointerdown', (e) => {
    if (e.target === overlay) close();
  });
  panel.appendChild(overlay);
  return close;
}
