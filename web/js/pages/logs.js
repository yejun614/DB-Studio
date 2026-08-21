// 로그 탐색기: 시간순 로그 항목과 쿼리별 누적 통계.
//
// 두 탭으로 나눈 이유: 시간순 로그는 "언제 무슨 일이 있었나"를 찾는 데 쓰고,
// 누적 통계는 "무엇을 개선할까"를 고르는 데 쓴다. 목적이 달라 한 화면에 섞으면
// 둘 다 읽기 어려워진다.
import { api } from '../core/api.js';
import { kindInfo, kindLabel } from '../core/store.js';
import {
  h, mount, icon, select, input, checkbox, spinner, emptyState, pageHeader,
  badge, toastError, formatDate, relativeTime, copyToClipboard,
} from '../core/ui.js';
import { formatMetricValue } from '../core/chart.js';
import { navigate } from '../core/router.js';
import { serverDbPicker } from '../core/connpick.js';
import { errorPanel } from './users.js';

const RANGES = [
  { value: '15m', label: '15분' },
  { value: '1h', label: '1시간' },
  { value: '6h', label: '6시간' },
  { value: '24h', label: '24시간' },
  { value: '7d', label: '7일' },
];

export async function renderLogs(outlet, params, query) {
  mount(outlet, spinner('로그 소스를 확인하는 중…'));

  let conns;
  let meta;
  let servers;
  try {
    // 서버 목록도 함께 받는다 — 커넥션을 평평하게 늘어놓으면 같은 서버의 DB가
    // 이름만 다른 항목으로 반복된다(데이터·스키마 화면과 같은 규칙).
    [conns, meta, servers] = await Promise.all([
      api.get('/connections/'),
      api.get('/logs/meta'),
      api.get('/servers/'),
    ]);
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  // 로그를 지원하는 커넥션만 고른다. SQLite는 서버가 없어 로그 소스가 없다.
  const accessible = conns.items.filter((i) => i.accessible);
  const usable = accessible.filter((i) => kindInfo(i.connection.kind)?.capabilities?.logs);
  // 목록에서 빠진 커넥션은 이유를 적어 준다. 조용히 사라지면 "권한이 없나,
  // 등록이 안 됐나"를 커넥션 화면까지 가서 확인해야 한다.
  const excluded = accessible.filter((i) => !kindInfo(i.connection.kind)?.capabilities?.logs);

  if (usable.length === 0) {
    mount(outlet,
      pageHeader('로그', 'DB 로그 탐색과 쿼리 분석'),
      emptyState(
        '로그를 조회할 수 있는 커넥션이 없습니다. ' +
        'SQLite는 서버 로그가 없어 지원하지 않으며, 그 외 DB는 모니터링 이상 권한이 필요합니다.',
        h('a.btn', { href: '/connections' }, 'DB 커넥션으로'),
      ),
    );
    return;
  }

  const selectedId = query.get('conn') || usable[0].connection.id;
  const current = usable.find((i) => i.connection.id === selectedId) ?? usable[0];
  const activeTab = query.get('tab') === 'stats' ? 'stats' : 'entries';

  const picker = serverDbPicker({
    usable,
    servers: servers.items ?? [],
    currentId: current.connection.id,
    onPick: (id) => navigate(`/logs?conn=${encodeURIComponent(id)}&tab=${activeTab}`),
  });

  const rangeSelect = select(RANGES, { value: sessionGet('logRange', '1h') });
  const severitySelect = select(
    [{ value: '', label: '전체 심각도' },
      ...meta.severities.map((s) => ({ value: s.value, label: `${s.label} 이상` }))],
    { value: sessionGet('logSeverity', '') },
  );
  const orderSelect = select(
    meta.statsOrders.map((o) => ({ value: o.value, label: o.label })),
    { value: sessionGet('logOrder', 'total') },
  );
  const searchInput = input({ type: 'search', placeholder: '메시지 또는 쿼리 검색', value: '' });
  const regexBox = checkbox('정규식', {});
  const minDuration = input({
    type: 'number', min: '0', placeholder: '0',
    value: sessionGet('logMinDuration', ''),
  });

  // 소스 필터는 실제 가용성을 확인한 뒤 채운다.
  const sourceFilters = h('div.source-filters');
  const body = h('div');
  const sourceState = new Map(); // kind → 체크 여부

  const tabs = h('div.tabs', {},
    tabButton('시간순 로그', activeTab === 'entries', () => switchTab('entries')),
    tabButton('쿼리 통계', activeTab === 'stats', () => switchTab('stats')),
  );

  function switchTab(tab) {
    navigate(`/logs?conn=${encodeURIComponent(current.connection.id)}&tab=${tab}`);
  }

  mount(outlet,
    pageHeader('로그', 'DB 로그 탐색과 쿼리 분석', [
      h('button.btn', { type: 'button', onclick: () => load() }, icon('refresh'), '다시 조회'),
    ]),
    h('div.card.filter-bar.log-filters', {},
      ...picker.nodes,
      h('label.field.field-inline', {}, h('span.field-label', {}, '기간'), rangeSelect),
      h('label.field.field-inline', {}, h('span.field-label', {}, '심각도'), severitySelect),
      h('label.field.field-inline', {},
        h('span.field-label', {}, '최소 시간'),
        h('div.input-with-suffix', {}, minDuration, h('span.input-suffix', {}, 'ms'))),
      h('label.field.field-inline.grow', {}, icon('list'), searchInput, regexBox),
      activeTab === 'stats'
        ? h('label.field.field-inline', {}, h('span.field-label', {}, '정렬'), orderSelect)
        : null,
      h('button.btn.btn-primary', { type: 'button', onclick: () => load() }, '조회'),
    ),
    excludedNote(excluded),
    sourceFilters,
    tabs,
    body,
  );

  // 필터 변경은 세션에 기억해 화면을 다시 열어도 유지된다.
  rangeSelect.addEventListener('change', () => { sessionSet('logRange', rangeSelect.value); load(); });
  severitySelect.addEventListener('change', () => { sessionSet('logSeverity', severitySelect.value); load(); });
  orderSelect.addEventListener('change', () => { sessionSet('logOrder', orderSelect.value); load(); });
  minDuration.addEventListener('change', () => sessionSet('logMinDuration', minDuration.value));
  searchInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') load();
  });

  function buildQuery() {
    const qs = new URLSearchParams();
    qs.set('range', rangeSelect.value);
    if (severitySelect.value) qs.set('severity', severitySelect.value);
    if (searchInput.value.trim()) {
      qs.set('q', searchInput.value.trim());
      if (regexBox.querySelector('input').checked) qs.set('regex', '1');
    }
    if (minDuration.value && Number(minDuration.value) > 0) {
      qs.set('minDuration', minDuration.value);
    }
    qs.set('order', orderSelect.value);
    qs.set('limit', '300');

    // 체크 해제된 소스는 제외한다. 전부 켜져 있으면 파라미터를 생략한다.
    const selected = [...sourceState.entries()].filter(([, on]) => on).map(([k]) => k);
    if (selected.length > 0 && selected.length < sourceState.size) {
      qs.set('sources', selected.join(','));
    }
    return qs;
  }

  async function load() {
    mount(body, spinner('로그를 읽는 중…'));
    try {
      const res = await api.get(
        `/connections/${current.connection.id}/logs?${buildQuery()}`);

      renderSourceFilters(res.sources);

      const parts = [];
      if (res.notes?.length) {
        parts.push(h('div.card.notice.notice-warn', {},
          icon('alert'),
          h('div', {}, h('strong', {}, '조회 참고 사항'),
            h('ul.note-list', {}, res.notes.map((n) => h('li', {}, n))))));
      }
      if (res.truncated) {
        parts.push(h('p.notice.notice-info', {}, icon('alert'),
          '결과가 상한에 걸려 일부만 표시합니다. 기간을 좁히거나 검색어를 추가하세요.'));
      }

      if (activeTab === 'stats') {
        parts.push(...statsView(res, current.connection));
      } else {
        parts.push(...entriesView(res, meta));
      }
      mount(body, ...parts);
    } catch (err) {
      mount(body, errorPanel(err));
      toastError(err);
    }
  }

  function renderSourceFilters(sources) {
    if (!sources?.length) {
      mount(sourceFilters);
      return;
    }
    const chips = sources.map((src) => {
      // 처음 렌더링에서만 기본값을 정한다. 사용자의 선택을 덮어쓰지 않는다.
      if (!sourceState.has(src.kind)) sourceState.set(src.kind, src.available);

      const box = h('input', {
        type: 'checkbox',
        checked: sourceState.get(src.kind),
        disabled: !src.available,
        onchange: (e) => {
          sourceState.set(src.kind, e.target.checked);
          load();
        },
      });
      return h('label.source-chip', {
        class: src.available ? '' : 'source-chip-off',
        title: src.hint || '',
      },
        box,
        h('span', {}, src.label),
        src.available
          ? badge(String(src.count), src.count > 0 ? 'info' : 'neutral')
          : badge('사용 불가', 'neutral'),
      );
    });

    const unavailable = sources.filter((s) => !s.available && s.hint);
    mount(sourceFilters,
      h('div.card.source-card', {},
        h('div.source-chips', {}, chips),
        unavailable.length
          ? h('details.source-hints', {},
              h('summary', {}, `사용할 수 없는 소스 ${unavailable.length}개 — 활성화 방법`),
              h('ul.note-list', {},
                unavailable.map((s) => h('li', {},
                  h('strong', {}, `${s.label}: `), s.hint))))
          : null,
      ),
    );
  }

  await load();
}

// excludedNote는 커넥션 목록에서 빠진 것들을 이유와 함께 한 줄로 알린다.
//
// SQLite에는 서버 프로세스가 없어 에러 로그도 슬로우 쿼리 로그도 존재하지 않는다.
// 기능을 켜 두면 언제나 빈 화면이 나오므로 어댑터가 아예 Logs=false로 선언한다
// (internal/dbx/sql_kinds.go). 화면에서 그 사실을 말해 주지 않으면 "왜 없지"가 남는다.
function excludedNote(excluded) {
  if (!excluded.length) return null;
  const names = excluded.map((i) => i.connection.name);
  const shown = names.length <= 3
    ? names.join(', ')
    : `${names.slice(0, 3).join(', ')} 외 ${names.length - 3}개`;
  const kinds = [...new Set(excluded.map((i) => kindLabel(i.connection.kind)))].join(', ');
  return h('p.muted.small.log-excluded', {},
    icon('alert', 13),
    h('span', {},
      `${shown} 는 커넥션 목록에 없습니다 — ${kinds}는 서버 프로세스가 없어 `
      + '에러 로그도 슬로우 쿼리 로그도 존재하지 않습니다.'),
  );
}

function tabButton(label, active, onClick) {
  return h('button.tab', { type: 'button', class: active ? 'tab-active' : '', onclick: onClick }, label);
}

function sessionGet(key, def) {
  return sessionStorage.getItem(`dbstudio.${key}`) ?? def;
}
function sessionSet(key, value) {
  sessionStorage.setItem(`dbstudio.${key}`, value);
}

// ---------- 시간순 로그 ----------

function entriesView(res, meta) {
  if (!res.entries.length) {
    // 통계는 있는데 시간순 항목만 없는 경우가 흔하다. PostgreSQL의
    // pg_stat_statements처럼 누적 통계만 제공하는 소스가 그렇다.
    // 그때 "로그가 없다"고만 말하면 기능이 고장난 것으로 오해하게 되므로
    // 어디를 봐야 하는지 안내한다.
    if (res.stats?.length) {
      return [emptyState(
        `이 기간에 기록된 시간순 로그 항목이 없습니다. ` +
        `다만 누적 쿼리 통계 ${res.stats.length}건이 수집되어 있습니다 — ` +
        `"쿼리 통계" 탭에서 확인하세요.`,
        h('button.btn.btn-primary', {
          type: 'button',
          onclick: () => document.querySelectorAll('.tab')[1]?.click(),
        }, '쿼리 통계 보기'),
      )];
    }
    const available = (res.sources ?? []).filter((s) => s.available);
    if (available.length === 0) {
      return [emptyState(
        '사용할 수 있는 로그 소스가 없습니다. 위의 활성화 방법을 확인하세요.')];
    }
    return [emptyState('조건에 맞는 로그 항목이 없습니다. 기간을 넓히거나 필터를 완화해보세요.')];
  }

  // 심각도 분포를 먼저 보여줘 무엇을 봐야 하는지 알려준다.
  const counts = {};
  for (const e of res.entries) counts[e.severity] = (counts[e.severity] ?? 0) + 1;

  return [
    h('div.log-summary', {},
      h('span.muted', {}, `${res.entries.length}건`),
      ...meta.severities
        .filter((s) => counts[s.value])
        .map((s) => badge(`${s.label} ${counts[s.value]}`, severityTone(s.value))),
    ),
    entriesTable(res.entries),
  ];
}

// 한 번에 그리는 행 수. 첫 화면을 채우고도 남을 만큼이면 충분하다.
const PAGE_SIZE = 60;

// entriesTable은 스크롤이 끝에 닿으면 다음 묶음을 이어 붙인다.
//
// 로그 한 건은 메시지·쿼리·메타까지 담아 세로로 길다. 300건을 한 번에 그리면
// 카드 하나가 3만 픽셀이 되고, 그 DOM을 만드느라 조회 직후 화면이 멈춘다.
// 서버 쪽 커서 페이지네이션을 쓰지 않는 이유: 로그 소스(슬로우 로그 테이블,
// 프로파일러, 에러 로그)를 여러 개 읽어 시각순으로 합치는 구조라 offset을
// 주면 매 페이지마다 전부 다시 읽게 된다. 받아 온 것을 나눠 그리는 편이 맞다.
function entriesTable(entries) {
  const tbody = h('tbody');
  const status = h('span.muted');
  const moreBtn = h('button.btn.btn-small', { type: 'button' }, '더 보기');
  // 끝까지 내려온 사람이 다시 필터를 만지려면 화면 몇 개를 거슬러 올라가야 한다.
  // 표 안이 아니라 문서가 스크롤되므로 window를 올린다.
  const topBtn = h('button.link-btn', {
    type: 'button',
    onclick: () => window.scrollTo({ top: 0, behavior: 'smooth' }),
  }, '↑ 맨 위로');
  // 관찰용 표식. 화면 아래쪽에 닿기 전에 미리 채우려고 rootMargin을 넉넉히 준다.
  const sentinel = h('div.log-sentinel');
  const footer = h('div.log-more', {}, status, moreBtn, topBtn);

  let rendered = 0;
  let observer = null;

  const appendPage = () => {
    const next = entries.slice(rendered, rendered + PAGE_SIZE);
    for (const e of next) tbody.appendChild(entryRow(e));
    rendered += next.length;

    if (rendered >= entries.length) {
      status.textContent = `${entries.length}건 모두 표시됨`;
      moreBtn.style.display = 'none';
      // 한 묶음에 다 들어갔으면 아래 줄 자체가 필요 없다 — 위 요약이 이미 같은
      // 숫자를 말하고 있다.
      if (rendered <= PAGE_SIZE) footer.style.display = 'none';
      // 다 그린 뒤에도 남겨 두면 스크롤할 때마다 헛일을 한다.
      observer?.disconnect();
      sentinel.remove();
      return;
    }
    status.textContent = `${entries.length}건 중 ${rendered}건 표시`;
    // 한 묶음을 붙여도 표식이 여전히 화면 안이면(긴 화면·짧은 행) 교차 상태가
    // 바뀌지 않아 콜백이 다시 오지 않는다. 다시 관찰해 한 번 더 확인시킨다.
    //
    // rAF가 아니라 setTimeout을 쓰는 이유: rAF는 프레임이 그려질 때만 돌아간다.
    // 배경 탭처럼 프레임이 멈춘 상태에서 이 예약이 사라지면 목록이 더 이상
    // 이어지지 않는다. 여기서 필요한 것은 "지금 작업이 끝난 뒤"일 뿐이다.
    if (observer) setTimeout(reobserve, 0);
  };

  const reobserve = () => {
    if (!observer || !sentinel.isConnected) return;
    observer.unobserve(sentinel);
    observer.observe(sentinel);
  };

  moreBtn.addEventListener('click', appendPage);

  // IntersectionObserver가 없는 환경에서도 "더 보기"로 끝까지 볼 수 있다.
  if (typeof IntersectionObserver === 'function') {
    observer = new IntersectionObserver((changes) => {
      if (changes.some((c) => c.isIntersecting)) appendPage();
    }, { rootMargin: '600px 0px' });
  }

  // 카드를 먼저 조립한 뒤에 첫 묶음을 그린다. 순서를 바꾸면 다 그려서 표식을
  // 지워야 하는 경우(항목이 한 묶음보다 적을 때)에 지운 표식이 다시 붙는다.
  const card = h('div.card.log-card', {},
    h('table.table.log-table', {},
      h('thead', {}, h('tr', {},
        h('th', {}, '시각'),
        h('th', {}, '심각도'),
        h('th', {}, '소스'),
        h('th', {}, '내용'),
        h('th', {}, '소요'),
      )),
      tbody,
    ),
    sentinel,
    footer,
  );

  appendPage();
  // 관찰은 화면에 붙은 뒤에 시작한다. 아직 문서에 없는 노드를 관찰하면
  // 첫 콜백이 "보이지 않음"으로 한 번 헛돈다.
  setTimeout(reobserve, 0);

  return card;
}

function entryRow(e) {
  const hasQuery = Boolean(e.query);
  return h('tr', { class: e.severity === 'error' || e.severity === 'fatal' ? 'row-danger' : '' },
    h('td.nowrap', {},
      h('div', {}, formatDate(e.at)),
      h('div.cell-sub', {}, relativeTime(e.at)),
    ),
    h('td', {}, severityBadge(e.severity)),
    h('td', {}, badge(sourceLabel(e.source), 'neutral')),
    h('td.log-message', {},
      h('div', {}, e.message),
      hasQuery
        ? h('details.log-query', {},
            h('summary', {}, '쿼리 보기'),
            h('div.log-query-body', {},
              h('div.panel-head', {},
                h('span.muted', {}, e.digest ? `다이제스트 ${e.digest}` : ''),
                h('button.btn.btn-small', {
                  type: 'button', onclick: () => copyToClipboard(e.query),
                }, icon('copy'), '복사'),
              ),
              h('pre.sql-block', {}, e.query),
              e.normalized && e.normalized !== e.query
                ? h('div', {},
                    h('h5.log-subhead', {}, '정규화된 형태'),
                    h('pre.sql-block.sql-muted', {}, e.normalized))
                : null,
            ))
        : null,
      metaLine(e),
    ),
    h('td.nowrap', {}, e.durationMs
      ? h('strong', {}, formatMetricValue(e.durationMs, 'ms'))
      : h('span.muted', {}, '—')),
  );
}

function metaLine(e) {
  const bits = [];
  if (e.user) bits.push(`사용자 ${e.user}`);
  if (e.database) bits.push(e.database);
  if (e.client) bits.push(e.client);
  if (e.rowsExamined) bits.push(`검사 ${e.rowsExamined.toLocaleString('ko-KR')}행`);
  if (e.rowsSent) bits.push(`반환 ${e.rowsSent.toLocaleString('ko-KR')}행`);
  for (const [k, v] of Object.entries(e.extra ?? {})) {
    if (['op', 'status', 'state'].includes(k)) continue;
    bits.push(`${k}=${v}`);
  }
  if (!bits.length) return null;
  return h('div.cell-sub', {}, bits.join(' · '));
}

// ---------- 쿼리 통계 ----------

function statsView(res, conn) {
  if (!res.stats.length) {
    if (res.entries?.length) {
      return [emptyState(
        `누적 쿼리 통계가 없습니다. 대신 시간순 로그 항목 ${res.entries.length}건이 있습니다.`,
        h('button.btn.btn-primary', {
          type: 'button',
          onclick: () => document.querySelectorAll('.tab')[0]?.click(),
        }, '시간순 로그 보기'),
      )];
    }
    return [emptyState(
      '쿼리 통계가 없습니다. 소스 필터에서 "누적 쿼리 통계"가 사용 가능한지 확인하세요.')];
  }

  const totalMs = res.stats.reduce((sum, s) => sum + s.totalMs, 0);
  const totalCalls = res.stats.reduce((sum, s) => sum + s.calls, 0);

  return [
    h('div.log-summary', {},
      h('span.muted', {}, `${res.stats.length}개 쿼리 형태`),
      badge(`총 ${formatMetricValue(totalMs, 'ms')}`, 'info'),
      badge(`${totalCalls.toLocaleString('ko-KR')}회 호출`, 'neutral'),
      h('span.muted', {}, `정렬: ${orderLabel(res.orderBy)}`),
    ),
    h('p.notice.notice-info', {}, icon('activity'),
      '리터럴 값만 다른 쿼리는 하나로 묶어 집계했습니다. ' +
      '총 소요 시간이 큰 쿼리부터 개선하면 효과가 가장 큽니다.'),
    h('div.card', {},
      h('table.table.stats-table', {},
        h('thead', {}, h('tr', {},
          h('th.col-rank', {}, '#'),
          h('th', {}, '쿼리'),
          h('th.nowrap', {}, '호출'),
          h('th.nowrap', {}, '총 시간'),
          h('th.nowrap', {}, '평균'),
          h('th.nowrap', {}, '최대'),
          h('th.nowrap', {}, '비중'),
        )),
        h('tbody', {}, res.stats.map((s, i) => statRow(s, i, totalMs))),
      ),
    ),
    h('p.muted.small', {}, `대상: ${conn.name}`),
  ];
}

function statRow(s, index, totalMs) {
  const share = totalMs > 0 ? (s.totalMs / totalMs) * 100 : 0;
  return h('tr', {},
    h('td.col-rank', {}, String(index + 1)),
    h('td.stat-query', {},
      h('details', {},
        h('summary', {}, h('code.stat-preview', {}, shorten(s.normalized, 120))),
        h('div.log-query-body', {},
          h('div.panel-head', {},
            h('span.muted', {}, `다이제스트 ${s.digest}`),
            h('button.btn.btn-small', {
              type: 'button', onclick: () => copyToClipboard(s.sample || s.normalized),
            }, icon('copy'), '복사'),
          ),
          h('pre.sql-block', {}, s.normalized),
          s.sample
            ? h('div', {},
                h('h5.log-subhead', {}, '실행 예시 (리터럴 포함)'),
                h('pre.sql-block.sql-muted', {}, s.sample))
            : null,
          statDetails(s),
        )),
    ),
    h('td.nowrap', {}, s.calls.toLocaleString('ko-KR')),
    h('td.nowrap', {}, h('strong', {}, formatMetricValue(s.totalMs, 'ms'))),
    h('td.nowrap', {}, formatMetricValue(s.meanMs, 'ms')),
    h('td.nowrap', {}, s.maxMs ? formatMetricValue(s.maxMs, 'ms') : h('span.muted', {}, '—')),
    h('td.nowrap', {}, shareBar(share)),
  );
}

function statDetails(s) {
  const rows = [];
  if (s.rowsTotal) rows.push(['반환 행 수', s.rowsTotal.toLocaleString('ko-KR')]);
  if (s.rowsPerCall) rows.push(['호출당 행 수', s.rowsPerCall.toFixed(1)]);
  if (s.cacheHitPct) rows.push(['캐시 적중률', `${s.cacheHitPct.toFixed(1)}%`]);
  if (s.minMs) rows.push(['최소 시간', formatMetricValue(s.minMs, 'ms')]);
  if (s.stddevMs) rows.push(['표준편차', formatMetricValue(s.stddevMs, 'ms')]);
  if (s.database) rows.push(['데이터베이스', s.database]);
  if (s.user) rows.push(['사용자', s.user]);
  if (s.firstSeen) rows.push(['최초 관측', formatDate(s.firstSeen)]);
  if (s.lastSeen) rows.push(['최근 관측', formatDate(s.lastSeen)]);
  for (const [k, v] of Object.entries(s.extra ?? {})) {
    rows.push([extraLabel(k), v]);
  }
  if (!rows.length) return null;

  return h('dl.stat-details', {}, rows.map(([k, v]) =>
    h('div.meta-row', {}, h('dt', {}, k), h('dd', {}, v))));
}

function extraLabel(key) {
  return {
    rowsExamined: '검사한 행 수',
    examinedPerSent: '반환 대비 검사 배수',
    noIndexUsed: '인덱스 미사용 횟수',
    logicalReads: '논리 읽기',
    bufferGets: '버퍼 조회',
  }[key] ?? key;
}

function shareBar(pct) {
  return h('div.share-bar', { title: `${pct.toFixed(1)}%` },
    h('div.share-fill', { style: { width: `${Math.min(100, pct)}%` } }),
    h('span.share-label', {}, `${pct.toFixed(1)}%`),
  );
}

function shorten(s, max) {
  const one = (s ?? '').replace(/\s+/g, ' ').trim();
  return one.length > max ? `${one.slice(0, max)}…` : one;
}

// ---------- 라벨 ----------

function severityBadge(severity) {
  const map = {
    debug: ['디버그', 'neutral'],
    info: ['정보', 'info'],
    warning: ['경고', 'warn'],
    error: ['오류', 'danger'],
    fatal: ['치명적', 'danger'],
  };
  const [label, tone] = map[severity] ?? [severity, 'neutral'];
  return badge(label, tone);
}

function severityTone(severity) {
  return { debug: 'neutral', info: 'info', warning: 'warn', error: 'danger', fatal: 'danger' }[severity] ?? 'neutral';
}

function sourceLabel(kind) {
  return {
    slow_query: '슬로우 쿼리',
    error_log: '서버 로그',
    profiler: '프로파일러',
    slowlog: 'SLOWLOG',
    statements: '누적 통계',
    current: '실행 중',
  }[kind] ?? kind;
}

function orderLabel(order) {
  return {
    total: '총 소요 시간', mean: '평균 소요 시간',
    calls: '호출 횟수', max: '최대 소요 시간',
  }[order] ?? order;
}
