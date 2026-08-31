// 백업 — 논리 덤프 만들기, 내려받기, 복구.
//
// 이 화면의 두 동작은 위험의 방향이 정반대다. 덤프는 **데이터를 밖으로 꺼내고**,
// 복구는 **데이터를 덮어쓴다.** 그래서 만들기는 옵션을 고르는 폼이고, 복구는
// 무엇이 어디로 가는지 확인시키는 대화상자다.
import { api } from '../core/api.js';
import { withProject } from '../core/project.js';
import { state } from '../core/store.js';
import {
  h, mount, icon, select, input, checkbox, spinner, emptyState, pageHeader,
  badge, envBadge, toast, toastError, openModal, confirmDialog, field,
  formatDate, relativeTime,
} from '../core/ui.js';
import { navigate } from '../core/router.js';
import { serverDbPicker } from '../core/connpick.js';
import { codeBlock } from '../core/highlight.js';
import { errorPanel } from './users.js';

// 진행 중인 작업은 폴링으로 따라간다.
//
// 매크로처럼 SSE를 쓰지 않는 이유: 덤프가 만드는 정보는 "지금 어느 테이블의 몇 번째
// 행"이라는 덮어쓰면 되는 값 하나다. 줄이 쌓이는 로그와 달리 놓쳐도 잃을 것이 없고,
// 그런 값에 스트림 배관을 놓는 것은 코드만 늘린다.
const POLL_MS = 1200;

// 폴링 타이머는 모듈에 하나만 둔다.
//
// 화면을 다시 그리는 것은 라우터만이 아니라 타이머 자신이기도 하다(스스로를 다시
// 부른다). 그래서 라우터가 붙잡은 정리 함수는 첫 타이머만 알고 있고, 그 뒤로 이어진
// 타이머는 아무도 모르는 상태가 된다 — 화면을 떠나도 백업 목록이 계속 되살아난다.
// 타이머를 한 곳에 두면 누가 다시 그렸든 하나만 살아 있다.
let pollTimer = null;

function stopPolling() {
  if (pollTimer !== null) {
    clearTimeout(pollTimer);
    pollTimer = null;
  }
}

export async function renderBackups(outlet, params, query) {
  stopPolling();
  mount(outlet, spinner('백업 목록을 불러오는 중…'));

  let conns;
  let servers;
  let res;
  try {
    // 서버 목록을 함께 받는 이유: 커넥션을 평평하게 늘어놓으면 같은 서버의 DB가
    // 이름만 다른 항목으로 반복된다. 다른 화면과 같은 두 단계 고르개를 쓴다.
    [conns, servers, res] = await Promise.all([
      api.get(withProject('/connections/')),
      api.get(withProject('/servers/')),
      api.get(`/backups/?conn=${encodeURIComponent(query.get('conn') ?? '')}`),
    ]);
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  const reload = () => renderBackups(outlet, params, query);

  // 덤프를 만들 수 있는 커넥션: 데이터 조회 권한이 있거나 최소한 스키마를 볼 수 있는 곳.
  const usable = conns.items.filter((i) => i.accessible);
  const filterId = query.get('conn') ?? '';

  // 목록 화면이므로 "전체"가 정상 상태다. 그것을 서버 고르개의 첫 항목으로 두고,
  // 그때는 DB 고르개를 감춘다(고를 대상이 정해지지 않았으므로 뜻이 없다).
  const picker = serverDbPicker({
    usable,
    servers: servers.items ?? [],
    currentId: filterId,
    onPick: (id) => navigate(id ? `/backups?conn=${encodeURIComponent(id)}` : '/backups'),
    allLabel: '모든 커넥션',
  });

  mount(outlet,
    pageHeader('백업', '논리 덤프를 만들고 되돌립니다', [
      usable.length
        ? h('button.btn.btn-primary', {
          type: 'button', onclick: () => openCreateDialog(usable, filterId, reload),
        }, icon('save'), '백업 만들기')
        : null,
    ]),
    h('div.card.filter-bar', {},
      ...picker.nodes,
      h('div.filter-sep'),
      h('span.muted.small', {},
        `보존 ${res.retention} · 덤프 상한 ${res.maxMB}MB · 파일은 gzip으로 저장됩니다`),
    ),
    h('p.notice.notice-info', {}, icon('alert'),
      h('span', {},
        h('b', {}, '논리 덤프입니다. '),
        '구조를 만드는 문장과 값을 넣는 문장이 담긴 텍스트 파일이며, 다른 서버에 다시 세울 수 있습니다. ' +
        '파일 수준 스냅숏이나 시점 복구(PITR)가 필요하면 DB가 제공하는 도구를 ',
        h('code', {}, '-backup-cmd'),
        ' 훅으로 연결하세요.')),
    res.items.length === 0
      ? emptyState('아직 백업이 없습니다.')
      : h('div.backup-list', {}, res.items.map((b) => backupCard(b, usable, reload))),
    restoreHistory(res.restores),
  );

  // 진행 중인 작업이 있으면 주기적으로 다시 그린다.
  const running = res.items.some((b) => b.status === 'running')
    || (res.restores ?? []).some((r) => r.status === 'running');
  if (running) {
    pollTimer = setTimeout(reload, POLL_MS);
  }
  // 화면을 떠날 때 라우터가 이것을 부른다.
  return stopPolling;
}

const SCOPE_LABEL = {
  full: ['전체 (구조+데이터)', 'accent'],
  schema: ['구조만', 'info'],
  data: ['데이터만', 'warn'],
};

const STATUS_LABEL = {
  running: ['진행 중', 'info'],
  success: ['완료', 'success'],
  failed: ['실패', 'danger'],
  canceled: ['취소', 'neutral'],
};

function statusBadge(status) {
  const [label, kind] = STATUS_LABEL[status] ?? [status, 'neutral'];
  return badge(label, kind);
}

function backupCard(b, conns, reload) {
  const [scopeLabel, scopeKind] = SCOPE_LABEL[b.scope] ?? [b.scope, 'neutral'];
  const done = b.status === 'success';

  return h('article.card.backup-card', {},
    h('div.backup-head', {},
      h('b', {}, b.connectionName || '(삭제된 커넥션)'),
      statusBadge(b.status),
      badge(scopeLabel, scopeKind),
      b.options?.dropIfExists ? badge('DROP 포함', 'danger') : null,
      b.fileMissing ? badge('파일 없음', 'danger') : null,
      h('span.muted.small', {}, formatDate(b.startedAt)),
    ),
    b.note ? h('p.backup-note', {}, b.note) : null,
    b.status === 'running'
      ? h('div.backup-progress', {}, spinner(b.progress || '준비 중…'))
      : null,
    b.error ? h('p.notice.notice-danger', {}, icon('alert'), b.error) : null,
    done
      ? h('dl.backup-stats', {},
        stat('크기', formatBytes(b.sizeBytes)),
        stat('테이블', b.tableCount.toLocaleString()),
        stat('행', b.rowCount.toLocaleString()),
        stat('문장', b.statementCount.toLocaleString()),
        stat('소요', `${(b.durationMs / 1000).toFixed(1)}초`),
      )
      : null,
    h('div.backup-actions', {},
      b.status === 'running'
        ? h('button.btn.btn-small.btn-danger', {
          type: 'button',
          onclick: async () => {
            try {
              await api.post(`/backups/${b.id}/cancel`);
              toast('취소를 요청했습니다', 'info');
              reload();
            } catch (err) { toastError(err); }
          },
        }, icon('stop'), '취소')
        : null,
      done && !b.fileMissing
        ? h('a.btn.btn-small', {
          href: `/api/v1/backups/${b.id}/download`,
          // 다운로드는 새 창이 아니라 같은 창에서 파일 응답으로 받는다.
          download: '',
        }, icon('save'), '내려받기')
        : null,
      done && !b.fileMissing
        ? h('button.btn.btn-small', {
          type: 'button', onclick: () => openPreview(b),
        }, icon('code'), '내용 보기')
        : null,
      done && !b.fileMissing
        ? h('button.btn.btn-small.btn-danger', {
          type: 'button', onclick: () => openRestoreDialog(b, conns, reload),
        }, icon('refresh'), '복구')
        : null,
      b.status !== 'running'
        ? h('button.btn.btn-small.btn-danger-ghost', {
          type: 'button',
          onclick: async () => {
            const ok = await confirmDialog({
              title: '백업 삭제',
              message: `${b.connectionName} 의 ${formatDate(b.startedAt)} 백업을 파일과 함께 삭제합니다.`,
              confirmLabel: '삭제', danger: true,
            });
            if (!ok) return;
            try {
              await api.del(`/backups/${b.id}`);
              toast('백업을 삭제했습니다', 'success');
              reload();
            } catch (err) { toastError(err); }
          },
        }, icon('trash'), '삭제')
        : null,
      h('span.muted.small', {}, `${b.actorName} 실행${b.trigger === 'macro' ? ' (매크로)' : ''}`),
    ),
  );
}

function stat(label, value) {
  return h('div', {}, h('dt', {}, label), h('dd', {}, value));
}

function formatBytes(n) {
  if (!n) return '0B';
  const units = ['B', 'KB', 'MB', 'GB'];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i += 1; }
  return `${i === 0 ? v : v.toFixed(1)}${units[i]}`;
}

// ---------- 만들기 ----------

function openCreateDialog(conns, preselect, reload) {
  const connSelect = serverDbPicker({
    usable: conns,
    currentId: preselect || conns[0].connection.id,
    onPick: () => syncWarn(),
    inline: false,
  });
  const scopeSelect = select([
    { value: 'full', label: '전체 — 구조와 데이터' },
    { value: 'schema', label: '구조만 — 테이블·인덱스·제약' },
    { value: 'data', label: '데이터만 — 대상에 구조가 이미 있을 때' },
  ], { value: 'full' });
  const tablesInput = input({ placeholder: '비워두면 전부. 쉼표로 구분 (예: public.orders, users)' });
  const noteInput = input({ placeholder: '예: 스키마 변경 전 백업' });
  const dropBox = h('input', { type: 'checkbox' });

  const warn = h('div');
  const syncWarn = () => {
    const conn = conns.find((i) => i.connection.id === connSelect.value)?.connection;
    const parts = [];
    if (conn?.environment === 'prod' && scopeSelect.value !== 'schema') {
      parts.push(h('p.notice.notice-warn', {}, icon('alert'),
        '운영 DB의 데이터를 파일로 만듭니다. 만들어진 파일은 내려받을 수 있으며, ' +
        '누가 언제 내려받았는지는 감사 로그에 남습니다.'));
    }
    if (dropBox.checked) {
      parts.push(h('p.notice.notice-danger', {}, icon('alert'),
        h('span', {},
          h('b', {}, 'DROP 문이 파일에 들어갑니다. '),
          '이 백업으로 복구하면 대상의 기존 테이블이 먼저 삭제됩니다.')));
    }
    mount(warn, parts);
  };
  scopeSelect.addEventListener('change', syncWarn);
  dropBox.addEventListener('change', syncWarn);
  syncWarn();

  openModal({
    title: '백업 만들기',
    width: 620,
    body: () => [
      ...connSelect.nodes,
      field('범위', scopeSelect),
      field('대상 테이블', tablesInput, '일부만 담고 싶을 때 지정합니다'),
      h('div.field', {},
        h('label.checkbox', {}, dropBox, h('span', {}, '복구 시 기존 테이블을 지우는 DROP 문 포함')),
        h('span.field-help', {},
          '대상이 비어 있지 않은 DB에 복구하려면 대개 필요합니다. 그만큼 위험합니다.')),
      field('메모', noteInput),
      warn,
    ],
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: async (e) => {
          // currentTarget 은 이벤트가 끝나면 null 이 된다. await 뒤에서 다시 만지면
          // 그 자리에서 예외가 나고, 그러면 실패를 알리는 토스트조차 뜨지 않는다 —
          // 눌렀는데 아무 일도 일어나지 않은 것처럼 보인다.
          const pressed = e.currentTarget;
          pressed.disabled = true;
          try {
            await api.post(`/connections/${connSelect.value}/backups`, {
              scope: scopeSelect.value,
              tables: tablesInput.value.split(',').map((s) => s.trim()).filter(Boolean),
              dropIfExists: dropBox.checked,
              note: noteInput.value.trim(),
            });
            close();
            toast('백업을 시작했습니다', 'success');
            reload();
          } catch (err) {
            toastError(err);
            pressed.disabled = false;
          }
        },
      }, icon('save'), '시작'),
    ],
  });
}

// ---------- 내용 보기 ----------

async function openPreview(b) {
  const body = h('div', {}, spinner('백업을 읽는 중…'));
  openModal({ title: `백업 내용 — ${b.connectionName}`, width: 860, body: () => body });
  try {
    const res = await api.get(`/backups/${b.id}/preview`);
    const lang = { sql: 'sql', jsonl: 'json', redis: 'shell' }[b.format] ?? 'sql';
    mount(body,
      h('p.field-help', {},
        '파일의 앞부분입니다. 전체는 내려받아 확인하세요.'),
      codeBlock(res.preview, lang, { className: 'backup-preview' }),
    );
  } catch (err) {
    mount(body, errorPanel(err));
  }
}

// ---------- 복구 ----------

function openRestoreDialog(b, conns, reload) {
  // 복구 대상은 원래 커넥션이 기본이지만 다른 곳으로도 되돌릴 수 있어야 한다
  // (운영 데이터를 개발 DB로 옮겨 재현하는 것이 가장 흔한 용도다).
  // 대신 종류가 같아야 한다 — SQL 덤프를 Mongo에 부을 수는 없다.
  const targets = conns.filter((i) => i.connection.kind === b.connectionKind);
  if (targets.length === 0) {
    toast(`${b.connectionKind} 종류의 커넥션이 없어 복구할 수 없습니다`, 'error');
    return;
  }
  const connSelect = serverDbPicker({
    usable: targets,
    currentId: targets.find((i) => i.connection.id === b.connectionId)?.connection.id
      ?? targets[0].connection.id,
    onPick: () => syncConfirm(),
    serverLabel: '복구 대상 서버',
    inline: false,
  });
  const confirmInput = input({ autocomplete: 'off' });
  const confirmField = h('div');

  const syncConfirm = () => {
    const conn = targets.find((i) => i.connection.id === connSelect.value)?.connection;
    const cross = conn && conn.id !== b.connectionId;
    const parts = [];
    if (cross) {
      parts.push(h('p.notice.notice-warn', {}, icon('alert'),
        `다른 커넥션(${conn.name})에 복구합니다. 원래 백업 대상은 ${b.connectionName} 입니다.`));
    }
    if (conn?.environment === 'prod') {
      confirmInput.placeholder = conn.name;
      parts.push(h('p.notice.notice-danger', {}, icon('alert'),
        h('span', {}, h('b', {}, '운영 DB입니다. '),
          '복구는 되돌릴 수 없습니다. 계속하려면 커넥션 이름을 정확히 입력하세요.')));
      parts.push(h('label.field', {},
        h('span.field-label', {}, `확인: "${conn.name}" 입력`), confirmInput));
    }
    mount(confirmField, parts);
  };
  syncConfirm();

  openModal({
    title: '백업 복구',
    width: 640,
    body: () => [
      h('p.modal-message', {},
        `${b.connectionName} 의 ${formatDate(b.startedAt)} 백업(${SCOPE_LABEL[b.scope]?.[0] ?? b.scope}, `,
        `${b.rowCount.toLocaleString()}행)을 되돌립니다.`),
      h('p.notice.notice-danger', {}, icon('alert'),
        h('span', {},
          h('b', {}, '복구는 되돌릴 수 없습니다. '),
          b.options?.dropIfExists
            ? '이 백업에는 DROP 문이 들어 있어 대상의 기존 테이블을 먼저 지웁니다.'
            : '이 백업에는 DROP 문이 없습니다. 대상에 같은 테이블이 이미 있으면 실패합니다.')),
      ...connSelect.nodes,
      confirmField,
    ],
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-danger', {
        type: 'button',
        onclick: async (e) => {
          // currentTarget 은 이벤트가 끝나면 null 이 된다. await 뒤에서 다시 만지면
          // 그 자리에서 예외가 나고, 그러면 실패를 알리는 토스트조차 뜨지 않는다 —
          // 눌렀는데 아무 일도 일어나지 않은 것처럼 보인다.
          const pressed = e.currentTarget;
          pressed.disabled = true;
          try {
            await api.post(`/backups/${b.id}/restore`, {
              connectionId: connSelect.value,
              confirm: confirmInput.value.trim(),
            });
            close();
            toast('복구를 시작했습니다', 'success');
            reload();
          } catch (err) {
            toastError(err);
            pressed.disabled = false;
          }
        },
      }, icon('refresh'), '복구 실행'),
    ],
  });
}

function restoreHistory(restores) {
  if (!restores || restores.length === 0) return null;
  return h('section.card', {},
    h('div.card-title', {}, '복구 이력'),
    h('div.table-scroll', {},
      h('table.table', {},
        h('thead', {}, h('tr', {},
          h('th', {}, '상태'), h('th', {}, '대상'), h('th', {}, '백업'),
          h('th', {}, '진행'), h('th', {}, '실행자'), h('th', {}, '시각'))),
        h('tbody', {}, restores.map((r) => h('tr', {},
          h('td', {}, statusBadge(r.status)),
          h('td', {}, r.connectionName || '—'),
          h('td', {}, h('div.cell-main', {}, r.backupLabel),
            r.error ? h('div.cell-sub.text-danger', {}, r.error) : null,
            r.failedStatement
              ? h('details.cell-sub', {}, h('summary', {}, '멈춘 문장'),
                codeBlock(r.failedStatement, 'sql', { className: 'mig-exec-sql' }))
              : null),
          h('td', {}, r.status === 'running'
            ? (r.progress || '진행 중…')
            : `${r.statementsDone.toLocaleString()} / ${r.statementsTotal.toLocaleString()}`),
          h('td', {}, r.actorName),
          h('td', {}, h('div.cell-main', {}, formatDate(r.startedAt)),
            h('div.cell-sub', {}, relativeTime(r.startedAt))),
        ))),
      )),
  );
}
