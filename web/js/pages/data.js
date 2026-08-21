// 데이터 브라우저 — 테이블/컬렉션/키의 값을 조회하고 고친다.
//
// 스키마 화면과 나란히 두지 않고 별도 메뉴로 만든 이유: 권한이 다르다.
// 스키마는 모니터링 등급이면 볼 수 있지만 데이터는 별도의 능력(data.read)이 필요하고,
// 두 화면이 한 곳에 있으면 "여기까지는 보이는데 저기부터 안 보이는" 이유를
// 사용자가 짐작해야 한다.
import { api } from '../core/api.js';
import { state, kindLabel } from '../core/store.js';
import {
  h, mount, icon, select, input, textarea, spinner, emptyState, pageHeader,
  badge, envBadge, toast, toastError, openModal, confirmDialog, copyToClipboard,
} from '../core/ui.js';
import { navigate } from '../core/router.js';
import { dbLogo } from '../core/dblogo.js';
import { serverDbPicker } from '../core/connpick.js';
import { codeBlock } from '../core/highlight.js';
import { errorPanel } from './users.js';

// 한 페이지 행 수. 서버 상한(500)보다 작게 두는 이유는 화면에 500행을 그리면
// 스크롤이 의미를 잃기 때문이다. 필요한 사람은 선택지에서 올릴 수 있다.
const PAGE_SIZES = [25, 50, 100, 200];

// columnMarks는 컬럼의 성질을 이름 옆 작은 기호로 표시한다.
//
// 배지(알약 모양)를 쓰지 않는 이유: 배지는 테두리와 안쪽 여백까지 21px를 차지해
// 컬럼 이름 줄보다 높다. 컬럼마다 그것이 붙으면 머리글 행 전체가 그만큼 두꺼워지고,
// 정작 읽어야 할 이름과 타입은 그대로인데 표만 커진다. 기호는 글자 높이 안에 들어간다.
//
// fk는 지금 이 화면의 응답에 없다(목록 조회는 기본키만 확인한다). 값이 생기면
// 여기서 함께 그려지도록 미리 자리를 만들어 둔다.
function columnMarks(col) {
  const marks = [];
  if (col.pk) {
    marks.push(h('span.col-mark.col-mark-pk', {
      title: '기본키(PK)', 'aria-label': '기본키', role: 'img',
    }, icon('key', 11)));
  }
  if (col.fk) {
    // 서버는 {namespace, table, column} 을 준다. 예전 형태(문자열)도 그대로 읽는다.
    const target = typeof col.fk === 'string' ? col.fk : fkTarget(col.fk);
    const label = target ? `외래키(FK) → ${target}` : '외래키(FK)';
    marks.push(h('span.col-mark.col-mark-fk', {
      title: label, 'aria-label': label, role: 'img',
    }, icon('link', 11)));
  }
  // NOT NULL은 표시하지 않는다. 대부분의 컬럼이 해당되어 거의 모든 이름 옆에
  // 기호가 붙고, 그러면 기호가 아무것도 구분해 주지 못한다.
  return marks;
}

// fkTarget은 "스키마.표.컬럼" 형태의 사람이 읽는 이름이다.
export function fkTarget(fk) {
  if (!fk?.table) return '';
  const table = fk.namespace ? `${fk.namespace}.${fk.table}` : fk.table;
  return fk.column ? `${table}.${fk.column}` : table;
}

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

// prettyJSON은 JSON처럼 보이는 값을 줄 맞춰 돌려준다. 아니면 null.
//
// 목록 칸에서는 한 줄로 뭉개져 '...'로 끝나던 값이다. 상세에서까지 한 줄이면
// 열어 본 보람이 없다.
function prettyJSON(text) {
  const t = text.trim();
  if (!(t.startsWith('{') && t.endsWith('}')) && !(t.startsWith('[') && t.endsWith(']'))) {
    return null;
  }
  try {
    return JSON.stringify(JSON.parse(t), null, 2);
  } catch {
    return null;
  }
}

// viewFields는 행 하나를 읽기 전용으로 펼친다.
// 목록에서 잘려 보이던 값을 전부 보여주고, 값마다 복사 버튼을 붙인다.
function viewFields(columns, values, opts = {}) {
  const { onFollow, followed = '' } = opts;
  return columns.map((col) => {
    const raw = values[col.name];
    const isNull = raw === null || raw === undefined;
    const text = isNull ? '' : String(raw);
    const json = isNull ? null : prettyJSON(text);

    let valueNode;
    if (isNull) {
      valueNode = h('span.cell-null', {}, 'NULL');
    } else if (json) {
      valueNode = codeBlock(json, 'json', { className: 'value-block' });
    } else if (UUID_RE.test(text)) {
      // UUID는 글자가 아니라 식별자다. 다른 값과 같은 모양으로 두면
      // 옮겨 적을 때 한 글자 틀린 것을 눈으로 잡을 수 없다.
      valueNode = h('code.value-uuid', { title: 'UUID' }, text);
    } else {
      valueNode = h('pre.value-block', {}, text);
    }

    // 외래키는 값을 보여주는 것으로 끝나지 않는다. 481이라는 숫자가 누구인지
    // 알려면 지금까지는 표를 옮겨 다녀야 했다. 옆에 펼쳐 보여주면 그 왕복이 없다.
    const canFollow = Boolean(onFollow) && Boolean(col.fk) && !isNull;
    const followBtn = canFollow
      ? h('button.link-btn.fk-follow', {
        type: 'button',
        title: `${fkTarget(col.fk)} 의 행을 옆에 펼칩니다`,
        onclick: () => onFollow(col, text),
      }, icon(followed === col.name ? 'chevron-left' : 'chevron-right', 12),
      followed === col.name ? '접기' : '따라가기')
      : null;

    return h(`div.field.row-field.row-view-field${followed === col.name ? '.is-followed' : ''}`, {},
      h('span.field-label', {},
        h('span.col-name', {}, col.name),
        ...columnMarks(col),
        h('span.col-type', {}, col.type),
        h('span.row-field-actions', {},
          followBtn,
          isNull ? null : h('button.link-btn', {
            type: 'button',
            onclick: () => copyToClipboard(text),
          }, '복사'),
        ),
      ),
      valueNode,
    );
  });
}

export async function renderData(outlet, params, query) {
  mount(outlet, spinner('커넥션 목록을 불러오는 중…'));

  let conns;
  let servers;
  try {
    // 서버 목록도 함께 받는다. 커넥션 하나하나를 평평하게 늘어놓으면 같은 서버의
    // DB가 이름만 다른 항목으로 반복되어, 목록이 길어질수록 "어느 서버의 것인가"가
    // 사라진다. 서버를 먼저 고르고 그 안에서 DB를 고르는 편이 실제 순서와 같다.
    [conns, servers] = await Promise.all([
      api.get('/connections/'),
      api.get('/servers/'),
    ]);
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  // 데이터 조회 능력이 있고, 그 종류가 데이터 조회를 지원하는 커넥션만 남긴다.
  const usable = conns.items.filter((i) =>
    i.accessible && (i.caps ?? []).includes('data.read') && i.dataCaps?.browse);

  if (usable.length === 0) {
    mount(outlet,
      pageHeader('데이터', '테이블·문서·키 값 조회와 수정'),
      emptyState(
        '데이터를 조회할 수 있는 커넥션이 없습니다. 슈퍼 어드민에게 "데이터 조회" 권한을 요청하세요.',
        h('a.btn', { href: '/connections' }, 'DB 커넥션으로')),
    );
    return;
  }

  const selectedId = query.get('conn') || usable[0].connection.id;
  const current = usable.find((i) => i.connection.id === selectedId) ?? usable[0];
  const conn = current.connection;
  const canWrite = (current.caps ?? []).includes('data.write');
  const canSQL = (current.caps ?? []).includes('sql.run');
  const support = current.dataCaps ?? {};

  // 서버 → DB 두 단계 선택.
  const picker = serverDbPicker({
    usable,
    servers: servers.items ?? [],
    currentId: conn.id,
    onPick: (id) => navigate(`/data?conn=${encodeURIComponent(id)}`),
  });

  const objectBox = h('div.data-objects');
  const tableBox = h('div.data-table-wrap');

  // 조회 상태. 화면 하나가 이 객체만 보고 그려지므로, 어디서 무엇이 바뀌었는지
  // 추적할 곳이 한 군데다.
  const view = {
    object: null,
    offset: 0,
    limit: 50,
    orderBy: '',
    desc: false,
    search: '',
    filters: [],
    page: null,
    // selected는 체크한 행의 키 문자열이다. 행 자체가 아니라 키를 담는 이유:
    // 페이지를 다시 읽으면 행 객체는 새로 만들어지지만 키는 그대로다.
    selected: new Set(),
    // comments는 컬럼 이름 → 스키마에 적힌 설명이다. 조회와 별개로 뒤늦게 채워진다.
    comments: {},
  };
  // 마지막으로 성공한 조회. 실패했을 때 되돌아갈 자리다.
  // 대상이 바뀌면 의미가 없어지므로 selectObject에서 비운다.
  let lastOk = null;

  // staged는 아직 적용하지 않은 변경이다.
  //
  // 화면에서 고친 것을 바로 DB에 보내지 않는 이유: 데이터 수정은 되돌릴 수 없는데,
  // 한 줄씩 저장하면 "무엇을 바꾸고 있는지"를 전체로 볼 기회가 없다. 모아 두었다가
  // 실행될 문장을 확인하고 한 번에 적용하면, 잘못 누른 것을 적용 전에 되돌릴 수 있다.
  //
  // 미리보기를 지원하지 않는 종류(Mongo·Redis)에서는 이 상자를 쓰지 않고
  // 지금까지처럼 바로 반영한다 — 보여줄 문장이 없는데 확인 단계만 늘리면 번거롭기만 하다.
  const staged = [];
  const canStage = () => Boolean(support.preview);

  mount(outlet,
    pageHeader('데이터', `${conn.name} 의 값을 조회하고 수정합니다`, [
      canSQL
        ? h('a.btn', { href: `/sql?conn=${encodeURIComponent(conn.id)}` }, icon('play'), 'SQL 콘솔')
        : null,
      h('button.btn', { type: 'button', onclick: () => loadObjects() }, icon('refresh'), '목록 새로고침'),
    ]),
    h('div.card.filter-bar', {},
      ...picker.nodes,
      envBadge(conn.environment),
      canWrite ? badge('수정 가능', 'warn') : badge('읽기 전용', 'neutral'),
      h('div.filter-sep'),
      // 데이터를 고치는 화면이라 "지금 어느 DB인가"를 잘못 보면 손해가 크다.
      // 종류만은 글자보다 로고가 먼저 눈에 들어온다.
      h('span.muted.small', {}, dbLogo(conn.kind, 14, { inline: true }), kindLabel(conn.kind)),
    ),
    h('div.data-layout', {}, objectBox, tableBox),
  );

  async function loadObjects() {
    mount(objectBox, spinner('목록을 읽는 중…'));
    let res;
    try {
      res = await api.get(`/connections/${conn.id}/data/objects`);
    } catch (err) {
      mount(objectBox, errorPanel(err));
      return;
    }
    if (res.objects.length === 0) {
      mount(objectBox, emptyState('조회할 수 있는 대상이 없습니다'));
      return;
    }

    const filterInput = input({ placeholder: '이름으로 거르기', autocomplete: 'off' });
    const list = h('div.object-list');
    const draw = () => {
      const needle = filterInput.value.trim().toLowerCase();
      const items = res.objects.filter((o) =>
        !needle || o.name.toLowerCase().includes(needle) ||
        (o.namespace ?? '').toLowerCase().includes(needle));
      mount(list, items.length === 0
        ? h('p.muted.small', {}, '일치하는 대상이 없습니다')
        : items.map((o) => objectButton(o)));
    };
    filterInput.addEventListener('input', draw);

    mount(objectBox,
      h('div.card', {},
        h('div.card-title', {}, `대상 ${res.objects.length}개`),
        filterInput,
        list,
      ),
    );
    draw();

    // 첫 대상을 자동으로 연다. 빈 화면에서 무엇을 눌러야 하는지 설명하는 것보다
    // 하나를 열어 보여주는 편이 빠르다.
    const target = query.get('table');
    const initial = res.objects.find((o) => o.name === target) ?? res.objects[0];
    if (initial) selectObject(initial);
  }

  function objectButton(o) {
    const btn = h('button.object-item', {
      type: 'button',
      dataset: { name: o.name, namespace: o.namespace ?? '' },
      onclick: () => selectObject(o),
    },
      h('span.object-name', {}, o.name),
      h('span.object-meta', {},
        o.kind === 'view' ? badge('뷰', 'neutral') : null,
        o.rowCount >= 0 ? h('span.muted.small', {}, `~${o.rowCount.toLocaleString()}`) : null,
      ),
    );
    if (view.object && view.object.name === o.name && (view.object.namespace ?? '') === (o.namespace ?? '')) {
      btn.classList.add('is-on');
    }
    return btn;
  }

  async function selectObject(o) {
    // 적용하지 않은 변경을 두고 다른 표로 옮기면 그 변경은 갈 곳이 없어진다.
    // 조용히 버리지 않고 묻는다 — 몇 건인지 알려 주는 것이 판단의 근거다.
    if (staged.length > 0) {
      const ok = await confirmDialog({
        title: '적용하지 않은 변경이 있습니다',
        message: `${view.object?.name ?? ''} 에 적용하지 않은 변경 ${staged.length}건이 있습니다. 버리고 다른 대상으로 이동할까요?`,
        confirmLabel: '버리고 이동', danger: true,
      });
      if (!ok) return;
      staged.length = 0;
    }
    view.object = o;
    lastOk = null; // 다른 대상의 결과로 되돌아가면 머리글과 행이 어긋난다
    view.offset = 0;
    view.orderBy = '';
    view.desc = false;
    view.search = '';
    view.filters = [];
    view.selected.clear();
    view.comments = {};
    for (const el of objectBox.querySelectorAll('.object-item')) {
      el.classList.toggle('is-on',
        el.dataset.name === o.name && el.dataset.namespace === (o.namespace ?? ''));
    }
    loadRows(true);
  }

  async function loadRows(withTotal = false) {
    if (!view.object) return;
    mount(tableBox, spinner(`${view.object.name} 조회 중…`));
    try {
      const res = await api.post(`/connections/${conn.id}/data/query`, {
        namespace: view.object.namespace ?? '',
        table: view.object.name,
        limit: view.limit,
        offset: view.offset,
        orderBy: view.orderBy,
        desc: view.desc,
        search: view.search,
        filters: view.filters,
        withTotal,
      });
      view.page = res.page;
      // 성공한 조건을 통째로 기억해 둔다. 다음 조회가 실패했을 때 여기로 되돌린다.
      lastOk = {
        object: view.object,
        offset: view.offset,
        limit: view.limit,
        orderBy: view.orderBy,
        desc: view.desc,
        search: view.search,
        filters: view.filters.map((f) => ({ ...f })),
        page: res.page,
      };
      drawTable();
      loadComments();
    } catch (err) {
      mount(tableBox, queryError(err));
    }
  }

  // loadComments는 컬럼 설명을 뒤늦게 채운다.
  //
  // 조회와 함께 받지 않는 이유: 값 조회는 드라이버가 주는 컬럼 정보만 쓰는데
  // 거기에는 주석이 없다. 주석은 스키마 introspection에서 나오고 그쪽은 느리다.
  // 그래서 표를 먼저 그리고, 설명은 도착하는 대로 붙인다 — 없어도 표는 읽힌다.
  async function loadComments() {
    const target = view.object;
    if (!target || target.kind === 'view') return;
    try {
      const res = await api.get(
        `/connections/${conn.id}/schema?table=${encodeURIComponent(target.name)}`);
      // 그 사이 다른 대상을 골랐으면 버린다.
      if (view.object !== target) return;
      const map = {};
      for (const col of res.table?.columns ?? []) {
        if (col.comment) map[col.name] = col.comment;
      }
      if (res.table?.comment) map[''] = res.table.comment;
      view.comments = map;
    } catch {
      // 설명을 못 읽는 것은 화면을 막을 이유가 아니다(권한·종류에 따라 없을 수 있다).
    }
  }

  // queryError는 실패를 보여주되 빠져나갈 길을 함께 준다.
  //
  // 조회가 실패하면 표 자리를 오류가 통째로 차지한다. 그런데 실패를 만든 것은
  // 대개 방금 친 검색어나 조건이고, 그것을 지울 입력칸도 함께 사라진다 —
  // 새로고침 말고는 되돌릴 방법이 없어진다.
  function queryError(err) {
    const actions = [];
    if (lastOk && lastOk.object === view.object) {
      actions.push(h('button.btn.btn-small', {
        type: 'button',
        onclick: () => {
          view.offset = lastOk.offset;
          view.limit = lastOk.limit;
          view.orderBy = lastOk.orderBy;
          view.desc = lastOk.desc;
          view.search = lastOk.search;
          view.filters = lastOk.filters.map((f) => ({ ...f }));
          view.page = lastOk.page;
          // 서버에 다시 묻지 않는다. 직전 결과를 그대로 다시 그리는 것이므로
          // 같은 실패를 되풀이할 여지가 없다.
          drawTable();
        },
      }, icon('refresh'), '이전 결과로 돌아가기'));
    }
    if (view.search || view.filters.length) {
      actions.push(h('button.btn.btn-small', {
        type: 'button',
        onclick: () => {
          view.search = '';
          view.filters = [];
          view.offset = 0;
          loadRows(true);
        },
      }, icon('x'), '검색·조건 지우고 다시 조회'));
    }
    actions.push(h('button.btn.btn-small', {
      type: 'button', onclick: () => loadRows(true),
    }, icon('play'), '다시 시도'));
    return errorPanel(err, actions);
  }

  function drawTable() {
    const page = view.page;
    const editable = canWrite && page.editable && support.mutate && view.object.kind !== 'view';

    const searchInput = input({
      value: view.search, placeholder: '모든 컬럼에서 검색', autocomplete: 'off',
    });
    searchInput.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') {
        view.search = searchInput.value.trim();
        view.offset = 0;
        loadRows(true);
      }
    });

    const sizeSelect = select(PAGE_SIZES.map((n) => ({ value: String(n), label: `${n}행` })),
      { value: String(view.limit) });
    sizeSelect.addEventListener('change', () => {
      view.limit = Number(sizeSelect.value);
      view.offset = 0;
      loadRows(false);
    });

    const toolbar = h('div.card.filter-bar', {},
      h('div.data-title', {},
        h('b', {}, view.object.name),
        view.object.namespace ? h('span.muted.small', {}, view.object.namespace) : null,
      ),
      support.filter !== false ? searchInput : null,
      support.filter !== false
        ? h('button.btn.btn-small', { type: 'button', onclick: () => openFilterDialog() },
            icon('list'), `조건 ${view.filters.length || ''}`)
        : null,
      h('div.filter-sep'),
      sizeSelect,
      editable
        ? h('button.btn.btn-small.btn-primary', { type: 'button', onclick: () => openRowEditor(null) },
            icon('plus'), '행 추가')
        : null,
    );

    const notices = [];
    if (!page.editable && page.reason) {
      notices.push(h('p.notice.notice-info', {}, icon('alert'), page.reason));
    }
    if (canWrite && !editable && page.editable) {
      notices.push(h('p.notice.notice-info', {}, icon('alert'), '뷰는 수정할 수 없습니다'));
    }
    for (const note of page.notes ?? []) {
      notices.push(h('p.notice.notice-warn', {}, icon('alert'), note));
    }

    // 잘린 셀 위치를 빠르게 찾기 위한 집합. 목록은 표시용으로 값을 자르므로
    // 그 사실을 셀에 표시해야 한다 — 잘린 값을 전체 값으로 착각하면 안 된다.
    const cut = new Set((page.truncated ?? []).map(([r, c]) => `${r}:${c}`));

    // 선택 칸은 수정 권한이 있을 때만 그린다. 고를 수 있는데 할 수 있는 일이 없으면
    // 체크박스는 자리만 차지한다.
    const pageKeys = editable
      ? page.rows.map((row) => rowKeyID(page, row)).filter(Boolean)
      : [];
    const allChecked = pageKeys.length > 0 && pageKeys.every((k) => view.selected.has(k));
    const headBox = h('input', {
      type: 'checkbox', checked: allChecked, title: '이 페이지 전체 선택',
      disabled: pageKeys.length === 0,
      onchange: (e) => {
        if (e.target.checked) pageKeys.forEach((k) => view.selected.add(k));
        else pageKeys.forEach((k) => view.selected.delete(k));
        drawTable();
      },
    });

    const head = h('tr', {},
      editable ? h('th.col-check', {}, headBox) : null,
      ...page.columns.map((col) => {
        const active = view.orderBy === col.name;
        const sortable = support.sort !== false;
        const colHead = h('span.col-head', {},
          h('span.col-name', {}, col.name), ...columnMarks(col));
        const th = h('th', { class: active ? 'is-sorted' : '' },
          colHead,
          h('span.col-type', {}, col.type),
        );
        th.dataset.col = col.name;
        if (sortable) {
          th.classList.add('sortable');
          // 정렬은 예전부터 있었지만 눌러 보기 전에는 알 수 없었다.
          // 무엇이 일어날지 미리 알려 준다.
          th.title = active
            ? `${col.name} 기준 ${view.desc ? '내림차순' : '오름차순'} — 누르면 방향이 바뀝니다`
            : `${col.name} 기준으로 정렬`;
          th.addEventListener('click', () => {
            if (view.orderBy === col.name) view.desc = !view.desc;
            else { view.orderBy = col.name; view.desc = false; }
            view.offset = 0;
            loadRows(false);
          });
          // 방향은 화살표 글자로 표시한다. 아이콘 집합에 정렬용 화살표가 없고,
          // 이 한 글자를 위해 SVG를 늘릴 이유가 없다.
          // 이름 줄 안에 넣는다 — 머리글 아래에 붙이면 줄이 하나 더 생긴다.
          colHead.appendChild(h('span.sort-arrow', {},
            active ? (view.desc ? '▼' : '▲') : '↕'));
          if (!active) colHead.lastChild.classList.add('is-hint');
        }
        return th;
      }),
    );

    // 행을 누르면 상세가 열린다. 예전에는 줄 왼쪽에 아이콘 세 개가 있었는데,
    // 그 자리는 어느 표에서나 폭을 먹고 값이 오른쪽으로 밀린다. 상세·수정·삭제는
    // 모두 상세 모달 안에서 할 수 있으므로 줄 자체를 입구로 삼는다.
    const body = h('tbody', {}, page.rows.map((row, ri) => {
      const id = rowKeyID(page, row);
      const pending = id ? staged.find((s) => s.id === id) : null;
      const tr = h('tr', {
        class: `is-clickable${pending ? ` is-staged is-staged-${pending.action}` : ''}`,
        title: pending
          ? (pending.action === 'delete' ? '적용 대기: 삭제' : '적용 대기: 수정')
          : '누르면 상세를 봅니다',
        onclick: () => openRowEditor({ row, index: ri }),
      },
        editable
          ? h('td.col-check', {}, h('input', {
            type: 'checkbox',
            checked: id ? view.selected.has(id) : false,
            disabled: !id,
            title: id ? '선택' : '기본키가 없어 고를 수 없습니다',
            // 체크는 행을 여는 동작과 다른 일이다. 여기서 막지 않으면
            // 고르려고 누를 때마다 상세가 함께 열린다.
            onclick: (e) => e.stopPropagation(),
            onchange: (e) => {
              if (e.target.checked) view.selected.add(id);
              else view.selected.delete(id);
              drawTable();
            },
          }))
          : null,
        ...row.map((value, ci) => cell(value, cut.has(`${ri}:${ci}`), page.columns[ci])),
      );
      return tr;
    }));

    const from = page.offset + 1;
    const to = page.offset + page.rows.length;
    const pager = h('div.data-pager', {},
      h('button.btn.btn-small', {
        type: 'button', disabled: page.offset === 0,
        onclick: () => { view.offset = Math.max(0, view.offset - view.limit); loadRows(false); },
      }, '← 이전'),
      h('span.muted.small', {},
        page.rows.length === 0
          ? '결과 없음'
          : `${from.toLocaleString()}–${to.toLocaleString()}${page.total >= 0 ? ` / ${page.total.toLocaleString()}` : ''}`),
      h('button.btn.btn-small', {
        type: 'button', disabled: !page.hasMore,
        onclick: () => { view.offset += view.limit; loadRows(false); },
      }, '다음 →'),
      page.total < 0
        ? h('button.link-btn', { type: 'button', onclick: () => loadRows(true) }, '전체 개수 세기')
        : null,
      h('span.muted.small', {}, `${page.elapsedMs.toFixed(1)}ms`),
    );

    const table = h('table.table.data-table', {}, h('thead', {}, head), body);
    bindColumnPopover(table);
    colPop.hidden = true;

    mount(tableBox,
      toolbar,
      stageBar(),
      selectionBar(),
      ...notices,
      view.filters.length ? filterChips() : null,
      page.rows.length === 0
        ? emptyState('조건에 맞는 데이터가 없습니다')
        : h('div.card.table-scroll', {}, table),
      pager,
    );
  }

  // stageBar는 적용을 기다리는 변경을 표 위에 붙인다.
  //
  // 표 위에 두는 이유: 이것은 "지금 화면이 DB와 다르다"는 경고다. 아래에 두면
  // 긴 표에서는 스크롤해야 보이고, 그러면 모아 둔 것을 잊은 채 화면을 떠나게 된다.
  function stageBar() {
    if (staged.length === 0) return null;
    const counts = { insert: 0, update: 0, delete: 0 };
    for (const s of staged) counts[s.action] += 1;
    const parts = [];
    if (counts.insert) parts.push(`추가 ${counts.insert}`);
    if (counts.update) parts.push(`수정 ${counts.update}`);
    if (counts.delete) parts.push(`삭제 ${counts.delete}`);

    return h('div.card.stage-bar', {},
      icon('alert'),
      h('b', {}, `적용 대기 ${staged.length}건`),
      h('span.muted.small', {}, parts.join(' · ')),
      h('div.filter-sep'),
      h('button.btn.btn-small', {
        type: 'button', onclick: () => { staged.length = 0; drawTable(); },
      }, icon('x'), '모두 취소'),
      h('button.btn.btn-small.btn-primary', {
        type: 'button', onclick: () => openApplyDialog(),
      }, icon('play'), '적용하기'),
    );
  }

  function selectionBar() {
    if (view.selected.size === 0) return null;
    return h('div.card.selection-bar', {},
      h('b', {}, `${view.selected.size}행 선택됨`),
      h('div.filter-sep'),
      h('button.btn.btn-small', {
        type: 'button', onclick: () => openBulkEditDialog(),
      }, icon('edit'), '선택 편집'),
      h('button.btn.btn-small.btn-danger-ghost', {
        type: 'button', onclick: () => stageSelectedDeletes(),
      }, icon('trash'), '선택 삭제'),
      h('button.btn.btn-small', {
        type: 'button', onclick: () => { view.selected.clear(); drawTable(); },
      }, '선택 해제'),
    );
  }

  function cell(value, truncated, col) {
    const td = value === null || value === undefined
      ? h('td.cell-null', {}, 'NULL')
      : h('td', {}, h('span.cell-value', {}, String(value)));

    const text = value === null || value === undefined ? '' : String(value);
    td.title = truncated
      ? '값이 길어 잘렸습니다. 상세에서 전체를 볼 수 있습니다'
      : text;
    // 컬럼 이름을 칸에 적어 두면 popover가 어느 컬럼인지 알 수 있다.
    // 위임 처리라서 칸마다 리스너를 달지 않는다.
    if (col) td.dataset.col = col.name;

    if (truncated) td.appendChild(h('span.cell-cut', {}, '…'));
    if (text) {
      td.addEventListener('dblclick', (e) => {
        e.stopPropagation();
        copyToClipboard(text);
      });
    }
    return td;
  }

  // ---------- 컬럼 설명 popover ----------
  //
  // 스키마에 적어 둔 컬럼 설명을 값 위에서 바로 보여준다. 브라우저 기본 툴팁(title)을
  // 쓰지 않는 이유: 뜨기까지 1초 가까이 걸리고, 줄바꿈과 강조를 쓸 수 없어 설명이
  // 길면 읽기 어렵다. 그리고 이 표의 칸은 이미 title로 "전체 값"을 보여주고 있다 —
  // 설명까지 같은 자리에 넣으면 둘이 섞인다.
  const colPop = h('div.col-pop', { hidden: true });
  document.body.appendChild(colPop);

  function bindColumnPopover(table) {
    const show = (target) => {
      const name = target.dataset.col;
      const note = name ? view.comments[name] : '';
      if (!note) {
        colPop.hidden = true;
        return;
      }
      mount(colPop, h('b', {}, name), h('p', {}, note));
      colPop.hidden = false;
      // 칸 아래에 붙이되 화면 밖으로 나가지 않게 한다.
      const r = target.getBoundingClientRect();
      const top = r.bottom + 6;
      colPop.style.top = `${Math.min(top, window.innerHeight - colPop.offsetHeight - 8)}px`;
      colPop.style.left = `${Math.min(r.left, window.innerWidth - colPop.offsetWidth - 8)}px`;
    };

    table.addEventListener('mouseover', (e) => {
      const target = e.target.closest('td[data-col], th[data-col]');
      if (!target) return;
      show(target);
    });
    table.addEventListener('mouseleave', () => { colPop.hidden = true; });
    // 스크롤하면 붙어 있던 자리가 어긋난다. 따라다니게 만들 이유는 없다.
    table.addEventListener('scroll', () => { colPop.hidden = true; }, true);
  }

  // ---------- 적용 대기 ----------

  // rowKeyID는 행을 가리키는 문자열이다. 기본키가 없으면 null.
  //
  // 행 객체가 아니라 키로 다루는 이유: 조회를 다시 하면 행 객체는 새로 만들어진다.
  // 키는 그대로이므로 선택과 적용 대기가 페이지를 넘어가도 살아남는다.
  function rowKeyID(page, row) {
    const key = keyOf(page, row);
    if (!key) return null;
    return JSON.stringify(key);
  }

  // stage는 변경 하나를 대기열에 넣는다. 같은 행에 대한 이전 변경은 대체한다 —
  // 한 행을 두 번 고쳤으면 마지막 것만 적용하는 것이 사용자가 본 화면과 같다.
  function stage(change) {
    const at = staged.findIndex((s) => s.id === change.id && s.id !== null);
    if (at >= 0 && change.id !== null) staged[at] = change;
    else staged.push(change);
    drawTable();
  }

  function stageSelectedDeletes() {
    const page = view.page;
    let added = 0;
    for (const row of page.rows) {
      const id = rowKeyID(page, row);
      if (!id || !view.selected.has(id)) continue;
      stage({ id, action: 'delete', key: keyOf(page, row), label: labelOf(page, row) });
      added += 1;
    }
    view.selected.clear();
    drawTable();
    toast(`삭제 ${added}건을 적용 대기에 넣었습니다`, 'info');
  }

  // openBulkEditDialog는 고른 행들의 한 컬럼을 같은 값으로 맞춘다.
  //
  // 여러 컬럼을 한 번에 바꾸게 하지 않는 이유: 그것은 사실상 "조건에 맞는 행을
  // 일괄 수정"이고, 그 일은 SQL 콘솔이 더 정확하게 한다. 여기서 필요한 것은
  // "고른 행들의 상태를 같은 값으로 맞추기" 정도다.
  function openBulkEditDialog() {
    const page = view.page;
    const cols = page.columns.filter((c) => !(page.primaryKey ?? []).includes(c.name));
    if (cols.length === 0) {
      toast('기본키 외에 바꿀 수 있는 컬럼이 없습니다', 'error');
      return;
    }
    const colSelect = select(cols.map((c) => ({ value: c.name, label: `${c.name} (${c.type})` })));
    const valueInput = input({ placeholder: '값', autocomplete: 'off' });
    const nullBox = h('input', { type: 'checkbox' });
    nullBox.addEventListener('change', () => { valueInput.disabled = nullBox.checked; });

    const note = h('p.field-help', {});
    const syncNote = () => {
      const col = cols.find((c) => c.name === colSelect.value);
      mount(note, view.comments[col?.name] || col?.type || '');
    };
    colSelect.addEventListener('change', syncNote);
    syncNote();

    openModal({
      title: `선택한 ${view.selected.size}행 편집`,
      width: 520,
      body: () => [
        h('label.field', {}, h('span.field-label', {}, '컬럼'), colSelect, note),
        h('label.field', {}, h('span.field-label', {}, '값'), valueInput),
        h('label.checkbox', {}, nullBox, h('span', {}, 'NULL로 설정')),
        h('p.notice.notice-info', {}, icon('alert'),
          '지금 바로 반영되지 않습니다. 적용 대기에 들어가고, 위의 "적용하기"에서 실행될 SQL을 확인한 뒤 적용됩니다.'),
      ],
      footer: (close) => [
        h('button.btn', { type: 'button', onclick: close }, '취소'),
        h('button.btn.btn-primary', {
          type: 'button',
          onclick: () => {
            const column = colSelect.value;
            const value = nullBox.checked ? null : valueInput.value;
            let added = 0;
            for (const row of page.rows) {
              const id = rowKeyID(page, row);
              if (!id || !view.selected.has(id)) continue;
              stage({
                id, action: 'update', key: keyOf(page, row),
                values: { [column]: value },
                label: labelOf(page, row),
              });
              added += 1;
            }
            view.selected.clear();
            close();
            drawTable();
            toast(`수정 ${added}건을 적용 대기에 넣었습니다`, 'info');
          },
        }, '적용 대기에 넣기'),
      ],
    });
  }

  // openApplyDialog는 실행될 문장을 보여주고 확인을 받는다.
  //
  // 서버에 dryRun으로 한 번 물어 **실제로 실행될 문장**을 받아 온다. 화면에서
  // 비슷하게 만들어 보여주면 그것은 추측이고, 방언과 인용 규칙이 다른 순간
  // 미리보기와 실행이 갈라진다.
  async function openApplyDialog() {
    const changes = staged.map((s) => ({ action: s.action, values: s.values, key: s.key }));
    let preview;
    try {
      preview = await api.post(`/connections/${conn.id}/data/batch`, {
        namespace: view.object.namespace ?? '',
        table: view.object.name,
        changes,
        dryRun: true,
      });
    } catch (err) {
      toastError(err);
      return;
    }

    const rows = preview.results ?? [];
    const body = h('div.sql-preview', {},
      ...rows.map((r, i) => h('div.sql-preview-item', {},
        h('div.sql-preview-head', {},
          h('span.muted.small', {}, `${i + 1}.`),
          badge(opActionLabel(staged[i]?.action), staged[i]?.action === 'delete' ? 'danger' : 'neutral'),
          staged[i]?.label ? h('span.muted.small', {}, staged[i].label) : null,
        ),
        codeBlock(r.statement, 'sql', { className: 'value-block' }),
        (r.params ?? []).length
          ? h('p.field-help', {}, `값: ${r.params.map((p) => (p === null ? 'NULL' : String(p))).join(', ')}`)
          : null,
      )),
    );

    openModal({
      title: `적용 확인 — ${staged.length}건`,
      width: 760,
      body: () => [
        h('p.notice.notice-warn', {}, icon('alert'),
          '아래 문장이 한 트랜잭션으로 실행됩니다. 하나라도 실패하면 전부 취소됩니다.'),
        body,
      ],
      footer: (close) => [
        h('button.btn', { type: 'button', onclick: close }, '취소'),
        h('button.btn.btn-danger', {
          type: 'button',
          onclick: async (e) => {
            e.currentTarget.disabled = true;
            try {
              const res = await api.post(`/connections/${conn.id}/data/batch`, {
                namespace: view.object.namespace ?? '',
                table: view.object.name,
                changes,
              });
              staged.length = 0;
              close();
              toast(`${res.results.length}건을 적용했습니다 (영향 ${res.affected}행)`, 'success');
              loadRows(false);
            } catch (err) {
              e.currentTarget.disabled = false;
              toastError(err);
            }
          },
        }, icon('play'), '적용'),
      ],
    });
  }

  function labelOf(page, row) {
    const key = keyOf(page, row);
    if (!key) return '';
    return Object.entries(key).map(([k, v]) => `${k}=${v}`).join(', ');
  }

  function filterChips() {
    return h('div.filter-chips', {},
      ...view.filters.map((f, i) => h('span.chip.chip-accent', {},
        `${f.column} ${opLabel(f.op)} ${f.op === 'isnull' || f.op === 'notnull' ? '' : f.value}`,
        h('button.chip-x', {
          type: 'button', title: '제거',
          onclick: () => { view.filters.splice(i, 1); view.offset = 0; loadRows(true); },
        }, '×'),
      )),
      h('button.link-btn', {
        type: 'button',
        onclick: () => { view.filters = []; view.offset = 0; loadRows(true); },
      }, '모두 지우기'),
    );
  }

  function openFilterDialog() {
    const cols = view.page?.columns ?? [];
    const colSelect = select(cols.map((c) => ({ value: c.name, label: c.name })));
    const opSelect = select(FILTER_OPS.map((o) => ({ value: o.value, label: o.label })));
    const valueInput = input({ placeholder: '값', autocomplete: 'off' });

    const syncValue = () => {
      const needsValue = opSelect.value !== 'isnull' && opSelect.value !== 'notnull';
      valueInput.disabled = !needsValue;
    };
    opSelect.addEventListener('change', syncValue);
    syncValue();

    openModal({
      title: '조건 추가',
      width: 520,
      body: () => [
        h('div.form-grid', {},
          h('label.field', {}, h('span.field-label', {}, '컬럼'), colSelect),
          h('label.field', {}, h('span.field-label', {}, '조건'), opSelect),
        ),
        h('label.field', {}, h('span.field-label', {}, '값'), valueInput),
      ],
      footer: (close) => [
        h('button.btn', { type: 'button', onclick: close }, '취소'),
        h('button.btn.btn-primary', {
          type: 'button',
          onclick: () => {
            view.filters.push({
              column: colSelect.value,
              op: opSelect.value,
              value: valueInput.disabled ? '' : valueInput.value,
            });
            view.offset = 0;
            close();
            loadRows(true);
          },
        }, '추가'),
      ],
    });
  }

  // 행 편집.
  //
  // 목록의 값을 그대로 쓰지 않고 서버에서 다시 읽는다(full: true). 목록은 표시용으로
  // 긴 값을 잘라서 보내므로, 그것을 저장하면 데이터가 잘린 채로 덮인다.
  async function openRowEditor(target, startInEdit = false) {
    const page = view.page;
    const isNew = target === null;
    let values = {};

    if (!isNew) {
      const key = keyOf(page, target.row);
      if (!key) {
        toast('기본키를 찾을 수 없어 수정할 수 없습니다', 'error');
        return;
      }
      try {
        const res = await api.post(`/connections/${conn.id}/data/query`, {
          namespace: view.object.namespace ?? '',
          table: view.object.name,
          limit: 1,
          full: true,
          filters: Object.entries(key).map(([column, value]) => ({
            column, op: 'eq', value: String(value),
          })),
        });
        const row = res.page.rows[0];
        if (!row) {
          toast('대상 행을 찾지 못했습니다. 이미 삭제되었을 수 있습니다', 'error');
          loadRows(false);
          return;
        }
        res.page.columns.forEach((col, i) => { values[col.name] = row[i]; });
      } catch (err) {
        toastError(err);
        return;
      }
    }

    // 편집 컨트롤은 미리 만들어 둔다. 보기 ↔ 수정을 오갈 때 입력하던 것이
    // 사라지면 토글이 아니라 초기화가 된다.
    const inputs = new Map();
    const editFields = page.columns.map((col) => {
      const value = values[col.name];
      const isNull = value === null || value === undefined;
      const text = isNull ? '' : String(value);
      // 값이 길거나 줄바꿈이 있으면 여러 줄 입력이 필요하다.
      const control = text.length > 120 || text.includes('\n')
        ? textarea({ value: text, rows: 6 })
        : input({ value: text, autocomplete: 'off' });
      // NOT NULL 컬럼에는 NULL을 고를 수 없게 한다. 고를 수 있게 두면
      // 저장을 눌러야 DB가 거절하고, 그때는 이미 다른 입력도 잃는다.
      const nullBox = h('input', {
        type: 'checkbox', checked: isNull, disabled: !col.nullable,
        onchange: (e) => { control.disabled = e.target.checked; },
      });
      control.disabled = isNull;
      inputs.set(col.name, { control, nullBox, col });
      // NULL 선택은 라벨 줄 오른쪽 끝에 둔다. 입력칸 아래에 두면 다음 컬럼의
      // 라벨과 붙어 어느 컬럼의 것인지 한 번 더 확인해야 하고, 컬럼마다 한 줄씩
      // 늘어 목록이 길어진다.
      return h('label.field.row-field', {},
        h('span.field-label', {},
          h('span.col-name', {}, col.name),
          ...columnMarks(col),
          h('span.col-type', {}, col.type),
          h('label.checkbox.small', {}, nullBox, h('span', {}, 'NULL')),
        ),
        control,
      );
    });

    const canEdit = !isNew && canWrite && page.editable && support.mutate
      && view.object.kind !== 'view';
    let editing = isNew || startInEdit;

    const box = h('div.row-editor');
    // FK를 따라가면 대상 행이 **오른쪽에 펼쳐진다**. 거기서 또 FK를 따라가면
    // 그 오른쪽으로 이어진다 — 값 하나가 무엇을 가리키는지 보려고 표를 옮겨
    // 다니지 않아도 된다.
    //
    // 펼침은 사슬이라 앞의 것을 접거나 다른 컬럼을 고르면 뒤의 것은 함께 사라진다.
    // 남겨 두면 지금 보고 있는 것이 어느 행에서 뻗어 나온 것인지 알 수 없다.
    const chain = h('div.row-chain', {}, box);
    // followed[i]는 i번째 칸에서 펼쳐 둔 컬럼 이름이다.
    const followed = [];
    const panels = [box];

    // widen은 사슬이 길어지면 모달을 넓힌다. 화면이 허락하는 데까지만 넓히고,
    // 그보다 길어지면 칸을 줄이는 대신 사슬을 옆으로 넘긴다(.row-chain의 가로 스크롤).
    // 폭을 지키는 쪽이 낫다 — 사슬은 값을 확인하려고 펼치는 것인데, 칸이 좁아지면
    // 정작 그 값이 잘린다.
    const widen = () => {
      const modal = chain.closest('.modal');
      if (!modal) return;
      const room = Math.max(720, window.innerWidth - 48);
      modal.style.maxWidth = `${Math.min(room, 720 + 372 * (panels.length - 1))}px`;
    };

    // showLast는 새로 펼친 칸으로 사슬을 밀어 준다. 스크롤이 생긴 뒤에는 방금 누른
    // 결과가 화면 밖에 나타날 수 있고, 그러면 아무 일도 일어나지 않은 것처럼 보인다.
    const showLast = () => {
      requestAnimationFrame(() => { chain.scrollLeft = chain.scrollWidth; });
    };

    // dropAfter는 i번째 뒤의 칸을 모두 걷어낸다.
    const dropAfter = (i) => {
      while (panels.length > i + 1) {
        panels.pop().remove();
        followed.pop();
      }
      widen();
    };

    // refRow는 외래키가 가리키는 행 하나를 읽는다.
    const refRow = async (ref, value) => {
      const body = {
        namespace: ref.namespace ?? '',
        table: ref.table,
        limit: 2,
        full: true,
      };
      let column = ref.column;
      if (!column) {
        // SQLite는 대상 컬럼을 비워 두면 "대상 표의 기본키"를 뜻한다.
        // 그때는 한 번 읽어 기본키 이름을 알아낸 뒤 다시 좁힌다.
        const probe = await api.post(`/connections/${conn.id}/data/query`,
          { ...body, limit: 1 });
        column = probe.page.primaryKey?.[0] ?? '';
        if (!column) throw new Error('대상 표의 기본키를 찾지 못했습니다');
      }
      const res = await api.post(`/connections/${conn.id}/data/query`, {
        ...body,
        filters: [{ column, op: 'eq', value: String(value) }],
      });
      return res.page;
    };

    // follow는 i번째 칸의 FK 컬럼을 눌렀을 때 대상 행을 옆에 펼친다.
    const follow = async (i, col, value) => {
      if (followed[i] === col.name) { // 같은 것을 다시 누르면 접는다
        dropAfter(i);
        redrawPanel(i);
        return;
      }
      dropAfter(i);
      const panel = h('div.row-panel', {}, spinner('대상 행을 읽는 중…'));
      chain.appendChild(panel);
      panels.push(panel);
      followed[i] = col.name;
      redrawPanel(i);
      widen();
      showLast();
      try {
        const page = await refRow(col.fk, value);
        const row = page.rows?.[0];
        if (!row) {
          mount(panel, h('div.row-panel-head', {}, h('strong', {}, fkTarget(col.fk))),
            h('p.muted.small', {}, '가리키는 행이 없습니다 (값이 남아 있는 참조일 수 있습니다)'));
          return;
        }
        const refValues = {};
        page.columns.forEach((c, n) => { refValues[c.name] = row[n]; });
        drawPanel(panels.length - 1, panel, fkTarget(col.fk), page.columns, refValues,
          page.total > 1 || page.hasMore);
      } catch (err) {
        mount(panel, h('div.row-panel-head', {}, h('strong', {}, fkTarget(col.fk))),
          h('p.notice.notice-error', {}, err.message ?? String(err)));
      }
    };

    // drawPanel은 펼쳐진 칸 하나를 그린다. 그 칸에서도 FK를 따라갈 수 있다.
    const drawPanel = (depth, panel, title, columns, refValues, many) => {
      panel.dataset.depth = String(depth);
      panel._draw = () => mount(panel,
        h('div.row-panel-head', {},
          h('strong', {}, title),
          many ? badge('여러 행 중 첫 행', 'warn') : null,
          h('button.icon-btn', {
            type: 'button', title: '접기',
            onclick: () => { dropAfter(depth - 1); redrawPanel(depth - 1); },
          }, icon('x', 14)),
        ),
        ...viewFields(columns, refValues, {
          followed: followed[depth] ?? '',
          onFollow: (c, v) => follow(depth, c, v),
        }),
      );
      panel._draw();
    };

    // redrawPanel은 그 칸만 다시 그린다(펼침 표시를 갱신하려고).
    const redrawPanel = (i) => {
      if (i === 0) { draw(); return; }
      panels[i]?._draw?.();
    };
    // 왼쪽 끝에 붙여 오른쪽의 닫기·저장과 구분한다. 모드 전환은 성격이 다른 동작이다.
    const modeBtn = h('button.btn', {
      type: 'button', style: { marginRight: 'auto' },
    });
    // 바닥 줄의 버튼은 모두 같은 크기다. 하나만 작으면 줄이 어긋나 보인다.
    const delBtn = h('button.btn.btn-danger-ghost', { type: 'button' },
      icon('trash', 14), '삭제');
    const saveBtn = h('button.btn.btn-primary', { type: 'button' });

    const draw = () => {
      mount(box, editing ? editFields : viewFields(page.columns, values, {
        followed: followed[0] ?? '',
        onFollow: (col, value) => follow(0, col, value),
      }));
      modeBtn.replaceChildren(
        icon(editing ? 'list' : 'edit', 14),
        document.createTextNode(editing ? '상세 보기' : '수정'),
      );
      modeBtn.style.display = canEdit ? '' : 'none';
      // 삭제는 두 모드 어디서나 같은 자리에 있다. 수정 모드에서만 보이면
      // "지우려면 먼저 수정으로 들어가야 하나"를 한 번 생각하게 된다.
      delBtn.style.display = canEdit ? '' : 'none';
      saveBtn.style.display = editing ? '' : 'none';
    };
    modeBtn.addEventListener('click', () => { editing = !editing; draw(); });

    const close = openModal({
      title: isNew
        ? `행 추가 — ${view.object.name}`
        : `행 상세 — ${view.object.name}`,
      width: 720,
      body: () => {
        draw();
        // 모달이 붙은 뒤에 폭을 맞춘다.
        setTimeout(widen, 0);
        return chain;
      },
      footer: (closeFn) => [
        modeBtn,
        delBtn,
        h('button.btn', { type: 'button', onclick: closeFn }, '닫기'),
        saveBtn,
      ],
    });

    delBtn.addEventListener('click', async () => {
      const key = keyOf(page, target.row);
      if (!key) {
        toast('기본키를 찾을 수 없어 삭제할 수 없습니다', 'error');
        return;
      }
      if (canStage()) {
        stage({
          id: rowKeyID(page, target.row), action: 'delete', key,
          label: labelOf(page, target.row),
        });
        close();
        toast('삭제를 적용 대기에 넣었습니다', 'info');
        return;
      }
      close();
      await deleteRow(target.row);
    });

    // 값을 모아 payload를 만든다. 추가할 때 비운 칸은 보내지 않는다 —
    // 빈 문자열과 "지정하지 않음"은 다르고, 후자는 DB의 기본값을 쓴다는 뜻이다.
    const collect = () => {
      const payload = {};
      for (const [name, { control, nullBox }] of inputs) {
        if (isNew && !nullBox.checked && control.value === '') continue;
        payload[name] = nullBox.checked ? null : control.value;
      }
      return payload;
    };

    // 적용 대기에 넣는 것과 바로 저장하는 것을 라벨로 구분하지 않는다.
    // 모달을 닫으면 "적용할까요"를 다시 묻기 때문에, 여기서 긴 이름을 붙이면
    // 같은 말을 두 번 하는 셈이다.
    saveBtn.replaceChildren(document.createTextNode(
      canStage() ? '적용' : (isNew ? '추가' : '저장')));
    saveBtn.addEventListener('click', async () => {
      const payload = collect();
      if (canStage()) {
        stage({
          id: isNew ? null : rowKeyID(page, target.row),
          action: isNew ? 'insert' : 'update',
          values: payload,
          key: isNew ? undefined : keyOf(page, target.row),
          label: isNew ? '새 행' : labelOf(page, target.row),
        });
        close();
        toast('적용 대기에 넣었습니다. 위의 "적용하기"에서 확인하세요', 'info');
        return;
      }
      try {
        const res = await api.post(`/connections/${conn.id}/data/mutate`, {
          namespace: view.object.namespace ?? '',
          table: view.object.name,
          action: isNew ? 'insert' : 'update',
          values: payload,
          key: isNew ? undefined : keyOf(page, target.row),
        });
        close();
        if (res.note) toast(res.note, 'info');
        else toast(isNew ? '행을 추가했습니다' : '행을 수정했습니다', 'success');
        loadRows(false);
      } catch (err) {
        toastError(err);
      }
    });
  }

  async function deleteRow(row) {
    const key = keyOf(view.page, row);
    if (!key) {
      toast('기본키를 찾을 수 없어 삭제할 수 없습니다', 'error');
      return;
    }
    const label = Object.entries(key).map(([k, v]) => `${k}=${v}`).join(', ');
    const ok = await confirmDialog({
      title: '행 삭제',
      message: `${view.object.name} 에서 ${label} 행을 삭제합니다. 되돌릴 수 없습니다.`,
      confirmLabel: '삭제',
      danger: true,
      // 운영 DB에서는 이름을 입력하게 한다. 운영 데이터 한 행이 사라지는 것은
      // 개발 DB의 한 행과 무게가 다르다.
      requireText: conn.environment === 'prod' ? conn.name : null,
    });
    if (!ok) return;
    try {
      const res = await api.post(`/connections/${conn.id}/data/mutate`, {
        namespace: view.object.namespace ?? '',
        table: view.object.name,
        action: 'delete',
        key,
      });
      if (res.note) toast(res.note, 'info');
      else toast('행을 삭제했습니다', 'success');
      loadRows(false);
    } catch (err) {
      toastError(err);
    }
  }

  await loadObjects();
}

// keyOf는 한 행에서 기본키 값을 뽑는다. 기본키가 없으면 null이다.
function keyOf(page, row) {
  if (!page.primaryKey?.length) return null;
  const key = {};
  for (const name of page.primaryKey) {
    const index = page.columns.findIndex((c) => c.name === name);
    if (index < 0) return null;
    key[name] = row[index];
  }
  return key;
}

const FILTER_OPS = [
  { value: 'eq', label: '= 같음' },
  { value: 'ne', label: '≠ 다름' },
  { value: 'contains', label: '포함' },
  { value: 'prefix', label: '시작' },
  { value: 'gt', label: '> 초과' },
  { value: 'gte', label: '≥ 이상' },
  { value: 'lt', label: '< 미만' },
  { value: 'lte', label: '≤ 이하' },
  { value: 'isnull', label: 'NULL 임' },
  { value: 'notnull', label: 'NULL 아님' },
];

function opLabel(op) {
  return FILTER_OPS.find((o) => o.value === op)?.label ?? op;
}

// opActionLabel은 적용 대기 항목의 동작 이름이다.
// insert/update/delete는 이 화면에서 "추가/수정/삭제"로만 부른다.
function opActionLabel(action) {
  return { insert: '추가', update: '수정', delete: '삭제' }[action] ?? action;
}
