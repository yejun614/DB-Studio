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

// 투명도의 하한. 0 까지 열어 두면 창을 잃어버린다 — 보이지 않는 창은 슬라이더를
// 되돌릴 방법도 함께 사라진 창이다.
const MIN_OPACITY = 0.3;

// 최소화한 아이콘의 크기와, 끌기로 볼 판정 거리.
//
// 4px 를 두는 이유: 누르는 손은 조금 흔들린다. 그 흔들림을 끌기로 보면 "눌렀는데
// 안 열린다"가 되고, 반대로 판정을 없애면 옮기려 잡은 것이 창을 열어 버린다.
const DOT_SIZE = 40;
const DRAG_SLOP = 4;

// 아이콘이 스스로 눈에 띄는 시간.
//
// 접는 순간 창이 사라지고 40px 짜리 동그라미 하나가 남는다. 넓은 모니터에서 그것은
// "화면에서 무언가 없어졌다"로만 보이고, 남은 아이콘은 눈이 좇지 못한다. 그래서 잠깐
// 물결을 그린다 — 잠깐이어야 한다. 계속 움직이는 아이콘은 그 자체가 소음이고,
// 사람은 그것을 보지 않는 법을 금방 배운다.
const DOT_ATTENTION_MS = 3600;

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
    // 옛 기억에는 투명도가 없다. 그리고 하한 밖의 값이 남아 있으면(손으로 고친
    // 저장소, 옛 하한) 창이 보이지 않는 채로 열린다.
    if (typeof saved.opacity !== 'number' || !(saved.opacity > 0)) saved.opacity = 1;
    saved.opacity = Math.min(1, Math.max(MIN_OPACITY, saved.opacity));
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

// beforeClose는 닫기를 막을 기회다. false 를 돌려주면 창이 남는다.
//
// 왜 필요한가: 이 창들은 오래 걸리는 일을 담고 있다(AI 답변 스트리밍). X 를 누르는
// 순간 그 일이 조용히 중단되면, 사람은 5분 기다린 답을 손짓 한 번으로 잃는다.
export function openFloatPanel({
  id, title, render, onClose, beforeClose,
  width = 560, height = 640, actions = [], iconName = 'menu',
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

  // 접어 두었을 때의 아이콘. 접혀 있지 않으면 null 이다.
  let dot = null;
  // 안에서 무언가 돌고 있는가(답변 스트리밍). 접을 때 아이콘에 그대로 옮긴다.
  let busy = false;

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
    // minimize는 창을 접어 아이콘 하나로 만든다.
    //
    // 창을 지우지 않고 감추기만 한다(hidden). 지우면 안에서 돌던 것(스트리밍,
    // 스크롤 자리, 적어 두던 글)이 함께 사라지고, 그것이 최소화와 닫기를 가르는
    // 유일한 차이다.
    minimize() {
      if (dot) return;
      // 자리를 먼저 잰다. 감춘 뒤에 재면 모두 0 이 나와(감춘 요소의 사각형은
      // 0,0,0,0 이다) 아이콘이 늘 화면 왼쪽 위 구석에 생긴다.
      const at = panel.getBoundingClientRect();
      panel.hidden = true;
      dot = makeDot(at);
      dot.classList.toggle('is-busy', busy);
      document.body.appendChild(dot);
    },
    restore() {
      if (!dot) return;
      dot.remove();
      dot = null;
      panel.hidden = false;
      handle.focus();
    },
    // pulse는 접어 둔 아이콘을 다시 한번 눈에 띄게 한다.
    //
    // 안에서 무언가 끝났을 때(받고 있던 답변이 도착했을 때) 부른다. 접어 둔 사람은
    // 다른 화면을 보고 있으므로, 그 순간 아이콘이 조용하면 답은 아무도 읽지 않는
    // 채로 남는다.
    pulse() {
      if (!dot) return;
      // 클래스를 뗐다가 다시 붙여야 애니메이션이 처음부터 다시 돈다.
      dot.classList.remove('is-new');
      void dot.offsetWidth;
      dot.classList.add('is-new');
      setTimeout(() => dot?.classList.remove('is-new'), DOT_ATTENTION_MS);
    },
    // setBusy는 "안에서 무언가 돌고 있다"를 아이콘에 표시한다(느린 맥박).
    setBusy(on) {
      busy = Boolean(on);
      dot?.classList.toggle('is-busy', busy);
    },
    get minimized() {
      return Boolean(dot);
    },
    // force 는 묻지 않고 닫는 길이다. 로그아웃처럼 셸을 내리는 경우에 쓴다 —
    // 세션이 이미 끝났으므로 "정말 중단할까요"는 물어볼 것이 없는 질문이다.
    async close({ force = false } = {}) {
      if (!force && beforeClose && (await beforeClose()) === false) return false;
      cleanup();
      dot?.remove();
      dot = null;
      panel.remove();
      open.delete(id);
      onClose?.();
      return true;
    },
  };

  const actionBar = head.querySelector('.float-actions');
  for (const action of actions) {
    actionBar.appendChild(h('button.icon-btn.float-btn', {
      type: 'button', title: action.label, 'aria-label': action.label,
      onclick: () => action.onClick(handle),
    }, icon(action.icon, 14)));
  }

  // 투명도 슬라이더.
  //
  // 이 창은 다른 화면을 보면서 쓰는 도구다. 그런데 답변이 길어지면 창도 커지고,
  // 커진 창은 정작 물어보려던 것을 덮는다 — 옮기거나 줄이는 것이 답일 때도 있지만
  // "지금 이 답을 보면서 뒤의 표를 함께 보고 싶다"는 그 둘로 해결되지 않는다.
  //
  // 손을 올려도 되돌리지 않는다. 되돌리면 슬라이더를 움직이는 동안에는 효과가
  // 보이지 않아 "안 먹는다"로 읽히고, 반투명하게 두고 읽는 것 자체가 목적이다.
  const opacityInput = h('input.float-opacity', {
    type: 'range',
    min: String(Math.round(MIN_OPACITY * 100)), max: '100', step: '5',
    value: String(Math.round((geo.opacity ?? 1) * 100)),
    title: '투명도', 'aria-label': '창 투명도',
  });
  actionBar.appendChild(h('label.float-opacity-wrap', {}, icon('eye', 13), opacityInput));

  // 최소화. 창을 닫지 않고 아이콘 하나로 접어 둔다.
  //
  // 닫기와 다른 물건이다: 닫으면 받고 있던 답변이 끊기고(그래서 물어본다), 다시
  // 열면 어느 대화였는지 찾아야 한다. 접어 두면 답변은 계속 오고 아이콘을 누르면
  // 그 자리에서 이어진다 — "답이 오는 동안 뒤 화면을 보고 있겠다"가 그 뜻이다.
  actionBar.appendChild(h('button.icon-btn.float-btn', {
    type: 'button', title: '최소화', 'aria-label': '최소화',
    onclick: () => handle.minimize(),
  }, icon('minus', 14)));

  actionBar.appendChild(h('button.icon-btn.float-btn', {
    type: 'button', title: '닫기', 'aria-label': '닫기',
    onclick: () => handle.close(),
  }, icon('x', 14)));

  // makeDot은 접어 둔 창을 대신하는 아이콘이다.
  //
  // 창의 **좌상단**에 만든다. 접기 전에 창이 있던 자리라, 눈이 방금 보던 곳에서
  // 그것을 찾는다. 화면 한 구석에 고정하면 넓은 모니터에서는 아이콘을 찾는 일이
  // 접는 일보다 오래 걸린다.
  function makeDot(box) {
    const el = h('button.float-dot.is-new', {
      type: 'button',
      title: `${titleEl.textContent} 열기`,
      'aria-label': `${titleEl.textContent} 열기`,
      style: {
        left: `${Math.max(EDGE, Math.min(box.left, window.innerWidth - DOT_SIZE - EDGE))}px`,
        top: `${Math.max(EDGE, Math.min(box.top, window.innerHeight - DOT_SIZE - EDGE))}px`,
      },
    },
    // 물결은 아이콘 **뒤에** 그린다. 아이콘 자체를 흔들면 무엇인지 읽기 어렵고,
    // 뒤에서 퍼지는 원은 눈에는 띄면서 그림은 그대로 둔다.
    h('span.float-dot-ping', { 'aria-hidden': 'true' }),
    icon(iconName, 18));
    // 잠깐 뒤에 물결을 멈춘다(위 상수의 이유).
    setTimeout(() => el.classList.remove('is-new'), DOT_ATTENTION_MS);

    // 누르기와 끌기를 한 요소에서 나눈다. 조금이라도 움직였으면 옮긴 것으로 보고
    // 열지 않는다 — 옮기려 잡았는데 창이 열리면 그 아이콘은 옮길 수 없는 아이콘이다.
    el.addEventListener('pointerdown', (e) => {
      if (e.button !== 0) return;
      e.preventDefault();
      const from = { px: e.clientX, py: e.clientY, x: el.offsetLeft, y: el.offsetTop };
      let moved = false;
      // 이미 사라진 포인터는 붙잡을 수 없다(pointercancel, 아주 짧은 터치).
      // 붙잡지 못해도 아래 pointermove 로 따라갈 수 있으므로 예외로 멈추지 않는다.
      try {
        el.setPointerCapture?.(e.pointerId);
      } catch {
        /* 붙잡을 포인터가 없다 */
      }
      const move = (ev) => {
        const dx = ev.clientX - from.px;
        const dy = ev.clientY - from.py;
        if (!moved && Math.abs(dx) + Math.abs(dy) < DRAG_SLOP) return;
        moved = true;
        el.classList.add('is-dragging');
        el.style.left = `${Math.max(0, Math.min(from.x + dx, window.innerWidth - DOT_SIZE))}px`;
        el.style.top = `${Math.max(0, Math.min(from.y + dy, window.innerHeight - DOT_SIZE))}px`;
      };
      const up = () => {
        el.removeEventListener('pointermove', move);
        el.removeEventListener('pointerup', up);
        el.removeEventListener('pointercancel', up);
        el.classList.remove('is-dragging');
        if (!moved) handle.restore();
      };
      el.addEventListener('pointermove', move);
      el.addEventListener('pointerup', up);
      el.addEventListener('pointercancel', up);
    });
    return el;
  }

  document.body.appendChild(panel);
  open.set(id, handle);
  handle.focus();
  panel.addEventListener('pointerdown', () => handle.focus());

  const cleanup = bindGeometry(id, panel, head, geo, opacityInput);
  render(body, handle);
  return handle;
}

// defaultGeometry는 처음 열 때의 자리다. 오른쪽 아래에 붙인다 —
// 왼쪽에는 사이드바가, 위에는 화면 제목이 있다.
function defaultGeometry(width, height) {
  const w = Math.min(width, window.innerWidth - 40);
  const h2 = Math.min(height, window.innerHeight - 40);
  return {
    x: window.innerWidth - w - 24, y: window.innerHeight - h2 - 24, w, h: h2,
    // 처음에는 불투명하다. 반투명한 창이 먼저 뜨면 고장으로 읽힌다.
    opacity: 1,
  };
}

// bindGeometry는 끌기·크기 조절·투명도·화면 크기 변화를 다룬다.
function bindGeometry(id, panel, head, geo, opacityInput) {
  const state = { ...geo };

  // 그리기와 저장을 나눈다. 끌고 있는 동안에는 화면만 따라오면 되고,
  // 포인터가 움직일 때마다 localStorage에 쓰면 긴 드래그가 눈에 띄게 끊긴다.
  const apply = () => {
    panel.style.left = `${state.x}px`;
    panel.style.top = `${state.y}px`;
    panel.style.width = `${state.w}px`;
    panel.style.height = `${state.h}px`;
    panel.style.opacity = String(state.opacity ?? 1);
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

  // 투명도는 움직이는 동안 바로 보이고, 손을 뗄 때 저장한다(끌기와 같은 이유:
  // 값이 바뀔 때마다 localStorage 에 쓰면 슬라이더가 뻑뻑해진다).
  const onOpacity = () => {
    const pct = Number(opacityInput?.value ?? 100);
    state.opacity = Math.min(1, Math.max(MIN_OPACITY, pct / 100));
    apply();
  };
  const onOpacityDone = () => save();
  if (opacityInput) {
    opacityInput.addEventListener('input', onOpacity);
    opacityInput.addEventListener('change', onOpacityDone);
    // 슬라이더를 잡는 것이 창을 옮기는 것이 되면 안 된다. 머리글 전체가 손잡이라
    // 여기서 멈추지 않으면 값을 고르는 동작이 곧 창을 끄는 동작이 된다.
    opacityInput.addEventListener('pointerdown', (e) => e.stopPropagation());
  }

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
    if (opacityInput) {
      opacityInput.removeEventListener('input', onOpacity);
      opacityInput.removeEventListener('change', onOpacityDone);
    }
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

// pulsePanelDot은 접어 둔 창의 아이콘을 한 번 더 눈에 띄게 한다.
// 열려 있으면 아무 일도 하지 않는다 — 보고 있는 창을 흔들 이유가 없다.
export function pulsePanelDot(id) {
  open.get(id)?.pulse();
}

// setPanelBusy는 창 안에서 무언가 돌고 있음을 알린다(접어 두면 아이콘이 맥박한다).
export function setPanelBusy(id, busy) {
  open.get(id)?.setBusy(busy);
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
  // 여기서는 묻지 않는다(force) — 세션이 끝나서 내리는 길이다.
  for (const handle of [...open.values()]) handle.close({ force: true });
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
