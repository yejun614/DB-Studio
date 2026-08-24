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
 */
export function serverDbPicker({
  usable, servers, currentId, onPick,
  serverLabel = '서버', dbLabelText = 'DB', allLabel = '',
}) {
  const groups = groupByServer(usable, servers);
  const found = groups.find((g) => g.dbs.some((i) => i.connection.id === currentId));
  // allLabel이 있으면 "고른 것이 없음"도 정상 상태다(목록 화면의 전체 보기).
  // 없으면 하나는 반드시 골라야 하므로 첫 서버로 시작한다.
  const current = found ?? (allLabel ? null : groups[0]);

  const dbSelect = select(
    (current?.dbs ?? []).map((i) => ({ value: i.connection.id, label: dbLabel(i.connection) })),
    { value: currentId },
  );
  dbSelect.addEventListener('change', () => onPick(dbSelect.value));

  const serverSelect = select(
    [
      ...(allLabel ? [{ value: '', label: allLabel }] : []),
      ...groups.map((g) => ({ value: g.id, label: g.label })),
    ],
    { value: current?.id ?? '' },
  );

  const dbField = h('label.field.field-inline', {},
    h('span.field-label', {}, dbLabelText), dbSelect);

  // 전체를 고른 상태에서 DB 고르개는 뜻이 없다. 남겨 두면 "여기서 뭘 골라야 하나"를
  // 묻게 되므로 감춘다(고를 것이 정해지면 다시 나타난다).
  const syncDbField = () => {
    dbField.style.display = allLabel && serverSelect.value === '' ? 'none' : '';
  };
  syncDbField();

  serverSelect.addEventListener('change', () => {
    syncDbField();
    if (allLabel && serverSelect.value === '') {
      onPick('');
      return;
    }
    const g = groups.find((x) => x.id === serverSelect.value);
    if (!g) return;
    // 서버를 바꾸면 그 서버의 첫 DB로 간다. 어느 DB를 볼지는 아직 모르지만,
    // 빈 화면을 두고 한 번 더 고르게 하는 것보다 하나를 열어 보여주는 편이 빠르다.
    onPick(g.dbs[0].connection.id);
  });

  return {
    groups,
    serverSelect,
    dbSelect,
    // 필터 바에 그대로 넣을 수 있는 라벨 묶음
    nodes: [
      h('label.field.field-inline', {}, h('span.field-label', {}, serverLabel), serverSelect),
      dbField,
    ],
  };
}
