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
  'erd-group-grip', 'erd-note-grip', 'erd-link-hit', 'erd-card-outline',
  // 다른 참여자가 고르고 있다는 표시와 커서, 빈 화면 안내문.
  'erd-card-holder', 'erd-cursor', 'erd-hint',
];

// diagramBounds는 그림 전체(테이블·메모·묶음)를 감싸는 사각형이다.
//
// 지금 보고 있는 화면이 아니라 전체인 이유: 내보내기는 "이 설계를 그림으로 남긴다"는
// 뜻이지 "지금 화면을 찍는다"가 아니다. 화면 밖의 테이블이 잘려 나가면 그 사실을
// 파일을 열어 보고서야 알게 된다.
export function diagramBounds(canvas, pad = 40) {
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
  for (const b of canvas.boxes().values()) put(b.x, b.y, b.w, b.h);
  for (const n of canvas.doc.notes ?? []) put(n.x, n.y, n.w || 200, n.h || 80);
  for (const g of canvas.doc.groups ?? []) put(g.x, g.y, g.w || 320, g.h || 240);
  if (minX === Infinity) return { x: 0, y: 0, w: 400, h: 300 };
  return {
    x: Math.round(minX - pad),
    y: Math.round(minY - pad),
    w: Math.round(maxX - minX + pad * 2),
    h: Math.round(maxY - minY + pad * 2),
  };
}

// buildSVG는 혼자서도 같은 그림이 되는 SVG 문자열을 만든다.
function buildSVG(canvas, box, background) {
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
  const marked = [];
  for (const el of source.querySelectorAll('.is-selected, .is-primary')) {
    const had = ['is-selected', 'is-primary'].filter((c) => el.classList.contains(c));
    marked.push({ el, had });
    el.classList.remove(...had);
  }
  try {
    return paintAndSerialize(source, box, background);
  } finally {
    for (const { el, had } of marked) el.classList.add(...had);
  }
}

// paintAndSerialize는 계산된 값을 박아 넣고 문자열로 만든다.
function paintAndSerialize(source, box, background) {
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
  const box = diagramBounds(canvas);
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

  const refresh = () => {
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
  };
  formatSelect.addEventListener('change', refresh);
  scaleSelect.addEventListener('change', refresh);
  refresh();

  openModal({
    title: '사진으로 내보내기',
    width: 520,
    body: () => [
      field('형식', formatSelect),
      field('배율', scaleSelect),
      h('div.field', {}, bgBox, sizeNote),
      h('p.field-help', {},
        '지금 보고 있는 화면이 아니라 그림 전체를 담습니다. 고른 표시와 다른 사람의 '
        + '커서는 빠집니다.'),
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
          const wantBg = bgBox.querySelector('input').checked;
          const background = wantBg ? canvasBackground(canvas) : '';
          const name = `${safeName(docName)}_${fileStamp()}`;
          const btn = e.currentTarget;
          btn.disabled = true;
          try {
            const svgText = buildSVG(canvas, box, background);
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
