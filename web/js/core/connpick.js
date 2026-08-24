// 서버 → DB 두 단계 커넥션 선택기.
//
// 커넥션을 평평하게 늘어놓으면 같은 서버의 DB가 이름만 다른 항목으로 반복되어,
// 목록이 길어질수록 "어느 서버의 것인가"가 사라진다. 서버를 먼저 고르고 그 안에서
// DB를 고르는 편이 실제 순서와 같다.
//
// 데이터·스키마 화면이 같은 규칙을 써야 하므로 여기 한 곳에 둔다.
import { h, select } from './ui.js';
import { kindLabel } from './store.js';

// groupByServer는 쓸 수 있는 커넥션을 서버별로 묶는다.
//
// 서버 목록을 기준으로 삼되 거기에 없는 커넥션도 버리지 않는다. 서버 정보가
// 어긋난 커넥션이 목록에서 조용히 사라지면 "있던 DB가 없어졌다"가 되고,
// 원인을 화면에서는 알 수 없다.
export function groupByServer(usable, serverItems) {
  const byID = new Map((serverItems ?? []).map((g) => [g.server.id, g.server]));
  // 서버 목록을 받지 못한 화면도 있다. 커넥션이 자기 서버의 id·이름을 들고 있으므로
  // (connection.serverId · serverName) 그것으로 묶는다 — 고르개 하나를 두기 위해
  // 화면마다 /servers/ 를 한 번 더 부르는 것은 값에 비해 비싸다.
  if (byID.size === 0) {
    for (const item of usable) {
      const c = item.connection;
      if (c.serverId && !byID.has(c.serverId)) {
        byID.set(c.serverId, { id: c.serverId, name: c.serverName || c.name, kind: c.kind });
      }
    }
  }
  const order = [...byID.keys()];
  const buckets = new Map();

  for (const item of usable) {
    const id = item.connection.serverId ?? '';
    if (!buckets.has(id)) buckets.set(id, []);
    buckets.get(id).push(item);
  }

  const groups = [];
  for (const id of [...order, ...[...buckets.keys()].filter((k) => !byID.has(k))]) {
    const dbs = buckets.get(id);
    if (!dbs?.length) continue;
    const srv = byID.get(id);
    groups.push({
      id: id || '(none)',
      label: srv
        ? `${srv.name} — ${kindLabel(srv.kind)}`
        : `서버 미지정 — ${kindLabel(dbs[0].connection.kind)}`,
      dbs,
    });
  }
  return groups;
}

// dbLabel은 DB 선택기에 쓸 이름이다.
// 커넥션 이름은 "서버 / DB" 형태인 경우가 많아 서버를 이미 고른 자리에서는
// 앞부분이 되풀이된다. 실제 데이터베이스 이름이 있으면 그것을 먼저 쓴다.
export function dbLabel(connection) {
  const db = (connection.databaseName ?? '').trim();
  if (!db) return connection.name;
  // SQLite는 파일 하나가 곧 데이터베이스라 databaseName이 전체 경로다. 그대로
  // 쓰면 선택기가 화면을 가로지른다. 파일 이름만으로도 어느 것인지 구분된다
  // (SQLite는 파일마다 서버가 따로 잡히므로 한 그룹 안에서 겹칠 일이 없다).
  return db.split(/[\\/]/).pop() || db;
}

/**
 * serverDbPicker는 서버·DB 두 선택기를 만들어 라벨과 함께 돌려준다.
 * @param {object} opts
 * @param {Array} opts.usable   {connection, ...} 목록
 * @param {Array} opts.servers  /servers/ 의 items
 * @param {string} opts.currentId  지금 고른 커넥션 id
 * @param {(id: string) => void} opts.onPick  DB가 정해질 때마다 호출
 * @param {string} opts.serverLabel  서버 쪽 라벨 (기본 '서버')
 * @param {string} opts.dbLabelText  DB 쪽 라벨 (기본 'DB')
 * @param {string} opts.serverHelp  서버 칸 아래 도움말 (전체 선택의 뜻 등)
 */
export function serverDbPicker({
  usable, servers, currentId, onPick,
  serverLabel = '서버', dbLabelText = 'DB',
  allLabel = '', allValue = '',
  placeholder = '', inline = true, help = '', serverHelp = '',
}) {
  const groups = groupByServer(usable, servers);
  const found = groups.find((g) => g.dbs.some((i) => i.connection.id === currentId));
  // "고른 것이 없음"이 정상 상태인 경우가 둘 있다: 목록 화면의 전체 보기(allLabel),
  // 그리고 아직 고르지 않은 대화상자(placeholder). 둘 다 아니면 하나는 반드시
  // 골라야 하므로 첫 서버로 시작한다.
  const special = currentId === allValue && (allLabel || placeholder);
  const current = found ?? ((allLabel || placeholder) ? null : groups[0]);

  const dbSelect = select(
    (current?.dbs ?? []).map((i) => ({ value: i.connection.id, label: dbLabel(i.connection) })),
    { value: currentId },
  );
  dbSelect.addEventListener('change', () => onPick(dbSelect.value));

  // 서버 고르개의 특별 항목: 아직 고르지 않음(placeholder)과 전체(allLabel)는 다른
  // 것이다 — 앞은 "정해야 한다", 뒤는 "정하지 않는 것이 답"이다.
  const PLACEHOLDER = '__none__';
  const specials = [];
  if (placeholder) specials.push({ value: PLACEHOLDER, label: placeholder });
  if (allLabel) specials.push({ value: allValue || PLACEHOLDER, label: allLabel });

  const serverSelect = select(
    [...specials, ...groups.map((g) => ({ value: g.id, label: g.label }))],
    { value: current?.id ?? (special && allLabel ? (allValue || PLACEHOLDER) : PLACEHOLDER) },
  );

  const fieldClass = inline ? 'label.field.field-inline' : 'label.field';
  const dbField = h(fieldClass, {},
    h('span.field-label', {}, dbLabelText), dbSelect,
    help ? h('span.field-help', {}, help) : null);

  // 서버가 정해지지 않은 상태(아직 안 골랐거나 전체)에서 DB 고르개는 뜻이 없다.
  // 남겨 두면 "여기서 뭘 골라야 하나"를 묻게 되므로 감춘다.
  const syncDbField = () => {
    const v = serverSelect.value;
    const hide = v === PLACEHOLDER || (allLabel && v === allValue);
    dbField.style.display = hide ? 'none' : '';
  };
  syncDbField();

  // 서버를 바꾸면 DB 목록을 다시 채운다.
  //
  // 필터 바에서는 onPick이 화면을 옮기며 전체를 다시 그리므로 이것이 없어도 표가
  // 나지 않았다. 하지만 대화상자·패널에서는 옮기지 않으므로, 다시 채우지 않으면
  // 이전 서버의 DB가 목록에 남아 **그것을 고를 수 있다** — 남의 서버 DB를 고른 채
  // 저장되는 길이다.
  const rebuildDb = (g) => {
    dbSelect.replaceChildren();
    for (const item of g?.dbs ?? []) {
      const opt = document.createElement('option');
      opt.value = item.connection.id;
      opt.textContent = dbLabel(item.connection);
      dbSelect.appendChild(opt);
    }
  };

  serverSelect.addEventListener('change', () => {
    syncDbField();
    const v = serverSelect.value;
    if (v === PLACEHOLDER) {
      onPick('');
      return;
    }
    if (allLabel && v === allValue) {
      onPick(allValue);
      return;
    }
    const g = groups.find((x) => x.id === v);
    if (!g) return;
    rebuildDb(g);
    // 서버를 바꾸면 그 서버의 첫 DB로 간다. 어느 DB를 볼지는 아직 모르지만,
    // 빈 화면을 두고 한 번 더 고르게 하는 것보다 하나를 열어 보여주는 편이 빠르다.
    dbSelect.value = g.dbs[0].connection.id;
    onPick(g.dbs[0].connection.id);
  });

  return {
    groups,
    serverSelect,
    dbSelect,
    // 지금 고른 값. 대화상자가 select 처럼 읽을 수 있게 getter 로 둔다
    // (호출부가 control.value 하나로 여러 종류의 칸을 다룬다).
    get value() {
      const v = serverSelect.value;
      if (v === PLACEHOLDER) return '';
      if (allLabel && v === allValue) return allValue;
      return dbSelect.value;
    },
    // 필터 바·대화상자에 그대로 넣을 수 있는 라벨 묶음
    nodes: [
      h(fieldClass, {}, h('span.field-label', {}, serverLabel), serverSelect,
        serverHelp ? h('span.field-help', {}, serverHelp) : null),
      dbField,
    ],
  };
}

// groupedSelect는 선택기 하나에 서버별 optgroup을 두는 압축판이다.
//
// 헤더의 작은 전환 칸처럼 라벨 두 개를 놓을 자리가 없는 곳에서는 두 단계 고르개가
// 줄을 넘겨 배치를 망친다. 그렇다고 커넥션을 평평하게 늘어놓으면 같은 서버의 DB가
// 이름만 다른 항목으로 반복되므로, 항목은 하나로 두고 묶음 제목으로 서버를 보여준다.
export function groupedSelect({ usable, servers, currentId, allLabel = '', allValue = '' }) {
  const groups = groupByServer(usable, servers);
  const el = document.createElement('select');
  el.className = 'input';
  if (allLabel) {
    const opt = document.createElement('option');
    opt.value = allValue;
    opt.textContent = allLabel;
    el.appendChild(opt);
  }
  for (const g of groups) {
    const grp = document.createElement('optgroup');
    grp.label = g.label;
    for (const item of g.dbs) {
      const opt = document.createElement('option');
      opt.value = item.connection.id;
      opt.textContent = dbLabel(item.connection);
      grp.appendChild(opt);
    }
    el.appendChild(grp);
  }
  el.value = usable.some((i) => i.connection.id === currentId) ? currentId : allValue;
  return el;
}
