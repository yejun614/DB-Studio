// MongoDB / Redis 전용 탐색 화면.
//
// 스키마 화면과 분리한 이유: 이 두 DB에서 실제로 필요한 정보(컬렉션 저장 크기,
// 쓰이지 않는 인덱스, 메모리 정책, TTL 없는 키, 큰 키)는 테이블·컬럼 모델에
// 담을 자리가 없다. 억지로 같은 화면에 넣으면 양쪽 다 읽기 어려워진다.
import { api } from '../core/api.js';
import { withProject } from '../core/project.js';
import { state, kindLabel } from '../core/store.js';
import {
  h, mount, icon, input, spinner, emptyState, pageHeader,
  badge, envBadge, relativeTime, formatDate,
} from '../core/ui.js';
import { formatBytes } from '../core/chart.js';
import { navigate } from '../core/router.js';
import { serverDbPicker } from '../core/connpick.js';
import { errorPanel } from './users.js';

export async function renderNoSQL(outlet, params, query) {
  mount(outlet, spinner('커넥션 목록을 불러오는 중…'));

  let conns;
  try {
    conns = await api.get(withProject('/connections/'));
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  // 전용 탐색을 지원하는 종류만 (Capabilities.explore).
  const usable = conns.items.filter((i) => {
    if (!i.accessible) return false;
    return state.meta?.dbKinds?.find((k) => k.kind === i.connection.kind)?.capabilities?.explore;
  });

  if (usable.length === 0) {
    mount(outlet,
      pageHeader('MongoDB · Redis', '문서/키-값 저장소 전용 탐색'),
      emptyState('전용 탐색을 지원하는 커넥션이 없습니다. MongoDB 또는 Redis를 등록하세요.',
        h('a.btn', { href: '/connections' }, 'DB 커넥션으로')),
    );
    return;
  }

  const selectedId = query.get('conn') || usable[0].connection.id;
  const current = usable.find((i) => i.connection.id === selectedId) ?? usable[0];

  const picker = serverDbPicker({
    usable,
    currentId: current.connection.id,
    onPick: (id) => navigate(`/nosql?conn=${encodeURIComponent(id)}`),
  });

  const body = h('div');

  mount(outlet,
    pageHeader('MongoDB · Redis', '문서/키-값 저장소 전용 탐색', [
      h('button.btn', { type: 'button', onclick: () => load() }, icon('refresh'), '다시 읽기'),
    ]),
    h('div.card.filter-bar', {},
      ...picker.nodes,
      envBadge(current.connection.environment),
      h('div.filter-sep'),
      h('a.btn.btn-small', { href: `/schema?conn=${encodeURIComponent(current.connection.id)}` },
        icon('database'), '스키마 화면'),
      h('a.btn.btn-small', { href: `/monitor?conn=${encodeURIComponent(current.connection.id)}` },
        icon('activity'), '모니터링'),
    ),
    body,
  );

  async function load() {
    mount(body, spinner(`${current.connection.name} 을(를) 조회하는 중…`));
    try {
      const res = await api.get(`/connections/${current.connection.id}/explore`);
      mount(body, ...exploreView(res));
    } catch (err) {
      mount(body, errorPanel(err));
    }
  }

  await load();
}

function exploreView(res) {
  const ex = res.explore;
  const parts = [];

  parts.push(h('div.schema-meta', {},
    h('span', {}, `${kindLabel(ex.kind)} · ${res.connection.name}`),
    h('span.muted', {}, `읽은 시각 ${formatDate(ex.capturedAt)}`),
  ));

  if (ex.notes?.length) {
    // 이 두 DB에서는 권한 부족으로 일부만 읽히는 일이 흔하다.
    // 조용히 빈 값을 보여주면 "없다"로 오해하게 된다.
    parts.push(h('div.card.notice.notice-warn', {}, icon('alert'),
      h('div', {},
        h('strong', {}, '읽는 중 참고 사항'),
        h('ul.note-list', {}, ex.notes.map((n) => h('li', {}, n))))));
  }

  if (ex.document) parts.push(...documentView(ex.document));
  else if (ex.keyspace) parts.push(...keyspaceView(ex.keyspace));
  else parts.push(emptyState('표시할 정보가 없습니다'));

  return parts;
}

// ---------- MongoDB ----------

function documentView(d) {
  const parts = [];

  parts.push(h('div.stat-row', {},
    statTile('컬렉션', d.stats.collections || d.collections.length, 'database'),
    statTile('문서', formatCount(d.stats.objects), 'list'),
    statTile('데이터', formatBytes(d.stats.dataSize), 'database'),
    statTile('저장 공간', formatBytes(d.stats.storageSize), 'database'),
    statTile('인덱스', `${d.stats.indexes || 0}개`, 'shield', formatBytes(d.stats.indexSize)),
    statTile('평균 문서 크기', formatBytes(d.stats.avgObjSize), 'activity'),
  ));

  if (Object.keys(d.server ?? {}).length) {
    parts.push(h('div.card.nosql-server', {},
      h('h2.card-title', {}, '서버'),
      h('dl.kv-grid', {}, serverRows(d.server, {
        version: '버전',
        storageEngine: '스토리지 엔진',
        host: '호스트',
        replicaSet: '레플리카 셋',
        primary: '프라이머리',
        uptimeSeconds: '가동 시간',
      }))));
  }

  // 쓰이지 않는 인덱스는 삭제 후보다. 화면에서 가장 먼저 보이는 것이 낫다.
  const unused = [];
  for (const c of d.collections) {
    for (const idx of c.indexes ?? []) {
      if (idx.name !== '_id_' && idx.ops === 0) {
        unused.push({ collection: c.name, index: idx });
      }
    }
  }
  if (unused.length) {
    parts.push(h('div.card.notice.notice-info', {}, icon('alert'),
      h('div', {},
        h('strong', {}, `서버 재시작 이후 한 번도 사용되지 않은 인덱스 ${unused.length}개`),
        h('ul.note-list', {}, unused.slice(0, 12).map((u) => h('li', {},
          h('code', {}, `${u.collection}.${u.index.name}`),
          u.index.sizeBytes ? ` — ${formatBytes(u.index.sizeBytes)}` : '',
          u.index.since ? ` (집계 시작 ${relativeTime(u.index.since)})` : ''))),
        h('p.field-help', {},
          '사용 횟수는 서버가 재시작되면 초기화됩니다. 가동 시간이 짧으면 판단 근거가 되지 못합니다.'))));
  }

  if (!d.collections.length) {
    parts.push(emptyState('컬렉션이 없습니다'));
    return parts;
  }

  const listBox = h('div.table-list');
  const search = input({ placeholder: '컬렉션 또는 필드 이름으로 검색', type: 'search' });
  const countLabel = h('span.muted');
  const renderList = () => {
    const q = search.value.trim().toLowerCase();
    const filtered = !q ? d.collections : d.collections.filter((c) =>
      c.name.toLowerCase().includes(q) ||
      (c.fields ?? []).some((f) => f.path.toLowerCase().includes(q)));
    countLabel.textContent = q
      ? `${filtered.length} / ${d.collections.length}개 컬렉션`
      : `${d.collections.length}개 컬렉션`;
    mount(listBox, filtered.length
      ? filtered.map((c) => collectionCard(c, d))
      : emptyState('검색 결과가 없습니다'));
  };
  search.addEventListener('input', renderList);

  parts.push(h('div.card.filter-bar', {},
    h('label.field.field-inline.grow', {}, icon('list'), search),
    countLabel));
  parts.push(listBox);
  renderList();
  return parts;
}

function collectionCard(c, d) {
  const typeBadge = c.type === 'view'
    ? badge('뷰', 'info')
    : c.type === 'timeseries' ? badge('시계열', 'accent') : null;

  return h('details.table-card', {},
    h('summary.table-summary', {},
      h('span.table-name', {}, c.name, typeBadge, c.capped ? badge('capped', 'warn') : null),
      h('span.table-badges', {},
        h('span.muted', {}, `문서 ${formatCount(c.documents)}`),
        c.dataSize ? h('span.muted', {}, formatBytes(c.dataSize)) : null,
        (c.indexes?.length ?? 0) > 0 ? h('span.muted', {}, `인덱스 ${c.indexes.length}`) : null,
      ),
    ),
    h('div.table-detail', {}, collectionBody(c, d)),
  );
}

function collectionBody(c, d) {
  if (c.type === 'view') {
    return h('p.notice.notice-info', {}, icon('alert'),
      c.viewOn ? `${c.viewOn} 컬렉션을 기반으로 하는 뷰입니다.` : (c.note || '뷰입니다.'));
  }

  const parts = [];
  parts.push(h('dl.kv-grid', {},
    kv('문서 수', formatCount(c.documents)),
    kv('데이터', formatBytes(c.dataSize)),
    kv('저장 공간', formatBytes(c.storageSize)),
    kv('평균 문서', formatBytes(c.avgObjSize)),
    kv('인덱스 크기', formatBytes(c.indexSize)),
    kv('샘플 문서', c.sampled ? `${c.sampled}개 / 최대 ${d.sampleSize}개` : '없음'),
  ));

  if (c.note) parts.push(h('p.muted', {}, c.note));

  if (c.fields?.length) {
    parts.push(h('h4.erd-sub', {}, `필드 ${c.fields.length}개`));
    parts.push(h('p.field-help', {},
      '존재 비율은 샘플 문서 중 그 필드가 있던 비율입니다. 100%가 아니면 코드에서 없을 수 있다고 가정해야 합니다.'));
    parts.push(h('table.table.nosql-fields', {},
      h('thead', {}, h('tr', {},
        h('th', {}, '필드'), h('th', {}, '타입'), h('th', {}, '존재 비율'), h('th', {}, '비고'))),
      h('tbody', {}, c.fields.map((f) => h('tr', { class: f.mixed ? 'row-warn' : '' },
        h('td', {}, h('code', {}, f.path)),
        h('td', {}, f.type || '—'),
        h('td', {}, presenceBar(f.presence)),
        h('td', {}, f.mixed ? badge('혼합 타입', 'warn') : (f.presence < 1 ? '선택적' : '')),
      )))));
  }

  if (c.indexes?.length) {
    parts.push(h('h4.erd-sub', {}, `인덱스 ${c.indexes.length}개`));
    parts.push(h('table.table', {},
      h('thead', {}, h('tr', {},
        h('th', {}, '이름'), h('th', {}, '키'), h('th', {}, '속성'),
        h('th', {}, '크기'), h('th', {}, '사용 횟수'))),
      h('tbody', {}, c.indexes.map((idx) => h('tr', {},
        h('td', {}, h('code', {}, idx.name)),
        h('td', {}, idx.keys || '—'),
        h('td', {},
          idx.unique ? badge('unique', 'success') : null,
          idx.sparse ? badge('sparse', 'info') : null,
          idx.partial ? badge('partial', 'info') : null,
          idx.ttlSeconds !== undefined && idx.ttlSeconds !== null
            ? badge(`TTL ${idx.ttlSeconds}s`, 'accent') : null),
        h('td', {}, idx.sizeBytes ? formatBytes(idx.sizeBytes) : '—'),
        h('td', {}, indexUsage(idx)),
      )))));
  } else {
    parts.push(h('p.muted', {}, '인덱스 정보를 읽지 못했습니다'));
  }
  return parts;
}

// indexUsage는 "확인 불가"와 "0회"를 구분해 보여준다.
// 인덱스를 지우자는 판단에서 이 둘은 전혀 다른 의미다.
function indexUsage(idx) {
  if (idx.ops === undefined || idx.ops === null) {
    return h('span.muted', { title: '$indexStats 권한이 없어 읽지 못했습니다' }, '확인 불가');
  }
  if (idx.ops === 0) return badge('0회', 'warn');
  return h('span', {}, formatCount(idx.ops));
}

function presenceBar(ratio) {
  const pct = Math.round((ratio ?? 0) * 100);
  const tone = pct === 100 ? 'ok' : pct >= 50 ? 'mid' : 'low';
  return h('span.presence', {},
    h('span.presence-bar', {}, h('span', { class: `presence-fill is-${tone}`, style: { width: `${pct}%` } })),
    h('span.presence-num', {}, `${pct}%`));
}

// ---------- Redis ----------

function keyspaceView(k) {
  const parts = [];
  const maxMem = k.memory.maxMemory;
  const usedPct = maxMem > 0 ? (k.memory.used / maxMem) * 100 : null;

  parts.push(h('div.stat-row', {},
    statTile('사용 메모리', formatBytes(k.memory.used),
      usedPct === null ? 'database' : 'database',
      usedPct === null ? '상한 없음' : `상한의 ${usedPct.toFixed(1)}%`),
    statTile('최고치', formatBytes(k.memory.peak), 'activity'),
    statTile('단편화', k.memory.fragmentation ? k.memory.fragmentation.toFixed(2) : '—', 'activity',
      k.memory.fragmentation > 1.5 ? '높음' : ''),
    statTile('적중률', k.stats.keyspaceHits + k.stats.keyspaceMisses > 0
      ? `${k.stats.hitRatio.toFixed(1)}%` : '—', 'shield'),
    statTile('초당 명령', formatCount(k.stats.opsPerSec), 'activity'),
    statTile('클라이언트', formatCount(k.stats.connectedClients), 'users',
      k.stats.blockedClients ? `대기 ${k.stats.blockedClients}` : ''),
  ));

  // 메모리 정책은 "가득 찼을 때 무슨 일이 일어나는가"를 결정한다.
  // noeviction이면 쓰기가 실패하므로 캐시로 쓰는 경우 사고의 원인이 된다.
  if (k.memory.policy) {
    const risky = k.memory.policy === 'noeviction' && maxMem > 0;
    parts.push(h('p', { class: risky ? 'notice notice-warn' : 'notice notice-info' },
      icon(risky ? 'alert' : 'shield'),
      h('span', {}, `메모리 정책 `, h('code', {}, k.memory.policy),
        risky
          ? ' — 상한에 도달하면 키를 지우지 않고 쓰기를 거부합니다. 캐시 용도라면 정책을 바꾸는 것을 검토하세요.'
          : maxMem > 0 ? ' · 상한 ' + formatBytes(maxMem) : ' · 메모리 상한이 없습니다')));
  }

  parts.push(h('div.nosql-grid', {},
    h('section.card', {},
      h('h2.card-title', {}, '서버'),
      h('dl.kv-grid', {}, serverRows(k.server, {
        redis_version: '버전',
        redis_mode: '모드',
        role: '역할',
        connected_slaves: '레플리카',
        master_link_status: '마스터 연결',
        os: 'OS',
        tcp_port: '포트',
      }), kv('가동 시간', formatUptime(k.stats.uptimeSeconds)))),
    h('section.card', {},
      h('h2.card-title', {}, '지속성'),
      h('dl.kv-grid', {},
        kv('AOF', k.persistence.aofEnabled ? badge('켜짐', 'success') : badge('꺼짐', 'neutral')),
        kv('마지막 RDB 저장', k.persistence.rdbLastSaveAt
          ? `${relativeTime(k.persistence.rdbLastSaveAt)} (${formatDate(k.persistence.rdbLastSaveAt)})`
          : '이력 없음'),
        kv('저장 이후 변경', formatCount(k.persistence.rdbChangesSince)),
        kv('마지막 저장 결과', k.persistence.lastSaveStatus === 'ok'
          ? badge('성공', 'success')
          : k.persistence.lastSaveStatus ? badge(k.persistence.lastSaveStatus, 'danger') : '—'),
      ),
      !k.persistence.aofEnabled && k.persistence.rdbChangesSince > 0
        ? h('p.field-help', {},
          'AOF가 꺼져 있으면 마지막 RDB 저장 이후의 변경은 재시작 시 사라집니다.')
        : null),
    h('section.card', {},
      h('h2.card-title', {}, '누적 통계'),
      h('dl.kv-grid', {},
        kv('총 명령 수', formatCount(k.stats.totalCommands)),
        kv('적중 / 실패', `${formatCount(k.stats.keyspaceHits)} / ${formatCount(k.stats.keyspaceMisses)}`),
        kv('만료된 키', formatCount(k.stats.expiredKeys)),
        kv('축출된 키', k.stats.evictedKeys > 0
          ? h('span.text-danger', {}, formatCount(k.stats.evictedKeys))
          : '0'),
        kv('슬로우로그', `${formatCount(k.stats.slowlogLen)}건`),
      )),
  ));

  if (k.databases?.length) {
    parts.push(h('section.card', {},
      h('h2.card-title', {}, '데이터베이스별 키'),
      h('table.table', {},
        h('thead', {}, h('tr', {},
          h('th', {}, 'DB'), h('th', {}, '키'), h('th', {}, '만료 설정됨'),
          h('th', {}, 'TTL 없음'), h('th', {}, '평균 TTL'))),
        h('tbody', {}, k.databases.map((db) => h('tr',
          { class: db.index === k.selectedDb ? 'row-current' : '' },
          h('td', {}, h('code', {}, `db${db.index}`),
            db.index === k.selectedDb ? badge('선택됨', 'accent') : null),
          h('td', {}, formatCount(db.keys)),
          h('td', {}, formatCount(db.expires)),
          h('td', {}, formatCount(db.keys - db.expires)),
          h('td', {}, db.avgTtlMs ? `${(db.avgTtlMs / 1000).toFixed(0)}초` : '—'),
        )))),
    ));
  }

  parts.push(h('section.card', {},
    h('h2.card-title', {},
      `키 접두사 그룹 ${k.groups.length}개`,
      h('span.muted', {}, ` · 표본 ${formatCount(k.scanned)}개`),
      k.truncated ? badge('표본 상한 도달', 'warn') : null),
    h('p.field-help', {},
      'SCAN으로 표본을 훑은 결과입니다. 전체 키를 훑으면 서버가 멈추므로 정확한 값이 아니라 분포를 봅니다.'),
    k.groups.length
      ? h('table.table', {},
        h('thead', {}, h('tr', {},
          h('th', {}, '접두사'), h('th', {}, '키'), h('th', {}, '타입'),
          h('th', {}, 'TTL 있음'), h('th', {}, '크기(표본)'), h('th', {}, '예시'))),
        h('tbody', {}, k.groups.map((g) => h('tr', {},
          h('td', {}, h('code', {}, g.prefix)),
          h('td', {}, formatCount(g.keys)),
          h('td', {}, typeTags(g.types)),
          h('td', {}, ttlCell(g)),
          h('td', {}, g.bytes ? formatBytes(g.bytes) : '—'),
          h('td.nosql-samples', {}, (g.sampleKeys ?? []).map((s) => h('code', {}, s))),
        ))))
      : emptyState('키가 없습니다'),
  ));

  if (k.bigKeys?.length) {
    parts.push(h('section.card', {},
      h('h2.card-title', {}, `큰 키 ${k.bigKeys.length}개`),
      h('p.field-help', {},
        '메모리 문제는 대개 소수의 키에서 옵니다. MEMORY USAGE 기준이며 표본 안에서만 비교합니다.'),
      h('table.table', {},
        h('thead', {}, h('tr', {},
          h('th', {}, '키'), h('th', {}, '타입'), h('th', {}, '메모리'), h('th', {}, 'TTL'))),
        h('tbody', {}, k.bigKeys.map((e) => h('tr', {},
          h('td', {}, h('code', {}, e.key)),
          h('td', {}, e.type || '—'),
          h('td', {}, formatBytes(e.bytes)),
          h('td', {}, e.ttl < 0 ? h('span.muted', {}, '없음') : `${formatCount(e.ttl)}초`),
        )))),
    ));
  }

  if (k.commands?.length) {
    parts.push(h('section.card', {},
      h('h2.card-title', {}, `명령 통계 상위 ${k.commands.length}개`),
      h('table.table', {},
        h('thead', {}, h('tr', {},
          h('th', {}, '명령'), h('th', {}, '호출'), h('th', {}, '평균 소요'),
          h('th', {}, '거부'), h('th', {}, '실패'))),
        h('tbody', {}, k.commands.map((cmd) => h('tr', {},
          h('td', {}, h('code', {}, cmd.name)),
          h('td', {}, formatCount(cmd.calls)),
          h('td', {}, `${cmd.usecPerCall.toFixed(1)}µs`),
          h('td', {}, cmd.rejected ? h('span.text-danger', {}, formatCount(cmd.rejected)) : '0'),
          h('td', {}, cmd.failed ? h('span.text-danger', {}, formatCount(cmd.failed)) : '0'),
        )))),
    ));
  }

  return parts;
}

// ttlCell은 TTL이 설정된 키 비율을 보여준다.
// 캐시 용도인데 TTL이 없는 그룹은 메모리가 계속 늘어나는 원인이 된다.
function typeTags(types) {
  const names = Object.keys(types ?? {});
  if (!names.length) return h('span.muted', { title: '표본 상한을 넘어 조사하지 않았습니다' }, '미조사');
  names.sort((a, b) => types[b] - types[a]);
  return h('span.tag-row', {}, names.map((n) =>
    badge(names.length > 1 ? `${n} ${types[n]}` : n, names.length > 1 ? 'warn' : 'neutral')));
}

function ttlCell(g) {
  const probed = Object.values(g.types ?? {}).reduce((a, b) => a + b, 0);
  if (!probed) return h('span.muted', {}, '미조사');
  const pct = Math.round((g.withTtl / probed) * 100);
  return h('span', { class: pct === 0 ? 'text-danger' : '' }, `${g.withTtl}/${probed} (${pct}%)`);
}

// ---------- 공용 ----------

function statTile(label, value, iconName, sub) {
  return h('div.stat', {},
    h('div.stat-icon', {}, icon(iconName, 18)),
    h('div', {},
      h('div.stat-value', {}, value),
      h('div.stat-label', {}, label),
      sub ? h('div.stat-sub', {}, sub) : null,
    ),
  );
}

function kv(label, value) {
  return h('div.kv', {}, h('dt', {}, label), h('dd', {}, value));
}

// serverRows는 알려진 키만 라벨을 붙여 순서대로 보여준다.
// 서버가 주는 필드는 버전마다 달라지므로 없는 것은 조용히 건너뛴다.
function serverRows(server, labels) {
  const rows = [];
  for (const [key, label] of Object.entries(labels)) {
    const value = server?.[key];
    if (value === undefined || value === '') continue;
    rows.push(kv(label, key === 'uptimeSeconds'
      ? formatUptime(Number(value))
      : key === 'primary' ? (value === 'true' ? '예' : '아니오') : value));
  }
  return rows;
}

function formatUptime(sec) {
  if (!sec) return '—';
  const days = Math.floor(sec / 86400);
  const hours = Math.floor((sec % 86400) / 3600);
  if (days > 0) return `${days}일 ${hours}시간`;
  const mins = Math.floor((sec % 3600) / 60);
  if (hours > 0) return `${hours}시간 ${mins}분`;
  return `${mins}분`;
}

function formatCount(n) {
  const v = Number(n ?? 0);
  if (!v) return '0';
  if (v >= 1e8) return `${(v / 1e8).toFixed(1)}억`;
  if (v >= 1e4) return `${(v / 1e4).toFixed(1)}만`;
  return v.toLocaleString('ko-KR');
}
