// 대시보드: 관리 중인 DB의 상태를 한 화면에서 본다.
//
// 구성 순서에 의도가 있다: 요약 숫자 → 지금 열린 문제 → DB별 부하 → 권한 → 기능 입구.
// "문제가 있는가"를 먼저 답하고, 그다음에 "어디서 나는가"로 좁힌다.
import { api } from '../core/api.js';
import { withProject } from '../core/project.js';
import { state, kindLabel } from '../core/store.js';
import {
  h, mount, icon, spinner, pageHeader, badge, envBadge, levelBadge,
  relativeTime, formatDate,
} from '../core/ui.js';
import { sparkline, formatMetricValue } from '../core/chart.js';
import { dbLogo } from '../core/dblogo.js';
import { metricLabel } from './monitor.js';
import { errorPanel } from './users.js';

// 대시보드는 열어 두는 화면이므로 주기적으로 갱신한다.
// 지표 수집 주기(기본 30초)와 같게 잡아 없는 데이터를 반복해서 받지 않는다.
const REFRESH_MS = 30_000;

// 부하 스파크라인에 쓸 지표. 앞에 있는 것부터 실제로 수집된 것을 고른다.
// DB 종류마다 수집되는 지표가 다르므로 하나로 고정할 수 없다.
const LOAD_METRICS = ['query.rate', 'connections.total', 'response_time'];

// 카드에 숫자로 보여줄 지표. 수집된 것만 표시한다.
// connections.used_pct는 위의 세션 막대가 이미 같은 값을 보여주므로 여기서 뺀다.
// 같은 수치를 두 번 적으면 카드에서 서로 다른 세 지표를 읽는 데 방해가 된다.
const KEY_METRICS = ['response_time', 'query.rate', 'cache.hit_ratio'];

// 이상 신호. 0이 아닐 때만 보여준다 — 평상시 카드를 깨끗하게 유지해야
// 0이 아닌 값이 눈에 들어온다.
const TROUBLE_METRICS = [
  'query.slow_rate', 'lock.deadlock_rate', 'connections.aborted_rate',
  'keys.evicted_rate', 'replication.lag', 'clients.blocked',
];

export async function renderDashboard(outlet) {
  const body = h('div');
  mount(outlet,
    pageHeader(`안녕하세요, ${state.user?.displayName || state.user?.username}`,
      '데이터베이스 관리 콘솔'),
    body,
  );

  const load = async (withSpinner = true) => {
    if (withSpinner) mount(body, spinner('상태를 확인하는 중…'));
    try {
      // 세 요청은 서로 독립적이다. 모니터링 쪽이 실패해도 커넥션 목록과
      // 기능 입구는 보여준다 — 화면 전체가 하나의 실패에 묶이면 안 된다.
      const [conns, overview, events] = await Promise.all([
        api.get(withProject('/connections/')),
        api.get(withProject('/monitor/overview')).catch((err) => ({ error: err })),
        api.get(withProject('/monitor/events?state=open&limit=6')).catch(() => ({ events: [] })),
      ]);
      mount(body, ...dashboardView(conns, overview, events));
      // 스파크라인은 카드를 그린 뒤 채운다. 커넥션 수만큼 요청이 나가므로
      // 첫 화면을 그것들 때문에 기다리게 하지 않는다.
      fillLoadCharts(overview);
    } catch (err) {
      mount(body, errorPanel(err));
    }
  };

  await load();
  const timer = setInterval(() => load(false), REFRESH_MS);
  return () => clearInterval(timer);
}

function dashboardView(conns, overview, events) {
  const items = conns.items ?? [];
  const monitored = overview.error ? [] : (overview.items ?? []);
  const summary = overview.summary ?? {};
  const parts = [];

  parts.push(summaryTiles(items, monitored, summary));
  parts.push(troubleSection(events.events ?? [], summary, monitored, overview));

  if (monitored.length > 0) {
    parts.push(h('section.card', {},
      h('h2.card-title', {}, '데이터베이스 상태',
        h('span.muted', {}, `${monitored.length}개`),
        h('a.notice-link', { href: '/monitor' }, '모니터링 상세'),
      ),
      h('div.db-grid', {}, monitored.map(dbCard)),
    ));
  } else if (overview.error) {
    parts.push(h('p.notice.notice-info', {}, icon('alert'),
      '모니터링 정보를 불러올 수 없습니다. 커넥션 접근 권한을 확인하세요.'));
  }

  if (items.length > 0) parts.push(accessCard(items));
  parts.push(featureGrid());
  return parts;
}

// ---------- 요약 ----------

function summaryTiles(items, monitored, summary) {
  const prod = items.filter((i) => i.connection.environment === 'prod');
  const down = monitored.filter((i) => i.state && !i.state.up);
  const pending = monitored.filter((i) => !i.state);

  return h('div.stat-row', {},
    statTile('전체 커넥션', items.length, 'database', { href: '/connections' }),
    statTile('운영 DB', prod.length, 'shield',
      { kind: prod.length > 0 ? 'warn' : null, href: '/connections' }),
    statTile('응답 없음', down.length, 'x',
      { kind: down.length > 0 ? 'danger' : null, href: '/monitor' }),
    statTile('열린 심각', summary.openCritical ?? 0, 'alert', {
      kind: (summary.openCritical ?? 0) > 0 ? 'danger' : null,
      href: '/events?state=open&severity=critical',
    }),
    statTile('열린 경고', summary.openWarning ?? 0, 'alert', {
      kind: (summary.openWarning ?? 0) > 0 ? 'warn' : null,
      href: '/events?state=open&severity=warning',
    }),
    statTile('24시간 이벤트', summary.last24h ?? 0, 'activity', {
      href: '/events?state=',
      sub: pending.length ? `수집 대기 ${pending.length}개` : '',
    }),
  );
}

// statTile은 요약 숫자 한 칸이다.
//
// href를 주면 링크가 된다: 문제를 가리키는 숫자는 그것을 설명하는 화면으로
// 갈 수 있어야 한다. 숫자만 보여주고 어디로 가야 할지 알려주지 않으면
// 사용자가 사이드바에서 다시 찾아 들어가야 한다.
function statTile(label, value, iconName, opts = {}) {
  const { kind, sub, href } = opts;
  const cls = `stat${kind ? `.stat-${kind}` : ''}${href ? '.is-link' : ''}`;
  const body = [
    h('div.stat-icon', {}, icon(iconName, 18)),
    h('div', {},
      h('div.stat-value', {}, value),
      h('div.stat-label', {}, label),
      sub ? h('div.stat-sub', {}, sub) : null,
    ),
  ];
  return href ? h(`a.${cls}`, { href }, body) : h(`div.${cls}`, {}, body);
}

// ---------- 지금 문제가 있는가 ----------

// troubleSection은 "특별한 문제는 없었는지"에 먼저 답한다.
// 문제가 없을 때도 그 사실을 명시한다 — 빈 화면은 "확인했다"와 구분되지 않는다.
function troubleSection(events, summary, monitored, overview) {
  const down = monitored.filter((i) => i.state && !i.state.up);

  if (down.length === 0 && events.length === 0) {
    if (overview.error || monitored.length === 0) {
      return h('p.notice.notice-info', {}, icon('alert'),
        '모니터링 중인 커넥션이 없습니다. 커넥션을 등록하면 상태와 부하를 여기서 볼 수 있습니다.');
    }
    return h('p.notice.notice-success', {}, icon('check'),
      h('span', {}, `열린 이벤트가 없고 ${monitored.length}개 커넥션이 모두 응답하고 있습니다.`,
        summary.unacked ? ` (미확인 이벤트 ${summary.unacked}건)` : ''));
  }

  return h('section.card.trouble', {},
    h('h2.card-title', {}, icon('alert'), '확인이 필요한 항목',
      h('a.notice-link', { href: '/events' }, '이벤트 전체'),
    ),
    h('ul.trouble-list', {},
      // 응답 없는 커넥션을 먼저 보여준다. 지표를 못 받는 상태에서는
      // 다른 경고가 없는 것도 "문제 없음"을 뜻하지 않는다.
      ...down.map((i) => h('li.trouble-item.is-down', {},
        badge('응답 없음', 'danger'),
        h('a.trouble-conn', { href: `/monitor?conn=${encodeURIComponent(i.connection.id)}` },
          i.connection.name),
        h('span.trouble-msg', {}, i.state.lastError || '수집에 실패했습니다'),
        h('span.trouble-time', {}, relativeTime(i.state.lastPolledAt)),
      )),
      ...events.map((e) => h('li.trouble-item', {},
        badge(severityLabel(e.severity), severityTone(e.severity)),
        h('a.trouble-conn', { href: `/monitor?conn=${encodeURIComponent(e.connectionId)}` },
          connName(monitored, e.connectionId)),
        h('span.trouble-msg', { title: e.message }, e.message),
        h('span.trouble-time', { title: formatDate(e.startedAt) }, relativeTime(e.startedAt)),
      )),
    ),
  );
}

function severityLabel(s) {
  return { critical: '심각', warning: '경고', info: '정보' }[s] ?? s;
}

function severityTone(s) {
  return { critical: 'danger', warning: 'warn', info: 'info' }[s] ?? 'neutral';
}

function connName(monitored, id) {
  return monitored.find((i) => i.connection.id === id)?.connection.name ?? '커넥션';
}

// ---------- DB 카드 ----------

function dbCard(item) {
  const c = item.connection;
  const st = item.state;
  const open = item.openEvents ?? {};

  const dot = !st
    ? h('span.dot.dot-unknown', { title: '첫 수집을 기다리는 중' })
    : st.up
      ? h('span.dot.dot-ok', { title: `정상 · ${relativeTime(st.lastPolledAt)}` })
      : h('span.dot.dot-fail', { title: st.lastError || '응답 없음' });

  // 카드 전체가 모니터링 상세로 가는 링크다. 이름만 링크였을 때는 눌러야 할 곳이
  // 카드 안의 작은 글씨 하나뿐이었다 — 카드를 눌러 볼 것이라고 기대하는 것이 자연스럽다.
  // 안에 다른 링크·버튼을 두지 않는다(링크 안의 링크는 키보드·스크린리더에서 깨진다).
  return h('a.db-card', {
    href: `/monitor?conn=${encodeURIComponent(c.id)}`,
    class: st && !st.up ? 'is-down' : '',
    title: `${c.name} 모니터링 상세로 이동`,
  },
    h('header.db-card-head', {},
      h('div.db-card-title', {}, dot, dbLogo(c.kind, 17), h('span', {}, c.name)),
      h('div.db-card-badges', {},
        envBadge(c.environment),
        open.critical ? badge(`심각 ${open.critical}`, 'danger') : null,
        open.warning ? badge(`경고 ${open.warning}`, 'warn') : null,
      ),
    ),
    h('div.db-card-meta', {},
      h('span', {}, kindLabel(c.kind)),
      h('span', {}, st ? `수집 ${relativeTime(st.lastPolledAt)}` : '수집 대기'),
    ),
    st && !st.up
      ? h('p.conn-error', {}, icon('alert'), st.lastError || '응답 없음',
        st.consecutiveFails > 1 ? h('span.muted', {}, ` (연속 ${st.consecutiveFails}회)`) : null)
      : null,
    poolBar(st),
    h('div.db-metrics', {}, keyMetrics(st)),
    troubleChips(st),
    // 스파크라인 자리를 미리 잡아 나중에 채워도 카드 높이가 흔들리지 않게 한다.
    h('div.db-spark', { dataset: { spark: c.id } },
      h('span.db-spark-body', {}, h('span.muted', {}, st ? '부하 불러오는 중…' : '수집 대기')),
    ),
  );
}

// poolBar는 세션(커넥션 풀) 사용률을 막대로 보여준다.
// 숫자만으로는 "상한에 얼마나 가까운가"가 눈에 들어오지 않는다.
function poolBar(st) {
  if (!st?.metrics) return null;
  const pct = st.metrics['connections.used_pct'];
  const total = st.metrics['connections.total'];
  const max = st.metrics['connections.max'];
  if (!pct && !total) return null;

  const ratio = pct ? Math.min(100, Math.max(0, pct.value)) : null;
  const tone = ratio === null ? 'ok' : ratio >= 90 ? 'low' : ratio >= 70 ? 'mid' : 'ok';
  // max가 20만처럼 큰 값일 수 있다(MongoDB). 자리수 구분 없이 적으면 숫자를 읽을 수 없다.
  const count = (v) => Math.round(v).toLocaleString('ko-KR');
  const detail = total && max
    ? `${count(total.value)} / ${count(max.value)}`
    : total ? `${count(total.value)}개` : '';

  return h('div.pool', {},
    h('div.pool-head', {},
      h('span', {}, '세션'),
      h('span.pool-detail', {}, detail, ratio !== null ? ` · ${ratio.toFixed(0)}%` : ''),
    ),
    ratio === null ? null : h('span.pool-bar', {},
      h('span', { class: `pool-fill is-${tone}`, style: { width: `${ratio}%` } })),
  );
}

function keyMetrics(st) {
  if (!st?.metrics) return [h('span.muted', {}, '지표 없음')];
  const out = [];
  for (const name of KEY_METRICS) {
    const m = st.metrics[name];
    if (!m) continue;
    out.push(h('div.db-metric', {},
      h('span.db-metric-label', {}, metricLabel(name)),
      h('span.db-metric-value', {}, formatMetricValue(m.value, m.unit)),
    ));
  }
  return out.length ? out : [h('span.muted', {}, '지표 없음')];
}

// troubleChips는 이상 신호를 보여준다.
//
// 0인 값을 늘어놓지 않는 이유: 항상 0인 칸이 여섯 개 있으면 0이 아닌 하나가 눈에
// 띄지 않는다. 대신 **모두 정상일 때는 그 사실을 한 줄로 말한다** —
// 아무것도 없는 자리는 "확인했고 이상 없음"과 "수집하지 않음"을 구분해 주지 못한다.
function troubleChips(st) {
  if (!st?.metrics) return null;
  const chips = [];
  let checked = 0;
  for (const name of TROUBLE_METRICS) {
    const m = st.metrics[name];
    if (!m) continue;
    checked++;
    if (m.value === 0) continue;
    chips.push(h('span.chip.chip-warn', {},
      metricLabel(name), h('b', {}, formatMetricValue(m.value, m.unit))));
  }
  if (chips.length) return h('div.db-trouble', {}, chips);
  if (checked === 0) return null; // 이 DB는 이상 신호 지표를 수집하지 않는다
  return h('div.db-trouble', {},
    h('span.db-ok', {}, icon('check', 12), `이상 신호 없음 (${checked}개 지표)`));
}

// fillLoadCharts는 카드마다 최근 6시간 부하 스파크라인을 채운다.
//
// 화면을 먼저 그린 뒤 진행하는 이유: 커넥션 수만큼 요청이 나가므로 그것들을
// 기다리면 첫 화면이 늦어진다. 실패는 해당 카드 안에만 표시한다.
async function fillLoadCharts(overview) {
  const items = (overview.items ?? []).filter((i) => i.state);
  await Promise.all(items.map(async (item) => {
    const slot = document.querySelector(
      `.db-spark[data-spark="${item.connection.id}"] .db-spark-body`);
    if (!slot) return; // 화면이 이미 바뀌었다

    const name = LOAD_METRICS.find((m) => item.state.metrics?.[m]);
    if (!name) {
      mount(slot, h('span.muted', {}, '부하 지표 없음'));
      return;
    }
    try {
      const res = await api.get(`/connections/${encodeURIComponent(item.connection.id)}/metrics`
        + `?range=6h&points=48&metrics=${encodeURIComponent(name)}`);
      const series = res.series?.[0];
      if (!series || series.points.length < 2) {
        mount(slot, h('span.muted', {}, '데이터가 모이는 중 (6시간 그래프)'));
        return;
      }
      const latest = series.points[series.points.length - 1];
      const unit = res.meta?.[0]?.unit ?? series.unit;
      mount(slot,
        sparkline(series.points, { width: 150, height: 34 }),
        h('span.db-spark-value', {}, formatMetricValue(latest.avg, unit)),
        h('span.db-spark-name', {}, `${metricLabel(name)} · 6시간`),
      );
    } catch {
      mount(slot, h('span.muted', {}, '부하를 불러오지 못했습니다'));
    }
  }));
}

// ---------- 권한 / 기능 ----------

function accessCard(items) {
  return h('section.card', {},
    h('h2.card-title', {}, '내 DB 접근 권한'),
    h('table.table', {},
      h('thead', {}, h('tr', {},
        h('th', {}, '커넥션'), h('th', {}, '환경'), h('th', {}, '종류'),
        h('th', {}, '권한'), h('th', {}, '최근 확인'),
      )),
      h('tbody', {}, items.map((i) => h('tr', {},
        h('td', {}, h('a', { href: '/connections' }, i.connection.name)),
        h('td', {}, envBadge(i.connection.environment)),
        h('td.nowrap', {}, dbLogo(i.connection.kind, 15, { inline: true }),
          kindLabel(i.connection.kind)),
        h('td', {}, i.accessible ? levelBadge(i.level) : badge('접근 불가', 'neutral')),
        h('td', {}, relativeTime(i.connection.lastCheckAt)),
      ))),
    ),
  );
}

// FEATURES는 각 기능으로 가는 입구다.
//
// 사이드바에도 같은 항목이 있지만, 처음 들어온 사람에게는 이름만으로 무엇을 하는
// 화면인지 알기 어렵다. 한 줄 설명을 붙여 어디로 가야 하는지 판단할 수 있게 한다.
const FEATURES = [
  ['/connections', 'database', 'DB 커넥션', '개발·운영 DB를 등록하고 연결을 확인한다'],
  ['/monitor', 'activity', '모니터링', '상태·부하 지표와 임계치 이벤트'],
  ['/logs', 'list', '로그 탐색', '느린 쿼리와 서버 오류를 한 화면에서'],
  ['/schema', 'database', '스키마', '구조 탐색·두 DB 비교·DDL 생성'],
  ['/nosql', 'list', 'Mongo·Redis', '컬렉션 통계·인덱스 사용·키 분포'],
  ['/erd', 'edit', 'ERD 설계', '여러 사람이 동시에 편집하는 스키마 초안'],
  ['/migrations', 'play', '마이그레이션', '계획 → 리뷰 → 실행 → 롤백'],
  ['/versions', 'refresh', '버전 이력', '적용 이력과 앱 밖에서 생긴 변경'],
  ['/vcs', 'copy', 'Git 연동', '마이그레이션을 브랜치에 커밋하고 PR 생성'],
  ['/assistant', 'settings', 'AI 어시스턴트', '질문으로 조회하고, 변경은 승인 후 실행'],
  ['/manual', 'list', '메뉴얼', '화면별 사용법과 매크로 Lua 레퍼런스'],
];

function featureGrid() {
  return h('section.card', {},
    h('h2.card-title', {}, '기능'),
    h('div.feature-grid', {}, FEATURES.map(([path, iconName, title, desc]) =>
      h('a.feature', { href: path },
        h('span.feature-icon', {}, icon(iconName, 16)),
        h('span.feature-body', {},
          h('span.feature-title', {}, title),
          h('span.feature-desc', {}, desc)),
      ))),
  );
}
