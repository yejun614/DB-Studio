// 서버 컴퓨터: DB Studio가 도는 기계 자신의 CPU·메모리·디스크·네트워크.
//
// 커넥션 모니터링과 화면을 나눈 이유: 저 쪽은 "이 DB가 어떤가"를 묻고 여기는
// "이 컴퓨터가 어떤가"를 묻는다. 장애 때 알고 싶은 순서도 다르다 — DB가 느릴 때
// 가장 먼저 확인해야 하는 것은 디스크가 찼는지, 다른 프로세스가 CPU를 다 쓰는지다.
import { api } from '../core/api.js';
import {
  h, mount, icon, select, spinner, emptyState, pageHeader, badge,
  toast, toastError, relativeTime, formatDate, openModal, field, input, checkbox,
} from '../core/ui.js';
import { lineChart, formatBytes, formatMetricValue } from '../core/chart.js';
import { errorPanel } from './users.js';

const RANGES = [
  { value: '1h', label: '1시간' },
  { value: '6h', label: '6시간' },
  { value: '24h', label: '24시간' },
  { value: '7d', label: '7일' },
];

// 자동 갱신 주기. 수집 주기(기본 30초)보다 짧게 잡아 새 값을 놓치지 않는다.
const REFRESH_MS = 15000;

// 차트로 그릴 지표. 디스크는 마운트마다 이름이 달라 여기 없고, 응답을 보고 정한다.
const CHARTS = [
  { metric: 'host.cpu', label: 'CPU 사용률', unit: 'percent' },
  { metric: 'host.memory', label: '메모리 사용률', unit: 'percent' },
  { metric: 'host.net.rx', label: '네트워크 수신', unit: 'bytes_per_sec' },
  { metric: 'host.net.tx', label: '네트워크 송신', unit: 'bytes_per_sec' },
];

export async function renderHost(outlet) {
  mount(outlet, spinner('서버 상태를 불러오는 중…'));

  let data;
  try {
    data = await api.get('/monitor/host');
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  const rangeSelect = select(RANGES, { value: savedRange() });
  rangeSelect.addEventListener('change', () => {
    sessionStorage.setItem('dbstudio.hostRange', rangeSelect.value);
    loadCharts();
  });

  const body = h('div');
  const header = pageHeader('서버 컴퓨터', hostSubtitle(data), [
    h('label.field.field-inline', {}, h('span.field-label', {}, '기간'), rangeSelect),
    h('button.btn', {
      type: 'button',
      onclick: () => openThresholdModal(data.thresholds, (saved) => {
        data.thresholds = saved;
        toast('임계값을 저장했습니다', 'success');
      }),
    }, icon('settings'), '임계값'),
    h('a.btn', { href: '/events?kind=host' }, icon('alert'), '호스트 이벤트'),
    h('button.btn', { type: 'button', onclick: () => reload(true) }, icon('refresh'), '새로고침'),
  ]);
  mount(outlet, header, body);

  // 제자리 갱신을 위해 만들어 둔 조각들. 15초마다 화면을 통째로 다시 만들면
  // 값을 읽는 도중에 시선이 끊긴다.
  let slots = null;
  const charts = new Map(); // metric → { chart, valueEl, unit }

  build(data);
  await loadCharts();

  const timer = setInterval(() => { reload(false); }, REFRESH_MS);
  return () => clearInterval(timer);

  async function reload(showSpinner) {
    if (showSpinner) mount(body, spinner());
    try {
      const fresh = await api.get('/monitor/host');
      data = fresh;
      if (showSpinner || !slots) build(fresh);
      else update(fresh);
      await loadCharts(!showSpinner);
    } catch (err) {
      mount(body, errorPanel(err));
      slots = null;
    }
  }

  // build는 화면 뼈대를 만든다. 값이 들어갈 자리는 slots에 기억해 둔다.
  function build(d) {
    charts.clear();
    const snap = d.snapshot ?? null;

    slots = {
      cpu: h('div.stat-value', {}, '—'),
      mem: h('div.stat-value', {}, '—'),
      disk: h('div.stat-value', {}, '—'),
      net: h('div.stat-value', {}, '—'),
      memSub: h('div.stat-label', {}, '메모리'),
      diskSub: h('div.stat-label', {}, '디스크'),
      netSub: h('div.stat-label', {}, '네트워크'),
      collected: h('span', {}, '—'),
      uptime: h('span', {}, '—'),
      diskBody: h('div'),
      notes: h('div'),
      chartGrid: h('div.chart-grid'),
      events: h('div'),
    };

    const notices = [];
    if (d.enabled === false) {
      notices.push(h('p.notice.notice-warn', {}, icon('alert'),
        d.note ?? '호스트 모니터링이 꺼져 있습니다.'));
    } else if (!snap) {
      notices.push(h('p.notice.notice-info', {}, icon('activity'),
        '첫 수집을 기다리고 있습니다. 잠시 후 값이 채워집니다.'));
    } else if (d.stale) {
      notices.push(h('p.notice.notice-info', {}, icon('activity'),
        '마지막으로 저장된 값을 보여주고 있습니다. 다음 수집에서 갱신됩니다.'));
    }

    mount(body,
      ...notices,
      h('div.card.detail-head', {},
        h('div.detail-title', {},
          icon('monitor', 20),
          h('h2', {}, snap?.info?.hostname || '이 컴퓨터'),
          snap?.info ? badge(`${snap.info.os}/${snap.info.arch}`, 'neutral') : null,
          snap?.info?.cpus ? badge(`CPU ${snap.info.cpus}개`, 'neutral') : null,
        ),
        h('div.detail-stats', {},
          kv('마지막 수집', slots.collected),
          kv('가동 시간', slots.uptime),
          kv('수집 주기', d.intervalSec ? `${d.intervalSec}초` : '—'),
          kv('보존 기간', d.retentionHours ? `${d.retentionHours}시간` : '—'),
        ),
      ),
      h('div.stat-row', {},
        statTile(slots.cpu, 'CPU', 'activity'),
        statTile(slots.mem, slots.memSub, 'box'),
        statTile(slots.disk, slots.diskSub, 'database'),
        statTile(slots.net, slots.netSub, 'share'),
      ),
      slots.chartGrid,
      h('section.card', {},
        h('h2.card-title', {}, '디스크'),
        slots.diskBody,
      ),
      h('section.card', {},
        h('h2.card-title', {}, '최근 호스트 이벤트'),
        slots.events,
      ),
      slots.notes,
    );

    update(d);
    loadEvents();
  }

  // update는 값만 갈아 끼운다.
  function update(d) {
    const snap = d.snapshot;
    if (!slots) return;
    if (!snap) {
      mount(slots.diskBody, emptyState('아직 수집된 디스크 정보가 없습니다'));
      return;
    }

    slots.cpu.textContent = snap.cpuPercent === null || snap.cpuPercent === undefined
      ? '—' : `${snap.cpuPercent.toFixed(0)}%`;
    slots.collected.textContent = snap.at ? relativeTime(snap.at) : '—';
    slots.uptime.textContent = snap.info?.bootAt
      ? `${sinceBoot(snap.info.bootAt)} (${formatDate(snap.info.bootAt)} 부팅)` : '—';

    const memPct = snap.memTotal ? (snap.memUsed / snap.memTotal) * 100 : null;
    slots.mem.textContent = memPct === null ? '—' : `${memPct.toFixed(0)}%`;
    slots.memSub.textContent = snap.memTotal
      ? `메모리 ${formatBytes(snap.memUsed)} / ${formatBytes(snap.memTotal)}` : '메모리';

    const disks = (snap.disks ?? []).filter((d2) => d2.total > 0);
    const worst = disks.reduce((acc, d2) => {
      const pct = ((d2.total - d2.free) / d2.total) * 100;
      return !acc || pct > acc.pct ? { pct, mount: d2.mount } : acc;
    }, null);
    slots.disk.textContent = worst ? `${worst.pct.toFixed(0)}%` : '—';
    slots.diskSub.textContent = worst ? `디스크 ${worst.mount} 최대` : '디스크';

    const rx = snap.netRxRate ?? null;
    const tx = snap.netTxRate ?? null;
    // 수신을 큰 값으로, 송신을 아래에 둔다. 둘을 한 줄에 나란히 쓰면 어느 쪽이
    // 어느 쪽인지 매번 화살표를 확인해야 한다.
    slots.net.textContent = rx === null ? '—' : `↓ ${formatBytes(rx)}/s`;
    slots.netSub.textContent = tx === null ? '네트워크' : `네트워크 · 송신 ↑ ${formatBytes(tx)}/s`;

    mount(slots.diskBody, disks.length
      ? diskTable(disks, d.thresholds)
      : emptyState('읽을 수 있는 디스크가 없습니다'));

    const notes = [...(snap.notes ?? [])];
    if (d.osLogNote) notes.push(d.osLogNote);
    mount(slots.notes, notes.length
      ? h('section.card', {},
          h('h2.card-title', {}, '수집 참고'),
          h('ul.note-list', {}, notes.map((n) => h('li', {}, n))))
      : null);
  }

  // loadCharts는 시계열을 받아 차트를 그리거나 제자리 갱신한다.
  async function loadCharts(quiet = false) {
    const metrics = CHARTS.map((c) => c.metric);
    if (!quiet) mount(slots.chartGrid, spinner('지표를 불러오는 중…'));
    let res;
    try {
      res = await api.get(`/monitor/host/series?range=${encodeURIComponent(rangeSelect.value)}`
        + `&metrics=${encodeURIComponent(metrics.join(','))}`);
    } catch (err) {
      if (!quiet) mount(slots.chartGrid, errorPanel(err));
      return;
    }

    const byMetric = new Map((res.series ?? []).map((s) => [s.metric, s.points ?? []]));
    const drawable = CHARTS.filter((c) => (byMetric.get(c.metric) ?? []).length > 0);
    if (drawable.length === 0) {
      charts.clear();
      mount(slots.chartGrid, emptyState('아직 그릴 만큼의 지표가 모이지 않았습니다'));
      return;
    }

    // 구성이 같으면 카드를 다시 만들지 않는다(깜빡임 방지).
    const sameShape = quiet && charts.size === drawable.length
      && drawable.every((c) => charts.has(c.metric));
    if (sameShape) {
      for (const c of drawable) {
        const slot = charts.get(c.metric);
        const points = byMetric.get(c.metric);
        slot.valueEl.textContent = latestText(points, c.unit);
        slot.chart.updateSeries?.(points);
      }
      return;
    }

    charts.clear();
    mount(slots.chartGrid, drawable.map((c) => {
      const points = byMetric.get(c.metric);
      const valueEl = h('span.chart-value', {}, latestText(points, c.unit));
      const chart = lineChart({
        points, unit: chartUnit(c.unit), label: c.label, height: 150, showBand: false,
      });
      charts.set(c.metric, { chart, valueEl, unit: c.unit });
      return h('section.card.chart-card', {},
        h('header.chart-head', {},
          h('div', {}, h('h3.chart-title', {}, c.label)),
          h('div.chart-current', {}, valueEl),
        ),
        chart,
      );
    }));
  }

  async function loadEvents() {
    try {
      const res = await api.get('/monitor/events?kind=host&limit=8');
      const events = res.events ?? [];
      mount(slots.events, events.length
        ? h('ul.host-events', {}, events.map((e) => h('li', {},
            badge(severityLabel(e.severity), severityKind(e.severity)),
            h('span.host-event-msg', {}, e.message),
            h('span.muted', {}, relativeTime(e.startedAt)),
            e.state === 'resolved' ? badge('해소', 'neutral') : null,
          )))
        : emptyState('호스트 이벤트가 없습니다'));
    } catch (err) {
      mount(slots.events, errorPanel(err));
    }
  }
}

// ---------- 조각 ----------

function hostSubtitle(d) {
  const snap = d.snapshot;
  if (!snap?.info) return 'DB Studio가 실행 중인 컴퓨터';
  const parts = [snap.info.hostname, `${snap.info.os}/${snap.info.arch}`];
  if (snap.info.cpus) parts.push(`CPU ${snap.info.cpus}개`);
  return parts.filter(Boolean).join(' · ');
}

function statTile(valueEl, label, iconName) {
  return h('div.stat', {},
    h('div.stat-icon', {}, icon(iconName, 18)),
    h('div', {}, valueEl, typeof label === 'string' ? h('div.stat-label', {}, label) : label),
  );
}

function kv(label, value) {
  return h('div.kv', {}, h('span.kv-label', {}, label), h('span.kv-value', {}, value));
}

// diskTable은 마운트별 사용률을 막대와 함께 보여준다.
//
// 숫자만 늘어놓지 않는 이유: "87%"는 읽어야 알지만 막대는 훑기만 해도 보인다.
// 임계값을 넘은 것은 색으로 구분해 목록에서 바로 눈에 띄게 한다.
function diskTable(disks, thresholds) {
  const warn = thresholds?.diskWarn ?? 85;
  const crit = thresholds?.diskCrit ?? 95;
  return h('div.host-disks', {}, disks.map((d) => {
    const used = d.total - d.free;
    const pct = (used / d.total) * 100;
    const level = pct >= crit ? 'crit' : pct >= warn ? 'warn' : 'ok';
    return h('div.host-disk', {},
      h('div.host-disk-head', {},
        h('span.host-disk-mount', {}, d.mount),
        d.fs ? h('span.muted', {}, d.fs) : null,
        h('span.host-disk-pct', { class: `host-${level}` }, `${pct.toFixed(1)}%`),
      ),
      h('div.host-bar', {}, barFill(pct, level)),
      h('div.host-disk-meta.muted', {},
        `사용 ${formatBytes(used)} · 여유 ${formatBytes(d.free)} · 전체 ${formatBytes(d.total)}`),
    );
  }));
}

// barFill은 사용률 막대의 채워진 부분이다.
// 폭을 커스텀 속성으로 넘기는 이유: 색과 모양은 CSS가 정하고, JS는 값만 준다.
function barFill(pct, level) {
  const el = h('div.host-bar-fill', { class: `host-bar-${level}` });
  el.style.setProperty('--host-bar-width', `${Math.min(100, Math.max(0, pct)).toFixed(1)}%`);
  return el;
}

function latestText(points, unit) {
  if (!points?.length) return '—';
  return formatMetricValue(points[points.length - 1].avg, chartUnit(unit));
}

// chartUnit은 저장 단위를 차트 포맷터의 단위로 옮긴다.
// 지금은 이름이 같지만, 서버가 쓰는 이름과 화면 포맷터의 이름이 언제나 같아야 할
// 이유는 없으므로 옮기는 자리를 하나 둔다.
function chartUnit(unit) { return unit; }

function sinceBoot(bootAt) {
  const ms = Date.now() - new Date(bootAt).getTime();
  if (!Number.isFinite(ms) || ms < 0) return '—';
  const days = Math.floor(ms / 86400000);
  const hours = Math.floor((ms % 86400000) / 3600000);
  if (days > 0) return `${days}일 ${hours}시간`;
  const mins = Math.floor((ms % 3600000) / 60000);
  return `${hours}시간 ${mins}분`;
}

function severityLabel(sev) {
  return { critical: '심각', warning: '경고', info: '정보' }[sev] ?? sev;
}

function severityKind(sev) {
  return { critical: 'danger', warning: 'warn', info: 'neutral' }[sev] ?? 'neutral';
}

function savedRange() {
  return sessionStorage.getItem('dbstudio.hostRange') || '6h';
}

// ---------- 임계값 ----------

// openThresholdModal은 언제 알릴지를 고치는 창이다.
//
// 값을 플래그가 아니라 화면에서 고치게 한 이유: "디스크 몇 %에서 알릴 것인가"는
// 서버를 띄우는 순간이 아니라 운영하면서 정해진다.
function openThresholdModal(current, onSaved) {
  const t = current ?? {};
  const num = (value) => input({ type: 'number', min: '1', max: '100', step: '1', value: String(value ?? '') });
  const cpuWarn = num(t.cpuWarn);
  const cpuCrit = num(t.cpuCrit);
  const memWarn = num(t.memWarn);
  const memCrit = num(t.memCrit);
  const diskWarn = num(t.diskWarn);
  const diskCrit = num(t.diskCrit);
  const sustain = input({ type: 'number', min: '0', max: '3600', step: '30', value: String(t.sustainSec ?? 300) });
  const osLog = checkbox('시스템 로그의 오류를 이벤트로 만들기', { checked: t.osLogEnabled !== false });

  const save = h('button.btn.btn-primary', { type: 'button' }, '저장');
  const closeModal = openModal({
    title: '호스트 임계값',
    width: 560,
    // form-grid는 두 칸짜리 격자다. 경고와 심각을 한 줄에 나란히 두면
    // "둘은 같은 지표의 두 단계"라는 것이 배치만으로 읽힌다.
    // 배열로 넘긴다. 한 겹 감싸면 모달 본문(flex column)의 간격이 사라진다.
    body: [
      h('p.field-help', {},
        '경고를 넘으면 경고 이벤트가, 심각을 넘으면 심각 이벤트가 열립니다. '
        + '값이 정상으로 돌아오면 자동으로 해소됩니다.'),
      h('div.form-grid', {},
        field('CPU 경고 (%)', cpuWarn),
        field('CPU 심각 (%)', cpuCrit),
        field('메모리 경고 (%)', memWarn),
        field('메모리 심각 (%)', memCrit),
        field('디스크 경고 (%)', diskWarn),
        field('디스크 심각 (%)', diskCrit),
      ),
      field('지속 시간 (초)', sustain,
        'CPU·메모리가 이 시간 이상 계속 넘어야 알립니다. 백업처럼 잠깐 치솟는 작업으로 '
        + '알림이 오는 것을 막습니다. 디스크에는 적용되지 않습니다.'),
      osLog,
    ],
    footer: save,
  });

  save.addEventListener('click', async () => {
    save.disabled = true;
    try {
      const res = await api.put('/monitor/host/thresholds', {
        cpuWarn: Number(cpuWarn.value), cpuCrit: Number(cpuCrit.value),
        memWarn: Number(memWarn.value), memCrit: Number(memCrit.value),
        diskWarn: Number(diskWarn.value), diskCrit: Number(diskCrit.value),
        sustainSec: Number(sustain.value),
        osLogEnabled: osLog.querySelector('input').checked,
      });
      onSaved(res.thresholds);
      closeModal();
    } catch (err) {
      toastError(err);
      save.disabled = false;
    }
  });
}
