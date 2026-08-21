// 마크다운 렌더러.
//
// 직접 만든 이유는 이 앱의 다른 선택들과 같다: 번들러가 없고 CSP가 외부 스크립트를
// 막으므로 라이브러리를 넣으려면 통째로 벤더링해야 한다. 우리가 필요한 문법은
// 모델이 실제로 쓰는 것 — 제목, 목록, 굵게, 인라인 코드, 코드 블록, 표, 링크 —
// 정도라서, 그만큼만 만드는 편이 수십 KB를 들여오는 것보다 낫다.
//
// **innerHTML을 쓰지 않는다.** 여기 들어오는 문자열은 LLM이 만든 것이고, 그 안에는
// 사용자가 조회한 DB 값이 섞여 있다. 즉 신뢰할 수 없는 입력이다. HTML을 문자열로
// 조립하면 그 순간 XSS 통로가 되므로, 이 파일은 처음부터 끝까지 DOM 노드만 만든다.
// 그래서 "이스케이프를 빠뜨렸다"는 실수가 나올 자리가 없다.
import { h } from './dom.js';

// renderMarkdown은 마크다운 문자열을 DocumentFragment로 만든다.
//
// opts.highlight는 코드 블록을 색칠하는 함수다: (코드, 언어) → 노드.
// 강조기를 여기서 import하지 않고 **주입받는** 이유가 둘 있다. 첫째, 이 파일은 언어
// 규칙을 몰라야 한다 — 마크다운을 그리는 일과 SQL·Lua를 아는 일은 다른 관심사다.
// 둘째, 스트리밍 중에는 강조를 끄는 편이 낫다. 토큰이 도착할 때마다 전체를 다시
// 그리는데 거기에 토큰화까지 얹으면 긴 코드에서 눈에 띄게 느려진다.
export function renderMarkdown(src, opts = {}) {
  const frag = document.createDocumentFragment();
  for (const node of parseBlocks(String(src ?? '').split(/\r?\n/), opts)) {
    frag.appendChild(node);
  }
  return frag;
}

// ---------- 블록 ----------

const RE_HEADING = /^(#{1,6})\s+(.*)$/;
const RE_FENCE = /^\s*(```|~~~)(.*)$/;
const RE_HR = /^\s*(-{3,}|\*{3,}|_{3,})\s*$/;
const RE_QUOTE = /^\s*>\s?(.*)$/;
const RE_LIST = /^(\s*)([-*+]|\d+[.)])\s+(.*)$/;
const RE_TABLE_SEP = /^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$/;

function parseBlocks(lines, opts = {}) {
  const out = [];
  let i = 0;

  while (i < lines.length) {
    const line = lines[i];

    if (line.trim() === '') {
      i++;
      continue;
    }

    // 코드 블록은 가장 먼저 본다. 그 안의 내용은 마크다운이 아니라 글자 그대로다 —
    // 여기서 순서를 놓치면 SQL 안의 `*`가 기울임으로 바뀐다.
    const fence = RE_FENCE.exec(line);
    if (fence) {
      const marker = fence[1];
      const lang = fence[2].trim();
      const body = [];
      i++;
      while (i < lines.length && !lines[i].trimStart().startsWith(marker)) {
        body.push(lines[i]);
        i++;
      }
      i++; // 닫는 울타리
      const text = body.join('\n');
      const code = h('code', {});
      if (lang) code.dataset.lang = lang;
      // 강조기가 주어지면 색을 입히고, 없으면 글자 그대로 둔다.
      // 어느 쪽이든 DOM 노드만 만든다 — 이 파일은 문자열로 HTML을 조립하지 않는다.
      if (opts.highlight) code.appendChild(opts.highlight(text, lang));
      else code.textContent = text;
      out.push(h('pre.md-code', {}, code));
      continue;
    }

    const heading = RE_HEADING.exec(line);
    if (heading) {
      // h1은 만들지 않는다. 이 조각은 화면 제목 아래에 들어가므로 문서의 제목이
      // 두 개가 되면 안 된다(스크린 리더의 문서 구조가 어긋난다).
      const level = Math.min(6, heading[1].length + 1);
      out.push(h(`h${level}.md-h`, {}, inline(heading[2])));
      i++;
      continue;
    }

    if (RE_HR.test(line)) {
      out.push(h('hr.md-hr'));
      i++;
      continue;
    }

    if (RE_QUOTE.test(line)) {
      const body = [];
      while (i < lines.length && RE_QUOTE.test(lines[i])) {
        body.push(RE_QUOTE.exec(lines[i])[1]);
        i++;
      }
      out.push(h('blockquote.md-quote', {}, ...parseBlocks(body, opts)));
      continue;
    }

    // 표: 첫 줄이 |로 나뉘고 다음 줄이 구분선이어야 한다.
    // 구분선을 요구하는 이유: 값에 |가 들어 있는 한 줄을 표로 오인하면
    // 그 줄이 통째로 사라진 것처럼 보인다.
    if (line.includes('|') && i + 1 < lines.length && RE_TABLE_SEP.test(lines[i + 1])) {
      const header = splitRow(line);
      const rows = [];
      i += 2;
      while (i < lines.length && lines[i].includes('|') && lines[i].trim() !== '') {
        rows.push(splitRow(lines[i]));
        i++;
      }
      out.push(h('div.md-table-wrap', {},
        h('table.md-table', {},
          h('thead', {}, h('tr', {}, header.map((c) => h('th', {}, inline(c))))),
          h('tbody', {}, rows.map((r) => h('tr', {}, r.map((c) => h('td', {}, inline(c)))))))));
      continue;
    }

    const list = RE_LIST.exec(line);
    if (list) {
      const [node, next] = parseList(lines, i, list[1].length);
      out.push(node);
      i = next;
      continue;
    }

    // 문단. 빈 줄이나 다른 블록이 나올 때까지 모은다.
    const para = [];
    while (i < lines.length && lines[i].trim() !== ''
      && !RE_HEADING.test(lines[i]) && !RE_FENCE.test(lines[i])
      && !RE_HR.test(lines[i]) && !RE_QUOTE.test(lines[i]) && !RE_LIST.test(lines[i])) {
      para.push(lines[i]);
      i++;
    }
    if (para.length) out.push(h('p.md-p', {}, softBreaks(para)));
  }

  return out;
}

// parseList는 같은 들여쓰기 깊이의 항목을 모아 목록 하나를 만든다.
//
// 중첩을 들여쓰기로 판단한다. 모델은 하위 항목을 2~4칸으로 들여쓰는데, 그것을
// 무시하면 계층이 사라져 "무엇의 하위 항목인가"를 읽을 수 없게 된다.
function parseList(lines, start, indent) {
  const first = RE_LIST.exec(lines[start]);
  const ordered = /\d/.test(first[2]);
  const items = [];
  let i = start;

  while (i < lines.length) {
    const m = RE_LIST.exec(lines[i]);
    if (!m) break;
    const depth = m[1].length;
    if (depth < indent) break;
    if (depth > indent) {
      // 더 깊은 항목은 직전 항목의 하위 목록이다.
      const [child, next] = parseList(lines, i, depth);
      if (items.length) items[items.length - 1].appendChild(child);
      else items.push(h('li.md-li', {}, child));
      i = next;
      continue;
    }
    if (/\d/.test(m[2]) !== ordered) break;

    const body = [m[3]];
    i++;
    // 항목 안에서 줄이 이어지는 경우(목록 표시 없이 들여쓴 줄)를 같은 항목으로 본다.
    while (i < lines.length && lines[i].trim() !== ''
      && !RE_LIST.test(lines[i]) && lines[i].startsWith(' '.repeat(indent + 1))) {
      body.push(lines[i].trim());
      i++;
    }
    items.push(h('li.md-li', {}, softBreaks(body)));
  }

  const tag = ordered ? 'ol.md-list' : 'ul.md-list';
  const node = h(tag, {}, items);
  if (ordered) {
    // 번호를 이어 받는다. "3."으로 시작하는 목록이 1부터 보이면 본문의 참조와 어긋난다.
    const startNum = parseInt(first[2], 10);
    if (Number.isFinite(startNum) && startNum !== 1) node.setAttribute('start', String(startNum));
  }
  return [node, i];
}

function splitRow(line) {
  return line.replace(/^\s*\|/, '').replace(/\|\s*$/, '').split('|').map((c) => c.trim());
}

// softBreaks는 문단 안의 줄바꿈을 <br>로 잇는다.
//
// 규약대로라면 한 줄 개행은 공백이지만, 대화에서는 사용자가 누른 줄바꿈이 그대로
// 보이는 편이 자연스럽다(모델도 그렇게 쓴다). GFM의 breaks 모드와 같은 선택이다.
function softBreaks(lines) {
  const out = [];
  lines.forEach((line, idx) => {
    if (idx > 0) out.push(h('br'));
    out.push(...inline(line));
  });
  return out;
}

// ---------- 인라인 ----------

// 인라인 문법. 코드가 가장 앞에 있는 것이 중요하다 —
// 백틱 안의 `*`나 `_`는 강조가 아니라 글자다.
//
// 정규식을 상수로 두지 않고 호출마다 새로 만드는 이유: inline()은 강조 안쪽을
// 다시 훑느라 자기 자신을 부르는데, /g 정규식은 lastIndex를 들고 있어서
// 하나를 공유하면 재귀가 바깥쪽의 진행 위치를 망가뜨린다.
const INLINE_SRC = [
  '(`+)([\\s\\S]*?)\\1',                             // `code`
  '\\*\\*([\\s\\S]+?)\\*\\*',                        // **bold**
  '__([\\s\\S]+?)__',                                // __bold__
  '~~([\\s\\S]+?)~~',                                // ~~del~~
  '\\*(?!\\s)([^*\\n]+?)\\*',                        // *em*
  '_(?!\\s)([^_\\n]+?)_',                            // _em_
  '\\[([^\\]]*)\\]\\(([^)\\s]+)(?:\\s+"[^"]*")?\\)', // [text](url)
  '(https?://[^\\s<>()]+)',                          // 맨 URL
].join('|');

// WORD는 밑줄 강조를 가려내는 "낱말 글자"다.
//
// 뒤돌아보기(lookbehind)를 쓰지 않고 손으로 확인하는 이유는 두 가지다. 하나는
// 오래된 사파리가 그 문법에서 SyntaxError를 내고, 그러면 모듈을 읽는 순간
// 화면 전체가 죽는다. 다른 하나는 이 검사가 이 앱에 특히 중요하기 때문이다 —
// 컬럼 이름은 대개 snake_case이고, `app_order_item`의 가운데를 기울임으로
// 바꿔 버리면 이름이 달라 보인다.
//
// 이 검사를 밑줄에만 적용하는 것이 요점이다. 별표에까지 적용하면 한국어가 깨진다:
// `*중요*합니다` 처럼 조사가 바로 붙는 것이 정상적인 문장이기 때문이다.
const WORD = /[\p{L}\p{N}_]/u;

function inline(text) {
  const out = [];
  const re = new RegExp(INLINE_SRC, 'g');
  let last = 0;

  for (let m = re.exec(text); m; m = re.exec(text)) {
    const [raw, , code, bold1, bold2, del, em1, em2, linkText, linkURL, bareURL] = m;

    // 낱말 안에 있는 밑줄은 강조가 아니라 이름의 일부다.
    if (em2 !== undefined && insideWord(text, m.index, raw.length)) {
      continue;
    }

    if (m.index > last) out.push(document.createTextNode(text.slice(last, m.index)));

    if (code !== undefined) {
      out.push(h('code.md-inline-code', {}, code.trim()));
    } else if (bold1 !== undefined || bold2 !== undefined) {
      out.push(h('strong', {}, inline(bold1 ?? bold2)));
    } else if (del !== undefined) {
      out.push(h('del', {}, inline(del)));
    } else if (em1 !== undefined || em2 !== undefined) {
      out.push(h('em', {}, inline(em1 ?? em2)));
    } else if (linkURL !== undefined) {
      out.push(link(linkURL, inline(linkText || linkURL)));
    } else if (bareURL !== undefined) {
      out.push(link(bareURL, [document.createTextNode(bareURL)]));
    } else {
      out.push(document.createTextNode(raw));
    }
    last = m.index + raw.length;
  }

  if (last < text.length) out.push(document.createTextNode(text.slice(last)));
  return out;
}

function insideWord(text, start, length) {
  const before = start > 0 ? text[start - 1] : '';
  const after = text[start + length] ?? '';
  return WORD.test(before) || WORD.test(after);
}

// link는 안전한 스킴일 때만 링크를 만든다.
//
// javascript: 를 그대로 href에 넣으면 모델이 만든 문자열이 코드가 된다.
// 허용 목록으로 막고, 그 밖의 주소는 링크가 아니라 글자로 남긴다 —
// 조용히 지우면 사용자는 원문에 무엇이 있었는지 알 수 없다.
function link(href, children) {
  const safe = /^(https?:|mailto:|\/)/i.test(href.trim());
  if (!safe) return h('span.md-unsafe-link', {}, ...children);
  return h('a.md-link', {
    href: href.trim(), target: '_blank', rel: 'noopener noreferrer',
  }, ...children);
}
