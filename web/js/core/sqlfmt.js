// SQL 정리(포매팅)와 즉석 구문 점검.
//
// 서버에 보내지 않고 브라우저에서 한다. 정리는 편집기의 글자를 바꾸는 일이므로
// 왕복 지연이 있으면 "버튼을 눌렀는데 아무 일도 안 일어난다"로 느껴지고,
// 여기서 잡는 오류(닫히지 않은 따옴표, 짝이 안 맞는 괄호)는 DB에 물어봐야 알 수 있는
// 것이 아니다. DB만 아는 것(테이블 이름 오타 등)은 서버의 구문 검사가 맡는다.
//
// **원문 보존이 첫 번째 규칙이다.** 문자열·주석 안은 한 글자도 건드리지 않는다.
// 정리 버튼이 리터럴을 바꿔 버리면 그것은 도구가 아니라 사고다.

// 절을 시작하는 낱말. 이 앞에서 줄을 바꾼다.
const CLAUSE = new Set([
  'SELECT', 'FROM', 'WHERE', 'HAVING', 'VALUES', 'SET', 'RETURNING',
  'UNION', 'EXCEPT', 'INTERSECT', 'LIMIT', 'OFFSET', 'FETCH',
  'INSERT', 'UPDATE', 'DELETE', 'CREATE', 'ALTER', 'DROP', 'TRUNCATE',
  'WITH', 'GRANT', 'REVOKE', 'EXPLAIN', 'MERGE', 'REPLACE', 'PRAGMA', 'USE',
]);

// JOIN 계열. LEFT/RIGHT/INNER 등은 뒤에 JOIN이 올 때만 절의 시작이다
// ("LEFT(name, 3)" 같은 함수와 구분해야 한다).
const JOIN_LEAD = new Set(['LEFT', 'RIGHT', 'INNER', 'OUTER', 'FULL', 'CROSS', 'NATURAL', 'STRAIGHT_JOIN']);

// GROUP/ORDER는 뒤에 BY가 붙어야 절이다.
const BY_LEAD = new Set(['GROUP', 'ORDER', 'PARTITION']);

// 조건을 잇는 낱말. WHERE 절 안에서 줄을 바꿔 한 줄에 조건 하나가 오게 한다.
const BOOLEAN = new Set(['AND', 'OR']);

// 대문자로 바꿀 예약어 전체.
const KEYWORDS = new Set([
  ...CLAUSE, ...JOIN_LEAD, ...BY_LEAD, ...BOOLEAN,
  'JOIN', 'ON', 'AS', 'BY', 'INTO', 'TABLE', 'VIEW', 'INDEX', 'SEQUENCE', 'SCHEMA',
  'DATABASE', 'TRIGGER', 'FUNCTION', 'PROCEDURE', 'CONSTRAINT', 'PRIMARY', 'FOREIGN',
  'KEY', 'REFERENCES', 'UNIQUE', 'CHECK', 'DEFAULT', 'NOT', 'NULL', 'IN', 'IS',
  'EXISTS', 'BETWEEN', 'LIKE', 'ILIKE', 'DISTINCT', 'ALL', 'ANY', 'SOME',
  'CASE', 'WHEN', 'THEN', 'ELSE', 'END', 'ASC', 'DESC', 'NULLS', 'FIRST', 'LAST',
  'CASCADE', 'RESTRICT', 'ADD', 'COLUMN', 'RENAME', 'TO', 'IF', 'RECURSIVE',
  'BEGIN', 'COMMIT', 'ROLLBACK', 'TRANSACTION', 'SAVEPOINT', 'USING', 'ONLY',
  'ROWS', 'ROW', 'NEXT', 'TEMPORARY', 'GENERATED', 'ALWAYS', 'IDENTITY', 'STORED',
  'AUTO_INCREMENT', 'AUTOINCREMENT', 'COMMENT', 'COLLATE', 'OVER', 'WINDOW',
  'FILTER', 'LATERAL', 'CONFLICT', 'DO', 'NOTHING', 'MATCHED', 'ANALYZE', 'VACUUM',
]);

// ---------- 토큰 나누기 ----------
//
// highlight.js의 tokenize와 나누지 않은 이유: 저것은 "색을 칠할 조각"을 만들고,
// 여기서는 "붙여도 되는지 판단할 문법 단위"가 필요하다. 예를 들어 강조기는 공백을
// 평문에 섞어 흘려보내는데, 포매터는 공백을 전부 버리고 다시 넣어야 한다.

// 토큰 종류: word | string | comment | number | punct | operator | unterminated
export function tokenizeSQL(text, kind = 'sql') {
  const out = [];
  const n = text.length;
  let i = 0;

  const push = (type, start, end, extra) => {
    out.push({ type, value: text.slice(start, end), start, ...extra });
  };

  while (i < n) {
    const c = text[i];

    if (c === ' ' || c === '\t' || c === '\r') { i += 1; continue; }
    if (c === '\n') {
      // 빈 줄은 사람이 넣은 문단 구분이다. 하나까지는 살린다.
      out.push({ type: 'newline', value: '\n', start: i });
      i += 1;
      continue;
    }

    // 줄 주석
    if ((c === '-' && text[i + 1] === '-') || (c === '#' && kind === 'mysql')) {
      const start = i;
      while (i < n && text[i] !== '\n') i += 1;
      push('comment', start, i, { line: true });
      continue;
    }

    // 블록 주석
    if (c === '/' && text[i + 1] === '*') {
      const start = i;
      i += 2;
      while (i < n && !(text[i] === '*' && text[i + 1] === '/')) i += 1;
      const closed = i < n;
      i = Math.min(n, i + 2);
      push('comment', start, i, { line: false, unterminated: !closed });
      continue;
    }

    // 달러 인용 (PostgreSQL 함수 본문)
    if (c === '$' && kind === 'postgres') {
      const m = /^\$([A-Za-z_]\w*)?\$/.exec(text.slice(i));
      if (m) {
        const start = i;
        const close = text.indexOf(m[0], i + m[0].length);
        const closed = close >= 0;
        i = closed ? close + m[0].length : n;
        push('string', start, i, { unterminated: !closed });
        continue;
      }
    }

    // 문자열과 인용 식별자
    if (c === "'" || c === '"' || c === '`' || (c === '[' && kind === 'mssql')) {
      const closer = c === '[' ? ']' : c;
      const start = i;
      i += 1;
      let closed = false;
      while (i < n) {
        if (text[i] === '\\' && kind === 'mysql' && c === "'") { i += 2; continue; }
        if (text[i] === closer) {
          // 같은 부호가 두 번이면 이스케이프다 ('' → 따옴표 한 개).
          if (closer !== ']' && text[i + 1] === closer) { i += 2; continue; }
          i += 1;
          closed = true;
          break;
        }
        i += 1;
      }
      push(c === "'" ? 'string' : 'ident', start, i, { unterminated: !closed, quote: c });
      continue;
    }

    // 숫자
    if (/[0-9]/.test(c)) {
      const m = /^\d+(?:\.\d+)?(?:[eE][+-]?\d+)?|^0[xX][0-9a-fA-F]+/.exec(text.slice(i));
      const start = i;
      i += m[0].length;
      push('number', start, i);
      continue;
    }

    // 낱말 (식별자·예약어). 바인드 변수(:name, @name, $1)도 한 덩어리로 본다.
    if (/[A-Za-z_¡-￿:@$?]/.test(c)) {
      const start = i;
      if (c === ':' || c === '@' || c === '$' || c === '?') i += 1;
      while (i < n && /[A-Za-z0-9_$¡-￿]/.test(text[i])) i += 1;
      if (i === start) { i += 1; push('operator', start, i); continue; }
      push('word', start, i, { upper: text.slice(start, i).toUpperCase() });
      continue;
    }

    if ('(),;.'.includes(c)) {
      push('punct', i, i + 1);
      i += 1;
      continue;
    }

    // 나머지는 연산자로 묶는다.
    const m = /^[=<>!+\-*/%|&^~]+/.exec(text.slice(i));
    const start = i;
    i += m ? m[0].length : 1;
    push('operator', start, i);
  }

  return out;
}

// ---------- 즉석 점검 ----------

// quickCheck는 DB에 물어보지 않고도 확실히 알 수 있는 문제만 찾는다.
// 여기서 통과했다고 문법이 맞는 것은 아니다 — 그것은 서버의 구문 검사가 판단한다.
export function quickCheck(text, kind = 'sql') {
  const issues = [];
  const tokens = tokenizeSQL(text, kind);
  const lineOf = (pos) => text.slice(0, pos).split('\n').length;

  for (const t of tokens) {
    if (!t.unterminated) continue;
    if (t.type === 'comment') {
      issues.push({ line: lineOf(t.start), message: '블록 주석이 닫히지 않았습니다 (*/ 누락)' });
    } else {
      const q = t.quote === '[' ? ']' : (t.quote ?? "'");
      issues.push({ line: lineOf(t.start), message: `${q} 로 닫히지 않은 ${t.type === 'string' ? '문자열' : '식별자'}입니다` });
    }
  }

  // 괄호 짝은 문장 단위로 본다. 앞 문장에서 남은 괄호가 뒤 문장까지 번지면
  // 정작 잘못된 자리를 가리키지 못한다.
  const stack = [];
  for (const t of tokens) {
    if (t.type !== 'punct') continue;
    if (t.value === '(') stack.push(t);
    else if (t.value === ')') {
      if (stack.length === 0) {
        issues.push({ line: lineOf(t.start), message: '여는 괄호 없이 ) 가 나왔습니다' });
      } else {
        stack.pop();
      }
    } else if (t.value === ';') {
      for (const open of stack) {
        issues.push({ line: lineOf(open.start), message: '괄호가 닫히지 않은 채 문장이 끝났습니다' });
      }
      stack.length = 0;
    }
  }
  for (const open of stack) {
    issues.push({ line: lineOf(open.start), message: '괄호가 닫히지 않았습니다' });
  }

  issues.sort((a, b) => a.line - b.line);
  return issues;
}

// ---------- 정리 ----------

// formatSQL은 문장을 읽기 좋은 모양으로 다시 배치한다.
//
// 완전한 파서가 아니다. 절의 시작·쉼표·괄호라는 세 가지 신호만 보고 줄을 나눈다.
// 그래서 어떤 입력에도 멈추지 않고, 알아보지 못한 것은 있는 그대로 흘려보낸다 —
// 정리 버튼이 문장을 망가뜨리지 않는 것이 예쁘게 만드는 것보다 훨씬 중요하다.
export function formatSQL(text, kind = 'sql') {
  const tokens = tokenizeSQL(text, kind).filter((t) => t.type !== 'newline');
  if (tokens.length === 0) return text;

  const lines = [];
  let line = '';
  let indent = 0;
  // 괄호 스택. 각 항목은 그 괄호를 열 때의 들여쓰기와 "줄을 나눠 쓰는 괄호인지"다.
  const parens = [];
  // 지금 문장이 정의문(CREATE/ALTER)인지. 정의문의 첫 괄호만 줄을 나눈다 —
  // 함수 호출까지 나누면 SELECT 한 줄이 스무 줄이 된다.
  let defStatement = false;
  let statementStart = true;

  const flush = () => {
    if (line.trim()) lines.push('  '.repeat(Math.max(0, indent)) + line.trim());
    line = '';
  };
  const newline = (nextIndent = indent) => {
    flush();
    indent = nextIndent;
  };

  const append = (token, glue = ' ') => {
    if (!line) line = token;
    else line += glue + token;
  };

  // 앞 토큰에 공백 없이 붙여야 하는가.
  const noSpaceBefore = (t) => t.type === 'punct'
    && (t.value === ',' || t.value === ';' || t.value === ')' || t.value === '.');
  const noSpaceAfter = (t) => t && t.type === 'punct' && (t.value === '(' || t.value === '.');
  // 여는 괄호를 앞 낱말에 붙일지. 함수 호출(`count(`)은 붙이고,
  // 예약어 뒤(`IN (`, `VALUES (`, `PRIMARY KEY (`)는 띄운다.
  const glueOpenParen = (prev) => prev && prev.type === 'word' && !KEYWORDS.has(prev.upper);

  for (let i = 0; i < tokens.length; i += 1) {
    const t = tokens[i];
    const prev = tokens[i - 1];
    const next = tokens[i + 1];
    const depth = parens.length;

    if (t.type === 'comment') {
      if (t.line) {
        // 줄 주석은 지금 줄 끝에 붙이고 줄을 끊는다. 다음 토큰이 주석 안으로
        // 들어가면 그 토큰이 통째로 사라진다.
        append(t.value);
        newline();
      } else {
        append(t.value);
      }
      continue;
    }

    if (t.type === 'punct' && t.value === ';') {
      append(';', '');
      flush();
      lines.push('');
      indent = 0;
      parens.length = 0;
      defStatement = false;
      statementStart = true;
      continue;
    }

    if (t.type === 'punct' && t.value === '(') {
      // 정의문의 최상위 괄호만 여러 줄로 편다.
      const split = defStatement && depth === 0;
      append('(', !split && glueOpenParen(prev) ? '' : ' ');
      parens.push({ indent, split });
      if (split) newline(indent + 1);
      continue;
    }

    if (t.type === 'punct' && t.value === ')') {
      const open = parens.pop();
      if (open?.split) {
        newline(open.indent);
        append(')', '');
      } else {
        append(')', '');
      }
      continue;
    }

    if (t.type === 'punct' && t.value === ',') {
      append(',', '');
      // 최상위(SELECT 목록)와 펼친 괄호 안(컬럼 정의)에서만 줄을 나눈다.
      // 최상위 목록의 둘째 줄부터는 한 단 들여쓴다 — 절과 항목이 같은 열에서
      // 시작하면 어디까지가 SELECT 목록인지 눈으로 구분되지 않는다.
      if (depth === 0) newline(1);
      else if (parens[depth - 1]?.split) newline();
      continue;
    }

    if (t.type === 'word') {
      const up = t.upper;
      // JOIN 앞에 LEFT/INNER 같은 수식어가 이미 붙어 있으면 그 줄을 이어 쓴다.
      // 그러지 않으면 "LEFT" 와 "JOIN" 이 두 줄로 갈라진다.
      const joinContinues = prev?.type === 'word' && (JOIN_LEAD.has(prev.upper) || prev.upper === 'OUTER');
      const isJoin = (up === 'JOIN' && !joinContinues)
        || (JOIN_LEAD.has(up) && next?.type === 'word' && (next.upper === 'JOIN' || next.upper === 'OUTER'));
      const isBy = BY_LEAD.has(up) && next?.type === 'word' && next.upper === 'BY';
      const startsClause = depth === 0 && !statementStart
        && (CLAUSE.has(up) || isJoin || isBy);

      if (startsClause) newline(0);
      // AND/OR는 조건 목록의 항목이므로 한 단 안으로 넣는다.
      else if (depth === 0 && BOOLEAN.has(up) && line) newline(1);

      const word = KEYWORDS.has(up) ? up : t.value;
      append(word, noSpaceAfter(prev) ? '' : ' ');

      if (statementStart) {
        defStatement = up === 'CREATE' || up === 'ALTER';
        statementStart = false;
      }
      continue;
    }

    // 문자열·식별자·숫자·연산자는 원문 그대로 둔다.
    append(t.value, noSpaceBefore(t) || noSpaceAfter(prev) ? '' : ' ');
    if (statementStart) statementStart = false;
  }
  flush();

  // 문장 사이의 빈 줄은 하나까지만 남기고, 끝의 빈 줄은 없앤다.
  const out = [];
  for (const l of lines) {
    if (l === '' && out[out.length - 1] === '') continue;
    out.push(l);
  }
  while (out.length && out[out.length - 1] === '') out.pop();
  return out.join('\n');
}
