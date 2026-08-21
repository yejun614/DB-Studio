// 구문 강조.
//
// 라이브러리를 쓰지 않는다. 이 앱은 번들러가 없고 CSP가 `script-src 'self'`이므로
// CDN은 애초에 불가능하며, highlight.js 계열을 통째로 넣으면 바이너리에 수백 KB가
// 붙는다. 여기서 필요한 것은 네 가지 언어(SQL·Lua·JSON·셸)의 토큰을 색으로 나누는
// 것뿐이고, 그것은 스캐너 한 개와 규칙 표로 끝난다.
//
// **DOM 노드로 만든다. innerHTML을 쓰지 않는다.** 강조 대상은 사용자가 쓴 SQL과
// 스크립트, 즉 신뢰할 수 없는 문자열이다. 문자열을 이어 붙여 innerHTML에 넣는 순간
// 이스케이프를 한 번만 빠뜨려도 저장형 XSS가 된다. createTextNode로 만들면
// 그런 실수를 할 자리가 없다.
import { h, mount } from './dom.js';

// 이 길이를 넘으면 강조를 포기하고 평문으로 그린다.
//
// 토큰 하나가 span 하나이므로 100KB짜리 덤프를 강조하면 수만 개의 노드가 생겨
// 브라우저가 멈춘다. 색이 없는 것보다 화면이 멈추는 것이 훨씬 나쁘다.
const MAX_HIGHLIGHT = 24000;

// ---------- 규칙 ----------
//
// 각 규칙은 [토큰 종류, sticky 정규식]이다. 위에서부터 시도해 처음 맞는 것을 쓰므로
// **순서가 곧 우선순위**다. 주석과 문자열이 맨 위에 있는 이유: 그 안에서는 다른
// 규칙이 적용되면 안 된다(주석 안의 SELECT는 키워드가 아니다).

const SQL_KEYWORDS = [
  'SELECT', 'FROM', 'WHERE', 'INSERT', 'INTO', 'VALUES', 'UPDATE', 'SET', 'DELETE',
  'CREATE', 'ALTER', 'DROP', 'TRUNCATE', 'TABLE', 'VIEW', 'INDEX', 'SEQUENCE', 'SCHEMA',
  'DATABASE', 'TRIGGER', 'FUNCTION', 'PROCEDURE', 'CONSTRAINT', 'PRIMARY', 'FOREIGN',
  'KEY', 'REFERENCES', 'UNIQUE', 'CHECK', 'DEFAULT', 'NOT', 'NULL', 'AND', 'OR', 'IN',
  'EXISTS', 'BETWEEN', 'LIKE', 'ILIKE', 'IS', 'AS', 'ON', 'JOIN', 'INNER', 'LEFT',
  'RIGHT', 'FULL', 'OUTER', 'CROSS', 'LATERAL', 'GROUP', 'BY', 'ORDER', 'HAVING',
  'LIMIT', 'OFFSET', 'FETCH', 'NEXT', 'ROWS', 'ONLY', 'UNION', 'ALL', 'EXCEPT',
  'INTERSECT', 'DISTINCT', 'CASE', 'WHEN', 'THEN', 'ELSE', 'END', 'WITH', 'RECURSIVE',
  'RETURNING', 'USING', 'CASCADE', 'RESTRICT', 'ADD', 'COLUMN', 'RENAME', 'TO', 'IF',
  'BEGIN', 'COMMIT', 'ROLLBACK', 'TRANSACTION', 'SAVEPOINT', 'GRANT', 'REVOKE',
  'EXPLAIN', 'ANALYZE', 'VACUUM', 'PRAGMA', 'SHOW', 'DESCRIBE', 'DESC', 'ASC',
  'AUTOINCREMENT', 'AUTO_INCREMENT', 'IDENTITY', 'GENERATED', 'ALWAYS', 'STORED',
  'COMMENT', 'ENGINE', 'CHARSET', 'COLLATE', 'TEMPORARY', 'REPLACE', 'CONFLICT',
  'DO', 'NOTHING', 'MERGE', 'MATCHED', 'OVER', 'PARTITION', 'WINDOW', 'FILTER',
];

// 파괴적인 낱말은 따로 칠한다. 실행 버튼을 누르기 전에 눈에 들어와야 하는 것들이며,
// 이 앱은 이미 마이그레이션 계획에서 같은 구분을 쓰고 있다.
const SQL_DANGER = ['DROP', 'DELETE', 'TRUNCATE', 'ALTER', 'REVOKE'];

const LUA_KEYWORDS = [
  'and', 'break', 'do', 'else', 'elseif', 'end', 'false', 'for', 'function', 'goto',
  'if', 'in', 'local', 'nil', 'not', 'or', 'repeat', 'return', 'then', 'true',
  'until', 'while',
];

// 매크로 스크립트에서 쓸 수 있는 전역. 표준 라이브러리 일부와 이 앱의 호스트 API다.
// 색이 다르면 "이건 내가 만든 이름이 아니라 주어진 것"이 바로 보인다.
const LUA_BUILTINS = [
  'vars', 'params', 'log', 'db', 'sh', 'macro', 'json', 'fail',
  'print', 'pairs', 'ipairs', 'type', 'tostring', 'tonumber', 'select', 'error',
  'assert', 'pcall', 'next', 'rawget', 'rawset', 'setmetatable', 'getmetatable',
  'table', 'string', 'math', 'os', 'io',
];

const SHELL_KEYWORDS = [
  'if', 'then', 'else', 'elif', 'fi', 'for', 'while', 'until', 'do', 'done',
  'case', 'esac', 'function', 'in', 'return', 'exit', 'break', 'continue',
  'local', 'export', 'set', 'unset', 'param', 'foreach', 'try', 'catch', 'finally',
];

function words(list, flags = '') {
  // 낱말 경계로 감싸 부분 일치를 막는다. "NOTE"가 "NOT"으로 칠해지면 안 된다.
  return new RegExp(`\\b(?:${list.join('|')})\\b`, `y${flags}`);
}

const RULES = {
  sql: [
    ['comment', /--[^\n]*|#[^\n]*|\/\*[\s\S]*?(?:\*\/|$)/y],
    // 달러 인용(PostgreSQL 함수 본문)은 통째로 문자열로 본다.
    ['string', /\$([A-Za-z_]\w*)?\$[\s\S]*?(?:\$\1?\$|$)/y],
    ['string', /'(?:''|\\.|[^'])*'?/y],
    // 인용된 식별자는 문자열과 다른 색이다. `"user"`와 `'user'`는 전혀 다른 것이고,
    // 그 차이를 못 보면 따옴표를 잘못 써서 나는 오류를 눈으로 찾을 수 없다.
    ['ident', /"(?:""|[^"])*"?|`(?:``|[^`])*`?|\[[^\]\n]*\]?/y],
    ['number', /\b\d+(?:\.\d+)?(?:[eE][+-]?\d+)?\b|\b0[xX][0-9a-fA-F]+\b/y],
    ['danger', words(SQL_DANGER, 'i')],
    ['keyword', words(SQL_KEYWORDS, 'i')],
    ['param', /[?]|[$:@][A-Za-z0-9_]+/y],
    ['func', /\b[A-Za-z_][\w$]*(?=\s*\()/y],
    ['operator', /[=<>!+\-*/%|&^~]+/y],
    ['punct', /[(),;.[\]]/y],
  ],
  lua: [
    ['comment', /--\[(=*)\[[\s\S]*?(?:\]\1\]|$)|--[^\n]*/y],
    ['string', /\[(=*)\[[\s\S]*?(?:\]\1\]|$)/y],
    ['string', /"(?:\\.|[^"\\])*"?|'(?:\\.|[^'\\])*'?/y],
    ['number', /\b0[xX][0-9a-fA-F]+\b|\b\d+(?:\.\d+)?(?:[eE][+-]?\d+)?\b/y],
    ['keyword', words(LUA_KEYWORDS)],
    ['builtin', words(LUA_BUILTINS)],
    ['func', /\b[A-Za-z_]\w*(?=\s*[({"'])/y],
    ['operator', /\.\.\.|\.\.|[=<>~]=|[+\-*/%^#<>=]/y],
    ['punct', /[(),;.[\]{}:]/y],
  ],
  json: [
    // 키와 값을 나눠 칠한다. 중첩이 깊어지면 그 구분이 구조를 읽는 유일한 실마리다.
    ['key', /"(?:\\.|[^"\\])*"(?=\s*:)/y],
    ['string', /"(?:\\.|[^"\\])*"?/y],
    ['number', /-?\b\d+(?:\.\d+)?(?:[eE][+-]?\d+)?\b/y],
    ['keyword', /\b(?:true|false|null)\b/y],
    ['punct', /[{}[\],:]/y],
  ],
  shell: [
    ['comment', /#[^\n]*/y],
    ['string', /"(?:\\.|[^"\\])*"?|'[^']*'?/y],
    // 변수 참조. 셸 스크립트를 읽을 때 가장 먼저 찾는 것이 이것이다.
    ['builtin', /\$\{[^}\n]*\}?|\$[A-Za-z_]\w*|\$[?$#@!*0-9]/y],
    ['number', /\b\d+\b/y],
    ['keyword', words(SHELL_KEYWORDS)],
    ['operator', /\|\||&&|[|&<>]+|[=!]=?/y],
    ['punct', /[(){};]/y],
  ],
};

// 언어 별칭. 노드 정의의 language 값과 화면에서 부르는 이름이 갈리지 않게 한다.
const ALIASES = {
  sql: 'sql', mysql: 'sql', postgres: 'sql', postgresql: 'sql', sqlite: 'sql',
  lua: 'lua',
  json: 'json',
  shell: 'shell', bash: 'shell', sh: 'shell', powershell: 'shell', pwsh: 'shell',
};

export function normalizeLanguage(lang) {
  return ALIASES[String(lang ?? '').toLowerCase()] ?? null;
}

// tokenize는 코드를 [종류, 문자열] 목록으로 나눈다.
//
// 규칙에 걸리지 않는 문자는 한 글자씩 평문으로 흘린다. 이렇게 하면 어떤 입력에도
// 무한 루프나 누락이 없다 — 강조기가 사용자의 편집을 막는 일은 없어야 한다.
export function tokenize(code, lang) {
  const rules = RULES[normalizeLanguage(lang)];
  if (!rules) return [['text', code]];

  const out = [];
  let plain = '';
  let i = 0;

  const flushPlain = () => {
    if (plain) {
      out.push(['text', plain]);
      plain = '';
    }
  };

  while (i < code.length) {
    // 공백은 규칙을 돌리지 않고 바로 흘린다. 대부분의 문자가 공백이므로
    // 여기서 걸러야 정규식 시도 횟수가 눈에 띄게 줄어든다.
    const ch = code[i];
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r') {
      plain += ch;
      i++;
      continue;
    }

    let matched = false;
    for (const [type, re] of rules) {
      re.lastIndex = i;
      const m = re.exec(code);
      if (m && m[0].length > 0) {
        flushPlain();
        out.push([type, m[0]]);
        i += m[0].length;
        matched = true;
        break;
      }
    }
    if (!matched) {
      plain += ch;
      i++;
    }
  }
  flushPlain();
  return out;
}

// highlightFragment는 강조된 DOM 조각을 만든다.
export function highlightFragment(code, lang) {
  const frag = document.createDocumentFragment();
  const text = String(code ?? '');

  if (!normalizeLanguage(lang) || text.length > MAX_HIGHLIGHT) {
    frag.appendChild(document.createTextNode(text));
    return frag;
  }

  for (const [type, value] of tokenize(text, lang)) {
    if (type === 'text') {
      frag.appendChild(document.createTextNode(value));
      continue;
    }
    const span = document.createElement('span');
    span.className = `tok tok-${type}`;
    span.textContent = value;
    frag.appendChild(span);
  }
  return frag;
}

// codeBlock은 읽기 전용 코드 표시를 만든다.
export function codeBlock(code, lang, { className = '' } = {}) {
  const pre = h(`pre.code-block${className ? `.${className}` : ''}`, {
    dataset: { lang: normalizeLanguage(lang) ?? 'text' },
  });
  pre.appendChild(highlightFragment(code, lang));
  return pre;
}

// codeEditor는 강조가 보이는 편집기를 만든다.
//
// 구현은 **투명한 textarea를 강조된 pre 위에 겹치는** 고전적인 방법이다.
// contenteditable을 쓰면 커서 위치·되돌리기·IME·붙여넣기를 전부 직접 다뤄야 하고,
// 그중 하나만 틀려도 한글 입력이 깨진다. textarea는 그 모든 것을 브라우저가 이미
// 올바르게 처리하며, 우리는 색만 뒤에 깐다.
//
// 두 요소의 글꼴·크기·행간·여백·줄바꿈 규칙이 하나라도 다르면 글자가 어긋난다.
// 그래서 CSS에서 두 선택자에 같은 값을 한 번에 준다(app.css의 `.code-editor` 절).
//
// lineNumbers를 켜면 왼쪽에 줄 번호 거터가 붙고 **자동 줄바꿈이 꺼진다**.
// 줄바꿈을 남겨두면 논리적인 한 줄이 화면에서는 여러 줄을 차지해 번호가 어긋나고,
// 그렇게 어긋난 번호는 없느니만 못하다(오류 메시지의 "3행"을 찾을 수 없게 된다).
// 대신 긴 줄은 가로 스크롤로 본다 — 코드 편집기에서 익숙한 방식이다.
export function codeEditor({
  value = '',
  language = 'sql',
  rows = 10,
  placeholder = '',
  lineNumbers = false,
  onInput = null,
  onSubmit = null,
} = {}) {
  const view = h('pre.code-hl', { 'aria-hidden': 'true' });
  const area = h('textarea.code-ta', {
    rows, placeholder, spellcheck: false, autocapitalize: 'off', autocomplete: 'off',
  });
  area.value = value;

  // 거터는 스크롤을 스스로 하지 않는다. 안쪽 목록을 textarea의 스크롤만큼 밀어
  // 맞춘다 — 두 개의 스크롤 컨테이너를 동기화하는 것보다 어긋날 여지가 적다.
  const gutterInner = lineNumbers ? h('div.code-gutter-inner') : null;
  const gutter = lineNumbers
    ? h('div.code-gutter', { 'aria-hidden': 'true' }, gutterInner)
    : null;

  const wrap = h(`div.code-editor${lineNumbers ? '.has-gutter' : ''}`, {
    dataset: { lang: normalizeLanguage(language) ?? 'text' },
  }, gutter, view, area);

  // paintGutter는 줄 수가 바뀔 때만 DOM을 다시 만든다. 타자 한 번마다 수백 개의
  // 노드를 갈아치우면 긴 스크립트에서 입력이 눈에 띄게 밀린다.
  let gutterLines = -1;
  const paintGutter = () => {
    if (!gutterInner) return;
    const count = area.value.split('\n').length;
    if (count !== gutterLines) {
      gutterLines = count;
      const frag = document.createDocumentFragment();
      for (let i = 1; i <= count; i += 1) {
        const el = document.createElement('span');
        el.textContent = String(i);
        frag.appendChild(el);
      }
      mount(gutterInner, frag);
      // 자릿수만큼 폭을 넓힌다. 고정 폭으로 두면 1000줄부터 번호가 잘린다.
      wrap.style.setProperty('--gutter-w', `${String(count).length + 2}ch`);
    }
    gutterInner.style.transform = `translateY(${-area.scrollTop}px)`;
  };

  const syncScroll = () => {
    view.scrollTop = area.scrollTop;
    view.scrollLeft = area.scrollLeft;
    paintGutter();
  };

  const paint = () => {
    // 끝에 줄바꿈을 하나 더 넣는다. 그러지 않으면 마지막 줄에서 엔터를 쳤을 때
    // pre의 높이가 늘지 않아 커서가 보이는 영역 밖으로 나간다.
    mount(view, highlightFragment(`${area.value}\n`, language));
    syncScroll();
  };

  // Escape를 누른 직후의 Tab은 들여쓰기가 아니라 포커스 이동이다(아래 keydown 참조).
  let escaped = false;

  area.addEventListener('input', () => {
    paint();
    onInput?.(area.value);
  });
  area.addEventListener('scroll', syncScroll);
  area.addEventListener('keydown', (e) => {
    // 이벤트를 함께 넘긴다. 호출부가 Shift 같은 보조키로 실행 범위를 나눈다
    // (SQL 콘솔의 전체 실행 / 한 문장 실행).
    if (onSubmit && (e.ctrlKey || e.metaKey) && e.key === 'Enter') {
      e.preventDefault();
      onSubmit(area.value, e);
      return;
    }
    // Tab은 들여쓰기다. 코드 편집기에서 Tab이 포커스를 옮기면 들여쓰기를 할 방법이 없다.
    // 대신 Escape → Tab으로 빠져나갈 수 있게 두어 키보드만 쓰는 사람을 가두지 않는다.
    if (e.key === 'Tab' && !e.shiftKey && !escaped) {
      e.preventDefault();
      const { selectionStart: s, selectionEnd: t } = area;
      area.value = `${area.value.slice(0, s)}  ${area.value.slice(t)}`;
      area.selectionStart = area.selectionEnd = s + 2;
      paint();
      onInput?.(area.value);
    }
    escaped = e.key === 'Escape';
  });

  const api = {
    el: wrap,
    textarea: area,
    get value() { return area.value; },
    set value(v) { area.value = v; paint(); },
    setLanguage(lang) {
      language = lang;
      wrap.dataset.lang = normalizeLanguage(lang) ?? 'text';
      paint();
    },
    focus() { area.focus(); },
  };
  paint();
  return api;
}
