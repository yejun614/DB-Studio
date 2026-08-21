// 초경량 DOM 헬퍼. 프레임워크 없이 컴포넌트를 조립하기 위한 최소 도구.

// h('div.card', { onclick }, children) 형태로 엘리먼트를 만든다.
// 선택자 문법: tag#id.class1.class2
export function h(selector, props = null, ...children) {
  const { tag, id, classes } = parseSelector(selector);
  const el = document.createElement(tag);
  if (id) el.id = id;
  if (classes.length) el.classList.add(...classes);

  if (props) {
    for (const [key, value] of Object.entries(props)) {
      if (value === null || value === undefined || value === false) continue;
      if (key === 'class') {
        el.classList.add(...String(value).split(/\s+/).filter(Boolean));
      } else if (key === 'dataset') {
        Object.assign(el.dataset, value);
      } else if (key === 'style' && typeof value === 'object') {
        Object.assign(el.style, value);
      } else if (key.startsWith('on') && typeof value === 'function') {
        el.addEventListener(key.slice(2).toLowerCase(), value);
      } else if (key === 'html') {
        // 신뢰할 수 있는 내부 마크업에만 사용한다. 사용자 입력은 절대 넣지 않는다.
        el.innerHTML = value;
      } else if (key in el && key !== 'list') {
        el[key] = value;
      } else {
        el.setAttribute(key, value === true ? '' : value);
      }
    }
  }

  append(el, children);
  return el;
}

function append(parent, children) {
  for (const child of children) {
    if (child === null || child === undefined || child === false) continue;
    if (Array.isArray(child)) {
      append(parent, child);
    } else if (child instanceof Node) {
      parent.appendChild(child);
    } else {
      parent.appendChild(document.createTextNode(String(child)));
    }
  }
}

function parseSelector(selector) {
  const match = /^([a-zA-Z][a-zA-Z0-9-]*)?(#[^.]+)?((?:\.[^.#]+)*)$/.exec(selector);
  if (!match) return { tag: 'div', id: '', classes: [] };
  return {
    tag: match[1] || 'div',
    id: match[2] ? match[2].slice(1) : '',
    classes: match[3] ? match[3].split('.').filter(Boolean) : [],
  };
}

export function clear(el) {
  while (el.firstChild) el.removeChild(el.firstChild);
  return el;
}

export function mount(el, ...children) {
  clear(el);
  append(el, children);
  return el;
}

// 아이콘: 외부 폰트나 이미지를 쓰지 않고 인라인 SVG로 그린다.
export function icon(name, size = 16) {
  const paths = ICONS[name];
  if (!paths) return h('span');
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('width', size);
  svg.setAttribute('height', size);
  svg.setAttribute('fill', 'none');
  svg.setAttribute('stroke', 'currentColor');
  svg.setAttribute('stroke-width', '1.8');
  svg.setAttribute('stroke-linecap', 'round');
  svg.setAttribute('stroke-linejoin', 'round');
  svg.classList.add('icon');
  for (const d of paths) {
    const p = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    p.setAttribute('d', d);
    svg.appendChild(p);
  }
  return svg;
}

const ICONS = {
  database: ['M12 3c4.97 0 9 1.34 9 3s-4.03 3-9 3-9-1.34-9-3 4.03-3 9-3z', 'M3 6v12c0 1.66 4.03 3 9 3s9-1.34 9-3V6', 'M3 12c0 1.66 4.03 3 9 3s9-1.34 9-3'],
  users: ['M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2', 'M9 11a4 4 0 1 0 0-8 4 4 0 0 0 0 8z', 'M23 21v-2a4 4 0 0 0-3-3.87', 'M16 3.13a4 4 0 0 1 0 7.75'],
  activity: ['M22 12h-4l-3 9L9 3l-3 9H2'],
  list: ['M8 6h13', 'M8 12h13', 'M8 18h13', 'M3 6h.01', 'M3 12h.01', 'M3 18h.01'],
  shield: ['M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z'],
  plus: ['M12 5v14', 'M5 12h14'],
  minus: ['M5 12h14'],
  // 화면에 맞추기: 네 귀퉁이로 펼치는 화살표
  maximize: ['M15 3h6v6', 'M9 21H3v-6', 'M21 3l-7 7', 'M3 21l7-7'],
  'chevron-left': ['M15 18l-6-6 6-6'],
  'chevron-right': ['M9 18l6-6-6-6'],
  check: ['M20 6L9 17l-5-5'],
  x: ['M18 6L6 18', 'M6 6l12 12'],
  logout: ['M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4', 'M16 17l5-5-5-5', 'M21 12H9'],
  key: ['M21 2l-2 2m-7.61 7.61a5.5 5.5 0 1 1-7.778 7.778 5.5 5.5 0 0 1 7.777-7.777zm0 0L15.5 7.5m0 0l3 3L22 7l-3-3m-3.5 3.5L19 4'],
  refresh: ['M23 4v6h-6', 'M1 20v-6h6', 'M3.51 9a9 9 0 0 1 14.85-3.36L23 10', 'M1 14l4.64 4.36A9 9 0 0 0 20.49 15'],
  trash: ['M3 6h18', 'M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6', 'M10 11v6', 'M14 11v6', 'M9 6V4a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v2'],
  edit: ['M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7', 'M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z'],
  settings: ['M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6z', 'M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06A1.65 1.65 0 0 0 4.6 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06A1.65 1.65 0 0 0 9 4.6 1.65 1.65 0 0 0 10 3.09V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06.06A1.65 1.65 0 0 0 19.4 9v0c.24.58.78.98 1.42 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z'],
  play: ['M5 3l14 9-14 9V3z'],
  lock: ['M19 11H5a2 2 0 0 0-2 2v7a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7a2 2 0 0 0-2-2z', 'M7 11V7a5 5 0 0 1 10 0v4'],
  link: ['M9 17H7A5 5 0 0 1 7 7h2', 'M15 7h2a5 5 0 0 1 0 10h-2', 'M8 12h8'],
  copy: ['M20 9h-9a2 2 0 0 0-2 2v9a2 2 0 0 0 2 2h9a2 2 0 0 0 2-2v-9a2 2 0 0 0-2-2z', 'M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1'],
  alert: ['M10.29 3.86L1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z', 'M12 9v4', 'M12 17h.01'],
  sun: ['M12 17a5 5 0 1 0 0-10 5 5 0 0 0 0 10z', 'M12 1v2', 'M12 21v2', 'M4.22 4.22l1.42 1.42', 'M18.36 18.36l1.42 1.42', 'M1 12h2', 'M21 12h2', 'M4.22 19.78l1.42-1.42', 'M18.36 5.64l1.42-1.42'],
  moon: ['M21 12.79A9 9 0 1 1 11.21 3a7 7 0 0 0 9.79 9.79z'],
  monitor: ['M20 3H4a1 1 0 0 0-1 1v12a1 1 0 0 0 1 1h16a1 1 0 0 0 1-1V4a1 1 0 0 0-1-1z', 'M8 21h8', 'M12 17v4'],
  menu: ['M4 6h16', 'M4 12h16', 'M4 18h16'],
  table: ['M3 5h18v14H3z', 'M3 10h18', 'M9 10v9', 'M15 10v9'],
  terminal: ['M4 17l6-5-6-5', 'M12 19h8'],
  // 노드 두 개와 연결선. 매크로(노드 에디터)를 나타낸다.
  workflow: [
    'M4 4h6v6H4z', 'M14 14h6v6h-6z', 'M10 7h2a2 2 0 0 1 2 2v8',
  ],
  stop: ['M6 6h12v12H6z'],
  save: ['M19 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11l5 5v11a2 2 0 0 1-2 2z', 'M17 21v-8H7v8', 'M7 3v5h8'],
  history: ['M3 3v5h5', 'M3.05 13A9 9 0 1 0 6 5.3L3 8', 'M12 7v5l4 2'],
  code: ['M16 18l6-6-6-6', 'M8 6l-6 6 6 6'],
  share: ['M4 12v7a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-7', 'M16 6l-4-4-4 4', 'M12 2v14'],
  undo: ['M3 7v6h6', 'M3 13a9 9 0 1 0 3-7.7L3 8'],
  // ERD 카드 표식용. 도메인을 눈으로 가르는 것이 목적이라 서로 확실히 구분되는
  // 모양만 고른다 — 비슷한 그림이 스무 개면 다음에 무엇을 골랐는지 기억하지 못한다.
  // AI 관련 표식. 별 세 개(반짝임)는 이 용도로 널리 쓰여 설명 없이 읽힌다.
  sparkles: ['M12 3l1.8 4.2L18 9l-4.2 1.8L12 15l-1.8-4.2L6 9l4.2-1.8z', 'M18 15l.9 2.1L21 18l-2.1.9L18 21l-.9-2.1L15 18l2.1-.9z'],
  // 표를 왼쪽에 작게 두고 더하기를 오른쪽 위에 크게 둔다.
  //
  // 처음에는 표 전체(24px)에 작은 십자를 얹었는데, 15px로 줄면 십자가 표의 칸선과
  // 섞여 그냥 네모로 보였다. 두 뜻(무엇을 + 어떻게)을 겹치지 않게 나눠 놓아야
  // 작은 크기에서도 읽힌다.
  'table-plus': [
    'M3 6h11v13H3z', 'M3 10.5h11', 'M8.5 10.5v8.5',
    'M18.5 3.5v7', 'M15 7h7',
  ],
  cart: ['M3 4h2l2.4 10.4a2 2 0 0 0 2 1.6h7.7a2 2 0 0 0 2-1.6L21 8H6', 'M10 20a1 1 0 1 0 0-.01', 'M18 20a1 1 0 1 0 0-.01'],
  box: ['M21 8l-9-5-9 5 9 5 9-5z', 'M3 8v8l9 5 9-5V8', 'M12 13v8'],
  money: ['M12 2v20', 'M17 6H9.5a3.5 3.5 0 0 0 0 7h5a3.5 3.5 0 0 1 0 7H6'],
  chat: ['M21 15a2 2 0 0 1-2 2H8l-4 4V5a2 2 0 0 1 2-2h13a2 2 0 0 1 2 2z'],
  mail: ['M3 6h18v12H3z', 'M3 7l9 6 9-6'],
  bell: ['M18 8a6 6 0 1 0-12 0c0 7-3 8-3 8h18s-3-1-3-8', 'M13.7 21a2 2 0 0 1-3.4 0'],
  calendar: ['M3 5h18v16H3z', 'M3 10h18', 'M8 3v4', 'M16 3v4'],
  chart: ['M4 20V10', 'M10 20V4', 'M16 20v-7', 'M22 20H2'],
  file: ['M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z', 'M14 2v6h6'],
  tag: ['M20.6 13.4 12 22l-9-9V4h9z', 'M7.5 7.5h.01'],
  location: ['M20 10c0 6-8 12-8 12s-8-6-8-12a8 8 0 1 1 16 0z', 'M12 10a2 2 0 1 0 0-.01'],
  truck: ['M3 6h11v9H3z', 'M14 9h4l3 3v3h-7', 'M7 18a1.5 1.5 0 1 0 0-.01', 'M17 18a1.5 1.5 0 1 0 0-.01'],
  star: ['M12 3l2.7 5.6 6.1.9-4.4 4.3 1 6.1-5.4-2.9-5.4 2.9 1-6.1L3.2 9.5l6.1-.9z'],
  flag: ['M4 21V4', 'M4 4h12l-2 4 2 4H4'],
  redo: ['M21 7v6h-6', 'M21 13a9 9 0 1 1-3-7.7L21 8'],
};
