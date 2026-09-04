// 스키마 탐색기: 테이블 목록 → 상세(컬럼/인덱스/외래키/제약), DDL 스크립트, 두 DB 비교.
import { api } from '../core/api.js';
import { withProject } from '../core/project.js';
import { state, kindLabel } from '../core/store.js';
import {
  h, mount, icon, select, input, spinner, emptyState, pageHeader,
  badge, envBadge, toast, toastError, formatDate, copyToClipboard, openModal,
} from '../core/ui.js';
import { navigate } from '../core/router.js';
import { serverDbPicker } from '../core/connpick.js';
import { setScreenDetail, screenConn } from '../core/screen.js';
import { codeBlock } from '../core/highlight.js';
import { errorPanel } from './users.js';
import { optionChips } from '../core/dboptions.js';

export async function renderSchema(outlet, params, query) {
  mount(outlet, spinner('커넥션 목록을 불러오는 중…'));

  let conns;
  let servers;
  try {
    [conns, servers] = await Promise.all([
      api.get(withProject('/connections/')),
      api.get(withProject('/servers/')),
    ]);
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  // 스키마 조회는 모니터링 등급 이상 + introspect를 지원하는 종류만 가능하다.
  const usable = conns.items.filter((i) => {
    if (!i.accessible) return false;
    const info = state.meta?.dbKinds?.find((k) => k.kind === i.connection.kind);
    return info?.capabilities?.introspect;
  });

  if (usable.length === 0) {
    mount(outlet,
      pageHeader('스키마', '데이터베이스 구조 탐색'),
      emptyState('스키마를 조회할 수 있는 커넥션이 없습니다. DB 커넥션을 등록하고 모니터링 이상 권한을 받으세요.',
        h('a.btn', { href: '/connections' }, 'DB 커넥션으로')),
    );
    return;
  }

  const selectedId = query.get('conn') || usable[0].connection.id;
  const current = usable.find((i) => i.connection.id === selectedId) ?? usable[0];

  // 어시스턴트에게 어느 DB의 스키마를 보고 있는지 알린다.
  setScreenDetail([screenConn(current.connection)]);

  const picker = serverDbPicker({
    usable,
    servers: servers.items ?? [],
    currentId: current.connection.id,
    onPick: (id) => navigate(`/schema?conn=${encodeURIComponent(id)}`),
  });

  const body = h('div');

  // 비교는 평소에 쓰지 않는 기능이다. 선택기를 늘 펼쳐 두면 "지금 무엇을 보고
  // 있는가"를 정하는 커넥션 선택과 뒤섞여, 매번 둘 중 어느 것이 본체인지 봐야 한다.
  // 토글로 접어 두고 누를 때만 대상 선택을 내놓는다.
  const otherUsable = usable.filter((i) => i.connection.id !== current.connection.id);
  const comparePanel = h('div.card.compare-panel', { style: { display: 'none' } });
  let comparePicker = null;

  const compareToggle = h('button.btn', {
    type: 'button',
    'aria-expanded': 'false',
    onclick: () => {
      const open = comparePanel.style.display === 'none';
      comparePanel.style.display = open ? '' : 'none';
      compareToggle.classList.toggle('is-on', open);
      compareToggle.setAttribute('aria-expanded', String(open));
      if (open && !comparePicker) buildComparePanel();
    },
  }, icon('activity'), '비교하기');

  function buildComparePanel() {
    if (otherUsable.length === 0) {
      mount(comparePanel, h('p.muted.small', {},
        '비교할 다른 커넥션이 없습니다. 같은 구조를 다른 DB와 맞춰 보려면 커넥션을 하나 더 등록하세요.'));
      return;
    }
    // 비교 대상도 같은 방식으로 고른다 — 서버를 먼저, 그 안에서 DB를.
    comparePicker = serverDbPicker({
      usable: otherUsable,
      servers: servers.items ?? [],
      currentId: otherUsable[0].connection.id,
      onPick: () => {},
      serverLabel: '비교 서버',
      dbLabelText: '비교 DB',
    });
    mount(comparePanel,
      h('div.compare-row', {},
        ...comparePicker.nodes,
        h('button.btn.btn-primary', {
          type: 'button',
          onclick: () => {
            const target = comparePicker.dbSelect.value;
            if (!target) {
              toast('비교할 DB를 선택하세요', 'error');
              return;
            }
            runCompare(body, current.connection, target, usable);
          },
        }, icon('activity'), '비교 실행'),
      ),
      h('p.field-help', {},
        `${current.connection.name} 의 구조를 기준으로, 고른 DB와의 차이를 보여줍니다.`),
    );
  }

  mount(outlet,
    pageHeader('스키마', '데이터베이스 구조 탐색 · 비교 · DDL 생성', [
      h('button.btn', { type: 'button', onclick: () => load() }, icon('refresh'), '다시 읽기'),
    ]),
    h('div.card.filter-bar', {},
      ...picker.nodes,
      h('div.filter-sep'),
      compareToggle,
      h('button.btn', {
        type: 'button',
        onclick: () => showDDL(current.connection),
      }, icon('copy'), 'DDL 생성'),
    ),
    comparePanel,
    body,
  );

  async function load() {
    mount(body, spinner(`${current.connection.name} 스키마를 읽는 중…`));
    try {
      const sc = await api.get(`/connections/${current.connection.id}/schema`);
      mount(body, ...schemaView(sc, current));
    } catch (err) {
      mount(body, errorPanel(err));
    }
  }

  await load();
}

// ---------- 스키마 표시 ----------

// COMMENT_KINDS는 설명(주석)을 저장할 수 있는 DB다.
//
// SQLite는 컬럼 주석을 저장하지 않는다. 고칠 수 있게 두면 적어 놓고 저장을 눌렀을 때
// "실행할 SQL이 없습니다"를 만나게 되므로, 그 화면에서는 아예 읽기 전용으로 둔다.
const COMMENT_KINDS = new Set(['mysql', 'postgres', 'mssql', 'oracle']);

function schemaView(sc, current) {
  const parts = [];
  // 설명은 DB에 저장되는 구조의 일부다(COMMENT). 고치는 것은 DDL이므로 여기서 바로
  // 실행하지 않고 마이그레이션 계획을 만든다 — 이 앱에서 DB 구조는 언제나
  // 계획 → 리뷰 → 승인 → 실행을 거친다.
  const canEdit = Boolean(current)
    && current.level === 'migrate'
    && COMMENT_KINDS.has(current.connection?.kind);
  const edit = canEdit ? newCommentEditor(current.connection) : null;

  parts.push(h('div.stat-row', {},
    statTile('테이블', sc.stats.tables, 'database'),
    statTile('컬럼', sc.stats.columns, 'list'),
    statTile('인덱스', sc.stats.indexes, 'activity'),
    statTile('외래키', sc.stats.foreignKeys, 'shield'),
    statTile('추정 행 수', formatCount(sc.stats.rowEstimate), 'list'),
    statTile('크기', formatBytes(sc.stats.sizeBytes), 'database'),
  ));

  parts.push(h('div.schema-meta', {},
    h('span', {}, `${kindLabel(sc.dialect)} · ${sc.name || '—'}`),
    h('span.muted', {}, `읽은 시각 ${formatDate(sc.capturedAt)}`),
    h('code.fingerprint', { title: '구조 지문 — 외부 편집 감지에 사용됩니다' }, sc.fingerprint),
  ));

  if (sc.notes?.length) {
    parts.push(h('div.card.notice.notice-warn', {},
      icon('alert'),
      h('div', {},
        h('strong', {}, '읽는 중 참고 사항'),
        h('ul.note-list', {}, sc.notes.map((n) => h('li', {}, n))),
      ),
    ));
  }

  if (sc.shape !== 'relational') {
    // 이 화면은 다른 DB와 나란히 비교하기 위한 최소 공통 표현이다.
    // 저장 크기·인덱스 사용 횟수·메모리 정책은 전용 화면에만 있으므로 그쪽으로 안내한다.
    parts.push(h('p.notice.notice-info', {}, icon('activity'),
      h('span', {},
        sc.shape === 'document'
          ? '문서 데이터베이스입니다. 아래 구조는 문서를 샘플링해 추론한 결과이며 강제된 스키마가 아닙니다.'
          : '키-값 저장소입니다. 아래는 키 접두사별 그룹 요약입니다.',
        sc.connection?.id
          ? h('a.notice-link', { href: `/nosql?conn=${encodeURIComponent(sc.connection.id)}` },
            '전용 탐색 화면 보기')
          : null)));
  }

  if (!sc.tables?.length) {
    parts.push(emptyState('테이블이 없습니다'));
    return parts;
  }

  // 검색 + 테이블 목록
  const listBox = h('div.table-list');
  const search = input({ placeholder: '이름으로 검색', type: 'search' });
  const countLabel = h('span.muted');
  // 찾는 대상을 고를 수 있게 한다. 한 낱말이 테이블 이름에도 컬럼 이름에도 흔히
  // 쓰이면(user, id, status …) 섞인 결과에서 원하는 쪽을 골라내야 한다.
  const scopeSelect = select([
    { value: 'all', label: '테이블 + 컬럼' },
    { value: 'table', label: '테이블 이름' },
    { value: 'column', label: '컬럼 이름' },
  ], { value: 'all' });

  const renderList = () => {
    const q = search.value.trim().toLowerCase();
    const scope = scopeSelect.value;
    const byTable = (t) => t.name.toLowerCase().includes(q)
      || (t.namespace ?? '').toLowerCase().includes(q);
    const byColumn = (t) => t.columns.some((c) => c.name.toLowerCase().includes(q));
    const match = (t) => {
      if (scope === 'table') return byTable(t);
      if (scope === 'column') return byColumn(t);
      return byTable(t) || byColumn(t);
    };
    const filtered = !q ? sc.tables : sc.tables.filter(match);
    countLabel.textContent = q
      ? `${filtered.length} / ${sc.tables.length}개 테이블`
      : `${sc.tables.length}개 테이블`;
    mount(listBox, filtered.length
      ? filtered.map((t) => tableCard(t, sc, scope === 'table' ? '' : q, edit))
      : emptyState('검색 결과가 없습니다'));
  };
  search.addEventListener('input', renderList);
  scopeSelect.addEventListener('change', renderList);

  parts.push(h('div.card.filter-bar', {},
    h('label.field.field-inline', {}, h('span.field-label', {}, '찾기'), scopeSelect),
    h('label.field.field-inline.grow', {}, icon('list'), search),
    countLabel,
  ));
  parts.push(listBox);
  if (edit) parts.push(edit.bar);
  renderList();

  if (sc.views?.length) {
    parts.push(h('section.card', {},
      h('h2.card-title', {}, `뷰 ${sc.views.length}개`),
      // 뷰 정의는 SQL이다. 다른 화면의 SQL에는 모두 강조가 붙는데 여기만
      // 회색 덩어리였다 — 같은 것을 다르게 보여줄 이유가 없다.
      h('div.view-list', {}, sc.views.map((v) => h('details.view-item', {},
        h('summary', {}, v.namespace ? `${v.namespace}.${v.name}` : v.name),
        v.definition
          ? codeBlock(v.definition, 'sql', { className: 'sql-block' })
          : h('pre.sql-block', {}, '(정의를 읽지 못했습니다)'),
      ))),
    ));
  }

  if (sc.enums?.length) {
    parts.push(h('section.card', {},
      h('h2.card-title', {}, `Enum 타입 ${sc.enums.length}개`),
      h('div.enum-list', {}, sc.enums.map((e) => h('div.enum-item', {},
        h('code', {}, e.namespace ? `${e.namespace}.${e.name}` : e.name),
        h('div.tag-row', {}, e.values.map((v) => badge(v, 'accent'))),
      ))),
    ));
  }

  return parts;
}

// newCommentEditor는 고친 설명을 모았다가 마이그레이션 계획으로 만든다.
//
// 한 줄 고칠 때마다 계획을 만들지 않는 이유: 설명은 대개 여러 줄을 훑으며 한 번에
// 채운다. 줄마다 계획이 생기면 승인 대기 목록이 설명 수정으로 가득 찬다.
function newCommentEditor(conn) {
  // 키는 "테이블\u0000컬럼"이다. 검색으로 목록을 다시 그려도 고친 값이 남아야 하므로
  // 입력 요소가 아니라 이 지도가 기억한다.
  const edits = new Map();
  // 화면에 살아 있는 입력 칸들. 되돌릴 때 값을 제자리에서 되돌리기 위해 들고 있는다.
  // 화면을 통째로 다시 그리면 펼쳐 둔 테이블이 모두 접혀, 고치던 자리를 잃는다.
  const boxes = [];
  const bar = h('div.schema-edit-bar', { hidden: true });
  const keyOf = (t, column) => `${(t.namespace ? `${t.namespace}.` : '') + t.name}\u0000${column}`.toLowerCase();

  const renderBar = () => {
    bar.hidden = edits.size === 0;
    if (!edits.size) {
      mount(bar);
      return;
    }
    mount(bar,
      h('span', {}, `설명 ${edits.size}건을 고쳤습니다`),
      h('span.muted.small', {}, '실제 DB를 바꾸는 일이라 마이그레이션으로 만들어 승인을 받습니다'),
      h('button.btn.btn-small', {
        type: 'button',
        onclick: () => {
          edits.clear();
          // 이미 사라진 칸은 건너뛴다(검색으로 목록이 다시 그려졌을 수 있다).
          for (const { box, original } of boxes) {
            if (!box.isConnected) continue;
            box.value = original;
            box.classList.remove('is-changed');
          }
          renderBar();
        },
      }, '되돌리기'),
      h('button.btn.btn-small.btn-primary', { type: 'button', onclick: () => submit() },
        icon('play', 13), ' 마이그레이션 만들기'),
    );
  };

  const submit = async () => {
    const items = [...edits.values()];
    try {
      const res = await api.post(`/connections/${conn.id}/schema/comments`, {
        title: `설명 수정 ${items.length}건`,
        items,
      });
      toast('마이그레이션 계획을 만들었습니다', 'success');
      navigate(`/migrations/${encodeURIComponent(res.migration.id)}`);
    } catch (err) {
      toastError(err);
    }
  };

  return {
    bar,
    // field는 설명 한 칸이다. 원래 값과 같아지면 목록에서 빠진다 —
    // 고쳤다가 되돌린 것을 "바꾼 것"으로 세면 계획에 빈 변경이 들어간다.
    field(t, column, original) {
      const key = keyOf(t, column);
      const box = input({
        value: edits.get(key)?.comment ?? original,
        // 줄마다 긴 안내가 반복되면 표가 안내문으로 덮인다. 열 이름이 이미
        // '설명'이므로 여기서는 짧게만 권한다.
        placeholder: '설명 추가',
        class: 'input comment-input',
      });
      boxes.push({ box, original: original ?? '' });
      box.classList.toggle('is-changed', edits.has(key));
      box.addEventListener('input', () => {
        const value = box.value;
        if (value.trim() === (original ?? '').trim()) edits.delete(key);
        else {
          edits.set(key, {
            namespace: t.namespace ?? '', table: t.name, column, comment: value,
          });
        }
        box.classList.toggle('is-changed', edits.has(key));
        renderBar();
      });
      return box;
    },
  };
}

function statTile(label, value, iconName) {
  return h('div.stat', {},
    h('div.stat-icon', {}, icon(iconName, 18)),
    h('div', {}, h('div.stat-value', {}, value), h('div.stat-label', {}, label)),
  );
}

// tableCard는 접었다 펼치는 테이블 상세다.
// 테이블이 수백 개일 수 있으므로 펼칠 때 내용을 만든다.
// hit는 컬럼 이름으로 걸린 검색어다. 어느 컬럼 때문에 이 표가 나왔는지
// 접힌 상태에서는 알 수 없어, 하나하나 펼쳐 보게 된다.
function tableCard(t, sc, hit = '', edit = null) {
  const detail = h('div.table-detail');
  let built = false;
  const matched = hit
    ? t.columns.filter((c) => c.name.toLowerCase().includes(hit)).map((c) => c.name)
    : [];

  const el = h('details.table-card', {
    ontoggle: (e) => {
      if (e.target.open && !built) {
        built = true;
        mount(detail, ...tableDetail(t, sc, edit));
      }
    },
  },
    h('summary.table-summary', {},
      h('div.table-name', {},
        icon('database', 14),
        h('strong', {}, t.namespace ? `${t.namespace}.${t.name}` : t.name),
        t.primaryKey ? null : badge('PK 없음', 'warn'),
      ),
      h('div.table-badges', {},
        matched.length
          ? h('span.col-hit', { title: matched.join(', ') },
              icon('list', 11),
              matched.length <= 2 ? matched.join(', ') : `${matched[0]} 외 ${matched.length - 1}`)
          : null,
        h('span.muted', {}, `${t.columns.length}컬럼`),
        t.indexes.length ? h('span.muted', {}, `${t.indexes.length}인덱스`) : null,
        t.foreignKeys.length ? h('span.muted', {}, `${t.foreignKeys.length}FK`) : null,
        t.rowEstimate > 0 ? badge(`~${formatCount(t.rowEstimate)}행`, 'neutral') : null,
        t.sizeBytes > 0 ? badge(formatBytes(t.sizeBytes), 'neutral') : null,
      ),
    ),
    detail,
  );
  return el;
}

function tableDetail(t, sc, edit = null) {
  const parts = [];
  if (edit) {
    parts.push(h('div.table-comment-edit', {},
      h('span.field-label', {}, '테이블 설명'),
      edit.field(t, '', t.comment ?? '')));
  } else if (t.comment) {
    parts.push(h('p.table-comment', {}, t.comment));
  }

  // 저장 설정(엔진·문자셋·테이블스페이스). 실제 DB 에서 읽은 값이라 여기서는
  // 보기만 한다 — 바꾸는 것은 마이그레이션이 할 일이고, 이 화면에서 조용히
  // ALTER TABLE 이 나가면 그것이 이력에 남지 않는다.
  const opts = optionChips(sc.dialect, t.options);
  if (opts) parts.push(opts);

  const pkCols = new Set((t.primaryKey?.columns ?? []).map((c) => c.toLowerCase()));
  const fkCols = new Map();
  for (const fk of t.foreignKeys) {
    for (const col of fk.columns) {
      fkCols.set(col.toLowerCase(), fk);
    }
  }

  parts.push(h('table.table.column-table', {},
    h('thead', {}, h('tr', {},
      h('th.col-key', {}, ''),
      h('th', {}, '컬럼'),
      h('th', {}, '타입'),
      h('th', {}, 'NULL'),
      h('th', {}, '기본값'),
      h('th', {}, '설명'),
    )),
    h('tbody', {}, t.columns.map((c) => {
      const isPK = pkCols.has(c.name.toLowerCase());
      const fk = fkCols.get(c.name.toLowerCase());
      return h('tr', {},
        h('td.col-key', {},
          isPK ? h('span.key-mark.key-pk', { title: '기본키' }, 'PK') : null,
          fk ? h('span.key-mark.key-fk', {
            title: `${fk.refTable}.${(fk.refColumns ?? []).join(',')} 참조`,
          }, 'FK') : null,
        ),
        h('td', {},
          h('div.cell-main', {},
            h('code', {}, c.name),
            c.identity ? badge('AI', 'info') : null,
            c.generated ? badge('생성', 'accent') : null,
          ),
        ),
        h('td', {}, h('code.type-cell', { title: c.rawType || '' }, typeLabel(c))),
        h('td', {}, c.nullable ? h('span.muted', {}, 'NULL') : h('strong', {}, 'NOT NULL')),
        h('td', {}, c.hasDefault ? h('code.default-cell', {}, c.default) : h('span.muted', {}, '—')),
        h('td.detail-cell', {}, edit
          ? edit.field(t, c.name, c.comment ?? '')
          : (c.comment || (c.presence !== undefined && c.presence < 1
            ? `샘플 중 ${Math.round(c.presence * 100)}% 문서에 존재`
            : ''))),
      );
    })),
  ));

  if (t.primaryKey) {
    parts.push(constraintRow('기본키', [
      h('code', {}, t.primaryKey.columns.join(', ')),
      t.primaryKey.name ? h('span.muted', {}, t.primaryKey.name) : null,
    ]));
  }

  if (t.indexes.length) {
    parts.push(h('div.constraint-block', {},
      h('h4', {}, '인덱스'),
      h('ul.constraint-list', {}, t.indexes.map((idx) => h('li', {},
        h('code', {}, idx.name),
        idx.unique ? badge('UNIQUE', 'info') : null,
        h('span', {}, `(${idx.columns.map(partLabel).join(', ')})`),
        idx.type ? h('span.muted', {}, idx.type) : null,
        idx.where ? h('span.muted', {}, `WHERE ${idx.where}`) : null,
      ))),
    ));
  }

  if (t.foreignKeys.length) {
    parts.push(h('div.constraint-block', {},
      h('h4', {}, '외래키'),
      h('ul.constraint-list', {}, t.foreignKeys.map((fk) => h('li', {},
        h('code', {}, fk.name),
        h('span', {}, `(${fk.columns.join(', ')}) → `),
        h('code', {}, `${fk.refNamespace ? `${fk.refNamespace}.` : ''}${fk.refTable}(${(fk.refColumns ?? []).join(', ')})`),
        fk.onDelete && fk.onDelete !== 'NO ACTION' ? badge(`ON DELETE ${fk.onDelete}`, 'warn') : null,
        fk.onUpdate && fk.onUpdate !== 'NO ACTION' ? badge(`ON UPDATE ${fk.onUpdate}`, 'warn') : null,
      ))),
    ));
  }

  if (t.checks.length) {
    parts.push(h('div.constraint-block', {},
      h('h4', {}, '체크 제약'),
      h('ul.constraint-list', {}, t.checks.map((ck) => h('li', {},
        h('code', {}, ck.name), h('span.expr', {}, ck.expression),
      ))),
    ));
  }

  const optKeys = Object.keys(t.options ?? {});
  if (optKeys.length) {
    parts.push(h('div.constraint-block', {},
      h('h4', {}, '옵션'),
      h('div.tag-row', {}, optKeys.map((k) => badge(`${k}=${t.options[k]}`, 'neutral'))),
    ));
  }

  void sc;
  return parts;
}

function constraintRow(label, children) {
  return h('div.constraint-block', {}, h('h4', {}, label), h('div.constraint-inline', {}, children));
}

function partLabel(p) {
  const name = p.expression || p.column;
  return p.descending ? `${name} DESC` : name;
}

function typeLabel(c) {
  const t = c.type ?? {};
  let s = t.base ?? '?';
  if (t.length) s += `(${t.length})`;
  else if (t.precision) s += `(${t.precision}${t.scale ? `,${t.scale}` : ''})`;
  if (t.enumName) s += `:${t.enumName}`;
  else if (t.values?.length) s += `(${t.values.join('|')})`;
  if (t.element) s += `<${t.element.base}>`;
  if (t.unsigned) s += ' unsigned';
  return s;
}

// ---------- 비교 ----------

async function runCompare(body, fromConn, toConnId, usable) {
  const toConn = usable.find((i) => i.connection.id === toConnId)?.connection;
  mount(body, spinner('두 스키마를 읽고 비교하는 중…'));
  try {
    const res = await api.post(`/connections/${fromConn.id}/schema/diff`, {
      targetConnectionId: toConnId,
    });
    mount(body, ...compareView(res, fromConn, toConn));
  } catch (err) {
    mount(body, errorPanel(err));
    toastError(err);
  }
}

function compareView(res, fromConn, toConn) {
  const parts = [];
  const { diff, plan } = res;

  parts.push(h('div.card.compare-head', {},
    h('div.compare-sides', {},
      h('div.compare-side', {},
        h('span.compare-label', {}, '현재 (변경 대상)'),
        h('strong', {}, res.from.connection.name),
        envBadge(res.from.connection.environment),
        h('span.muted', {}, `${res.from.stats.tables}테이블 · ${res.from.stats.columns}컬럼`),
      ),
      h('div.compare-arrow', {}, '→'),
      h('div.compare-side', {},
        h('span.compare-label', {}, '목표'),
        h('strong', {}, res.to.connection.name),
        envBadge(res.to.connection.environment),
        h('span.muted', {}, `${res.to.stats.tables}테이블 · ${res.to.stats.columns}컬럼`),
      ),
    ),
    h('div.compare-counts', {},
      badge(`변경 ${diff.changes.length}건`, diff.changes.length ? 'info' : 'success'),
      diff.destructiveCount
        ? badge(`파괴적 ${diff.destructiveCount}건`, 'danger')
        : null,
    ),
  ));

  if (plan.warnings?.length) {
    parts.push(h('div.card.notice.notice-warn', {},
      icon('alert'),
      h('div', {}, h('strong', {}, '경고'),
        h('ul.note-list', {}, plan.warnings.map((w) => h('li', {}, w)))),
    ));
  }
  if (plan.irreversible?.length) {
    parts.push(h('div.card.notice.notice-danger', {},
      icon('alert'),
      h('div', {}, h('strong', {}, '롤백할 수 없는 변경'),
        h('ul.note-list', {}, plan.irreversible.map((w) => h('li', {}, w)))),
    ));
  }

  if (!diff.changes.length) {
    parts.push(h('div.card.empty', {}, icon('check', 28),
      h('h3', {}, '두 스키마가 동일합니다'),
      h('p.muted', {}, '구조 차이가 없습니다.')));
    return parts;
  }

  parts.push(h('section.card', {},
    h('h2.card-title', {}, '변경 목록'),
    h('table.table.change-table', {},
      h('thead', {}, h('tr', {},
        h('th', {}, '종류'), h('th', {}, '대상'), h('th', {}, '내용'), h('th', {}, ''),
      )),
      h('tbody', {}, diff.changes.map((c) => h('tr', { class: c.destructive ? 'row-danger' : '' },
        h('td', {}, badge(changeKindLabel(c.kind), changeKindTone(c.kind))),
        h('td', {}, c.table ? h('code', {}, c.table) : h('span.muted', {}, '—'),
          c.object ? h('div.cell-sub', {}, c.object) : null),
        h('td', {}, c.summary,
          c.lossyDetail ? h('div.lossy-detail', {}, icon('alert', 12), c.lossyDetail) : null),
        h('td', {}, c.destructive ? badge('파괴적', 'danger') : null),
      ))),
    ),
  ));

  parts.push(sqlPanel('적용 SQL (up)', plan.up, plan.dialect));
  if (plan.down?.length) {
    parts.push(sqlPanel('롤백 SQL (down)', plan.down, plan.dialect));
  }
  return parts;
}

function sqlPanel(title, statements, dialect) {
  const sqlText = statements.map((s) => {
    const note = s.note ? `-- ${s.note}\n` : '';
    return note + s.sql + (s.sql.trim().endsWith(';') ? '' : ';');
  }).join('\n');

  return h('section.card', {},
    h('div.panel-head', {},
      h('h2.card-title', {}, title, badge(`${statements.length}문장`, 'neutral'), badge(dialect, 'info')),
      h('button.btn.btn-small', {
        type: 'button', onclick: () => copyToClipboard(sqlText),
      }, icon('copy'), '복사'),
    ),
    h('pre.sql-block', {}, ...statements.map((s) => h('div', {
      class: s.destructive ? 'sql-line sql-danger' : 'sql-line',
    },
      s.note ? h('span.sql-comment', {}, `-- ${s.note}\n`) : null,
      s.sql + (s.sql.trim().endsWith(';') ? '' : ';'),
    ))),
  );
}

// ---------- DDL 생성 ----------

async function showDDL(conn) {
  const dialectSelect = select(
    (state.meta?.dbKinds ?? [])
      .filter((k) => k.capabilities?.migrate)
      .map((k) => ({ value: k.kind, label: k.label })),
    { value: conn.kind },
  );
  const body = h('div.ddl-body');

  const load = async () => {
    mount(body, spinner('DDL을 생성하는 중…'));
    try {
      const res = await api.get(
        `/connections/${conn.id}/schema/ddl?dialect=${encodeURIComponent(dialectSelect.value)}`);
      const parts = [];
      if (res.plan.warnings?.length) {
        parts.push(h('div.notice.notice-warn', {}, icon('alert'),
          h('ul.note-list', {}, res.plan.warnings.map((w) => h('li', {}, w)))));
      }
      parts.push(h('div.panel-head', {},
        h('span.muted', {}, `${res.plan.up.length}개 문장`),
        h('button.btn.btn-small', {
          type: 'button', onclick: () => copyToClipboard(res.upSql),
        }, icon('copy'), '전체 복사'),
      ));
      parts.push(h('pre.sql-block.sql-scroll', {}, res.upSql));
      mount(body, ...parts);
    } catch (err) {
      mount(body, errorPanel(err));
    }
  };
  dialectSelect.addEventListener('change', load);

  openModal({
    title: `DDL 생성 — ${conn.name}`,
    width: 860,
    body: () => [
      h('label.field.field-inline', {},
        h('span.field-label', {}, '대상 DB 종류'), dialectSelect,
        h('span.field-help', {}, '다른 종류를 선택하면 타입을 변환해 생성합니다')),
      body,
    ],
    footer: (close) => [h('button.btn', { type: 'button', onclick: close }, '닫기')],
  });
  await load();
}

// ---------- 라벨 ----------

const CHANGE_LABELS = {
  create_table: '테이블 생성', drop_table: '테이블 삭제', alter_table_comment: '주석 변경',
  alter_table_options: '저장 설정 변경',
  add_column: '컬럼 추가', drop_column: '컬럼 삭제', alter_column: '컬럼 변경',
  add_primary_key: 'PK 추가', drop_primary_key: 'PK 삭제',
  add_index: '인덱스 추가', drop_index: '인덱스 삭제',
  add_foreign_key: 'FK 추가', drop_foreign_key: 'FK 삭제',
  add_check: '체크 추가', drop_check: '체크 삭제',
  create_enum: 'Enum 생성', drop_enum: 'Enum 삭제', alter_enum: 'Enum 변경',
  create_view: '뷰 생성', drop_view: '뷰 삭제', replace_view: '뷰 변경',
};

function changeKindLabel(kind) {
  return CHANGE_LABELS[kind] ?? kind;
}

function changeKindTone(kind) {
  if (kind.startsWith('drop')) return 'danger';
  if (kind.startsWith('create') || kind.startsWith('add')) return 'success';
  return 'warn';
}

function formatCount(n) {
  if (!n) return '0';
  if (n >= 1e8) return `${(n / 1e8).toFixed(1)}억`;
  if (n >= 1e4) return `${(n / 1e4).toFixed(1)}만`;
  return n.toLocaleString('ko-KR');
}

function formatBytes(n) {
  if (!n) return '—';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)}${units[i]}`;
}
