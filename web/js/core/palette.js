// 명령 팔레트: 키보드만으로 화면과 동작을 찾아 실행한다.
//
// 왜 필요한가: 메뉴가 서른 개를 넘었고, 그중 절반은 커넥션을 고른 뒤에야 뜻이 생긴다
// ("shop 의 스키마"는 두 화면을 거쳐야 닿는다). 어디에 있든 한 손으로 이름을 쳐서
// 바로 가는 길이 있으면, 화면 구조를 외우지 않아도 앱을 쓸 수 있다.
//
// 항목의 출처는 셋이다.
//   1) 화면 — 사이드바가 이미 들고 있는 목록을 그대로 받는다(권한 필터도 그대로).
//   2) 커넥션 — 처음 열 때 한 번 받아 두고, 커넥션마다 자주 가는 화면을 항목으로 만든다.
//   3) 동작 — 화면 이동이 아닌 것(테마, 어시스턴트, 로그아웃).
//
// 되돌릴 수 없는 동작은 넣지 않는다. 팔레트는 눈으로 확인하지 않고 Enter를 누르는
// 곳이라, 지우는 일이 여기 있으면 손이 미끄러지는 자리가 된다.
import { h, mount, icon } from './dom.js';
import { api } from './api.js';
import { state } from './store.js';

// 최근 쓴 것을 기억하는 자리. 사람마다 자주 가는 곳이 다르고, 그것은 서버가 아니라
// 이 브라우저의 습관이다.
const RECENT_KEY = 'dbstudio.palette.recent';
const RECENT_MAX = 6;

let overlay = null;
let paletteCleanup = null;
let connectionItems = null;
let connectionsLoading = false;

// ---------- 찾기 ----------

const CHO = ['ㄱ', 'ㄲ', 'ㄴ', 'ㄷ', 'ㄸ', 'ㄹ', 'ㅁ', 'ㅂ', 'ㅃ', 'ㅅ', 'ㅆ',
  'ㅇ', 'ㅈ', 'ㅉ', 'ㅊ', 'ㅋ', 'ㅌ', 'ㅍ', 'ㅎ'];

// initials는 한글의 첫 자음만 뽑는다("마이그레이션" → "ㅁㅇㄱㄹㅇㅅ").
//
// 한글 화면에서 이것이 없으면 팔레트는 "정확히 어떻게 쓰는지 아는 말"만 찾을 수 있다.
// ㅁㅇㄱ 로 마이그레이션에 닿는 것은 한국어 사용자에게 자연스러운 줄임이다.
function initials(text) {
  let out = '';
  for (const ch of text) {
    const code = ch.charCodeAt(0);
    if (code >= 0xac00 && code <= 0xd7a3) {
      out += CHO[Math.floor((code - 0xac00) / 588)];
      continue;
    }
    out += ch;
  }
  return out;
}

// subsequence는 글자가 순서대로 들어 있는지 본다("sqlc" → "SQL 콘솔").
function subsequence(haystack, needle) {
  let at = 0;
  for (const ch of needle) {
    at = haystack.indexOf(ch, at);
    if (at < 0) return false;
    at += 1;
  }
  return true;
}

// score는 얼마나 잘 맞는지다. 큰 값이 위로 온다. 0이면 맞지 않는다.
//
// 여러 방식을 두는 이유: 사람은 이름의 앞을 치기도 하고(스키), 가운데 낱말을 치기도
// 하고(콘솔), 초성만 치기도 한다(ㅁㅇㄱ). 하나만 지원하면 나머지 두 습관은 "안 나온다"가
// 된다. 대신 순서는 분명히 둔다 — 앞에서 맞은 것이 가운데에서 맞은 것보다 위다.
function score(item, query) {
  if (!query) return 1;
  const q = query.toLowerCase();
  const hay = `${item.label} ${item.section ?? ''} ${item.hint ?? ''} ${item.keywords ?? ''}`
    .toLowerCase();
  const at = hay.indexOf(q);
  if (at === 0) return 1000;
  if (at > 0) {
    // 낱말의 시작에서 맞은 것을 낱말 가운데에서 맞은 것보다 위로 둔다.
    const boundary = /[\s·/]/.test(hay[at - 1]) ? 300 : 100;
    return boundary - Math.min(at, 60);
  }
  const cho = initials(hay);
  const choAt = cho.indexOf(initials(q));
  if (choAt >= 0) return 80 - Math.min(choAt, 60);
  if (subsequence(hay, q)) return 40;
  return 0;
}

// ---------- 항목 만들기 ----------

function readRecent() {
  try {
    const raw = JSON.parse(localStorage.getItem(RECENT_KEY) ?? '[]');
    return Array.isArray(raw) ? raw.filter((x) => typeof x === 'string') : [];
  } catch {
    return [];
  }
}

function rememberRecent(id) {
  try {
    const next = [id, ...readRecent().filter((x) => x !== id)].slice(0, RECENT_MAX);
    localStorage.setItem(RECENT_KEY, JSON.stringify(next));
  } catch {
    // 기억하지 못할 뿐이다. 팔레트는 그대로 동작한다.
  }
}

// pageItems는 사이드바 목록을 팔레트 항목으로 바꾼다.
function pageItems(nav, opts) {
  const out = [];
  for (const group of nav) {
    for (const item of group.items) {
      if (item.requires && !state.permissions?.[item.requires]) continue;
      out.push({
        id: `page:${item.path}`,
        label: item.label,
        section: group.section,
        icon: item.icon,
        keywords: `${item.path} ${item.keywords ?? ''}`,
        run: () => {
          if (item.popup) {
            opts.onPopup?.(item.path);
            return;
          }
          opts.navigate(item.path);
        },
      });
    }
  }
  return out;
}

// connectionTargets는 커넥션 하나로 갈 수 있는 화면들이다.
//
// 커넥션마다 여러 줄을 만드는 이유: 사람이 떠올리는 것은 "shop 의 데이터"이지
// "데이터 화면을 연 다음 shop 고르기"가 아니다. 목록이 길어지는 것은 문제가 되지
// 않는다 — 팔레트는 걸러서 보여주는 곳이고, 빈 칸일 때는 최근 것만 보여준다.
const CONNECTION_TARGETS = [
  { path: '/schema', label: '스키마', icon: 'database' },
  { path: '/structure', label: '구조', icon: 'workflow' },
  { path: '/data', label: '데이터', icon: 'table', requires: 'anyData' },
  { path: '/sql', label: 'SQL 콘솔', icon: 'terminal', requires: 'anyData' },
];

function connectionItemsFrom(list, opts) {
  const out = [];
  for (const entry of list) {
    const conn = entry.connection;
    if (!conn || !entry.accessible) continue;
    for (const target of CONNECTION_TARGETS) {
      if (target.requires && !state.permissions?.[target.requires]) continue;
      out.push({
        id: `conn:${conn.id}:${target.path}`,
        label: `${conn.name} · ${target.label}`,
        section: '커넥션',
        icon: target.icon,
        hint: `${conn.kind}${conn.databaseName ? ` / ${conn.databaseName}` : ''}`,
        keywords: `${conn.host ?? ''} ${conn.databaseName ?? ''} ${target.path}`,
        run: () => opts.navigate(`${target.path}?conn=${encodeURIComponent(conn.id)}`),
      });
    }
  }
  return out;
}

// loadConnections는 커넥션 목록을 한 번만 받아 둔다.
//
// 팔레트를 열 때마다 받지 않는 이유: 팔레트는 자주, 짧게 열린다. 열 때마다 요청을
// 보내면 목록이 늦게 채워져 "쳤는데 안 나온다"가 된다. 커넥션이 추가·삭제되는 일은
// 드물고, 그때는 새로고침하면 된다.
async function loadConnections(opts, onReady) {
  if (connectionItems || connectionsLoading) return;
  connectionsLoading = true;
  try {
    const res = await api.get('/connections/');
    connectionItems = connectionItemsFrom(res.items ?? [], opts);
    onReady?.();
  } catch {
    // 못 받아도 화면 항목은 그대로 쓸 수 있다.
    connectionItems = [];
  } finally {
    connectionsLoading = false;
  }
}

// ---------- 화면 ----------

function renderList(box, items, cursor, onPick) {
  if (!items.length) {
    mount(box, h('p.palette-empty', {}, '찾는 것이 없습니다'));
    return;
  }
  mount(box, items.map((item, i) => h('button.palette-item', {
    type: 'button',
    class: i === cursor ? 'is-cursor' : '',
    // mousedown 으로 처리한다. click 은 blur 뒤에 오고, blur 가 팔레트를 닫으면
    // 그 클릭은 아무 데도 닿지 않는다.
    onmousedown: (e) => { e.preventDefault(); onPick(item); },
  },
  h('span.palette-icon', {}, icon(item.icon ?? 'list', 15)),
  h('span.palette-label', {}, item.label),
  item.hint ? h('span.palette-hint', {}, item.hint) : null,
  h('span.palette-section', {}, item.section ?? ''))));

  const active = box.querySelector('.is-cursor');
  active?.scrollIntoView({ block: 'nearest' });
}

export function closePalette() {
  paletteCleanup?.();
  paletteCleanup = null;
  overlay?.remove();
  overlay = null;
}

// openPalette는 팔레트를 연다. nav 는 사이드바 목록, opts 는 바깥 세계와의 연결이다.
export function openPalette(nav, opts) {
  if (overlay) {
    closePalette();
    return;
  }
  const input = h('input.palette-input', {
    type: 'text',
    placeholder: '화면·커넥션·동작을 찾습니다 (초성도 됩니다)',
    autocomplete: 'off',
    spellcheck: 'false',
  });
  const list = h('div.palette-list');
  const box = h('div.palette', {},
    h('div.palette-head', {}, icon('list', 16), input),
    list,
    h('div.palette-foot', {},
      h('span', {}, '↑↓ 이동 · Enter 실행 · Esc 닫기'),
    ),
  );
  overlay = h('div.palette-overlay', {
    // 바깥을 누르면 닫는다. 팔레트는 지나가는 도구라, 닫는 방법이 여럿이어야 한다.
    onmousedown: (e) => { if (e.target === overlay) closePalette(); },
  }, box);

  const all = () => [...pageItems(nav, opts), ...(connectionItems ?? []), ...actionItems(opts)];
  let shown = [];
  let cursor = 0;

  const draw = () => {
    const query = input.value.trim();
    if (!query) {
      // 빈 칸에서는 최근 것을 먼저 보여준다. 팔레트를 여는 이유의 절반은
      // "아까 그 화면으로 다시"이기 때문이다.
      const items = all();
      const recent = readRecent()
        .map((id) => items.find((x) => x.id === id))
        .filter(Boolean);
      const rest = items.filter((x) => !recent.includes(x));
      shown = [...recent, ...rest].slice(0, 40);
    } else {
      shown = all()
        .map((item) => ({ item, s: score(item, query) }))
        .filter((x) => x.s > 0)
        .sort((a, b) => b.s - a.s)
        .slice(0, 40)
        .map((x) => x.item);
    }
    cursor = Math.min(cursor, Math.max(0, shown.length - 1));
    renderList(list, shown, cursor, pick);
  };

  const pick = (item) => {
    if (!item) return;
    rememberRecent(item.id);
    closePalette();
    item.run();
  };

  input.addEventListener('input', () => { cursor = 0; draw(); });
  input.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowDown' || (e.key === 'n' && e.ctrlKey)) {
      e.preventDefault();
      cursor = shown.length ? (cursor + 1) % shown.length : 0;
      draw();
      return;
    }
    if (e.key === 'ArrowUp' || (e.key === 'p' && e.ctrlKey)) {
      e.preventDefault();
      cursor = shown.length ? (cursor - 1 + shown.length) % shown.length : 0;
      draw();
      return;
    }
    if (e.key === 'Enter') {
      e.preventDefault();
      pick(shown[cursor]);
      return;
    }
    if (e.key === 'Escape') {
      e.preventDefault();
      closePalette();
    }
  });

  // 브라우저 뒤로/앞으로 가기로 화면이 바뀌면 팔레트도 닫는다. 남겨 두면 바뀐 화면
  // 위에 옛 화면에서 열었던 상자가 떠 있게 된다.
  const onPop = () => closePalette();
  window.addEventListener('popstate', onPop);
  paletteCleanup = () => window.removeEventListener('popstate', onPop);

  document.body.appendChild(overlay);
  input.focus();
  draw();
  // 커넥션 목록은 늦게 와도 좋다. 오면 그 자리에서 다시 그린다.
  loadConnections(opts, () => { if (overlay) draw(); });
}

// actionItems는 화면 이동이 아닌 것들이다.
function actionItems(opts) {
  const out = [];
  for (const action of opts.actions ?? []) {
    out.push({
      id: `action:${action.id}`,
      label: action.label,
      section: '동작',
      icon: action.icon ?? 'settings',
      hint: action.hint ?? '',
      keywords: action.keywords ?? '',
      run: action.run,
    });
  }
  return out;
}

// bindPalette는 어디서든 팔레트를 부를 수 있게 키를 건다.
//
// Ctrl+K 와 Ctrl+Shift+P 둘 다 받는 이유: 앞의 것은 검색 상자를 여는 관습이고
// (Slack·Notion·GitHub), 뒤의 것은 명령 팔레트의 관습이다(VS Code). 사람마다 손에
// 익은 것이 다르고, 둘 다 이 앱에서 다른 뜻으로 쓰이지 않는다.
export function bindPalette(nav, opts) {
  const onKey = (e) => {
    const key = e.key.toLowerCase();
    const combo = (e.ctrlKey || e.metaKey) && !e.altKey
      && (key === 'k' || (key === 'p' && e.shiftKey));
    if (!combo) return;
    e.preventDefault();
    openPalette(nav, opts);
  };
  document.addEventListener('keydown', onKey);
  return () => document.removeEventListener('keydown', onKey);
}
