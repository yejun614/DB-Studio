// 클러스터 화면 (슈퍼 어드민 전용).
//
// 이 화면이 답해야 하는 질문은 셋이다: 어떤 서버들이 참여하고 있는가, 그 서버들의 데이터가
// 서로 같은가(복제 지연), 그리고 각 서버 컴퓨터는 지금 괜찮은가. 마지막 하나를 여기 둔
// 이유는 분산 배치에서 "서버 컴퓨터" 화면이 자기 자신만 보여 주기 때문이다 — 노드가 셋이면
// 세 화면을 돌아다녀야 하고, 그러면 아무도 보지 않게 된다.
import { api } from '../core/api.js';
import {
  h, mount, icon, spinner, badge, pageHeader, toast, toastError,
  formatDate, relativeTime, confirmDialog,
} from '../core/ui.js';
import { errorPanel } from './users.js';

export async function renderCluster(outlet) {
  mount(outlet, spinner('클러스터 상태를 불러오는 중…'));

  let data;
  try {
    data = await api.get('/cluster/');
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  const status = data.status ?? {};
  if (!status.enabled) {
    mount(outlet,
      pageHeader('클러스터', '여러 서버의 DB Studio를 하나처럼 다룹니다'),
      offPanel());
    return;
  }

  const reload = () => renderCluster(outlet);
  mount(outlet,
    pageHeader('클러스터', roleLine(status)),
    statusCard(status),
    h('div.card', {},
      h('h2.card-title', {}, '노드'),
      h('p.field-help', {},
        '마스터가 메타 데이터베이스의 주인입니다. 리플리카는 그 내용을 복제해 조회를 처리하고, '
        + '변경은 마스터로 넘깁니다.'),
      h('div.node-grid', {}, (data.nodes ?? []).map((n) => nodeCard(n, reload))),
    ),
  );
}

function roleLine(status) {
  const role = status.role === 'master' ? '마스터' : '리플리카';
  return `이 노드는 ${role} 입니다 — ${status.nodeName}`;
}

function offPanel() {
  return h('div.card', {},
    h('h2.card-title', {}, '단일 서버로 동작 중'),
    h('p.muted', {},
      '이 서버는 클러스터에 참여하고 있지 않습니다. 여러 서버를 함께 쓰려면 '
      + '한 대를 마스터로, 나머지를 리플리카로 띄우세요.'),
    // 줄 끝의 쉘 이어쓰기는 소스에서 '\\\n' 으로 적는다(역슬래시 한 글자 + 줄바꿈).
    // '\\n' 으로 적으면 줄바꿈이 아니라 역슬래시와 n 두 글자가 화면에 나오고,
    // 그것을 복사한 명령은 실행되지 않는다.
    h('pre.code-block', {},
      '# 마스터\n'
      + 'DBSTUDIO_CLUSTER_SECRET=<공용 비밀> \\\n'
      + 'DBSTUDIO_MASTER_KEY=<공용 암호화 키> \\\n'
      + 'dbstudio -cluster-role=master -cluster-advertise=http://master:8080\n\n'
      + '# 리플리카\n'
      + 'DBSTUDIO_CLUSTER_SECRET=<같은 비밀> \\\n'
      + 'DBSTUDIO_MASTER_KEY=<같은 암호화 키> \\\n'
      + 'dbstudio -cluster-role=replica -cluster-master=http://master:8080 \\\n'
      + '  -cluster-advertise=http://replica-1:8080'),
    h('p.field-help', {},
      '비밀은 환경변수로만 받습니다. 명령줄 인자는 프로세스 목록에 그대로 보이고, '
      + '이 값 하나면 클러스터의 모든 데이터를 받아 갈 수 있습니다.'),
    h('p.field-help', {},
      '마스터 암호화 키는 모든 노드가 같아야 합니다. 주지 않으면 노드마다 새로 만들고, '
      + '그 노드는 복제받은 DB 자격증명을 복호화하지 못합니다 — 복제는 정상으로 보이는데 '
      + '그 노드에서만 접속이 실패합니다.'),
  );
}

// statusCard는 이 노드에서 본 복제 상태다.
function statusCard(status) {
  const rows = [
    ['역할', status.role === 'master' ? '마스터' : '리플리카'],
    ['노드 이름', status.nodeName],
    ['이 노드 주소', status.address || '(알리지 않음)'],
  ];
  if (status.role === 'replica') {
    rows.push(['마스터 주소', status.masterUrl || '']);
    rows.push(['복제 지점', `${status.appliedSeq} / ${status.masterSeq}`]);
    if (status.lastSyncAt) {
      rows.push(['마지막 동기화', `${formatDate(status.lastSyncAt)} (${relativeTime(status.lastSyncAt)})`]);
    }
  } else {
    rows.push(['복제 지점', String(status.masterSeq)]);
  }

  return h('div.card', {},
    h('div.card-title', {},
      h('span', {}, '복제 상태'),
      lagBadge(status),
    ),
    status.lastError
      ? h('p.notice.notice-error', {}, status.lastError)
      : null,
    h('dl.cluster-meta', {}, rows.map(([k, v]) =>
      h('div.meta-row', {}, h('dt', {}, k), h('dd', {}, v)))),
    status.role === 'replica'
      ? h('p.field-help', {},
        '이 노드에서 무언가를 바꾸면 마스터에 저장된 뒤 곧바로 되돌아옵니다. '
        + '마스터에 닿지 못하면 조회는 계속되고 변경만 거부됩니다.')
      : null,
  );
}

// lagBadge는 지연을 한눈에 보여준다. 숫자는 "복제 로그 몇 줄이 밀렸는가"다.
function lagBadge(status) {
  if (status.role === 'master') return badge('기준 노드', 'info');
  if (!status.connected) return badge('연결 끊김', 'danger');
  if (status.lag === 0) return badge('동기화됨', 'success');
  if (status.lag < 50) return badge(`${status.lag}건 지연`, 'warn');
  return badge(`${status.lag}건 지연`, 'danger');
}

function nodeCard(node, reload) {
  const host = node.host ?? null;
  const isMaster = node.role === 'master';
  return h('div.node-card', { class: node.stale ? 'is-stale' : '' },
    h('div.node-head', {},
      h('span.node-name', {}, icon(isMaster ? 'database' : 'copy', 14), node.name),
      isMaster ? badge('마스터', 'info') : badge('리플리카', 'neutral'),
      node.isMe ? badge('이 노드', 'success') : null,
      node.status === 'left' ? badge('내려감', 'neutral') : null,
      node.stale ? badge('소식 끊김', 'danger') : null,
    ),
    h('dl.cluster-meta', {},
      metaRow('주소', node.address || '(알리지 않음)'),
      metaRow('버전', `${node.version || '?'} · ${node.platform || '?'}`),
      metaRow('마지막 연락', node.lastSeenAt
        ? `${formatDate(node.lastSeenAt)} (${relativeTime(node.lastSeenAt)})` : '-'),
      isMaster ? null : metaRow('복제 지연', node.lag > 0 ? `${node.lag}건` : '없음'),
    ),
    host ? hostLine(host) : h('p.field-help', {}, '이 노드의 컴퓨터 상태가 아직 오지 않았습니다.'),
    // 마스터는 목록에서 내릴 수 없다. 내리는 순간 이 클러스터에는 쓰기를 받을 노드가
    // 사라지고, 그 사실은 다음 저장에서야 드러난다. 마스터를 바꾸는 일은 역할을 바꿔
    // 다시 띄우는 것이지 목록에서 지우는 것이 아니다.
    node.isMe || isMaster ? null : h('div.node-actions', {},
      h('button.btn.btn-sm.btn-danger', {
        type: 'button',
        onclick: async () => {
          const ok = await confirmDialog({
            title: '노드 내리기',
            message: `"${node.name}" 를 목록에서 내립니다. 그 노드가 담당하던 DB는 `
              + '다른 노드에서 다시 지정해야 합니다.',
            confirmLabel: '내리기',
            danger: true,
          });
          if (!ok) return;
          try {
            await api.del(`/cluster/nodes/${encodeURIComponent(node.id)}`);
            toast('노드를 목록에서 내렸습니다', 'success');
            reload();
          } catch (err) {
            toastError(err);
          }
        },
      }, '내리기'),
    ),
  );
}

function metaRow(k, v) {
  return h('div.meta-row', {}, h('dt', {}, k), h('dd', {}, v));
}

// hostLine은 그 서버 컴퓨터의 상태를 한 줄로 줄인다.
//
// 자세한 그래프를 여기 두지 않는 이유: 이 화면의 목적은 "어느 노드를 들여다봐야 하는가"를
// 고르는 것이다. 고른 뒤에는 그 노드의 [서버 컴퓨터] 화면이 시계열을 보여준다.
function hostLine(host) {
  // 값의 모양은 hostmon.Snapshot 그대로다(서버가 하트비트에 실어 보낸 원문).
  // 비율을 여기서 계산하는 이유: 저장된 것은 바이트이고, 그것을 화면이 다시 계산하면
  // "서버 컴퓨터" 화면과 같은 숫자가 나온다 — 두 화면이 다른 수를 말하면 아무도 믿지 않는다.
  const cpu = typeof host?.cpuPercent === 'number' ? host.cpuPercent : null;
  const mem = host?.memTotal ? (host.memUsed / host.memTotal) * 100 : null;
  const disks = (host?.disks ?? []).map((d) => ({
    mount: d.mount,
    used: d.total ? ((d.total - d.free) / d.total) * 100 : null,
  })).filter((d) => d.used !== null);
  // 가장 찬 디스크만 보여준다. 노드마다 디스크가 여럿이면 카드가 목록이 되고,
  // 목록이 되는 순간 "어느 노드를 봐야 하는가"라는 이 화면의 질문이 흐려진다.
  const worst = disks.reduce((a, d) => (a === null || d.used > a.used ? d : a), null);
  return h('div.node-host', {},
    gauge('CPU', cpu),
    gauge('메모리', mem),
    gauge(worst ? `디스크 ${worst.mount}` : '디스크', worst?.used ?? null),
  );
}

function gauge(label, value) {
  const pct = typeof value === 'number' ? Math.max(0, Math.min(100, value)) : null;
  const level = pct === null ? '' : pct >= 90 ? 'is-critical' : pct >= 75 ? 'is-warn' : '';
  return h('div.node-gauge', { class: level },
    h('span.node-gauge-label', {}, label),
    h('div.node-gauge-bar', {}, h('div.node-gauge-fill', { style: `width:${pct ?? 0}%` })),
    h('span.node-gauge-value', {}, pct === null ? '-' : `${pct.toFixed(0)}%`),
  );
}
