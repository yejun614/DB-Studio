// 분산 스토리지(하둡 HDFS · Ceph) 관리 화면.
//
// DB 화면과 나눈 이유: 여기에는 테이블도 SQL도 없다. 대신 용량·노드·경로·풀이 있고,
// 운영자가 이 화면에서 답을 얻고 싶은 질문은 셋이다 — 클러스터는 괜찮은가, 무엇이
// 자리를 차지하고 있는가, 지금 무슨 일이 돌고 있는가.
import { api } from '../core/api.js';
import { state, kindLabel } from '../core/store.js';
import {
  h, mount, icon, select, input, spinner, emptyState, pageHeader,
  badge, envBadge, formatDate, relativeTime, toast, toastError, confirmDialog, openModal,
} from '../core/ui.js';
import { formatBytes } from '../core/chart.js';
import { navigate } from '../core/router.js';
import { errorPanel } from './users.js';

export async function renderStorage(outlet, params, query) {
  mount(outlet, spinner('커넥션 목록을 불러오는 중…'));

  let conns;
  try {
    conns = await api.get('/connections/');
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  // 스토리지 능력이 켜진 종류만 고른다. 종류 이름을 화면에서 나열하면
  // 종류가 늘 때마다 여기도 고쳐야 한다.
  const usable = conns.items.filter((i) => i.accessible
    && state.meta?.dbKinds?.find((k) => k.kind === i.connection.kind)?.capabilities?.storage);

  if (usable.length === 0) {
    mount(outlet,
      pageHeader('스토리지', '하둡 HDFS · Ceph 클러스터 관리'),
      emptyState('등록된 스토리지 클러스터가 없습니다. 하둡 또는 Ceph를 커넥션으로 등록하세요.',
        h('a.btn', { href: '/connections' }, 'DB 커넥션으로')),
    );
    return;
  }

  const selectedId = query.get('conn') || usable[0].connection.id;
  const current = usable.find((i) => i.connection.id === selectedId) ?? usable[0];
  const conn = current.connection;

  const connSelect = select(
    usable.map((i) => ({
      value: i.connection.id,
      label: `${i.connection.name} — ${kindLabel(i.connection.kind)}`,
    })),
    { value: conn.id },
  );
  connSelect.addEventListener('change', () => {
    navigate(`/storage?conn=${encodeURIComponent(connSelect.value)}`);
  });

  const body = h('div');

  mount(outlet,
    pageHeader('스토리지', `${conn.name} 클러스터를 관리합니다`, [
      h('button.btn', { type: 'button', onclick: () => load() }, icon('refresh'), '다시 읽기'),
    ]),
    h('div.card.filter-bar', {},
      h('label.field.field-inline', {}, h('span.field-label', {}, '클러스터'), connSelect),
      envBadge(conn.environment),
      h('div.filter-sep'),
      h('a.btn.btn-small', { href: `/monitor?conn=${encodeURIComponent(conn.id)}` },
        icon('activity'), '모니터링'),
      h('a.btn.btn-small', { href: `/events?conn=${encodeURIComponent(conn.id)}` },
        icon('alert'), '이벤트'),
    ),
    body,
  );

  async function load() {
    mount(body, spinner(`${conn.name} 상태를 읽는 중…`));
    try {
      const res = await api.get(`/connections/${conn.id}/storage`);
      mount(body, ...clusterView(conn, res));
    } catch (err) {
      mount(body, errorPanel(err));
    }
  }
  await load();
}

// clusterView는 개요 + 종류별 목록이다.
function clusterView(conn, res) {
  const ov = res.overview ?? {};
  const feats = res.features ?? {};
  const out = [overviewCard(ov)];

  if (feats.browse) out.push(browserCard(conn, feats.write));
  if (feats.apps) out.push(appsCard(conn));
  if (feats.pools) out.push(listCard(conn, 'pools'));
  if (feats.osds) out.push(listCard(conn, 'osds'));
  if (feats.buckets) out.push(listCard(conn, 'buckets'));
  return out;
}

function healthBadge(health) {
  const level = health?.level ?? 'unknown';
  const label = health?.summary || level;
  if (level === 'ok') return badge(label, 'success');
  if (level === 'warn') return badge(label, 'warn');
  if (level === 'error') return badge(label, 'danger');
  return badge(label, 'neutral');
}

function overviewCard(ov) {
  const cap = ov.capacity ?? {};
  const pct = cap.total > 0 ? (cap.used / cap.total) * 100 : null;
  return h('div.card', {},
    h('div.card-title', {},
      h('span', {}, '클러스터 상태'),
      healthBadge(ov.health),
      ov.version ? h('span.muted.small', {}, ov.version) : null,
    ),
    // 용량이 먼저다. 스토리지에서 가장 자주 확인하는 것이고, 90%를 넘는 순간부터는
    // 다른 모든 지표보다 급한 문제가 된다.
    h('div.storage-capacity', {},
      // 색이 바뀌는 지점을 기본 임계값 룰과 맞춘다(85% 주의 / 95% 위험).
      // 화면과 알림이 다른 기준을 쓰면 "노란데 알림은 안 온다"가 된다.
      h('div.storage-gauge', { class: pct === null ? '' : pct >= 95 ? 'is-critical' : pct >= 85 ? 'is-warn' : '' },
        h('div.storage-gauge-fill', { style: `width:${pct ?? 0}%` })),
      h('div.storage-capacity-text', {},
        h('strong', {}, pct === null ? '용량 정보 없음' : `${pct.toFixed(1)}% 사용`),
        h('span.muted', {}, `${formatBytes(cap.used ?? 0)} / ${formatBytes(cap.total ?? 0)}`
          + (cap.available ? ` · 남은 공간 ${formatBytes(cap.available)}` : '')),
      ),
    ),
    (ov.health?.checks ?? []).length
      ? h('ul.storage-checks', {}, ov.health.checks.map((c) => h('li', {}, c)))
      : null,
    h('div.storage-facts', {}, (ov.facts ?? []).map((f) =>
      h('div.storage-fact', { class: f.level ? `is-${f.level}` : '' },
        h('span.storage-fact-label', {}, f.label),
        h('span.storage-fact-value', {}, f.value)))),
    (ov.notes ?? []).length
      ? h('div.storage-notes', {}, ov.notes.map((n) => h('p.field-help', {}, n)))
      : null,
  );
}

// ---------- HDFS 탐색 ----------

function browserCard(conn, canWrite) {
  const card = h('div.card');
  let path = '/';
  // go는 경로를 바꿔 다시 그린다. breadcrumb과 디렉터리 클릭이 같은 통로를 쓴다 —
  // 두 곳이 각자 상태를 만지면 "위로 갔는데 목록은 그대로"가 된다.
  const go = (next) => { path = next; draw(); };

  const draw = async () => {
    mount(card, h('h2.card-title', {}, 'HDFS 탐색'), spinner('경로를 읽는 중…'));
    let res;
    try {
      res = await api.get(`/connections/${conn.id}/storage/browse?path=${encodeURIComponent(path)}`);
    } catch (err) {
      mount(card, h('h2.card-title', {}, 'HDFS 탐색'), errorPanel(err),
        h('button.btn', { type: 'button', onclick: () => { path = '/'; draw(); } }, '루트로'));
      return;
    }
    path = res.path;
    mount(card,
      h('div.card-title', {},
        h('span', {}, 'HDFS 탐색'),
        summaryChip(res.summary),
        canWrite
          ? h('button.btn.btn-small', {
            type: 'button',
            onclick: () => openMkdir(conn, path, draw),
          }, icon('plus'), '디렉터리 만들기')
          : null,
      ),
      breadcrumb(path, go),
      entryTable(conn, path, res.entries ?? [], canWrite, draw, go),
      res.summaryNote ? h('p.field-help', {}, `경로 요약을 읽지 못했습니다: ${res.summaryNote}`) : null,
    );
  };
  draw();
  return card;
}

function summaryChip(sum) {
  if (!sum) return null;
  const parts = [`파일 ${sum.files.toLocaleString('ko-KR')}개`,
    `디렉터리 ${sum.directories.toLocaleString('ko-KR')}개`,
    formatBytes(sum.length)];
  // 쿼터는 -1이 "제한 없음"이다. 그 숫자를 그대로 보여주면 아무도 못 읽는다.
  if (sum.spaceQuota > 0) {
    parts.push(`쿼터 ${formatBytes(sum.spaceUsed)} / ${formatBytes(sum.spaceQuota)}`);
  }
  return h('span.muted.small', {}, parts.join(' · '));
}

// breadcrumb는 위로 올라가는 유일한 길이다. 목록에 ".."를 넣지 않는 이유는
// 그것이 실제 디렉터리처럼 보여 이름·크기 칸이 비는 줄을 만들기 때문이다.
function breadcrumb(path, go) {
  const parts = path.split('/').filter(Boolean);
  const nodes = [h('button.link-btn', { type: 'button', onclick: () => go('/') }, '/')];
  let acc = '';
  parts.forEach((part, i) => {
    acc += '/' + part;
    const target = acc;
    nodes.push(h('span.crumb-sep', {}, '/'));
    nodes.push(i === parts.length - 1
      ? h('span.crumb-current', {}, part)
      : h('button.link-btn', { type: 'button', onclick: () => go(target) }, part));
  });
  return h('div.crumbs', {}, nodes);
}

function entryTable(conn, path, entries, canWrite, redraw, go) {
  if (!entries.length) return emptyState('빈 디렉터리입니다.');
  const join = (name) => (path === '/' ? `/${name}` : `${path}/${name}`);
  return h('table.table', {},
    h('thead', {}, h('tr', {},
      h('th', {}, '이름'), h('th', {}, '크기'), h('th', {}, '소유자'),
      h('th', {}, '권한'), h('th', {}, '수정'), canWrite ? h('th', {}, '') : null)),
    h('tbody', {}, entries.map((e) => h('tr', {},
      h('td', {},
        e.dir
          ? h('button.link-btn', { type: 'button', onclick: () => go(join(e.name)) },
            icon('database', 12), e.name)
          : h('span', {}, e.name),
      ),
      h('td', {}, e.dir ? '-' : formatBytes(e.size)),
      h('td', {}, `${e.owner}:${e.group}`),
      h('td', {}, h('code', {}, e.permission)),
      h('td', {}, e.modifiedAt && !e.modifiedAt.startsWith('0001')
        ? h('span', { title: formatDate(e.modifiedAt) }, relativeTime(e.modifiedAt)) : '-'),
      canWrite
        ? h('td.row-actions', {},
          h('button.icon-btn', {
            type: 'button', title: '이름 바꾸기',
            onclick: () => openRename(conn, join(e.name), redraw),
          }, icon('edit', 13)),
          h('button.icon-btn', {
            type: 'button', title: '삭제',
            onclick: () => confirmDelete(conn, join(e.name), e.dir, redraw),
          }, icon('trash', 13)),
        )
        : null,
    ))),
  );
}

function openMkdir(conn, base, done) {
  const name = input({ placeholder: '새 디렉터리 이름', autofocus: true });
  openModal({
    title: '디렉터리 만들기',
    width: 460,
    body: () => [
      h('p.field-help', {}, `${base} 아래에 만듭니다.`),
      h('label.field', {}, h('span.field-label', {}, '이름'), name),
    ],
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: async () => {
          const value = name.value.trim();
          if (!value) return;
          const target = base === '/' ? `/${value}` : `${base}/${value}`;
          try {
            await api.post(`/connections/${conn.id}/storage/mkdir`, { path: target });
            toast('디렉터리를 만들었습니다', 'success');
            close();
            done();
          } catch (err) {
            toastError(err);
          }
        },
      }, '만들기'),
    ],
  });
}

function openRename(conn, from, done) {
  const parts = from.split('/');
  const base = parts.slice(0, -1).join('/') || '';
  const name = input({ value: parts[parts.length - 1], autofocus: true });
  openModal({
    title: '이름 바꾸기',
    width: 460,
    body: () => [
      h('p.field-help', {}, `${from}`),
      h('label.field', {}, h('span.field-label', {}, '새 이름'), name),
    ],
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: async () => {
          const value = name.value.trim();
          if (!value) return;
          try {
            await api.post(`/connections/${conn.id}/storage/rename`,
              { path: from, to: `${base}/${value}` });
            toast('이름을 바꿨습니다', 'success');
            close();
            done();
          } catch (err) {
            toastError(err);
          }
        },
      }, '바꾸기'),
    ],
  });
}

// confirmDelete는 지우기 전에 무엇이 사라지는지 먼저 세어 보여준다.
//
// 재귀 삭제는 "몇 개가 사라지는지 모른 채" 누르게 되는 대표적인 조작이다. HDFS의
// 휴지통이 꺼져 있으면 되돌릴 방법이 없다.
async function confirmDelete(conn, path, isDir, done) {
  let impact = null;
  if (isDir) {
    try {
      impact = await api.post(
        `/connections/${conn.id}/storage/delete?dryRun=1`, { path, recursive: true });
    } catch {
      impact = null;
    }
  }
  const detail = impact && impact.files !== undefined
    ? `파일 ${Number(impact.files).toLocaleString('ko-KR')}개 · `
      + `디렉터리 ${Number(impact.directories).toLocaleString('ko-KR')}개 · `
      + `${formatBytes(impact.length ?? 0)}`
    : '';
  const ok = await confirmDialog({
    title: '삭제',
    message: `${path} 을(를) 지웁니다. 되돌릴 수 없습니다.`
      + (detail ? ` 이 경로 아래에 ${detail} 가 있습니다.` : ''),
    confirmLabel: '삭제',
    danger: true,
    requireText: isDir ? path.split('/').pop() : null,
  });
  if (!ok) return;
  try {
    await api.post(`/connections/${conn.id}/storage/delete`, { path, recursive: isDir });
    toast('삭제했습니다', 'success');
    done();
  } catch (err) {
    toastError(err);
  }
}

// ---------- YARN ----------

function appsCard(conn) {
  const card = h('div.card');
  const draw = async () => {
    mount(card, h('h2.card-title', {}, 'YARN 애플리케이션'), spinner('목록을 읽는 중…'));
    try {
      const res = await api.get(`/connections/${conn.id}/storage/apps?limit=50`);
      const apps = res.apps ?? [];
      mount(card,
        h('div.card-title', {}, h('span', {}, 'YARN 애플리케이션'),
          h('span.muted.small', {}, `${apps.length}개`)),
        apps.length === 0
          ? emptyState('실행 중이거나 최근 끝난 애플리케이션이 없습니다.')
          : h('table.table', {},
            h('thead', {}, h('tr', {},
              h('th', {}, '이름'), h('th', {}, '사용자'), h('th', {}, '큐'),
              h('th', {}, '상태'), h('th', {}, '진행'), h('th', {}, '시작'))),
            h('tbody', {}, apps.map((a) => h('tr', {},
              h('td', {}, h('span', { title: a.id }, a.name)),
              h('td', {}, a.user),
              h('td', {}, a.queue),
              h('td', {}, appState(a.state)),
              h('td', {}, `${Math.round(a.progress ?? 0)}%`),
              h('td', {}, a.startAt && !a.startAt.startsWith('0001')
                ? relativeTime(a.startAt) : '-'),
            )))),
      );
    } catch (err) {
      mount(card, h('h2.card-title', {}, 'YARN 애플리케이션'), errorPanel(err));
    }
  };
  draw();
  return card;
}

function appState(s) {
  if (s === 'RUNNING') return badge(s, 'info');
  if (s === 'FINISHED' || s === 'SUCCEEDED') return badge(s, 'success');
  if (s === 'FAILED' || s === 'KILLED') return badge(s, 'danger');
  return badge(s ?? '-', 'neutral');
}

// ---------- Ceph 목록 ----------

const listSpec = {
  pools: {
    title: '풀',
    columns: ['이름', '종류', '복제', 'PG', '사용량', '객체', '앱'],
    row: (p) => [p.name, p.type, `${p.size} (min ${p.minSize})`, p.pgNum,
      formatBytes(p.used), (p.objects ?? 0).toLocaleString('ko-KR'), p.app ?? '-'],
    empty: '풀이 없습니다.',
  },
  osds: {
    title: 'OSD',
    columns: ['ID', '호스트', '상태', '사용량', '가중치'],
    row: (o) => [
      `osd.${o.id}`,
      o.host || '-',
      h('span', {}, o.up ? badge('up', 'success') : badge('down', 'danger'),
        o.in ? badge('in', 'neutral') : badge('out', 'warn')),
      o.total ? `${formatBytes(o.used)} / ${formatBytes(o.total)}` : '-',
      (o.weight ?? 0).toFixed(2),
    ],
    empty: 'OSD가 없습니다.',
  },
  buckets: {
    title: '오브젝트 버킷 (RGW)',
    columns: ['이름', '소유자'],
    row: (b) => [b.name, b.owner || '-'],
    empty: '버킷이 없습니다.',
  },
};

function listCard(conn, kind) {
  const spec = listSpec[kind];
  const card = h('div.card');
  (async () => {
    mount(card, h('h2.card-title', {}, spec.title), spinner('목록을 읽는 중…'));
    try {
      const res = await api.get(`/connections/${conn.id}/storage/${kind}`);
      const items = res[kind] ?? [];
      mount(card,
        h('div.card-title', {}, h('span', {}, spec.title),
          h('span.muted.small', {}, `${items.length}개`)),
        res.note ? h('p.field-help', {}, res.note) : null,
        items.length === 0
          ? emptyState(spec.empty)
          : h('table.table', {},
            h('thead', {}, h('tr', {}, spec.columns.map((c) => h('th', {}, c)))),
            h('tbody', {}, items.map((item) =>
              h('tr', {}, spec.row(item).map((cell) => h('td', {}, cell)))))),
      );
    } catch (err) {
      mount(card, h('h2.card-title', {}, spec.title), errorPanel(err));
    }
  })();
  return card;
}
