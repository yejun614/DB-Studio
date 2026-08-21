// 메시지 브로커(RabbitMQ · Kafka) 관리 화면.
//
// DB 화면과 나눈 이유: 여기에는 테이블도 SQL도 없다. 대신 큐·토픽·컨슈머 그룹이 있고,
// 운영자가 이 화면에서 답을 얻고 싶은 질문은 셋이다 — 브로커는 괜찮은가, 메시지가
// 쌓이고 있나, 그것을 줄이는 소비자가 있나.
//
// 스토리지 화면과 나눈 이유: 스토리지의 첫 질문은 "얼마나 찼나"지만, 브로커의 첫 질문은
// "쌓이고 있나"다. 큐 깊이·랙은 그 자체로 좋지도 나쁘지도 않다 — 소비자가 따라오고
// 있으면 정상이고, 따라오지 못하면 장애의 전조다.
import { api } from '../core/api.js';
import { state, kindLabel } from '../core/store.js';
import {
  h, mount, icon, select, spinner, emptyState, pageHeader,
  badge, envBadge, formatDate, relativeTime, toast, toastError, confirmDialog, openModal,
} from '../core/ui.js';
import { formatBytes } from '../core/chart.js';
import { navigate } from '../core/router.js';
import { errorPanel } from './users.js';

export async function renderBroker(outlet, params, query) {
  mount(outlet, spinner('커넥션 목록을 불러오는 중…'));

  let conns;
  try {
    conns = await api.get('/connections/');
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  // 브로커 능력이 켜진 종류만 고른다. 종류 이름을 화면에서 나열하면
  // 종류가 늘 때마다 여기도 고쳐야 한다.
  const usable = conns.items.filter((i) => i.accessible
    && state.meta?.dbKinds?.find((k) => k.kind === i.connection.kind)?.capabilities?.broker);

  if (usable.length === 0) {
    mount(outlet,
      pageHeader('메시지 브로커', 'RabbitMQ · Kafka 클러스터 관리'),
      emptyState('등록된 메시지 브로커가 없습니다. RabbitMQ 또는 Kafka를 커넥션으로 등록하세요.',
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
    navigate(`/broker?conn=${encodeURIComponent(connSelect.value)}`);
  });

  const body = h('div');

  mount(outlet,
    pageHeader('메시지 브로커', `${conn.name} 클러스터를 관리합니다`, [
      h('button.btn', { type: 'button', onclick: () => load() }, icon('refresh'), '다시 읽기'),
    ]),
    h('div.card.filter-bar', {},
      h('label.field.field-inline', {}, h('span.field-label', {}, '브로커'), connSelect),
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
      const res = await api.get(`/connections/${conn.id}/broker`);
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

  if (feats.queues) out.push(queuesCard(conn, feats.write));
  if (feats.exchanges) out.push(exchangesCard(conn));
  if (feats.connections) out.push(connectionsCard(conn, feats.write));
  if (feats.topics) out.push(topicsCard(conn));
  if (feats.groups) out.push(groupsCard(conn));
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
  return h('div.card', {},
    h('div.card-title', {},
      h('span', {}, '브로커 상태'),
      healthBadge(ov.health),
      ov.version ? h('span.muted.small', {}, ov.version) : null,
    ),
    // 적체가 먼저다. 브로커에서 가장 자주 확인하는 것이고, 소비자가 따라오지
    // 못하는 순간부터는 다른 모든 지표보다 급한 문제가 된다.
    h('div.storage-capacity', {},
      h('div.storage-capacity-text', {},
        h('strong', {}, `밀린 메시지 ${ov.backlog?.toLocaleString('ko-KR') ?? 0}개`),
        h('span.muted', {}, `소비자 ${ov.consumers?.toLocaleString('ko-KR') ?? 0}명이 처리 중`),
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

// ---------- RabbitMQ 큐 ----------

function queuesCard(conn, canWrite) {
  const card = h('div.card');
  const draw = async () => {
    mount(card, h('h2.card-title', {}, '큐'), spinner('목록을 읽는 중…'));
    try {
      const res = await api.get(`/connections/${conn.id}/broker/queues`);
      const queues = res.queues ?? [];
      mount(card,
        h('div.card-title', {}, h('span', {}, '큐'),
          h('span.muted.small', {}, `${queues.length}개`)),
        queues.length === 0
          ? emptyState('큐가 없습니다.')
          : h('table.table', {},
            h('thead', {}, h('tr', {},
              h('th', {}, '이름'), h('th', {}, '상태'), h('th', {}, '대기'),
              h('th', {}, '미확인'), h('th', {}, '소비자'), h('th', {}, '초당 발행 · 소비'),
              h('th', {}, '메모리'), canWrite ? h('th', {}, '') : null)),
            h('tbody', {}, queues.map((q) => h('tr', { class: q.starved ? 'is-starved' : '' },
              h('td', {}, h('span', { title: `vhost ${q.vhost}` }, q.name)),
              h('td', {}, queueState(q.state)),
              h('td', {}, q.ready.toLocaleString('ko-KR')),
              h('td', {}, q.unacked.toLocaleString('ko-KR')),
              h('td', {}, q.consumers.toLocaleString('ko-KR')),
              h('td', {}, `${q.publishRate?.toFixed(1) ?? '-'} · ${q.deliverRate?.toFixed(1) ?? '-'}`),
              h('td', {}, q.memory ? formatBytes(q.memory) : '-'),
              canWrite
                ? h('td.row-actions', {},
                  h('button.icon-btn', {
                    type: 'button', title: '메시지 비우기',
                    onclick: () => confirmPurge(conn, q, draw),
                  }, icon('trash', 13)),
                  h('button.icon-btn', {
                    type: 'button', title: '큐 삭제',
                    onclick: () => confirmDeleteQueue(conn, q, draw),
                  }, icon('x', 13)),
                )
                : null,
            )))),
      );
    } catch (err) {
      mount(card, h('h2.card-title', {}, '큐'), errorPanel(err));
    }
  };
  draw();
  return card;
}

function queueState(s) {
  if (s === 'running') return badge('정상', 'success');
  if (s === 'flow' || s === 'blocked') return badge(s, 'warn');
  return badge(s ?? '-', 'neutral');
}

async function confirmPurge(conn, q, done) {
  const ok = await confirmDialog({
    title: '메시지 비우기',
    message: `${q.name} 큐의 메시지 ${q.total.toLocaleString('ko-KR')}개를 모두 버립니다. 되돌릴 수 없습니다.`,
    confirmLabel: '비우기',
    danger: true,
    requireText: q.name,
  });
  if (!ok) return;
  try {
    await api.post(`/connections/${conn.id}/broker/purge`, { vhost: q.vhost, name: q.name });
    toast('큐를 비웠습니다', 'success');
    done();
  } catch (err) {
    toastError(err);
  }
}

async function confirmDeleteQueue(conn, q, done) {
  const ok = await confirmDialog({
    title: '큐 삭제',
    message: `${q.name} 큐를 지웁니다. 바인딩과 메시지가 함께 사라지며 되돌릴 수 없습니다.`,
    confirmLabel: '삭제',
    danger: true,
    requireText: q.name,
  });
  if (!ok) return;
  try {
    await api.post(`/connections/${conn.id}/broker/delete-queue`, { vhost: q.vhost, name: q.name });
    toast('큐를 삭제했습니다', 'success');
    done();
  } catch (err) {
    toastError(err);
  }
}

// ---------- RabbitMQ 익스체인지 ----------

function exchangesCard(conn) {
  const card = h('div.card');
  const draw = async () => {
    mount(card, h('h2.card-title', {}, '익스체인지'), spinner('목록을 읽는 중…'));
    try {
      const res = await api.get(`/connections/${conn.id}/broker/exchanges`);
      const exchanges = res.exchanges ?? [];
      mount(card,
        h('div.card-title', {}, h('span', {}, '익스체인지'),
          h('span.muted.small', {}, `${exchanges.length}개`)),
        exchanges.length === 0
          ? emptyState('익스체인지가 없습니다.')
          : h('table.table', {},
            h('thead', {}, h('tr', {},
              h('th', {}, '이름'), h('th', {}, '종류'), h('th', {}, '지속성'),
              h('th', {}, '초당 들어옴 · 나감'))),
            h('tbody', {}, exchanges.map((e) => h('tr', {},
              h('td', {}, e.name),
              h('td', {}, h('code', {}, e.type)),
              h('td', {}, e.durable ? badge('durable', 'neutral') : badge('일시', 'warn')),
              h('td', {}, `${e.inRate?.toFixed(1) ?? '-'} · ${e.outRate?.toFixed(1) ?? '-'}`),
            )))),
      );
    } catch (err) {
      mount(card, h('h2.card-title', {}, '익스체인지'), errorPanel(err));
    }
  };
  draw();
  return card;
}

// ---------- RabbitMQ 연결 ----------

function connectionsCard(conn, canWrite) {
  const card = h('div.card');
  const draw = async () => {
    mount(card, h('h2.card-title', {}, '클라이언트 연결'), spinner('목록을 읽는 중…'));
    try {
      const res = await api.get(`/connections/${conn.id}/broker/connections`);
      const connections = res.connections ?? [];
      mount(card,
        h('div.card-title', {}, h('span', {}, '클라이언트 연결'),
          h('span.muted.small', {}, `${connections.length}개`)),
        connections.length === 0
          ? emptyState('연결된 클라이언트가 없습니다.')
          : h('table.table', {},
            h('thead', {}, h('tr', {},
              h('th', {}, '사용자'), h('th', {}, 'vhost'), h('th', {}, '상태'),
              h('th', {}, '채널'), h('th', {}, '피어'), h('th', {}, '프로토콜'),
              h('th', {}, '연결 시각'), canWrite ? h('th', {}, '') : null)),
            h('tbody', {}, connections.map((c) => h('tr', {},
              h('td', {}, c.user),
              h('td', {}, c.vhost),
              h('td', {}, c.state),
              h('td', {}, c.channels),
              h('td', {}, c.peer),
              h('td', {}, c.protocol || '-'),
              h('td', {}, c.connectedAt && !c.connectedAt.startsWith('0001')
                ? h('span', { title: formatDate(c.connectedAt) }, relativeTime(c.connectedAt)) : '-'),
              canWrite
                ? h('td.row-actions', {},
                  h('button.icon-btn', {
                    type: 'button', title: '연결 끊기',
                    onclick: () => confirmCloseConnection(conn, c, draw),
                  }, icon('x', 13)),
                )
                : null,
            )))),
      );
    } catch (err) {
      mount(card, h('h2.card-title', {}, '클라이언트 연결'), errorPanel(err));
    }
  };
  draw();
  return card;
}

async function confirmCloseConnection(conn, c, done) {
  const ok = await confirmDialog({
    title: '연결 끊기',
    message: `${c.user} (${c.peer}) 연결을 끊습니다. 클라이언트는 즉시 재연결을 시도할 수 있습니다.`,
    confirmLabel: '끊기',
    danger: true,
  });
  if (!ok) return;
  try {
    await api.post(`/connections/${conn.id}/broker/close-connection`, { name: c.name });
    toast('연결을 끊었습니다', 'success');
    done();
  } catch (err) {
    toastError(err);
  }
}

// ---------- Kafka 토픽 ----------

function topicsCard(conn) {
  const card = h('div.card');
  const draw = async () => {
    mount(card, h('h2.card-title', {}, '토픽'), spinner('목록을 읽는 중…'));
    try {
      const res = await api.get(`/connections/${conn.id}/broker/topics?limit=200`);
      const topics = res.topics ?? [];
      mount(card,
        h('div.card-title', {}, h('span', {}, '토픽'),
          h('span.muted.small', {}, `${topics.length}개`)),
        topics.length === 0
          ? emptyState('토픽이 없습니다.')
          : h('table.table', {},
            h('thead', {}, h('tr', {},
              h('th', {}, '이름'), h('th', {}, '파티션'), h('th', {}, '복제'),
              h('th', {}, '보관 메시지'), h('th', {}, '복제 부족'), h('th', {}, '오프라인'))),
            h('tbody', {}, topics.map((t) => h('tr', {},
              h('td', {},
                t.internal
                  ? h('span', { title: '카프카 내부 토픽' }, t.name)
                  : h('button.link-btn', {
                    type: 'button',
                    onclick: () => openTopicConfig(conn, t),
                  }, t.name)),
              h('td', {}, t.partitions),
              h('td', {}, t.replicationFactor),
              h('td', {}, t.messages < 0 ? '-' : t.messages.toLocaleString('ko-KR')),
              h('td', {}, t.underReplicated
                ? badge(t.underReplicated, 'warn') : h('span.muted', {}, '0')),
              h('td', {}, t.offline
                ? badge(t.offline, 'danger') : h('span.muted', {}, '0')),
            )))),
      );
    } catch (err) {
      mount(card, h('h2.card-title', {}, '토픽'), errorPanel(err));
    }
  };
  draw();
  return card;
}

// openTopicConfig는 토픽 설정을 모달로 보여준다.
async function openTopicConfig(conn, t) {
  const body = h('div');
  openModal({
    title: t.name,
    width: 560,
    body: () => [body],
  });
  mount(body, spinner('설정을 읽는 중…'));
  try {
    const res = await api.get(`/connections/${conn.id}/broker/topics/${encodeURIComponent(t.name)}/config`);
    const entries = Object.entries(res.config ?? {});
    mount(body,
      h('p.field-help', {}, `파티션 ${t.partitions}개 · 복제 ${t.replicationFactor} · `
        + `보관 메시지 ${t.messages < 0 ? '알 수 없음' : t.messages.toLocaleString('ko-KR')}개`),
      entries.length === 0
        ? emptyState('설정이 없습니다.')
        : h('table.table', {},
          h('thead', {}, h('tr', {}, h('th', {}, '키'), h('th', {}, '값'))),
          h('tbody', {}, entries.map(([k, v]) => h('tr', {},
            h('td', {}, h('code', {}, k)),
            h('td', {}, v || h('span.muted', {}, '(기본값)')))))),
    );
  } catch (err) {
    mount(body, errorPanel(err));
  }
}

// ---------- Kafka 컨슈머 그룹 ----------

function groupsCard(conn) {
  const card = h('div.card');
  const draw = async () => {
    mount(card, h('h2.card-title', {}, '컨슈머 그룹'), spinner('목록을 읽는 중…'));
    try {
      const res = await api.get(`/connections/${conn.id}/broker/groups`);
      const groups = res.groups ?? [];
      mount(card,
        h('div.card-title', {}, h('span', {}, '컨슈머 그룹'),
          h('span.muted.small', {}, `${groups.length}개`)),
        groups.length === 0
          ? emptyState('컨슈머 그룹이 없습니다.')
          : h('table.table', {},
            h('thead', {}, h('tr', {},
              h('th', {}, '이름'), h('th', {}, '상태'), h('th', {}, '멤버'),
              h('th', {}, '랙'), h('th', {}, '토픽별 랙'))),
            h('tbody', {}, groups.map((g) => h('tr', { class: g.lag > 0 && g.members === 0 ? 'is-starved' : '' },
              h('td', {}, g.name),
              h('td', {}, groupState(g.state)),
              h('td', {}, g.members),
              h('td', {}, g.lag > 0
                ? badge(g.lag.toLocaleString('ko-KR'), g.members === 0 ? 'warn' : 'neutral')
                : h('span.muted', {}, '0')),
              h('td', {}, (g.topics ?? []).slice(0, 3).map((tl) =>
                h('span.muted.small', { style: 'margin-right:8px' },
                  `${tl.topic} ${tl.lag.toLocaleString('ko-KR')}`)),
              ),
            )))),
      );
    } catch (err) {
      mount(card, h('h2.card-title', {}, '컨슈머 그룹'), errorPanel(err));
    }
  };
  draw();
  return card;
}

function groupState(s) {
  if (s === 'Stable') return badge('정상', 'success');
  if (s === 'Empty') return badge('비어 있음', 'neutral');
  if (s === 'Dead') return badge('죽음', 'danger');
  if (s === 'PreparingRebalance' || s === 'CompletingRebalance' || s === 'Assigning') {
    return badge('재조정 중', 'warn');
  }
  return badge(s ?? '-', 'neutral');
}
