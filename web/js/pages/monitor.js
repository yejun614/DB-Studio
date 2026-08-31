// 모니터링 대시보드: 커넥션 상태 그리드 → 지표 차트 → 이벤트 타임라인.
import { api } from '../core/api.js';
import { withProject } from '../core/project.js';
import { state, kindLabel } from '../core/store.js';
import {
  h, mount, icon, select, spinner, emptyState, pageHeader,
  badge, envBadge, toast, toastError, relativeTime, formatDate, confirmDialog,
} from '../core/ui.js';
import { lineChart, sparkline, formatMetricValue, formatBytes } from '../core/chart.js';
import { dbLogo } from '../core/dblogo.js';
import { navigate } from '../core/router.js';
import { errorPanel } from './users.js';

const RANGES = [
  { value: '15m', label: '15분' },
  { value: '1h', label: '1시간' },
  { value: '6h', label: '6시간' },
  { value: '24h', label: '24시간' },
  { value: '7d', label: '7일' },
  { value: '30d', label: '30일' },
];

// 자동 갱신 주기. 폴링 간격보다 약간 짧게 잡아 새 데이터를 놓치지 않는다.
const REFRESH_MS = 15000;

// 카드 스파크라인에 쓸 지표. 앞에 있는 것부터 실제로 수집된 것을 고른다.
// response_time을 먼저 두는 이유: 모든 DB 종류에서 수집되므로 카드마다 다른
// 지표가 그려져 서로 비교가 안 되는 일을 줄인다.
const SPARK_METRICS = ['response_time', 'query.rate', 'connections.total'];
const SPARK_RANGE = '6h';

// 스파크라인 응답 캐시. 개요는 15초마다 다시 그리지만 6시간 구간 그래프는
// 그 사이에 눈에 띄게 변하지 않는다. 커넥션 수만큼 나가는 요청을 매번
// 반복하지 않도록 짧게 캐시하고, 캐시가 있으면 즉시 그린다.
const SPARK_TTL_MS = 60_000;
const sparkCache = new Map();

export async function renderMonitor(outlet, params, query) {
  mount(outlet, spinner('모니터링 상태를 불러오는 중…'));

  let data;
  try {
    data = await api.get(withProject('/monitor/overview'));
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  const selectedId = query.get('conn') || '';
  const body = h('div');
  let timer = null;

  const header = pageHeader('모니터링', `${data.config.intervalSec}초 주기로 수집`, [
    h('a.btn', { href: '/events' }, icon('alert'), '이벤트'),
    // DB가 느릴 때 다음으로 보는 곳이 "이 컴퓨터가 어떤가"다. 메뉴로 돌아가지 않고
    // 여기서 바로 건너갈 수 있어야 한다. 권한이 없으면 서버가 막으므로 감춘다.
    state.permissions?.manageConnections
      ? h('a.btn', { href: '/monitor/host' }, icon('monitor'), '서버 컴퓨터') : null,
    h('a.btn', { href: '/monitor/rules' }, icon('settings'), '감시 룰'),
    h('button.btn', { type: 'button', onclick: () => load(true) }, icon('refresh'), '새로고침'),
  ]);

  mount(outlet, header, body);

  // 상세 화면이 자기 자신을 갱신하는 방법. 주기 갱신은 이것만 부른다.
  let refreshDetail = null;

  async function load(showSpinner = false) {
    if (showSpinner) mount(body, spinner());
    try {
      const fresh = await api.get(withProject('/monitor/overview'));
      if (selectedId) {
        refreshDetail = await renderDetail(body, fresh, selectedId);
      } else {
        refreshDetail = null;
        mount(body, ...overview(fresh));
        // 화면을 먼저 그린 뒤 채운다. 커넥션 수만큼 요청이 나가므로
        // 이것들을 기다리면 상태 그리드가 늦게 뜬다.
        fillSparklines(fresh, showSpinner);
      }
    } catch (err) {
      mount(body, errorPanel(err));
    }
  }

  await load();

  // 주기 갱신은 값만 바꾼다.
  //
  // 예전에는 15초마다 화면을 통째로 다시 만들었다. 차트가 사라졌다 스피너가 떴다
  // 다시 그려지므로, 그래프를 들여다보는 동안 계속 눈이 끊겼다. 상세 화면은
  // 차트 데이터만 받아 제자리에서 갱신하고, 나머지(이벤트·스냅샷 목록)는
  // 사람이 새로고침을 누를 때만 다시 읽는다.
  timer = setInterval(() => {
    if (refreshDetail) refreshDetail();
    else load(false);
  }, REFRESH_MS);
  return () => clearInterval(timer);
}

// ---------- 개요 ----------

function overview(data) {
  const parts = [];
  const { summary, items } = data;

  parts.push(h('div.stat-row', {},
    statTile('열린 심각', summary.openCritical, 'alert', summary.openCritical > 0 ? 'danger' : null),
    statTile('열린 경고', summary.openWarning, 'alert', summary.openWarning > 0 ? 'warn' : null),
    statTile('미확인', summary.unacked, 'list'),
    statTile('24시간 이벤트', summary.last24h, 'activity'),
    statTile('감시 대상', items.length, 'database'),
    statTile('응답 없음', items.filter((i) => i.state && !i.state.up).length, 'x',
      items.some((i) => i.state && !i.state.up) ? 'danger' : null),
  ));

  if (items.length === 0) {
    parts.push(emptyState('모니터링할 커넥션이 없습니다',
      h('a.btn', { href: '/connections' }, 'DB 커넥션 등록')));
    return parts;
  }

  const pending = items.filter((i) => !i.state);
  if (pending.length > 0) {
    parts.push(h('p.notice.notice-info', {}, icon('activity'),
      `${pending.length}개 커넥션은 아직 첫 수집을 기다리고 있습니다.`));
  }

  parts.push(h('div.monitor-grid', {}, items.map(connectionCard)));
  return parts;
}

function statTile(label, value, iconName, kind) {
  return h(`div.stat${kind ? `.stat-${kind}` : ''}`, {},
    h('div.stat-icon', {}, icon(iconName, 18)),
    h('div', {}, h('div.stat-value', {}, value), h('div.stat-label', {}, label)),
  );
}

function connectionCard(item) {
  const c = item.connection;
  const st = item.state;
  const open = item.openEvents ?? {};

  const statusDot = !st
    ? h('span.dot.dot-unknown', { title: '수집 대기 중' })
    : st.up
      ? h('span.dot.dot-ok', { title: `정상 · ${relativeTime(st.lastPolledAt)}` })
      : h('span.dot.dot-fail', { title: st.lastError || '응답 없음' });

  // 카드에는 주요 지표만 몇 개 보여주고 상세는 클릭해서 본다.
  const highlights = [];
  if (st?.metrics) {
    for (const name of ['response_time', 'connections.total', 'connections.used_pct',
      'query.rate', 'cache.hit_ratio', 'size.data']) {
      const m = st.metrics[name];
      if (!m) continue;
      highlights.push(h('div.metric-chip', {},
        h('span.metric-chip-label', {}, metricLabel(name)),
        h('span.metric-chip-value', {}, formatMetricValue(m.value, m.unit)),
      ));
    }
  }

  return h('article.monitor-card', {
    class: st && !st.up ? 'monitor-card-down' : '',
    onclick: () => navigate(`/monitor?conn=${encodeURIComponent(c.id)}`),
  },
    h('header.monitor-card-head', {},
      h('div.conn-title', {}, statusDot, dbLogo(c.kind, 17), h('h3', {}, c.name)),
      h('div.monitor-card-badges', {},
        envBadge(c.environment),
        open.critical ? badge(`심각 ${open.critical}`, 'danger') : null,
        open.warning ? badge(`경고 ${open.warning}`, 'warn') : null,
      ),
    ),
    h('div.monitor-card-meta', {},
      h('span.muted', {}, kindLabel(c.kind)),
      st ? h('span.muted', {}, `수집 ${relativeTime(st.lastPolledAt)}`) : h('span.muted', {}, '수집 대기'),
    ),
    st && !st.up
      ? h('p.conn-error', {}, icon('alert'),
          st.lastError || '응답 없음',
          st.consecutiveFails > 1 ? h('span.muted', {}, ` (연속 ${st.consecutiveFails}회)`) : null)
      : null,
    highlights.length ? h('div.metric-chips', {}, highlights) : null,
    // 스파크라인 자리를 미리 잡아 나중에 채워도 카드 높이가 흔들리지 않게 한다.
    st ? h('div.monitor-spark', { dataset: { spark: c.id } },
      h('div.monitor-spark-body', {}, sparkContent(c.id) ?? h('span.muted', {}, '추이 불러오는 중…')))
      : null,
    st?.notes?.length
      ? h('details.collect-notes', {},
          h('summary', {}, `수집 참고 ${st.notes.length}건`),
          h('ul.note-list', {}, st.notes.map((n) => h('li', {}, n))))
      : null,
  );
}

// sparkContent는 캐시된 추이를 카드에 넣을 노드로 만든다.
// 캐시가 없으면 null. 그 자리는 fillSparklines가 채운다.
function sparkContent(connID) {
  const hit = sparkCache.get(connID);
  if (!hit) return null;
  if (hit.message) return h('span.muted', {}, hit.message);
  return [
    h('div.monitor-spark-head', {},
      h('span.monitor-spark-name', {}, `${metricLabel(hit.metric)} · 6시간`),
      h('span.monitor-spark-value', {}, formatMetricValue(hit.latest, hit.unit)),
    ),
    sparkline(hit.points, { width: 260, height: 40 }),
  ];
}

// fillSparklines는 카드마다 최근 6시간 추이를 채운다.
//
// 개요는 15초마다 다시 그리므로 매번 커넥션 수만큼 요청을 보내면 낭비가 크다.
// 캐시가 살아 있으면 요청 없이 이미 그려진 것을 그대로 두고, 만료된 것만 받는다.
// force(수동 새로고침)일 때는 캐시를 무시한다.
async function fillSparklines(overviewData, force = false) {
  const now = Date.now();
  const items = (overviewData.items ?? []).filter((i) => i.state);

  await Promise.all(items.map(async (item) => {
    const connID = item.connection.id;
    const cached = sparkCache.get(connID);
    if (!force && cached && now - cached.at < SPARK_TTL_MS) return; // 이미 그려져 있다

    const name = SPARK_METRICS.find((m) => item.state.metrics?.[m]);
    if (!name) {
      writeSpark(connID, { at: now, message: '추이 지표 없음' });
      return;
    }
    try {
      const res = await api.get(`/connections/${encodeURIComponent(connID)}/metrics`
        + `?range=${SPARK_RANGE}&points=48&metrics=${encodeURIComponent(name)}`);
      // 서버가 정의한 라벨을 캐시해 카드 표기가 상세 화면과 어긋나지 않게 한다.
      state.monitorMeta = state.monitorMeta ?? {};
      for (const m of res.meta ?? []) state.monitorMeta[m.name] = m.label;

      const series = res.series?.[0];
      if (!series || series.points.length < 2) {
        writeSpark(connID, { at: now, message: '데이터가 모이는 중 (6시간)' });
        return;
      }
      const points = series.points;
      writeSpark(connID, {
        at: now,
        metric: name,
        unit: res.meta?.[0]?.unit ?? series.unit,
        points,
        latest: points[points.length - 1].avg,
      });
    } catch {
      // 한 커넥션의 실패가 다른 카드까지 비우지 않도록 카드 안에만 적는다.
      writeSpark(connID, { at: now, message: '추이를 불러오지 못했습니다' });
    }
  }));
}

// writeSpark는 캐시를 갱신하고, 해당 카드가 아직 화면에 있으면 다시 그린다.
function writeSpark(connID, entry) {
  sparkCache.set(connID, entry);
  const slot = document.querySelector(
    `.monitor-spark[data-spark="${CSS.escape(connID)}"] .monitor-spark-body`);
  if (!slot) return; // 화면이 이미 바뀌었다
  mount(slot, sparkContent(connID));
}

export function metricLabel(name) {
  const found = state.monitorMeta?.[name];
  if (found) return found;
  const fallback = {
    response_time: '응답', 'connections.total': '세션',
    'connections.used_pct': '세션 사용률', 'query.rate': '쿼리',
    'cache.hit_ratio': '캐시', 'size.data': '데이터',
  };
  return fallback[name] ?? name;
}

// ---------- 커넥션 상세 ----------

async function renderDetail(body, overviewData, connID) {
  const item = overviewData.items.find((i) => i.connection.id === connID);
  if (!item) {
    mount(body, errorPanel({ message: '이 커넥션에 접근할 수 없습니다' }));
    return;
  }
  const conn = item.connection;
  const st = item.state;

  const rangeSelect = select(RANGES, { value: sessionRange() });
  rangeSelect.addEventListener('change', () => {
    sessionStorage.setItem('dbstudio.monitorRange', rangeSelect.value);
    loadCharts();
  });

  const chartsBox = h('div.chart-grid');
  const eventsBox = h('div');
  const snapshotsBox = h('div');
  // 이미 그려진 차트를 지표 이름으로 찾아 제자리 갱신한다.
  // (loadCharts보다 먼저 선언해야 한다 — 첫 호출이 이 줄보다 위에서 일어난다.)
  const drawn = new Map(); // metric → { chart, valueEl, unit }

  // 스키마가 없는 종류(하둡·Ceph)에는 드리프트 확인도 스냅샷 이력도 뜻이 없다.
  // 버튼만 남겨 두면 눌렀을 때 "지원하지 않습니다"가 돌아오고, 그 순간 사용자는
  // 자기가 뭘 잘못했는지 찾기 시작한다.
  const hasSchema = state.meta?.dbKinds
    ?.find((k) => k.kind === conn.kind)?.capabilities?.introspect !== false;
  const driftBtn = h('button.btn', { type: 'button' }, icon('refresh'), '스키마 변경 확인');
  driftBtn.addEventListener('click', async () => {
    driftBtn.disabled = true;
    const original = driftBtn.textContent;
    driftBtn.textContent = '확인 중…';
    try {
      const res = await api.post(`/connections/${connID}/drift/check`);
      toast(res.changed
        ? '스키마 변경이 감지되어 새 스냅샷을 저장했습니다'
        : '스키마가 마지막 스냅샷과 동일합니다',
        res.changed ? 'warn' : 'success');
      await loadSnapshots();
    } catch (err) {
      toastError(err);
    } finally {
      driftBtn.disabled = false;
      driftBtn.textContent = original;
    }
  });

  mount(body,
    h('div.card.detail-head', {},
      h('div.detail-title', {},
        h('a.btn.btn-small', { href: '/monitor' }, '← 전체'),
        dbLogo(conn.kind, 20),
        h('h2', {}, conn.name),
        envBadge(conn.environment),
        badge(kindLabel(conn.kind), 'neutral'),
        st ? (st.up ? badge('정상', 'success') : badge('응답 없음', 'danger')) : badge('수집 대기', 'neutral'),
      ),
      h('div.detail-actions', {},
        h('label.field.field-inline', {}, h('span.field-label', {}, '기간'), rangeSelect),
        hasSchema ? driftBtn : null,
      ),
      st ? h('div.detail-stats', {},
        kv('마지막 수집', relativeTime(st.lastPolledAt)),
        kv('마지막 정상', relativeTime(st.lastOkAt)),
        kv('응답 시간', formatMetricValue(st.latencyMs, 'ms')),
        st.consecutiveFails ? kv('연속 실패', `${st.consecutiveFails}회`) : null,
      ) : null,
    ),
    chartsBox,
    h('section.card', {}, h('h2.card-title', {}, '최근 이벤트'), eventsBox),
    hasSchema ? h('section.card', {}, h('h2.card-title', {}, '스키마 스냅샷 이력'), snapshotsBox) : null,
  );

  await Promise.all([loadCharts(), loadEvents(), hasSchema ? loadSnapshots() : null]);

  // 주기 갱신이 부를 함수. 지표만 조용히 다시 읽는다.
  return () => { loadCharts(true); };

  async function loadCharts(quiet = false) {
    // 조용한 갱신(주기 실행)에서는 스피너를 띄우지 않는다. 15초마다 화면이
    // 사라졌다 나타나는 것이 바로 그 깜빡임의 정체였다.
    if (!quiet) mount(chartsBox, spinner('지표를 불러오는 중…'));
    try {
      const res = await api.get(
        `/connections/${connID}/metrics?range=${encodeURIComponent(rangeSelect.value)}`);
      if (!res.series.length) {
        drawn.clear();
        mount(chartsBox, emptyState('아직 수집된 지표가 없습니다. 첫 폴링을 기다려주세요.'));
        return;
      }
      // 메타를 전역에 캐시해 카드 라벨이 서버 정의를 따르게 한다.
      state.monitorMeta = state.monitorMeta ?? {};
      for (const m of res.meta) state.monitorMeta[m.name] = m.label;

      // 지표 구성이 같으면 카드를 다시 만들지 않는다. 지표가 늘거나 줄었을 때만
      // 새로 그린다(수집 대상이 바뀌는 것은 드물고, 그때는 다시 그려야 맞다).
      const sameShape = quiet && drawn.size === res.series.length
        && res.series.every((s) => drawn.has(s.metric));

      if (sameShape) {
        for (const s of res.series) {
          const slot = drawn.get(s.metric);
          const latest = s.points.length ? s.points[s.points.length - 1].avg : null;
          slot.valueEl.textContent = formatMetricValue(latest, slot.unit);
          slot.chart.updateSeries?.(s.points);
        }
        return;
      }

      drawn.clear();
      const cards = res.series.map((s, i) => {
        const meta = res.meta[i] ?? { label: s.metric, unit: s.unit, help: '' };
        const latest = s.points.length ? s.points[s.points.length - 1].avg : null;
        const valueEl = h('span.chart-value', {}, formatMetricValue(latest, meta.unit));
        const chart = lineChart({
          points: s.points, unit: meta.unit, label: meta.label,
          height: 160, showBand: s.bucketSec > 60,
        });
        drawn.set(s.metric, { chart, valueEl, unit: meta.unit });
        return h('section.card.chart-card', {},
          h('header.chart-head', {},
            h('div', {},
              h('h3.chart-title', {}, meta.label),
              meta.help ? h('p.chart-help', {}, meta.help) : null,
            ),
            h('div.chart-current', {},
              valueEl,
              s.source === 'hourly' ? badge('시간 평균', 'neutral') : null,
            ),
          ),
          chart,
        );
      });
      mount(chartsBox, ...cards);
    } catch (err) {
      // 조용한 갱신이 실패하면 이미 그려진 차트를 지우지 않는다.
      // 잠깐의 네트워크 오류로 보고 있던 그래프가 사라지는 편이 더 나쁘다.
      if (!quiet) mount(chartsBox, errorPanel(err));
    }
  }

  async function loadEvents() {
    mount(eventsBox, spinner());
    try {
      const res = await api.get(`/monitor/events?connectionId=${encodeURIComponent(connID)}&limit=20`);
      mount(eventsBox, res.events.length
        ? eventTable(res.events, res.connectionNames, () => { loadEvents(); })
        : emptyState('이벤트가 없습니다'));
    } catch (err) {
      mount(eventsBox, errorPanel(err));
    }
  }

  async function loadSnapshots() {
    mount(snapshotsBox, spinner());
    try {
      const res = await api.get(`/connections/${connID}/snapshots?limit=20`);
      if (!res.snapshots.length) {
        mount(snapshotsBox, emptyState('스냅샷이 없습니다. 첫 확인 후 기준선이 저장됩니다.'));
        return;
      }
      mount(snapshotsBox, h('table.table', {},
        h('thead', {}, h('tr', {},
          h('th', {}, '시각'), h('th', {}, '출처'), h('th', {}, '지문'), h('th', {}, '변경 내역'),
        )),
        h('tbody', {}, res.snapshots.map((s) => h('tr', {},
          h('td.nowrap', {}, formatDate(s.capturedAt)),
          h('td', {}, badge(snapshotSourceLabel(s.source), s.source === 'monitor' ? 'info' : 'accent')),
          h('td', {}, h('code.fingerprint', {}, s.fingerprint.slice(0, 12))),
          h('td.detail-cell', {}, s.changeSummary?.length
            ? h('details', {},
                h('summary', {}, `${s.changeSummary.length}건`),
                h('ul.note-list', {}, s.changeSummary.map((cs) => h('li', {}, cs))))
            : h('span.muted', {}, '—')),
        ))),
      ));
    } catch (err) {
      mount(snapshotsBox, errorPanel(err));
    }
  }
}

function kv(label, value) {
  return h('div.kv', {}, h('span.kv-label', {}, label), h('span.kv-value', {}, value));
}

function sessionRange() {
  return sessionStorage.getItem('dbstudio.monitorRange') || '1h';
}

function snapshotSourceLabel(source) {
  return { monitor: '자동 수집', manual: '수동 확인', migration: '마이그레이션' }[source] ?? source;
}

// ---------- 이벤트 화면 ----------

export async function renderEvents(outlet, params, query) {
  const stateFilter = select(
    [{ value: 'open', label: '열림' }, { value: 'resolved', label: '해소' }, { value: '', label: '전체' }],
    { value: query.get('state') ?? 'open' },
  );
  const severityFilter = select(
    [{ value: '', label: '전체 심각도' }, { value: 'critical', label: '심각' },
      { value: 'warning', label: '경고' }, { value: 'info', label: '정보' }],
    { value: query.get('severity') ?? '' },
  );
  const kindFilter = select(
    [{ value: '', label: '전체 종류' }, { value: 'threshold', label: '임계치' },
      { value: 'connectivity', label: '접속' }, { value: 'drift', label: '스키마 변경' },
      { value: 'host', label: '서버 컴퓨터' }],
    { value: query.get('kind') ?? '' },
  );

  const body = h('div');
  mount(outlet,
    pageHeader('이벤트', 'DB 상태 변화와 임계치 위반 이력', [
      h('a.btn', { href: '/monitor' }, '← 모니터링'),
      h('button.btn', { type: 'button', onclick: () => load() }, icon('refresh'), '새로고침'),
    ]),
    h('div.card.filter-bar', {},
      h('label.field.field-inline', {}, h('span.field-label', {}, '상태'), stateFilter),
      h('label.field.field-inline', {}, h('span.field-label', {}, '심각도'), severityFilter),
      h('label.field.field-inline', {}, h('span.field-label', {}, '종류'), kindFilter),
    ),
    body,
  );

  for (const el of [stateFilter, severityFilter, kindFilter]) {
    el.addEventListener('change', load);
  }

  async function load() {
    mount(body, spinner());
    const qs = new URLSearchParams();
    if (stateFilter.value) qs.set('state', stateFilter.value);
    if (severityFilter.value) qs.set('severity', severityFilter.value);
    if (kindFilter.value) qs.set('kind', kindFilter.value);
    qs.set('limit', '100');
    try {
      const res = await api.get(withProject(`/monitor/events?${qs}`));
      mount(body, res.events.length
        ? h('div.card', {}, eventTable(res.events, res.connectionNames, load))
        : emptyState('조건에 맞는 이벤트가 없습니다'));
    } catch (err) {
      mount(body, errorPanel(err));
    }
  }

  await load();
}

function eventTable(events, names, reload) {
  return h('table.table.event-table', {},
    h('thead', {}, h('tr', {},
      h('th', {}, '심각도'),
      h('th', {}, '시각'),
      h('th', {}, '커넥션'),
      h('th', {}, '내용'),
      h('th', {}, '상태'),
      h('th.col-actions', {}, ''),
    )),
    h('tbody', {}, events.map((e) => eventRow(e, names, reload))),
  );
}

function eventRow(e, names, reload) {
  const isOpen = e.state === 'open';
  return h('tr', { class: isOpen && e.severity === 'critical' ? 'row-danger' : '' },
    h('td', {}, severityBadge(e.severity)),
    h('td.nowrap', {},
      h('div', {}, formatDate(e.startedAt)),
      e.occurrences > 1 ? h('div.cell-sub', {}, `${e.occurrences}회 발생`) : null,
    ),
    // 호스트 이벤트에는 커넥션이 없다. 빈 칸으로 두면 "커넥션을 못 찾았다"로 읽히므로
    // 무엇에 대한 이벤트인지 적는다.
    h('td', {}, names?.[e.connectionId]
      ?? (e.kind === 'host'
        ? h('a', { href: '/monitor/host' }, '서버 컴퓨터')
        : h('span.muted', {}, '—'))),
    h('td', {},
      h('div', {}, e.message),
      h('div.cell-sub', {},
        badge(eventKindLabel(e.kind), 'neutral'),
        e.metric ? h('code.event-metric', {}, e.metric) : null,
        e.threshold !== null && e.threshold !== undefined
          ? h('span.muted', {}, ` 임계 ${e.threshold}`) : null,
      ),
      e.detail?.changes?.length
        ? h('details.event-detail', {},
            h('summary', {}, '변경 내역'),
            h('ul.note-list', {}, e.detail.changes.map((cs) => h('li', {}, cs))),
            e.detail.truncated ? h('p.muted', {}, `그 외 ${e.detail.truncated}건`) : null)
        : null,
      // 시스템 로그 이벤트는 여러 줄을 한 건으로 묶는다. 요약만 보이면
      // "무슨 오류인지"를 확인하러 서버에 들어가야 한다.
      e.detail?.lines?.length
        ? h('details.event-detail', {},
            h('summary', {}, `기록된 줄 ${e.detail.lines.length}건`),
            h('ul.note-list', {}, e.detail.lines.map((line) => h('li', {}, line))))
        : null,
      // 스키마 드리프트는 "앱 밖에서 누가 바꿨다"는 뜻이다. 그 상태를 이력에
      // 남기지 않으면 이후의 마이그레이션 기준이 실제와 어긋난 채로 남는다.
      //
      // 이미 닫힌 이벤트에는 등록 버튼을 내놓지 않는다. 등록하면 이벤트가 함께
      // 해소되므로, 남겨 두면 "할 일이 끝난 자리에 남은 버튼"이 되고 한 번 더
      // 누르게 만든다(두 번째 등록은 바뀐 것이 없어 아무 일도 하지 않는다).
      e.kind === 'drift' && e.connectionId && isOpen
        ? h('div.cell-actions', {},
            h('button.btn.btn-small', {
              type: 'button',
              onclick: () => registerExternalEdit(e.connectionId, reload),
            }, icon('plus'), '외부 편집으로 버전 등록'),
            h('a.btn.btn-small', { href: `/versions?conn=${encodeURIComponent(e.connectionId)}` },
              '버전 이력'))
        : null,
    ),
    h('td', {},
      isOpen ? badge('열림', 'warn') : badge('해소', 'success'),
      e.ackedAt ? h('div.cell-sub', {}, `확인 ${relativeTime(e.ackedAt)}`) : null,
      e.resolvedAt ? h('div.cell-sub', {}, `해소 ${relativeTime(e.resolvedAt)}`) : null,
    ),
    h('td.col-actions', {},
      h('div.row-actions', {},
        isOpen && !e.ackedAt ? h('button.icon-btn', {
          type: 'button', title: '확인 처리',
          onclick: async () => {
            try {
              await api.post(`/monitor/events/${e.id}/ack`);
              toast('확인 처리했습니다', 'success', 2000);
              reload();
            } catch (err) { toastError(err); }
          },
        }, icon('check')) : null,
        isOpen ? h('button.icon-btn', {
          type: 'button', title: '수동 해소',
          onclick: async () => {
            const ok = await confirmDialog({
              title: '이벤트 해소',
              message: '이 이벤트를 수동으로 닫습니다. 조건이 계속 위반되면 다시 열립니다.',
              confirmLabel: '해소',
            });
            if (!ok) return;
            try {
              await api.post(`/monitor/events/${e.id}/resolve`);
              toast('해소 처리했습니다', 'success', 2000);
              reload();
            } catch (err) { toastError(err); }
          },
        }, icon('x')) : null,
      ),
    ),
  );
}

// registerExternalEdit는 드리프트로 감지된 현재 상태를 버전으로 확정한다.
// 이벤트 화면에서 한 번에 처리할 수 있어야 후속 조치가 실제로 이뤄진다.
async function registerExternalEdit(connectionID, reload) {
  try {
    const res = await api.post(`/connections/${encodeURIComponent(connectionID)}/versions`, {
      note: '스키마 변경 이벤트 후속 등록',
    });
    if (res.created) {
      const resolved = res.resolvedEvents
        ? ` · 관련 이벤트 ${res.resolvedEvents}건 해소`
        : '';
      toast(`v${res.version.versionNo} 을 외부 편집으로 등록했습니다 `
        + `(변경 ${res.changes?.length ?? 0}건)${resolved}`, 'success');
    } else {
      toast(res.message ?? '이전 버전과 구조가 같아 새 버전을 만들지 않았습니다', 'info');
    }
    reload?.();
  } catch (err) {
    toastError(err);
  }
}

function severityBadge(severity) {
  const map = {
    critical: ['심각', 'danger'],
    warning: ['경고', 'warn'],
    info: ['정보', 'info'],
  };
  const [label, tone] = map[severity] ?? [severity, 'neutral'];
  return badge(label, tone);
}

function eventKindLabel(kind) {
  return {
    threshold: '임계치', connectivity: '접속', drift: '스키마 변경',
    collect_error: '수집 오류', host: '서버 컴퓨터',
  }[kind] ?? kind;
}

export { formatBytes, sparkline };
