// 사이드바의 폭 조절(데스크톱)과 서랍 열고 닫기(모바일).
//
// 두 동작을 한 파일에 둔 이유: 같은 요소의 같은 상태(폭, 열림)를 다루고, 화면 폭이
// 경계를 넘을 때 서로를 정리해 줘야 한다. 떨어뜨려 놓으면 "좁은 화면에서 드래그로
// 폭을 바꾼 뒤 넓히면 사이드바가 사라져 있다" 같은 상태가 생긴다.
import { h, icon } from './dom.js';

const WIDTH_KEY = 'dbstudio-sidebar-width';
const HIDDEN_KEY = 'dbstudio-sidebar-hidden';
const MIN_WIDTH = 190;
const MAX_WIDTH = 420;
const DEFAULT_WIDTH = 240;
// 서랍으로 바뀌는 경계. CSS의 미디어 쿼리와 같은 값이어야 한다.
const NARROW = '(max-width: 900px)';

const narrowQuery = window.matchMedia(NARROW);

export function isNarrow() {
  return narrowQuery.matches;
}

function clamp(px) {
  return Math.min(MAX_WIDTH, Math.max(MIN_WIDTH, Math.round(px)));
}

export function storedWidth() {
  const raw = Number(localStorage.getItem(WIDTH_KEY));
  return Number.isFinite(raw) && raw > 0 ? clamp(raw) : DEFAULT_WIDTH;
}

// applyWidth는 폭을 문서 전체에 반영한다. 셸의 그리드가 이 변수를 읽는다.
export function applyWidth(px, { persist = true } = {}) {
  const w = clamp(px);
  document.documentElement.style.setProperty('--sidebar-w', `${w}px`);
  if (persist) {
    // 기본값이면 항목을 지운다. 나중에 기본값을 바꿨을 때 옛 값이 발목을 잡지 않는다.
    if (w === DEFAULT_WIDTH) localStorage.removeItem(WIDTH_KEY);
    else localStorage.setItem(WIDTH_KEY, String(w));
  }
  for (const el of document.querySelectorAll('.sidebar-resize')) {
    el.setAttribute('aria-valuenow', String(w));
  }
  return w;
}

// createResizer는 사이드바 오른쪽 가장자리의 손잡이를 만든다.
//
// 마우스만이 아니라 키보드로도 조절할 수 있어야 한다(role="separator" + 화살표).
// 포인터 이벤트를 쓰면 마우스·터치·펜을 한 코드로 처리하고, setPointerCapture 덕분에
// 커서가 창 밖으로 나가도 드래그가 끊기지 않는다.
export function createResizer() {
  const handle = h('div.sidebar-resize', {
    role: 'separator',
    'aria-orientation': 'vertical',
    'aria-label': '사이드바 폭 조절',
    'aria-valuemin': String(MIN_WIDTH),
    'aria-valuemax': String(MAX_WIDTH),
    'aria-valuenow': String(storedWidth()),
    tabindex: '0',
    title: '드래그해서 폭 조절 (더블클릭하면 기본값)',
  });

  handle.addEventListener('pointerdown', (e) => {
    if (isNarrow() || e.button !== 0) return;
    e.preventDefault();
    handle.setPointerCapture(e.pointerId);
    document.body.classList.add('is-resizing');

    const move = (ev) => applyWidth(ev.clientX, { persist: false });
    const up = (ev) => {
      handle.releasePointerCapture(ev.pointerId);
      document.body.classList.remove('is-resizing');
      handle.removeEventListener('pointermove', move);
      handle.removeEventListener('pointerup', up);
      handle.removeEventListener('pointercancel', up);
      applyWidth(ev.clientX); // 놓는 순간에만 저장한다. 드래그 중 저장은 낭비다.
    };
    handle.addEventListener('pointermove', move);
    handle.addEventListener('pointerup', up);
    handle.addEventListener('pointercancel', up);
  });

  handle.addEventListener('dblclick', () => applyWidth(DEFAULT_WIDTH));

  handle.addEventListener('keydown', (e) => {
    const step = e.shiftKey ? 40 : 10;
    const current = storedWidth();
    if (e.key === 'ArrowLeft') applyWidth(current - step);
    else if (e.key === 'ArrowRight') applyWidth(current + step);
    else if (e.key === 'Home') applyWidth(MIN_WIDTH);
    else if (e.key === 'End') applyWidth(MAX_WIDTH);
    else if (e.key === 'Enter' || e.key === ' ') applyWidth(DEFAULT_WIDTH);
    else return;
    e.preventDefault();
  });

  return handle;
}

// ---------- 넓은 화면에서 숨기기 ----------
//
// 서랍(좁은 화면)과 따로 두는 이유: 좁은 화면에서 사이드바는 본문을 덮는 서랍이고,
// 넓은 화면에서는 본문과 자리를 나눠 쓰는 칸이다. 숨긴다는 말의 뜻이 서로 다르다 —
// 서랍은 "덮은 것을 치운다"이고, 여기서는 "본문에 자리를 내준다"이다.
//
// 상태를 브라우저에 남긴다. 도면이나 표를 넓게 보려고 접어 둔 사람이 화면을 옮길
// 때마다 다시 접어야 하면, 접는 기능은 없는 것과 같다.

let hideBtn = null;
let showBtn = null;

export function isHidden() {
  return document.body.classList.contains('nav-hidden');
}

function storedHidden() {
  try {
    return localStorage.getItem(HIDDEN_KEY) === '1';
  } catch {
    // 사생활 보호 모드에서는 localStorage 접근이 예외를 던진다. 보이는 쪽이 기본이다.
    return false;
  }
}

// setHidden은 숨김 상태를 적용한다.
export function setHidden(hidden, { persist = true } = {}) {
  document.body.classList.toggle('nav-hidden', hidden);
  hideBtn?.setAttribute('aria-expanded', hidden ? 'false' : 'true');
  showBtn?.setAttribute('aria-expanded', hidden ? 'false' : 'true');
  if (persist) {
    try {
      if (hidden) localStorage.setItem(HIDDEN_KEY, '1');
      else localStorage.removeItem(HIDDEN_KEY);
    } catch {
      // 저장에 실패해도 이번 화면에는 적용된다.
    }
  }
}

export function toggleHidden() {
  // 좁은 화면에서는 서랍이 그 일을 한다. 여기서 또 접으면 서랍을 열 수 없게 된다.
  if (isNarrow()) {
    if (isDrawerOpen()) closeDrawer({ restoreFocus: true });
    else openDrawer();
    return;
  }
  setHidden(!isHidden());
  // 다시 보이게 했으면 접기 단추로, 접었으면 펴기 단추로 초점을 옮긴다.
  // 그러지 않으면 키보드 사용자의 초점이 사라진 요소에 남는다.
  (isHidden() ? showBtn : hideBtn)?.focus();
}

// createHideButton은 사이드바 안의 접기 단추다.
export function createHideButton() {
  hideBtn = h('button.sidebar-hide', {
    type: 'button',
    'aria-label': '사이드바 숨기기 (Ctrl+B)',
    'aria-controls': 'sidebar',
    'aria-expanded': 'true',
    title: '사이드바 숨기기 (Ctrl+B)',
    onclick: () => {
      setHidden(true);
      // 접고 나면 이 단추는 사라진다. 초점을 옮기지 않으면 키보드 사용자의 초점이
      // 보이지 않는 요소에 남아, 다음 Tab 이 어디로 갈지 알 수 없다.
      showBtn?.focus();
    },
  }, icon('chevron-left', 16));
  return hideBtn;
}

// createShowButton은 접었을 때만 보이는 작은 손잡이다.
//
// 화면 왼쪽 위 가장자리에 둔다. 접고 나면 사이드바가 있던 자리가 통째로 사라지는데,
// 다시 부를 방법이 화면 어디에도 없으면 그것은 숨기기가 아니라 잃어버리기다.
export function createShowButton() {
  showBtn = h('button.sidebar-show', {
    type: 'button',
    'aria-label': '사이드바 보이기 (Ctrl+B)',
    'aria-controls': 'sidebar',
    'aria-expanded': 'false',
    title: '사이드바 보이기 (Ctrl+B)',
    onclick: () => {
      setHidden(false);
      hideBtn?.focus();
    },
  }, icon('menu', 16));
  return showBtn;
}

// ---------- 모바일 서랍 ----------

let toggleBtn = null;

export function isDrawerOpen() {
  return document.body.classList.contains('nav-open');
}

export function openDrawer() {
  if (isDrawerOpen()) return;
  document.body.classList.add('nav-open');
  toggleBtn?.setAttribute('aria-expanded', 'true');
  // 열자마자 첫 메뉴로 초점을 옮긴다. 그러지 않으면 키보드 사용자는 열린 서랍을
  // 지나쳐 본문으로 간다.
  //
  // 서랍은 닫혀 있을 때 visibility: hidden이라 클래스를 붙인 직후에는 초점을 받지
  // 못한다. offsetWidth를 읽어 스타일 계산을 강제한 뒤 옮긴다 —
  // requestAnimationFrame으로 미루면 탭이 보이지 않는 상태(백그라운드·헤드리스)에서
  // 콜백이 지연되거나 아예 실행되지 않는다.
  const first = document.querySelector('.sidebar .nav-item');
  if (first) {
    void first.offsetWidth;
    first.focus();
  }
}

export function closeDrawer({ restoreFocus = false } = {}) {
  if (!isDrawerOpen()) return;
  document.body.classList.remove('nav-open');
  toggleBtn?.setAttribute('aria-expanded', 'false');
  if (restoreFocus) toggleBtn?.focus();
}

// createTopbar는 좁은 화면에서만 보이는 상단 막대를 만든다.
// 햄버거 버튼과 현재 위치(브랜드)만 둔다 — 좁은 화면에서 상단 막대가 두 줄이 되면
// 정작 본문이 밀린다.
export function createTopbar() {
  toggleBtn = h('button.topbar-toggle', {
    type: 'button',
    'aria-label': '메뉴 열기',
    'aria-expanded': 'false',
    'aria-controls': 'sidebar',
    onclick: () => (isDrawerOpen() ? closeDrawer({ restoreFocus: true }) : openDrawer()),
  }, icon('menu', 20));

  return h('header.topbar', {},
    toggleBtn,
    h('a.topbar-brand', { href: '/' }, icon('database', 18), h('span', {}, 'DB Studio')),
  );
}

// createBackdrop은 서랍 뒤의 어두운 막이다. 눌러서 닫는 가장 흔한 방법이므로
// 장식이 아니라 버튼처럼 동작해야 한다.
export function createBackdrop() {
  return h('div.nav-backdrop', {
    onclick: () => closeDrawer({ restoreFocus: true }),
    'aria-hidden': 'true',
  });
}

// bindGlobalHandlers는 서랍을 닫아야 하는 나머지 경우를 처리한다.
// 셸을 다시 만들 때마다 부르므로, 문서 수준 리스너는 한 번만 붙인다.
let bound = false;

export function bindGlobalHandlers() {
  applyWidth(storedWidth(), { persist: false });
  setHidden(storedHidden(), { persist: false });
  if (bound) return;
  bound = true;

  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') closeDrawer({ restoreFocus: true });
    // Ctrl+B 로 접었다 편다. 편집기들이 오래 써 온 자리라 손이 먼저 기억한다.
    //
    // 글자를 치는 중에는 가로채지 않는다. 입력 칸에서 Ctrl+B 는 굵게가 될 수 있고,
    // 무엇보다 사람이 지금 하는 일과 상관없는 화면이 움직이면 놀란다.
    if ((e.ctrlKey || e.metaKey) && !e.shiftKey && !e.altKey
      && (e.key === 'b' || e.key === 'B')) {
      const el = document.activeElement;
      const typing = el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA'
        || el.isContentEditable);
      if (typing) return;
      e.preventDefault();
      toggleHidden();
    }
  });

  // 메뉴를 고르면 화면이 바뀐다. 서랍이 그 위에 남아 있으면 결과를 볼 수 없다.
  document.addEventListener('click', (e) => {
    if (e.target.closest('.sidebar a, .sidebar button')) closeDrawer();
  });

  // 넓은 화면으로 돌아가면 서랍 상태를 정리한다. 남겨 두면 본문 위에 막이 덮인 채로
  // 열림 상태가 유지된다.
  narrowQuery.addEventListener('change', (e) => {
    if (!e.matches) closeDrawer();
  });
}
