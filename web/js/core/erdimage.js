// 다이어그램을 그림 파일로 내보낸다.
//
// 서버를 거치지 않는다. 그림은 이미 화면에 SVG로 그려져 있고, 서버로 보내면 같은
// 그림을 두 번 만드는 셈이다(그러려면 서버에도 브라우저가 필요하다).
//
// 어려운 점은 하나뿐이다: 화면의 SVG는 **혼자서는 아무것도 아니다**. 색과 글꼴이
// 모두 app.css의 클래스와 CSS 변수에 들어 있어서, 그대로 떼어 내면 검은 글씨의
// 뼈대만 남는다. 그래서 내보내기 전에 계산된 값을 요소마다 박아 넣는다.
import { h, icon } from './dom.js';
import { openModal, field, select, checkbox, toast } from './ui.js';

// 그림에 박아 넣을 속성. 이것만 있으면 SVG는 스타일시트 없이 같은 그림이 된다.
const PAINTED = [
  'fill', 'fill-opacity', 'stroke', 'stroke-width', 'stroke-opacity',
  'stroke-dasharray', 'stroke-linecap', 'stroke-linejoin',
  'font-family', 'font-size', 'font-weight', 'font-style',
  'text-anchor', 'dominant-baseline', 'opacity', 'letter-spacing',
];

// 부모에게서 물려받는 속성. 값이 부모와 같으면 적지 않아도 같은 그림이 된다.
//
// 이 목록을 따로 두는 이유: opacity 는 물려받지 않는다. 부모와 값이 같다고 빼면
// 부모의 반투명이 한 번 더 곱해져 그림이 흐려진다.
const INHERITED = new Set([
  'fill', 'fill-opacity', 'stroke', 'stroke-width', 'stroke-opacity',
  'stroke-dasharray', 'stroke-linecap', 'stroke-linejoin',
  'font-family', 'font-size', 'font-weight', 'font-style',
  'text-anchor', 'letter-spacing',
]);

// 브라우저가 그릴 수 있는 그림의 한계. 이보다 크면 canvas가 조용히 빈 그림을 준다.
const MAX_PIXELS = 16000;

// 그림에서 빼는 것들. 모두 **마우스를 위한 것**이지 내용이 아니다.
//
// 크기 손잡이(묶음·메모 오른쪽 아래의 작은 사각형)가 대표적이다. 화면에서는 "여기를
// 끌면 커진다"는 안내지만, 파일로 받은 사람에게는 아무 뜻 없는 사각형 하나다.
// 관계선의 투명한 굵은 선(누르기 쉬우라고 겹쳐 둔 것)도 보이지는 않지만 파일만
// 키우고, 벡터 편집기에서 열면 정체를 알 수 없는 도형으로 걸린다.
const CHROME_ONLY = [
  'erd-group-grip', 'erd-note-grip', 'erd-card-grip', 'erd-link-hit', 'erd-card-outline',
  // 폭을 끄는 동안 보여 주는 px 숫자.
  'erd-card-wnote',
  // 마우스를 올렸거나 골라서 잠깐 카드 위로 올려 둔 관계선. 같은 선이 아래
  // 레이어에 이미 있어서 남기면 한 선이 두 번 그려진다.
  'erd-link-temp',
  // 다른 참여자가 고르고 있다는 표시와 커서, 빈 화면 안내문.
  'erd-card-holder', 'erd-cursor', 'erd-hint',
];

// 내보낼 범위. 도면 하나를 다 담는 것만이 답이 아니다 — 표 쉰 개짜리 설계에서
// 지금 이야기하고 있는 세 개만 붙이고 싶을 때가 훨씬 많다.
export const SCOPES = { all: 'all', marks: 'marks', group: 'group' };

function groupRect(g) {
  return { x: g.x, y: g.y, w: g.w || 320, h: g.h || 240 };
}

function noteRect(n) {
  return { x: n.x, y: n.y, w: n.w || 200, h: n.h || 80 };
}

// centerIn은 상자의 **중심**이 사각형 안에 드는지다.
//
// 묶음은 그냥 사각형이고 무엇이 자기 것인지 모른다. 그래서 "안에 있는가"를 자리로
// 정해야 한다. 걸치기만 해도 넣으면 가장자리에 살짝 닿은 표까지 들어오고, 완전히
// 든 것만 넣으면 조금 삐져나온 표가 빠진다 — 둘 다 눈으로 본 것과 다르다.
// 중심으로 정하면 사람이 "이건 이 묶음 안이다"라고 보는 것과 거의 같다.
// 삐져나온 부분이 잘릴 걱정도 없다. 범위는 든 것들의 실제 크기로 다시 재기 때문이다.
function centerIn(rect, b) {
  const cx = b.x + b.w / 2;
  const cy = b.y + b.h / 2;
  return cx >= rect.x && cx <= rect.x + rect.w && cy >= rect.y && cy <= rect.y + rect.h;
}

/**
 * exportScope는 내보낼 요소의 목록이다. null 이면 "전부"다.
 *
 * null 을 쓰는 이유: 전체 내보내기는 아무것도 거르지 않는 길로 그대로 돌아야 한다.
 * 빈 목록과 "제한 없음"을 같은 것으로 두면 언젠가 빈 그림이 나온다.
 */
export function exportScope(canvas, scope, groupId) {
  if (scope !== SCOPES.marks && scope !== SCOPES.group) return null;

  const tables = new Set();
  const notes = new Set();
  const groups = new Set();
  const boxes = canvas.boxes();

  const addInside = (rect) => {
    for (const [key, b] of boxes) if (centerIn(rect, b)) tables.add(key);
    for (const n of canvas.doc.notes ?? []) if (centerIn(rect, noteRect(n))) notes.add(n.id);
    // 묶음 안의 묶음도 함께 담는다.
    for (const g of canvas.doc.groups ?? []) if (centerIn(rect, groupRect(g))) groups.add(g.id);
  };
  const findGroup = (id) => (canvas.doc.groups ?? []).find((g) => g.id === id);

  if (scope === SCOPES.group) {
    const g = findGroup(groupId);
    if (!g) return null;
    groups.add(g.id);
    addInside(groupRect(g));
  } else {
    for (const m of canvas.marks ?? []) {
      if (m.kind === 'table') tables.add(m.id);
      else if (m.kind === 'note') notes.add(m.id);
      else if (m.kind === 'group') {
        groups.add(m.id);
        // 묶음을 골랐다면 그 안에 든 것까지 뜻한 것이다. 사각형만 나오면
        // 빈 테두리 하나가 그림이 된다.
        const g = findGroup(m.id);
        if (g) addInside(groupRect(g));
      }
    }
  }

  // 관계선은 **양쪽이 다 들어야** 남긴다. 한쪽만 들면 선이 빈 곳으로 뻗어 나가고,
  // 그림을 받은 사람은 잘려 나간 표가 있다고 읽는다.
  const links = new Set();
  for (const [fkID, r] of canvas.linkSpots ?? []) {
    if (tables.has(r.fromKey) && tables.has(r.toKey)) links.add(fkID);
  }
  return {
    tables, notes, groups, links,
  };
}

// scopeCount는 이 범위에 몇 개가 들었는지다. 누르기 전에 보여 주려고 쓴다.
export function scopeCount(scope) {
  if (!scope) return null;
  return { tables: scope.tables.size, notes: scope.notes.size, groups: scope.groups.size };
}

// outOfScope는 이 요소가 내보낼 범위 밖인지다.
function outOfScope(el, scope) {
  const d = el.dataset;
  if (!d) return false;
  if (d.key !== undefined) return !scope.tables.has(d.key);
  if (d.note !== undefined) return !scope.notes.has(d.note);
  if (d.group !== undefined) return !scope.groups.has(d.group);
  if (d.fk !== undefined) return !scope.links.has(d.fk);
  return false;
}

// diagramBounds는 그림에 담을 것들을 감싸는 사각형이다.
//
// 지금 보고 있는 화면이 아닌 이유: 내보내기는 "이 설계를 그림으로 남긴다"는 뜻이지
// "지금 화면을 찍는다"가 아니다. 화면 밖의 테이블이 잘려 나가면 그 사실을 파일을
// 열어 보고서야 알게 된다.
export function diagramBounds(canvas, pad = 40, scope = null) {
  let minX = Infinity;
  let minY = Infinity;
  let maxX = -Infinity;
  let maxY = -Infinity;
  const put = (x, y, w, hh) => {
    minX = Math.min(minX, x);
    minY = Math.min(minY, y);
    maxX = Math.max(maxX, x + w);
    maxY = Math.max(maxY, y + hh);
  };
  for (const [key, b] of canvas.boxes()) {
    if (scope && !scope.tables.has(key)) continue;
    put(b.x, b.y, b.w, b.h);
  }
  for (const n of canvas.doc.notes ?? []) {
    if (scope && !scope.notes.has(n.id)) continue;
    put(n.x, n.y, n.w || 200, n.h || 80);
  }
  for (const g of canvas.doc.groups ?? []) {
    if (scope && !scope.groups.has(g.id)) continue;
    put(g.x, g.y, g.w || 320, g.h || 240);
  }
  // 관계선도 센다. 길찾기가 카드를 피해 돌아가면 선은 카드 밖으로 나간다 —
  // 카드만 보고 자르면 돌아간 부분이 잘려 나가, 그림만 받은 사람에게는 선이
  // 허공에서 끊긴 것으로 보인다.
  const links = canvas.svg?.querySelector('.erd-layer-links');
  if (links?.firstChild) {
    try {
      if (scope) {
        // 범위를 골랐으면 남는 선만 센다. 레이어 전체를 재면 빠질 선까지 담아
        // 그림에 아무것도 없는 여백이 생긴다.
        for (const el of links.querySelectorAll('.erd-link[data-fk]')) {
          if (!scope.links.has(el.dataset.fk)) continue;
          const bb = el.getBBox();
          if (bb.width || bb.height) put(bb.x, bb.y, bb.width, bb.height);
        }
      } else {
        const bb = links.getBBox();
        if (bb.width || bb.height) put(bb.x, bb.y, bb.width, bb.height);
      }
    } catch { /* 화면에 붙어 있지 않으면 getBBox가 안 된다 */ }
  }
  if (minX === Infinity) return { x: 0, y: 0, w: 400, h: 300 };
  return {
    x: Math.round(minX - pad),
    y: Math.round(minY - pad),
    w: Math.round(maxX - minX + pad * 2),
    h: Math.round(maxY - minY + pad * 2),
  };
}

// buildSVG는 혼자서도 같은 그림이 되는 SVG 문자열을 만든다.
function buildSVG(canvas, box, background, scope = null) {
  const source = canvas.svg;

  // "고른 표시"는 화면에서 **잠깐 떼고** 읽는다.
  //
  // 계산된 값을 박아 넣는 방식이라, 골라 둔 것의 파란 테두리(묶음은 파선까지)가
  // 그대로 그림에 굳는다. 어느 속성이 달라졌는지 하나씩 되돌리는 것은 종류마다
  // 다른 규칙을 손으로 흉내 내는 일이라 언젠가 어긋난다. 클래스를 떼면 브라우저가
  // 원래 모습을 계산해 준다.
  //
  // 떼고 붙이는 사이에 화면이 다시 그려지지는 않는다. 이 함수가 끝날 때까지
  // 브라우저는 그리지 않으므로 사람 눈에는 아무 일도 일어나지 않는다.
  // 폭 손잡이에 손이 닿은 카드 표시도 같이 뗀다. 남으면 그 카드만 테두리 색이
  // 다른 그림이 나오고, 받은 사람에게는 "이 표는 왜 다른가"가 된다.
  const MARKS = ['is-selected', 'is-primary', 'is-grip-hover', 'is-resizing'];
  const marked = [];
  for (const el of source.querySelectorAll(MARKS.map((c) => `.${c}`).join(', '))) {
    const had = MARKS.filter((c) => el.classList.contains(c));
    marked.push({ el, had });
    el.classList.remove(...had);
  }
  try {
    return paintAndSerialize(source, box, background, scope);
  } finally {
    for (const { el, had } of marked) el.classList.add(...had);
  }
}

// paintAndSerialize는 계산된 값을 박아 넣고 문자열로 만든다.
function paintAndSerialize(source, box, background, scope = null) {
  const clone = source.cloneNode(true);

  // 계산된 값을 요소마다 박는다. 원본과 사본은 같은 순서로 훑을 수 있다
  // (cloneNode는 순서를 지킨다). 원본에서 읽는 이유는 문서에 붙어 있는 요소만
  // 계산된 값을 갖기 때문이다.
  const from = [source, ...source.querySelectorAll('*')];
  const to = [clone, ...clone.querySelectorAll('*')];
  for (let i = 0; i < from.length; i += 1) {
    const computed = getComputedStyle(from[i]);
    // 부모와 같은 값은 적지 않는다. 글꼴 이름만 해도 한 줄에 200자가 넘어서,
    // 모든 요소에 적으면 테이블 쉰 개짜리 그림이 몇 MB가 된다.
    const parent = i > 0 && from[i].parentElement ? getComputedStyle(from[i].parentElement) : null;
    const parts = [];
    for (const name of PAINTED) {
      const value = computed.getPropertyValue(name);
      // 빈 값만 거른다. 'none' 도 반드시 적어야 한다 — 관계선은 fill:none 인데,
      // 안 적으면 SVG의 기본값(검정 채우기)이 살아나 선이 검은 띠가 된다.
      if (!value) continue;
      if (parent && INHERITED.has(name) && parent.getPropertyValue(name) === value) continue;
      parts.push(`${name}:${value}`);
    }
    if (parts.length) to[i].setAttribute('style', parts.join(';'));
    else to[i].removeAttribute('style');
    // 클래스는 지운다. 스타일시트 없는 곳에서는 뜻이 없고, 남겨 두면 파일을 받은
    // 사람이 그것으로 무언가를 할 수 있다고 오해한다.
    to[i].removeAttribute('class');
  }

  // 지금 이 화면의 사정(남의 커서, 범위 사각형, 고른 표시)은 그림에 남을 것이 아니다.
  for (const el of [...clone.querySelectorAll('.erd-layer-cursors, .erd-layer-band')]) el.remove();
  const drop = [];
  for (let i = 0; i < from.length; i += 1) {
    const classes = from[i].classList;
    if (!classes) continue;
    if (CHROME_ONLY.some((name) => classes.contains(name))) drop.push(to[i]);
    else if (scope && outOfScope(from[i], scope)) drop.push(to[i]);
  }
  for (const el of drop) el.remove();

  clone.setAttribute('xmlns', 'http://www.w3.org/2000/svg');
  clone.setAttribute('width', String(box.w));
  clone.setAttribute('height', String(box.h));
  clone.setAttribute('viewBox', `${box.x} ${box.y} ${box.w} ${box.h}`);

  if (background) {
    const rect = document.createElementNS('http://www.w3.org/2000/svg', 'rect');
    rect.setAttribute('x', String(box.x));
    rect.setAttribute('y', String(box.y));
    rect.setAttribute('width', String(box.w));
    rect.setAttribute('height', String(box.h));
    rect.setAttribute('fill', background);
    clone.insertBefore(rect, clone.firstChild);
  }
  return new XMLSerializer().serializeToString(clone);
}

/**
 * withTheme는 잠깐 다른 테마로 바꿔 놓고 함수를 돌린다.
 *
 * 그림의 색은 화면에서 **계산된 값을 읽어 박는** 방식이라, 다른 테마로 내보내려면
 * 화면을 잠깐 그 테마로 만들어야 한다. 색 이름을 따로 표로 들고 있는 방법도 있지만,
 * 그러면 app.css를 고친 사람이 이 표를 잊고 "내보낸 그림만 색이 다르다"가 된다.
 *
 * 읽고 되돌리는 사이에 화면이 다시 그려지지는 않는다. 모두 같은 동기 블록 안에서
 * 일어나므로 브라우저는 그 사이에 그리지 않고, 사람 눈에는 아무 일도 없다.
 */
function withTheme(mode, fn) {
  if (!mode) return fn();
  const root = document.documentElement;
  const had = root.getAttribute('data-theme');
  root.setAttribute('data-theme', mode);
  try {
    return fn();
  } finally {
    if (had === null) root.removeAttribute('data-theme');
    else root.setAttribute('data-theme', had);
  }
}

// rasterize는 SVG를 픽셀 그림으로 굽는다.
async function rasterize(svgText, box, { scale, mime, background }) {
  const width = Math.round(box.w * scale);
  const height = Math.round(box.h * scale);
  const img = new Image();
  // data: URL 로 싣는다. 바깥을 가리키지 않는 그림이라 캔버스가 오염되지 않는다.
  img.src = `data:image/svg+xml;charset=utf-8,${encodeURIComponent(svgText)}`;
  await img.decode();

  const el = document.createElement('canvas');
  el.width = width;
  el.height = height;
  const ctx = el.getContext('2d');
  // JPG에는 투명이 없다. 배경을 깔지 않으면 검게 굳는다.
  if (background) {
    ctx.fillStyle = background;
    ctx.fillRect(0, 0, width, height);
  }
  ctx.drawImage(img, 0, 0, width, height);
  return new Promise((resolve, reject) => {
    el.toBlob(
      (blob) => (blob ? resolve(blob) : reject(new Error('브라우저가 그림을 만들지 못했습니다'))),
      mime, 0.92,
    );
  });
}

function saveBlob(blob, filename) {
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  // 즉시 해제하면 브라우저가 저장을 시작하기 전에 사라질 수 있다.
  setTimeout(() => URL.revokeObjectURL(url), 10000);
}

function fileStamp() {
  const d = new Date();
  const two = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}${two(d.getMonth() + 1)}${two(d.getDate())}`;
}

function safeName(name) {
  const cleaned = String(name ?? 'diagram').replace(/[\\/:*?"<>|]/g, '_').trim();
  return cleaned || 'diagram';
}

// parseRGB는 'rgb(...)' / 'rgba(...)' 를 숫자로 푼다. 다른 표기는 null.
function parseRGB(value) {
  const m = /^rgba?\(([^)]+)\)$/.exec(String(value ?? '').trim());
  if (!m) return null;
  const parts = m[1].split(/[,/]/).map((x) => Number(x.trim()));
  if (parts.length < 3 || parts.slice(0, 3).some(Number.isNaN)) return null;
  return { r: parts[0], g: parts[1], b: parts[2], a: parts.length > 3 ? parts[3] : 1 };
}

// 배경색은 캔버스가 깔고 있는 색을 그대로 쓴다. 점무늬는 옮기지 않는다 —
// 그림으로 남길 때 필요한 것은 격자가 아니라 내용이다.
//
// 다만 **불투명하게** 만들어 내보낸다. 캔버스의 배경은 반투명이라(화면에서는 그 아래
// 페이지 색과 겹쳐 보인다) 그대로 쓰면 파일을 흰 바탕에서 열었을 때 색이 바랜다.
// 그래서 뒤에 깔린 불투명한 색을 찾아 미리 겹쳐 둔다.
function canvasBackground(canvas) {
  const wrap = canvas.svg.parentElement;
  const front = parseRGB(wrap ? getComputedStyle(wrap).backgroundColor : '');
  if (!front || front.a === 0) return behindColor(wrap);
  if (front.a >= 1) return `rgb(${front.r}, ${front.g}, ${front.b})`;
  const back = parseRGB(behindColor(wrap)) ?? { r: 255, g: 255, b: 255, a: 1 };
  const mix = (f, k) => Math.round(f * front.a + k * (1 - front.a));
  return `rgb(${mix(front.r, back.r)}, ${mix(front.g, back.g)}, ${mix(front.b, back.b)})`;
}

// behindColor는 이 요소 뒤에 실제로 깔린 불투명한 색이다.
function behindColor(el) {
  let node = el?.parentElement;
  while (node) {
    const c = parseRGB(getComputedStyle(node).backgroundColor);
    if (c && c.a >= 1) return `rgb(${c.r}, ${c.g}, ${c.b})`;
    node = node.parentElement;
  }
  return '#ffffff';
}

// openImageExportDialog는 형식·배율을 고르는 창을 연다.
export function openImageExportDialog(canvas, docName) {
  // 고를 수 있는 범위만 목록에 넣는다. 고른 것이 없는데 "고른 것만"이 보이면
  // 눌러 보고서야 안 되는 것을 알게 된다.
  const groups = (canvas.doc.groups ?? []).filter((g) => g?.id);
  const marks = canvas.marks ?? [];
  const markCount = marks.filter((m) => m.kind !== 'link').length;
  const pickedGroup = marks.find((m) => m.kind === 'group');

  const scopeOptions = [{ value: SCOPES.all, label: '전체 — 이 설계에 있는 모든 것' }];
  if (markCount) {
    scopeOptions.push({ value: SCOPES.marks, label: `고른 것만 — ${markCount}개` });
  }
  if (groups.length) {
    scopeOptions.push({ value: SCOPES.group, label: '묶음 하나 — 그 안에 든 것까지' });
  }
  // 기본은 언제나 전체다.
  //
  // 골라 둔 것이 있으면 그것을 기본으로 하고 싶어지지만, 두 실수의 무게가 다르다.
  // 전체를 받았는데 일부만 원했다면 그 자리에서 알아채고 다시 하면 된다. 반대로
  // 일부만 받았는데 전체로 알았다면 파일을 남에게 보낸 뒤에 알게 된다.
  // 골라 둔 묶음은 아래 묶음 칸에 미리 채워 두므로, 한 번 고르면 그만이다.
  const scopeSelect = select(scopeOptions, { value: SCOPES.all });
  const groupSelect = select(groups.map((g, i) => ({
    value: g.id, label: (g.label ?? '').trim() || `이름 없는 묶음 ${i + 1}`,
  })), { value: pickedGroup?.id ?? groups[0]?.id });
  const groupField = field('묶음', groupSelect);
  const scopeNote = h('p.field-help');
  // 범위마다 알아야 할 것이 다르다. 셋을 다 적어 두면 창이 설명서가 되고, 설명서가
  // 된 창은 아무도 읽지 않는다.
  const scopeHelp = h('p.field-help');

  let scope = null;
  let box = diagramBounds(canvas);

  const formatSelect = select([
    { value: 'png', label: 'PNG — 투명 배경을 쓸 수 있습니다' },
    { value: 'svg', label: 'SVG — 벡터, 확대해도 또렷합니다' },
    { value: 'jpg', label: 'JPG — 용량이 작고 붙여넣기 편합니다' },
  ], { value: 'png' });
  const scaleSelect = select([
    { value: '1', label: '1× (기본)' },
    { value: '2', label: '2× (고해상도)' },
    { value: '3', label: '3×' },
    { value: '4', label: '4×' },
  ], { value: '2' });
  const bgBox = checkbox('배경 채우기', { checked: true });
  const sizeNote = h('p.field-help');
  // 테마. 지금 화면을 그대로 쓰거나, 어느 쪽이든 골라 낼 수 있다.
  //
  // 필요한 이유: 다크 테마로 보며 일하다가 밝은 문서에 붙이면 검은 덩어리가 되고,
  // 반대도 마찬가지다. 붙일 곳의 색을 아는 사람은 여기서 고르면 그만이다.
  const themeSelect = select([
    { value: '', label: '지금 화면과 같게' },
    { value: 'light', label: '라이트 — 흰 배경 문서에 붙일 때' },
    { value: 'dark', label: '다크 — 어두운 발표 자료에 붙일 때' },
  ], { value: '' });

  // 미리보기. 내보내기는 되돌릴 수 없는 일은 아니지만, 배경·테마·범위를 잘못 골라
  // 받은 파일은 열어 보고서야 알게 되고 그때마다 창을 다시 열어야 한다.
  const previewImg = h('img.export-preview-img', { alt: '' });
  const previewNote = h('span.export-preview-note');
  const previewBox = h('div.export-preview', {}, previewImg, previewNote);

  const refresh = () => {
    // 범위가 바뀌면 담을 사각형이 달라진다. 크기 안내가 옛 값을 말하면 사람은
    // 그 값을 믿고 배율을 정한다.
    scope = exportScope(canvas, scopeSelect.value, groupSelect.value);
    box = diagramBounds(canvas, 40, scope);
    groupField.hidden = scopeSelect.value !== SCOPES.group;
    if (scopeSelect.value === SCOPES.group) {
      scopeHelp.textContent = '묶음 안에 들어온 표와 메모를 담습니다. 테두리에 조금 걸친 '
        + '표도 잘리지 않고 통째로 들어갑니다.';
    } else if (scopeSelect.value === SCOPES.marks) {
      scopeHelp.textContent = '골라 둔 것만 담습니다. 관계선은 양쪽 표가 함께 담길 때만 '
        + '그려집니다.';
    } else {
      scopeHelp.textContent = '';
    }
    scopeHelp.hidden = !scopeHelp.textContent;
    // 전체에서도 센다. 범위를 바꿀 때만 줄이 나타나면 아래 고르개가 위아래로
    // 밀려, 누르려던 것을 놓친다.
    const counted = scopeCount(scope) ?? {
      tables: canvas.boxes().size,
      notes: (canvas.doc.notes ?? []).length,
      groups: (canvas.doc.groups ?? []).length,
      links: (canvas.linkSpots ?? new Map()).size,
    };
    const links = scope ? scope.links.size : counted.links;
    if (counted.tables + counted.notes + counted.groups === 0) {
      scopeNote.textContent = '담을 것이 없습니다. 지금 내보내면 빈 그림이 나옵니다.';
    } else {
      const parts = [];
      if (counted.tables) parts.push(`표 ${counted.tables}개`);
      if (counted.notes) parts.push(`메모 ${counted.notes}개`);
      if (counted.groups) parts.push(`묶음 ${counted.groups}개`);
      if (links) parts.push(`관계선 ${links}개`);
      scopeNote.textContent = `이 그림에 담기는 것: ${parts.join(' · ')}`;
    }
    scopeNote.hidden = !scopeNote.textContent;

    const format = formatSelect.value;
    const vector = format === 'svg';
    scaleSelect.disabled = vector;
    // JPG는 투명을 담지 못한다. 끌 수 있게 두면 검게 굳은 그림이 나온다.
    const bgInput = bgBox.querySelector('input');
    bgInput.disabled = format === 'jpg';
    if (format === 'jpg') bgInput.checked = true;
    const scale = vector ? 1 : Number(scaleSelect.value);
    const w = Math.round(box.w * scale);
    const hh = Math.round(box.h * scale);
    sizeNote.textContent = vector
      ? `${box.w} × ${box.h} — 벡터라 배율이 필요 없습니다`
      : `${w} × ${hh} 픽셀`
        + (Math.max(w, hh) > MAX_PIXELS ? ' — 너무 큽니다. 배율을 낮추세요.' : '');
    // 투명 배경일 때는 뒤에 체크무늬를 깔아 준다. 미리보기 판의 색을 그대로 두면
    // 투명한 그림과 그 색으로 채운 그림을 구별할 수 없다.
    previewBox.classList.toggle('is-alpha', !bgInput.checked && format !== 'jpg');
    schedulePreview();
  };

  // makeSVG는 지금 고른 설정으로 그림 문자열을 만든다. 미리보기와 내보내기가
  // **같은 함수**를 쓴다 — 다르면 미리보기는 미리보기가 아니다.
  const makeSVG = () => withTheme(themeSelect.value, () => {
    const wantBg = bgBox.querySelector('input').checked;
    const background = wantBg ? canvasBackground(canvas) : '';
    return { svgText: buildSVG(canvas, box, background, scope), background };
  });

  // 미리보기는 한 박자 늦게 만든다. 고르개를 연달아 바꿀 때마다 도면 전체의
  // 계산된 스타일을 다시 읽으면 창이 뻑뻑해진다.
  let previewTimer = 0;
  const schedulePreview = () => {
    clearTimeout(previewTimer);
    previewTimer = setTimeout(() => {
      const empty = scope && scope.tables.size + scope.notes.size + scope.groups.size === 0;
      if (empty) {
        previewImg.removeAttribute('src');
        previewImg.hidden = true;
        previewNote.textContent = '담을 것이 없습니다';
        return;
      }
      try {
        const { svgText } = makeSVG();
        previewImg.src = `data:image/svg+xml;charset=utf-8,${encodeURIComponent(svgText)}`;
        previewImg.hidden = false;
        previewNote.textContent = '';
      } catch (err) {
        previewImg.hidden = true;
        previewNote.textContent = `미리보기를 만들지 못했습니다: ${err.message}`;
      }
    }, 120);
  };
  formatSelect.addEventListener('change', refresh);
  scaleSelect.addEventListener('change', refresh);
  scopeSelect.addEventListener('change', refresh);
  groupSelect.addEventListener('change', refresh);
  themeSelect.addEventListener('change', refresh);
  bgBox.addEventListener('change', refresh);
  refresh();

  openModal({
    title: '사진으로 내보내기',
    width: 620,
    onClose: () => clearTimeout(previewTimer),
    body: () => [
      previewBox,
      field('범위', scopeSelect),
      groupField,
      scopeNote,
      scopeHelp,
      markCount ? null : h('p.field-help', {},
        '표·메모·묶음을 먼저 골라 두면 "고른 것만"을 고를 수 있습니다. Shift(또는 Ctrl)를 '
        + '누르고 클릭하거나, 빈 곳을 끌어 훑으세요.'),
      field('테마', themeSelect),
      field('형식', formatSelect),
      field('배율', scaleSelect),
      h('div.field', {}, bgBox, sizeNote),
      h('p.field-help', {},
        '보고 있는 화면이 아니라 고른 범위 전체가 들어갑니다 — 화면 밖에 있는 표도 '
        + '담깁니다. 고른 표시와 다른 사람의 커서는 빠집니다.'),
      h('p.field-help', {},
        '글꼴은 그림에 담지 않습니다(4MB가 넘습니다). 이 글꼴이 없는 컴퓨터에서 열면 '
        + '비슷한 글꼴로 그려집니다.'),
    ],
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: async (e) => {
          const format = formatSelect.value;
          const scale = format === 'svg' ? 1 : Number(scaleSelect.value);
          if (Math.max(box.w, box.h) * scale > MAX_PIXELS) {
            toast('그림이 너무 큽니다. 배율을 낮추세요', 'error');
            return;
          }
          if (scope && scope.tables.size + scope.notes.size + scope.groups.size === 0) {
            toast('이 범위에 든 것이 없습니다', 'error');
            return;
          }
          // 파일 이름에 범위를 적는다. 같은 설계에서 부분만 몇 장 내보내면
          // 이름이 같아 어느 것이 무엇인지 알 수 없다.
          const tag = scopeSelect.value === SCOPES.group
            ? safeName(groupSelect.options[groupSelect.selectedIndex]?.text ?? '묶음')
            : (scopeSelect.value === SCOPES.marks ? `선택${markCount}개` : '');
          const name = [safeName(docName), tag, themeSelect.value, fileStamp()]
            .filter(Boolean).join('_');
          const btn = e.currentTarget;
          btn.disabled = true;
          try {
            const { svgText, background } = makeSVG();
            if (format === 'svg') {
              saveBlob(new Blob([svgText], { type: 'image/svg+xml;charset=utf-8' }), `${name}.svg`);
            } else {
              const mime = format === 'jpg' ? 'image/jpeg' : 'image/png';
              // 배경은 SVG에 이미 깔려 있다. 굽는 쪽에도 같은 색을 주는 이유는
              // JPG처럼 투명을 모르는 형식에서 남는 자리를 메우기 위해서다.
              const blob = await rasterize(svgText, box, { scale, mime, background });
              saveBlob(blob, `${name}.${format}`);
            }
            toast('그림을 내려받았습니다', 'success');
            close();
          } catch (err) {
            toast(`그림을 만들지 못했습니다: ${err.message}`, 'error');
            btn.disabled = false;
          }
        },
      }, icon('save', 14), ' 내보내기'),
    ],
  });
}
