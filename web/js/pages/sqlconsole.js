// SQL 콘솔 — 임의의 SQL(또는 Mongo/Redis 명령)을 실행한다.
//
// 데이터 화면과 분리한 이유: 권한이 다르고(sql.run), 위험의 성격도 다르다.
// 표에서 한 행을 고치는 것은 무엇이 바뀔지 눈에 보이지만, 여기서 실행하는 문장은
// 실행하기 전까지 몇 행이 바뀔지 아무도 모른다. 그래서 이 화면에는 기본적으로
// **읽기 전용 스위치가 켜져 있고**, 끄는 것은 의식적인 동작이다.
import { api } from '../core/api.js';
import { withProject } from '../core/project.js';
import { state, kindLabel } from '../core/store.js';
import {
  h, mount, icon, select, spinner, emptyState, pageHeader,
  badge, envBadge, toast, toastError, confirmDialog, copyToClipboard, formatDate,
} from '../core/ui.js';
import { navigate } from '../core/router.js';
import { codeEditor, codeBlock } from '../core/highlight.js';
import { formatSQL, quickCheck } from '../core/sqlfmt.js';
import { serverDbPicker } from '../core/connpick.js';
import { errorPanel } from './users.js';

// 최근 실행 이력은 브라우저에만 둔다. 서버에 남기면 다른 사람의 문장을 열람하는
// 통로가 되고(감사 로그는 슈퍼 어드민 전용이다), 여기서 필요한 것은 "방금 뭘 쳤더라"뿐이다.
const HISTORY_KEY = 'dbstudio.sql.history';
const HISTORY_MAX = 30;

export async function renderSQLConsole(outlet, params, query) {
  mount(outlet, spinner('커넥션 목록을 불러오는 중…'));

  let conns;
  let servers;
  try {
    // 서버 → DB 두 단계로 고른다(데이터·스키마 화면과 같은 규칙).
    [conns, servers] = await Promise.all([
      api.get(withProject('/connections/')),
      api.get(withProject('/servers/')),
    ]);
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  const usable = conns.items.filter((i) =>
    i.accessible && (i.caps ?? []).includes('sql.run') && i.dataCaps?.statement);

  if (usable.length === 0) {
    mount(outlet,
      pageHeader('SQL 콘솔', '임의 문장 실행'),
      emptyState('SQL을 실행할 수 있는 커넥션이 없습니다. 슈퍼 어드민에게 "SQL 실행" 권한을 요청하세요.',
        h('a.btn', { href: '/connections' }, 'DB 커넥션으로')),
    );
    return;
  }

  const selectedId = query.get('conn') || usable[0].connection.id;
  const current = usable.find((i) => i.connection.id === selectedId) ?? usable[0];
  const conn = current.connection;
  const support = current.dataCaps ?? {};

  const picker = serverDbPicker({
    usable,
    servers: servers.items ?? [],
    currentId: conn.id,
    onPick: (id) => navigate(`/sql?conn=${encodeURIComponent(id)}`),
  });

  // 강조 언어는 DB 종류를 따른다. Mongo는 JSON 명령이고 Redis는 한 줄짜리 명령이라
  // SQL 규칙을 씌우면 오히려 엉뚱한 낱말이 키워드로 칠해진다.
  const editorLang = { mongodb: 'json', redis: 'shell' }[conn.kind] ?? 'sql';
  const editor = codeEditor({
    value: query.get('q') ?? '',
    language: editorLang,
    rows: 10,
    placeholder: support.statementHelp ?? '',
    // 줄 번호는 오류 메시지를 문장 안의 자리와 잇는 유일한 실마리다. DB가 "3행"
    // 이라고 알려줘도 번호가 없으면 세어 가며 찾아야 한다.
    lineNumbers: true,
    // Ctrl+Enter는 지금까지처럼 전체 실행이다. 손에 익은 동작을 바꾸지 않고,
    // 한 문장은 Shift를 더해 고른다.
    onSubmit: (_value, e) => run(e?.shiftKey && runOneBtn ? 'one' : 'all'),
  });

  const readOnly = h('input', { type: 'checkbox', checked: true });
  const resultBox = h('div.sql-results');
  const runBtn = h('button.btn.btn-primary', { type: 'button' },
    icon('play'), '전체 실행', h('kbd.btn-key', {}, 'Ctrl+Enter'));
  // 한 문장 실행은 편집기에 여러 문장을 늘어놓고 그중 하나만 돌려 보는 흐름을 위한 것이다.
  // MongoDB는 스크립트 전체가 명령 하나이므로 나눌 것이 없다.
  //
  // 단축키를 툴팁이 아니라 버튼에 적는다. 툴팁은 마우스를 올려 기다린 사람만 보고,
  // 그 사람은 이미 마우스로 누르는 중이라 단축키가 필요 없다.
  const oneLabel = h('span', {}, '한 문장 실행');
  const runOneBtn = conn.kind === 'mongodb'
    ? null
    : h('button.btn', { type: 'button' },
      icon('play'), oneLabel, h('kbd.btn-key', {}, 'Ctrl+Shift+Enter'));

  // pickOne은 실행할 한 문장을 고른다.
  // 고른 것이 없으면(빈 편집기) null.
  const pickOne = () => {
    const area = editor.textarea;
    const selected = area.value.slice(area.selectionStart, area.selectionEnd).trim();
    // 사용자가 직접 고른 범위가 언제나 우선이다. 그 이상 추측하지 않는다.
    if (selected) return selected;
    return statementAt(area.value, area.selectionStart, conn.kind);
  };

  // 선택 범위가 있으면 버튼이 무엇을 실행할지 이름으로 알린다.
  const syncOneLabel = () => {
    const area = editor.textarea;
    oneLabel.textContent = area.selectionStart !== area.selectionEnd ? '선택 실행' : '한 문장 실행';
  };
  if (runOneBtn) {
    for (const ev of ['keyup', 'mouseup', 'input', 'focus', 'select']) {
      editor.textarea.addEventListener(ev, syncOneLabel);
    }
  }

  const run = async (scope = 'all') => {
    const statement = scope === 'one' ? pickOne() : editor.value.trim();
    if (!statement) {
      toast(scope === 'one' ? '커서가 놓인 문장을 찾지 못했습니다' : '실행할 문장을 입력하세요', 'error');
      return;
    }

    // 운영 DB에 쓰기 문장을 실행할 때만 확인을 요구한다. 매번 물으면
    // 사람은 읽지 않고 누르게 되고, 그러면 확인 창은 없는 것과 같다.
    if (!readOnly.checked && conn.environment === 'prod') {
      const ok = await confirmDialog({
        title: '운영 DB에서 실행',
        message: `${conn.name} 은(는) 운영 데이터베이스입니다. 읽기 전용을 끈 상태로 실행하면 데이터가 바뀔 수 있습니다.`,
        confirmLabel: '실행',
        danger: true,
        requireText: conn.name,
      });
      if (!ok) return;
    }

    runBtn.disabled = true;
    if (runOneBtn) runOneBtn.disabled = true;
    mount(resultBox, spinner('실행 중…'));
    try {
      const res = await api.post(`/connections/${conn.id}/statement`, {
        statement,
        readOnly: readOnly.checked,
        maxRows: 500,
      });
      pushHistory(conn.id, statement);
      mount(resultBox, ...resultViews(res.results));
    } catch (err) {
      mount(resultBox, errorPanel(err));
    } finally {
      runBtn.disabled = false;
      if (runOneBtn) runOneBtn.disabled = false;
    }
  };

  runBtn.addEventListener('click', () => run('all'));
  runOneBtn?.addEventListener('click', () => run('one'));

  // ---------- 검사와 정리 ----------
  //
  // 두 버튼은 실행 버튼과 떨어뜨려 편집기 아래에 둔다. 실행 줄에 함께 놓으면
  // "검사"를 누르려다 "실행"을 누르는 사고가 난다 — 운영 DB에서는 그 한 번이 크다.

  const checkBox = h('div.sql-check');

  // 정리는 SQL에만 있다. Mongo는 JSON, Redis는 한 줄 명령이라 정리할 문법이 없다.
  const formatBtn = editorLang === 'sql'
    ? h('button.btn.btn-small', { type: 'button', title: '들여쓰기와 예약어 대소문자를 정리합니다' },
      icon('edit'), '포맷팅')
    : null;
  formatBtn?.addEventListener('click', () => {
    const before = editor.value;
    if (!before.trim()) {
      toast('정리할 문장이 없습니다', 'error');
      return;
    }
    const after = formatSQL(before, conn.kind);
    if (after === before) {
      toast('이미 정리된 상태입니다', 'info');
      return;
    }
    editor.value = after;
    editor.focus();
    toast('문장을 정리했습니다', 'success');
  });

  // 편집기를 비우는 버튼. 긴 문장을 지우려고 전체 선택 후 지우는 동작을 줄인다.
  // 실수로 눌러도 되돌릴 수 있게, 지운 내용을 한 번은 되돌려 준다.
  let cleared = '';
  const clearBtn = h('button.btn.btn-small', { type: 'button', title: '입력란을 비웁니다' },
    icon('x'), '초기화');
  clearBtn.addEventListener('click', () => {
    const before = editor.value;
    if (!before.trim() && !cleared) {
      toast('이미 비어 있습니다', 'info');
      return;
    }
    if (cleared && !before.trim()) {
      // 방금 비운 것을 되돌린다.
      editor.value = cleared;
      cleared = '';
      editor.focus();
      toast('되돌렸습니다', 'success');
      return;
    }
    cleared = before;
    editor.value = '';
    editor.focus();
    toast('입력란을 비웠습니다 — 한 번 더 누르면 되돌립니다', 'info');
  });

  // 실행 계획. DB마다 이름이 다르므로 서버가 알려준 접두사를 붙여 실행한다.
  //
  // PostgreSQL·MySQL의 ANALYZE는 문장을 **실제로 실행한 뒤** 걸린 시간을 알려준다.
  // 그래서 조회 문장에만 의미가 있고, 쓰기 문장은 읽기 전용 판정에서 막힌다.
  const explainBtn = support.explain
    ? h('button.btn.btn-small', {
      type: 'button',
      title: `${support.explain.trim()} 를 붙여 실행 계획과 비용을 봅니다`,
    }, icon('activity'), '실행 분석')
    : null;
  explainBtn?.addEventListener('click', async () => {
    const statement = (pickOne() || editor.value.trim()).replace(/;\s*$/, '');
    if (!statement) {
      toast('분석할 문장을 입력하세요', 'error');
      return;
    }
    explainBtn.disabled = true;
    mount(resultBox, spinner('실행 계획을 분석하는 중…'));
    try {
      const res = await api.post(`/connections/${conn.id}/statement`, {
        statement: support.explain + statement,
        // 계획을 보는 것이므로 읽기 전용을 강제한다. ANALYZE가 붙은 쓰기 문장은
        // 서버가 거절한다 — 그것은 "분석"이 아니라 실행이다.
        readOnly: true,
        maxRows: 500,
      });
      mount(resultBox, ...resultViews(res.results));
    } catch (err) {
      mount(resultBox, errorPanel(err));
    } finally {
      explainBtn.disabled = false;
    }
  });

  // 구문 검사는 서버가 대상 DB의 드라이버로 문장을 **준비만** 해 본다.
  // 그래서 문법뿐 아니라 테이블·컬럼 이름의 오타까지 잡히고, 아무것도 실행되지 않는다.
  const checkBtn = support.statementCheck
    ? h('button.btn.btn-small', { type: 'button', title: '실행하지 않고 문장을 확인합니다' },
      icon('check'), '구문 검사')
    : null;
  checkBtn?.addEventListener('click', async () => {
    const statement = editor.value.trim();
    if (!statement) {
      toast('검사할 문장을 입력하세요', 'error');
      return;
    }
    // 브라우저에서 확실히 알 수 있는 것(닫히지 않은 따옴표·괄호)은 먼저 본다.
    // 이런 입력을 서버로 보내면 DB가 문장 끝을 기다리다 엉뚱한 오류를 돌려준다.
    const local = quickCheck(statement, conn.kind);
    if (local.length > 0) {
      mount(checkBox, checkResultView({ local }));
      return;
    }
    checkBtn.disabled = true;
    mount(checkBox, spinner('문장을 확인하는 중…'));
    try {
      const res = await api.post(`/connections/${conn.id}/statement/check`, { statement });
      mount(checkBox, checkResultView({ checks: res.checks, errors: res.errors }));
    } catch (err) {
      mount(checkBox, errorPanel(err));
    } finally {
      checkBtn.disabled = false;
    }
  });

  mount(outlet,
    pageHeader('SQL 콘솔', `${conn.name} 에서 문장을 실행합니다`, [
      h('a.btn', { href: `/data?conn=${encodeURIComponent(conn.id)}` }, icon('database'), '데이터 화면'),
    ]),
    h('div.card.filter-bar', {},
      ...picker.nodes,
      envBadge(conn.environment),
      h('div.filter-sep'),
      h('label.checkbox', {}, readOnly, h('span', {}, '읽기 전용')),
      runOneBtn,
      runBtn,
    ),
    h('div.card', {},
      h('div.card-title', {}, support.statementLabel ?? 'SQL'),
      editor.el,
      h('div.sql-editor-tools', {}, checkBtn, formatBtn, explainBtn, clearBtn,
        h('span.muted.small', {}, '검사와 포맷팅은 문장을 실행하지 않습니다')),
      checkBox,
      h('p.field-help', {}, support.statementHelp ?? ''),
    ),
    historyCard(conn.id, editorLang, (stmt) => { editor.value = stmt; editor.focus(); }),
    resultBox,
  );

  editor.focus();

  function resultViews(results) {
    return buildResultViews(results, editorLang);
  }
}

function buildResultViews(results, lang) {
  if (!results?.length) return [emptyState('결과가 없습니다')];
  return results.map((r, i) => {
    const head = h('div.card-title', {},
      h('span', {}, `문장 ${i + 1}`),
      r.error ? badge('실패', 'danger') : badge(kindLabel2(r), 'success'),
      h('span.muted.small', {}, `${r.elapsedMs.toFixed(1)}ms`),
      h('button.link-btn', { type: 'button', onclick: () => copyToClipboard(r.statement) }, '문장 복사'),
    );

    if (r.error) {
      return h('div.card', {}, head,
        codeBlock(r.statement, lang),
        h('p.notice.notice-danger', {}, icon('alert'), r.error));
    }

    if (r.kind !== 'rows') {
      return h('div.card', {}, head,
        codeBlock(r.statement, lang),
        h('p.notice.notice-info', {}, icon('check'),
          r.affected >= 0 ? `${r.affected}건 처리되었습니다` : '실행되었습니다'));
    }

    if (!r.rows?.length) {
      return h('div.card', {}, head, codeBlock(r.statement, lang),
        emptyState('반환된 행이 없습니다'));
    }

    return h('div.card', {}, head,
      codeBlock(r.statement, lang),
      r.truncated
        ? h('p.notice.notice-warn', {}, icon('alert'),
            `결과가 많아 앞의 ${r.rows.length}행만 보여줍니다`)
        : null,
      h('div.table-scroll', {},
        h('table.table.data-table', {},
          h('thead', {}, h('tr', {}, r.columns.map((c) =>
            h('th', {}, h('span.col-name', {}, c.name), h('span.col-type', {}, c.type))))),
          h('tbody', {}, r.rows.map((row) => h('tr', {}, row.map((v) =>
            v === null || v === undefined
              ? h('td.cell-null', {}, 'NULL')
              : h('td', {}, h('span.cell-value', {}, String(v))))))),
        )),
      h('p.muted.small', {}, `${r.rows.length.toLocaleString()}행`),
    );
  });
}

function kindLabel2(r) {
  return r.kind === 'rows' ? `${r.rows?.length ?? 0}행` : '완료';
}

// ---------- 구문 검사 결과 ----------

// checkResultView는 검사 결과를 문장 단위로 보여준다.
//
// "확인할 수 없음"(skipped)을 실패와 다른 색으로 그리는 것이 이 화면의 핵심이다.
// 둘을 같이 보여주면 사람은 멀쩡한 문장을 고치기 시작한다.
function checkResultView({ local, checks, errors }) {
  if (local?.length) {
    return h('div.notice.notice-danger', {}, icon('alert'),
      h('div', {},
        h('strong', {}, `문장이 완성되지 않았습니다 (${local.length}곳)`),
        h('ul.note-list', {}, local.map((i) =>
          h('li', {}, h('code', {}, `${i.line}행`), ` ${i.message}`))),
        h('p.muted.small', {}, '먼저 이 부분을 고친 뒤 다시 검사하세요.')));
  }

  const list = checks ?? [];
  if (list.length === 0) return emptyState('검사할 문장이 없습니다');

  const skipped = list.filter((c) => c.status === 'skipped').length;
  const head = errors > 0
    ? h('p.notice.notice-danger', {}, icon('alert'), `${errors}개 문장에 문제가 있습니다`)
    : h('p.notice.notice-success', {}, icon('check'),
      skipped > 0
        ? `문제를 찾지 못했습니다 (${skipped}개 문장은 확인할 수 없었습니다)`
        : `${list.length}개 문장 모두 이상 없습니다`);

  return h('div.sql-check-body', {}, head,
    h('div.sql-check-list', {}, list.map((ck) => h('div.sql-check-item', {},
      h('div.sql-check-head', {},
        h('span.muted.small', {}, `문장 ${ck.index + 1}`),
        ck.status === 'ok' ? badge('통과', 'success')
          : ck.status === 'skipped' ? badge('확인 불가', 'neutral')
            : badge('오류', 'danger')),
      codeBlock(ck.statement.length > 200 ? `${ck.statement.slice(0, 200)}…` : ck.statement,
        'sql', { className: 'history-sql' }),
      ck.error ? h('p.sql-check-msg.is-error', {}, ck.error) : null,
      ck.reason ? h('p.sql-check-msg.muted', {}, ck.reason) : null,
    ))));
}

// ---------- 문장 나누기 ----------
//
// 서버의 splitSQL(internal/dbx/data_sql.go)과 같은 규칙을 화면에서도 쓴다.
// 커서가 놓인 문장 하나만 보내려면 어디서 끊기는지 화면이 먼저 알아야 한다.
//
// 규칙이 두 곳에 있는 것은 감수한다. 어긋나더라도 결과는 "조금 어긋난 조각을
// 보냄"이고, 서버가 받은 것을 다시 나눠 실행하므로 엉뚱한 문장이 실행되지는 않는다.
// (한 문장 실행을 서버에 맡기려면 커서 위치를 보내야 하는데, 그러면 이 API가
// 편집기 사정을 알아야 한다.)
function splitStatements(text, kind) {
  // 레디스는 한 줄이 명령 하나다. 서버도 줄 단위로 나눈다.
  if (kind === 'redis') {
    const out = [];
    let at = 0;
    for (const line of text.split('\n')) {
      const s = line.trim();
      if (s && !s.startsWith('#')) out.push({ start: at, end: at + line.length, text: s });
      at += line.length + 1;
    }
    return out;
  }

  const out = [];
  const n = text.length;
  let start = 0;
  let i = 0;
  // 범위는 앞뒤 공백을 뺀 실제 문장 자리로 잡는다. 공백까지 포함하면 세미콜론
  // 바로 뒤에 있는 커서가 "다음 문장의 앞 여백"으로 읽혀, 방금 친 문장 대신
  // 아래 문장이 실행된다.
  const flush = (end, next) => {
    const raw = text.slice(start, end);
    const body = raw.trim();
    if (body) {
      const lead = raw.length - raw.trimStart().length;
      out.push({ start: start + lead, end: start + lead + body.length, text: body });
    }
    start = next;
  };

  while (i < n) {
    const c = text[i];
    // 줄 주석
    if ((c === '-' && text[i + 1] === '-') || (c === '#' && kind === 'mysql')) {
      while (i < n && text[i] !== '\n') i += 1;
      continue;
    }
    // 블록 주석
    if (c === '/' && text[i + 1] === '*') {
      i += 2;
      while (i < n && !(text[i] === '*' && text[i + 1] === '/')) i += 1;
      i = Math.min(n, i + 2);
      continue;
    }
    // 문자열·식별자 인용
    if (c === "'" || c === '"' || c === '`') {
      i += 1;
      while (i < n) {
        if (text[i] === '\\' && kind === 'mysql' && c === "'") { i += 2; continue; }
        if (text[i] === c) {
          if (text[i + 1] === c) { i += 2; continue; } // 두 번 = 이스케이프
          i += 1;
          break;
        }
        i += 1;
      }
      continue;
    }
    // PostgreSQL 달러 인용: $$ ... $$ 또는 $tag$ ... $tag$
    if (c === '$' && kind === 'postgres') {
      const m = /^\$([A-Za-z_][A-Za-z0-9_]*)?\$/.exec(text.slice(i));
      if (m) {
        const close = text.indexOf(m[0], i + m[0].length);
        i = close < 0 ? n : close + m[0].length;
        continue;
      }
    }
    if (c === ';') {
      flush(i, i + 1);
      i += 1;
      continue;
    }
    i += 1;
  }
  flush(n, n);
  return out;
}

// statementAt은 커서가 놓인 문장을 돌려준다.
// 문장 밖(빈 줄, 세미콜론 바로 뒤)이면 바로 앞 문장으로 본다 —
// 방금 친 문장을 다시 돌려 보려는 것이 보통이다.
function statementAt(text, pos, kind) {
  const parts = splitStatements(text, kind);
  if (!parts.length) return '';
  const hit = parts.find((p) => pos >= p.start && pos <= p.end);
  if (hit) return hit.text;
  const before = parts.filter((p) => p.end <= pos).pop();
  return (before ?? parts[0]).text;
}

// ---------- 실행 이력 ----------

function readHistory() {
  try {
    return JSON.parse(localStorage.getItem(HISTORY_KEY) ?? '[]');
  } catch {
    // 저장 형식이 바뀌었거나 손상된 경우. 이력은 없어도 되는 정보이므로 조용히 비운다.
    return [];
  }
}

function pushHistory(connId, statement) {
  const items = readHistory().filter((i) => !(i.connId === connId && i.statement === statement));
  items.unshift({ connId, statement, at: new Date().toISOString() });
  try {
    localStorage.setItem(HISTORY_KEY, JSON.stringify(items.slice(0, HISTORY_MAX)));
  } catch {
    // 용량 초과 등. 이력 저장 실패가 실행을 방해해서는 안 된다.
  }
}

// dropHistory는 이력 한 건을 지운다. 문장과 커넥션이 같은 항목이 유일하므로
// (pushHistory가 같은 문장을 앞으로 끌어올린다) 그 둘로 찾는다.
function dropHistory(connId, statement) {
  const items = readHistory().filter((i) => !(i.connId === connId && i.statement === statement));
  try {
    localStorage.setItem(HISTORY_KEY, JSON.stringify(items));
  } catch { /* 지우기 실패는 실행을 방해하지 않는다 */ }
}

function clearHistory(connId) {
  // 다른 커넥션의 이력은 남긴다. "이 화면에서 보이는 것만 지운다"가
  // 버튼이 있는 자리에서 기대되는 동작이다.
  const items = readHistory().filter((i) => i.connId !== connId);
  try {
    localStorage.setItem(HISTORY_KEY, JSON.stringify(items));
  } catch { /* 위와 같다 */ }
}

// historyCard는 최근 실행 목록을 그린다.
//
// 비어 있어도 빈 상자를 반환하는 이유: 지우고 나면 다시 그려야 하는데, null을
// 돌려주면 붙일 자리가 사라진다. 대신 내용이 없을 때는 아무것도 그리지 않는다.
function historyCard(connId, lang, onPick) {
  const box = h('div');
  // 펼침 상태는 다시 그려도 유지한다. 항목 하나를 지웠다고 목록이 접히면,
  // 지운 결과를 확인하려고 매번 다시 펼쳐야 한다.
  let open = false;

  const draw = () => {
    const items = readHistory().filter((i) => i.connId === connId);
    if (items.length === 0) {
      mount(box);
      return;
    }
    const card = h('details.card.history-card', { open },
      h('summary', {}, `최근 실행 ${items.length}건`),
      h('div.history-actions', {},
        h('button.btn.btn-small.btn-danger-ghost', {
          type: 'button',
          onclick: async () => {
            const ok = await confirmDialog({
              title: '최근 실행 초기화',
              message: `이 커넥션의 최근 실행 ${items.length}건을 지웁니다. 이 기록은 이 브라우저에만 있습니다.`,
              confirmLabel: '초기화', danger: true,
            });
            if (!ok) return;
            clearHistory(connId);
            draw();
          },
        }, icon('trash'), '전체 초기화'),
      ),
      h('div.history-list', {}, items.map((i) => h('div.history-row', {},
        h('button.history-item', { type: 'button', onclick: () => onPick(i.statement) },
          codeBlock(i.statement.length > 160 ? `${i.statement.slice(0, 160)}…` : i.statement,
            lang, { className: 'history-sql' }),
          h('span.muted.small', {}, formatDate(i.at)),
        ),
        h('button.icon-btn.danger', {
          type: 'button', title: '이 기록 지우기',
          onclick: () => { dropHistory(connId, i.statement); draw(); },
        }, icon('trash', 14)),
      ))),
    );
    card.addEventListener('toggle', () => { open = card.open; });
    mount(box, card);
  };

  draw();
  return box;
}
