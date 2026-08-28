// 마이그레이션: 목록 → 상세(변경 목록·SQL·리뷰) → 사전 검사 → 실행/롤백.
import { api } from '../core/api.js';
import { state, kindLabel } from '../core/store.js';
import {
  h, mount, icon, select, input, textarea, spinner, emptyState, pageHeader,
  badge, envBadge, toast, toastError, relativeTime, formatDate, openModal,
  copyToClipboard,
} from '../core/ui.js';
import { navigate } from '../core/router.js';
import { serverDbPicker } from '../core/connpick.js';
import { peoplePicker } from '../core/searchpick.js';
import { codeBlock } from '../core/highlight.js';
import { errorPanel } from './users.js';
import { openPushDialog } from './vcs.js';

// 상태 표시. 색은 "지금 무엇을 해야 하는가"를 나타낸다:
// 초안/리뷰는 중립, 승인은 행동 가능, 적용은 완료, 실패/반려는 주의.
const STATUS = {
  draft: ['초안', 'neutral'],
  in_review: ['리뷰 중', 'info'],
  approved: ['승인됨', 'accent'],
  rejected: ['반려됨', 'warn'],
  applied: ['적용됨', 'success'],
  rolled_back: ['롤백됨', 'neutral'],
  failed: ['실패', 'danger'],
  closed: ['닫힘', 'neutral'],
};

export function migrationStatusBadge(status) {
  const [label, kind] = STATUS[status] ?? [status, 'neutral'];
  return badge(label, kind);
}

// ---------- 목록 ----------

export async function renderMigrations(outlet, params, query) {
  mount(outlet, spinner('마이그레이션을 불러오는 중…'));

  const status = query.get('status') ?? '';
  let res;
  try {
    res = await api.get(`/migrations/${status ? `?status=${encodeURIComponent(status)}` : ''}`);
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  const statusFilter = select([
    { value: '', label: '전체 상태' },
    ...Object.entries(STATUS).map(([value, [label]]) => ({ value, label })),
  ], { value: status });
  statusFilter.addEventListener('change', () => {
    navigate(statusFilter.value ? `/migrations?status=${statusFilter.value}` : '/migrations');
  });

  mount(outlet,
    pageHeader('마이그레이션', 'ERD 초안을 검토·승인하고 대상 DB에 적용합니다', [
      h('a.btn', { href: '/erd' }, icon('edit'), 'ERD 초안으로'),
    ]),
    h('div.card.filter-bar', {},
      h('label.field.field-inline', {}, h('span.field-label', {}, '상태'), statusFilter),
      h('p.field-help', {},
        '마이그레이션은 ERD 초안에서 만듭니다. 초안 화면의 "마이그레이션 만들기"를 사용하세요.'),
    ),
    res.items.length === 0
      ? emptyState('마이그레이션이 없습니다. ERD 초안에서 변경 계획을 만들어 시작하세요.',
        h('a.btn.btn-primary', { href: '/erd' }, 'ERD 초안으로'))
      : h('div.mig-list', {}, res.items.map((item) => migrationRow(item))),
  );
}

function migrationRow(item) {
  const m = item.migration;
  const conn = item.connection;
  const approvals = (m.reviews ?? []).length;
  return h('article.card.mig-row', {},
    h('div.mig-row-main', {},
      h('a.mig-title', { href: `/migrations/${encodeURIComponent(m.id)}` }, m.title),
      h('div.mig-row-meta', {},
        migrationStatusBadge(m.status),
        conn ? envBadge(conn.environment) : null,
        conn ? h('span.muted', {}, `${conn.name} · ${kindLabel(conn.kind)}`) : null,
        m.destructiveCount > 0 ? badge(`파괴적 ${m.destructiveCount}`, 'danger') : null,
      ),
    ),
    h('dl.mig-row-stats', {},
      h('div', {}, h('dt', {}, '변경'), h('dd', {}, String(m.diff?.changes?.length ?? 0))),
      h('div', {}, h('dt', {}, '버전'),
        h('dd', {}, m.fromVersionNo ? `v${m.fromVersionNo}${m.toVersionNo ? ` → v${m.toVersionNo}` : ''}` : '—')),
      h('div', {}, h('dt', {}, '담당'),
        h('dd', {}, m.assigneeName || h('span.muted', {}, '미정'))),
      h('div', {}, h('dt', {}, '리뷰'), h('dd', {}, `${approvals}건`)),
      h('div', {}, h('dt', {}, '수정'), h('dd', {}, relativeTime(m.updatedAt))),
    ),
    h('a.btn.btn-small', { href: `/migrations/${encodeURIComponent(m.id)}` }, '열기'),
  );
}

// ---------- 상세 ----------

export async function renderMigrationDetail(outlet, params) {
  const id = params.id;
  mount(outlet, spinner('마이그레이션을 불러오는 중…'));

  const reload = () => renderMigrationDetail(outlet, params);

  let res;
  try {
    res = await api.get(`/migrations/${encodeURIComponent(id)}`);
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  const m = res.migration;
  const conn = res.connection;
  const precheckBox = h('div');

  mount(outlet,
    pageHeader(m.title, null, [
      h('a.btn', { href: '/migrations' }, '목록'),
    ]),
    h('div.card.mig-head', {},
      h('div.mig-head-row', {},
        migrationStatusBadge(m.status),
        conn ? envBadge(conn.environment) : null,
        conn ? h('span', {}, `${conn.name} · ${kindLabel(conn.kind)}`) : null,
        m.destructiveCount > 0 ? badge(`파괴적 변경 ${m.destructiveCount}건`, 'danger') : null,
        // 롤백 계획임을 눈에 띄게 알린다. 검토하는 사람이 "왜 테이블을 지우는 SQL이
        // 있는가"를 이해하려면 이것이 되돌리기라는 사실을 먼저 알아야 한다.
        m.rollbackToNo ? badge(`v${m.rollbackToNo} 으로 롤백`, 'info') : null,
        badge(`승인 ${res.approvals}/${res.requiredApprovals}`,
          res.approvals >= res.requiredApprovals ? 'success' : 'neutral'),
      ),
      h('dl.mig-meta', {},
        // 기준 버전이 비는 경우가 있다: 앱 밖에서 스키마가 바뀐 상태에서 만든
        // 계획이다. 그 구조는 아직 이력에 없으므로 "—" 대신 그렇게 적는다.
        metaRow('기준 버전', m.fromVersionNo
          ? `v${m.fromVersionNo}`
          : h('span.muted', {}, '이력에 없는 상태')),
        metaRow('결과 버전', m.toVersionNo ? `v${m.toVersionNo}` : '—'),
        metaRow('만든 시각', formatDate(m.createdAt)),
        m.appliedAt ? metaRow('적용 시각', formatDate(m.appliedAt)) : null,
        m.rolledBackAt ? metaRow('롤백 시각', formatDate(m.rolledBackAt)) : null,
        metaRow('SQL 문장', `${m.plan?.up?.length ?? 0}개`),
      ),
      peopleRow(m, reload),
      m.error ? h('div.notice.notice-danger', {}, icon('alert'), h('div', {}, h('strong', {}, '실행 오류'), h('p', {}, m.error))) : null,
      actionBar(m, res, precheckBox, reload),
    ),
    precheckBox,
    warningsPanel(m),
    changesPanel(m),
    sqlPanel(m),
    reviewsPanel(m, res, reload),
    executionPanel(m),
    pushPanel(m),
  );
}

// pushPanel은 이 마이그레이션의 Git 푸시 이력을 보여준다.
// 비동기로 채우는 이유: 푸시가 없는 마이그레이션이 많고, 상세 화면 로딩을
// Git 조회 때문에 늦출 이유가 없다.
function pushPanel(m) {
  const box = h('div');
  const section = h('section.card', {}, h('h2', {}, 'Git 푸시 이력'), box);
  mount(box, h('p.muted', {}, '불러오는 중…'));
  api.get(`/vcs/pushes?migration=${encodeURIComponent(m.id)}`)
    .then((res) => {
      const pushes = res.pushes ?? [];
      if (pushes.length === 0) {
        mount(box, h('p.muted', {}, '아직 저장소에 올리지 않았습니다.'));
        return;
      }
      mount(box, h('ul.push-list', {}, pushes.map((p) => h('li', {},
        p.status === 'ok' ? badge('성공', 'success') : badge('실패', 'danger'),
        ' ', h('code', {}, p.branch),
        p.prUrl
          ? h('span', {}, ' → ',
            h('a', { href: p.prUrl, target: '_blank', rel: 'noopener noreferrer' },
              `PR #${p.prNumber ?? ''}`))
          : null,
        h('span.muted', {}, ` · ${p.integrationName} · ${relativeTime(p.createdAt)}`),
        p.error ? h('div.cell-sub.text-danger', {}, p.error) : null,
      ))));
    })
    .catch(() => {
      mount(box, h('p.muted', {}, '푸시 이력을 불러오지 못했습니다.'));
    });
  return section;
}

function metaRow(label, value) {
  return h('div.meta-row', {}, h('dt', {}, label), h('dd', {}, value));
}

// peopleRow는 "누가 끌고 가는가 · 누구의 확인을 기다리는가"를 한 줄로 보여준다.
//
// 상태 배지 옆이 아니라 메타 아래에 두는 이유: 이것은 계획의 속성이 아니라 사람의
// 배정이고, 리뷰 대기가 길어질수록 가장 먼저 찾게 되는 값이다.
function peopleRow(m, reload) {
  const reviewers = m.reviewers ?? [];
  return h('div.mig-people', {},
    h('div.mig-people-item', {},
      h('span.field-label', {}, '담당자'),
      m.assigneeName
        ? badge(m.assigneeName, 'accent')
        : h('span.muted', {}, '아직 정하지 않았습니다')),
    h('div.mig-people-item', {},
      h('span.field-label', {}, '리뷰어'),
      reviewers.length
        ? h('span.mig-people-list', {}, reviewers.map((r) => badge(r.name || r.userId, 'neutral')))
        : h('span.muted', {}, '아직 정하지 않았습니다')),
    h('button.btn.btn-small', {
      type: 'button',
      onclick: () => openAssignDialog(m, reload),
    }, icon('users'), '지정'),
    // 초안에서 다음에 할 일은 하나다: 볼 사람을 정하는 것.
    (m.status === 'draft' || m.status === 'rejected') && reviewers.length === 0
      ? h('p.field-help.mig-people-hint', {},
        '리뷰어를 지정하면 리뷰가 시작됩니다.')
      : null,
  );
}

// pendingReviewers는 지정됐지만 아직 의견을 남기지 않은 사람을 보여준다.
//
// 승인 수만 보면 "2/2 중 1"까지는 알아도 누구를 기다리는지는 모른다. 기다리는
// 대상이 이름으로 보여야 재촉할 사람이 정해진다.
function pendingReviewers(m) {
  const decided = new Set((m.reviews ?? []).map((r) => r.reviewerId).filter(Boolean));
  const waiting = (m.reviewers ?? []).filter((r) => !decided.has(r.userId));
  if (waiting.length === 0) return null;
  return h('p.field-help', {},
    '기다리는 리뷰어: ',
    ...waiting.map((r, i) => h('span', {}, i ? ', ' : '', h('b', {}, r.name || r.userId))));
}

// openAssignDialog는 담당자 한 명과 리뷰어 여럿을 함께 정한다.
//
// 후보는 서버가 고른다 — 대상 커넥션에 migrate 등급이 있는 사람만 온다. 지정해도
// 열어 보지 못하는 사람을 고를 수 있으면, 부탁은 했는데 아무 일도 일어나지 않는다.
function openAssignDialog(m, reload) {
  const body = h('div', {}, spinner('사람 목록을 불러오는 중…'));
  const footer = h('div.rollback-footer');
  const close = openModal({
    title: '담당자 · 리뷰어 지정',
    width: 560,
    body: () => body,
    footer: () => footer,
  });

  (async () => {
    let people;
    try {
      people = (await api.get(`/migrations/${encodeURIComponent(m.id)}/people`)).items ?? [];
    } catch (err) {
      mount(body, errorPanel(err));
      return;
    }
    if (people.length === 0) {
      mount(body, h('p.notice.notice-warn', {}, icon('alert'),
        '이 DB의 마이그레이션 권한을 가진 사람이 없습니다. 먼저 권한을 주세요.'));
      mount(footer, h('button.btn', { type: 'button', onclick: close }, '닫기'));
      return;
    }

    const label = (p) => `${p.displayName || p.username} (${p.username})`;
    const assignee = select(
      [{ value: '', label: '정하지 않음' }, ...people.map((p) => ({ value: p.id, label: label(p) }))],
      { value: m.assigneeId ?? '' },
    );
    // 리뷰어는 검색해서 고른다. 체크박스를 늘어놓으면 사람이 늘어날수록 눈으로
    // 훑어 찾게 되고, 대화상자 높이도 사람 수만큼 늘어난다.
    const selfNote = h('span.field-help');
    const reviewers = peoplePicker({
      items: people.map((p) => ({
        id: p.id,
        label: p.displayName || p.username,
        sub: p.displayName ? p.username : '',
      })),
      selected: (m.reviewers ?? []).map((r) => r.userId),
      placeholder: '이름 또는 아이디로 검색',
    });

    // 담당자는 리뷰어가 될 수 없다.
    //
    // 자기가 끌고 가는 계획을 자기가 검토하는 것은 검토가 아니다. 저장할 때 막을
    // 수도 있지만, 그러면 사람이 고르고 나서야 안 된다는 말을 듣는다. 담당자를
    // 고르는 순간 후보에서 빼고, 이미 골라 두었다면 그 자리에서 빠진다.
    const syncExclusion = () => {
      const dropped = reviewers.setExcluded(assignee.value ? [assignee.value] : []);
      const who = people.find((p) => p.id === assignee.value);
      selfNote.textContent = assignee.value
        ? `담당자(${who ? label(who) : assignee.value})는 리뷰어로 고를 수 없습니다.`
        : '';
      if (dropped) toast('담당자는 리뷰어에서 뺐습니다', 'info');
    };
    assignee.addEventListener('change', syncExclusion);

    mount(body,
      h('p.modal-message', {},
        '담당자는 이 계획을 끝까지 끌고 갈 한 사람이고, 리뷰어는 검토를 부탁할 사람들입니다. ' +
        '지정은 실행을 막거나 열지 않습니다 — 실행 조건은 그대로 승인 수입니다.'),
      // .notice 는 flex 상자다. 글자 조각과 <b> 를 그대로 넣으면 조각마다 flex
      // 항목이 되어 한 글자씩 세로로 쪼개진다 — 한 덩어리(span)로 싸야 한다.
      m.status === 'draft'
        ? h('p.notice.notice-info', {}, icon('activity'),
          h('span', {},
            '리뷰어를 한 명이라도 고르면 이 계획은 바로 ', h('b', {}, '리뷰 중'),
            ' 이 됩니다. 따로 "리뷰 요청"을 누를 필요가 없습니다.'))
        : null,
      h('label.field', {}, h('span.field-label', {}, '담당자'), assignee),
      h('div.field', {},
        h('span.field-label', {}, '리뷰어'),
        reviewers.node,
        selfNote),
    );
    syncExclusion();

    mount(footer,
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: async (e) => {
          e.currentTarget.disabled = true;
          try {
            await api.put(`/migrations/${encodeURIComponent(m.id)}/assignment`, {
              assigneeId: assignee.value,
              reviewerIds: reviewers.value,
            });
            close();
            toast('담당자와 리뷰어를 저장했습니다', 'success');
            reload();
          } catch (err) {
            toastError(err);
            e.currentTarget.disabled = false;
          }
        },
      }, icon('check'), '저장'),
    );
  })();
}

// myDecision은 내가 이 계획에 남긴 결정이다(없으면 null).
//
// 한 사람의 결정은 하나로 남는다(서버가 이전 결정을 대신한다). 그래서 "지금 내가
// 무엇으로 남겨 두었는가"를 화면이 말해 줄 수 있다.
function myDecision(m) {
  const me = state.user?.id;
  if (!me) return null;
  const mine = (m.reviews ?? [])
    .filter((r) => r.reviewerId === me && r.decision !== 'comment')
    .pop();
  return mine?.decision ?? null;
}

function actionBar(m, res, precheckBox, reload) {
  const buttons = [];

  // 리뷰는 리뷰어를 지정하면 시작된다. 따로 "리뷰 요청"을 누를 자리를 두지 않는다 —
  // 누구에게도 부탁하지 않은 채 열려 있는 리뷰는 아무도 자기 일로 여기지 않는다.
  // 반려된 계획도 리뷰어를 다시 지정하면 다시 리뷰로 들어간다.
  //
  // 승인됨·반려됨에서도 검토 창을 연다. 실행 전이라면 마음을 바꿀 수 있어야 하고
  // (반려를 잘못 눌렀거나, 지적한 것이 그 자리에서 설명된 경우가 있다), 그 길이
  // 없으면 되돌리는 방법은 리뷰 기록을 통째로 지우는 것뿐이다.
  if (m.status === 'in_review' || m.status === 'approved' || m.status === 'rejected') {
    const mine = myDecision(m);
    buttons.push(h('button.btn', {
      type: 'button',
      onclick: () => openReviewDialog(m, res, reload),
    }, icon('check'), mine ? '검토 바꾸기' : '검토하기'));
  }
  if (m.status === 'approved') {
    buttons.push(h('button.btn', {
      type: 'button',
      onclick: () => runPrecheck(m, precheckBox),
    }, icon('shield'), '사전 검사'));
    buttons.push(h('button.btn.btn-danger', {
      type: 'button',
      onclick: () => openApplyDialog(m, res, reload),
    }, icon('play'), '실행'));
  }
  if (m.status === 'applied' || m.status === 'failed') {
    buttons.push(h('button.btn.btn-danger', {
      type: 'button',
      onclick: () => openRollbackDialog(m, res, reload),
    }, icon('refresh'), '롤백'));
  }
  // Git 푸시는 어느 상태에서든 할 수 있다. 리뷰를 저장소에서 받는 팀도 있고,
  // 적용 후 기록으로 남기는 팀도 있다 — 워크플로를 앱이 강제할 이유가 없다.
  buttons.push(h('button.btn', {
    type: 'button',
    onclick: () => openPushDialog(m, res.connection, reload),
  }, icon('copy'), 'Git에 올리기'));

  // 진행하지 않기로 한 계획은 지우는 대신 닫는다. 지우면 "이런 계획을 세웠다가
  // 접었다"는 사실과 그때의 리뷰까지 사라져, 같은 논의가 다시 올라왔을 때 왜
  // 접었는지 아무도 모른다. 닫힌 계획은 다시 열 수 있다.
  if (m.status !== 'applied' && m.status !== 'rolled_back' && m.status !== 'closed') {
    buttons.push(h('button.btn', {
      type: 'button',
      onclick: () => changeStatus(m.id, 'closed', reload),
    }, icon('x'), '닫기'));
  }
  if (m.status === 'closed') {
    // 닫혀 있는 동안 대상 DB가 바뀌었을 수 있으므로 초안으로 돌아간다.
    // 그때의 승인은 지금 구조를 본 것이 아니다.
    buttons.push(h('button.btn.btn-primary', {
      type: 'button',
      onclick: () => changeStatus(m.id, 'draft', reload),
    }, icon('refresh'), '다시 열기'));
  }

  return h('div.mig-actions', {}, buttons);
}

async function changeStatus(id, status, reload) {
  try {
    await api.post(`/migrations/${encodeURIComponent(id)}/status`, { status });
    toast('상태를 변경했습니다', 'success');
    reload();
  } catch (err) {
    toastError(err);
  }
}

async function runPrecheck(m, box) {
  mount(box, h('div.card', {}, spinner('대상 DB를 확인하는 중…')));
  try {
    const res = await api.post(`/migrations/${encodeURIComponent(m.id)}/precheck`);
    mount(box, precheckView(res.precheck));
  } catch (err) {
    mount(box, errorPanel(err));
  }
}

function precheckView(pc) {
  return h('section.card.mig-precheck', {},
    h('h2', {}, icon('shield'), '사전 검사',
      pc.ok ? badge('실행 가능', 'success') : badge('실행 불가', 'danger')),
    pc.blockers.length
      ? h('div.notice.notice-danger', {}, icon('alert'),
        h('div', {}, h('strong', {}, '실행을 막는 사유'),
          h('ul.note-list', {}, pc.blockers.map((b) => h('li', {}, b)))))
      : null,
    pc.drifted
      ? h('div.notice.notice-warn', {}, icon('alert'),
        h('div', {},
          h('strong', {}, '계획을 만든 뒤 대상 DB가 바뀌었습니다'),
          h('p.muted', {}, `기준 지문 ${pc.expectedFingerprint?.slice(0, 12)}… → 현재 ${pc.actualFingerprint?.slice(0, 12)}…`),
          pc.driftChanges?.length
            ? h('ul.note-list', {}, pc.driftChanges.map((c) => h('li', {}, c)))
            : null,
          h('p', {}, 'ERD 초안에서 마이그레이션을 다시 만들어야 합니다.')))
      : null,
    pc.warnings.length
      ? h('div.notice.notice-warn', {}, icon('alert'),
        h('div', {}, h('strong', {}, '주의'),
          h('ul.note-list', {}, pc.warnings.map((w) => h('li', {}, w)))))
      : null,
    h('dl.mig-meta', {},
      metaRow('승인', `${pc.approvals}/${pc.requiredApprovals}명`),
      metaRow('트랜잭션 DDL', pc.transactionalDdl ? '지원 (실패 시 전부 되돌아감)' : '미지원 (부분 적용 가능)'),
      metaRow('백업 훅', pc.backupConfigured ? '설정됨' : '설정되지 않음'),
    ),
  );
}

function warningsPanel(m) {
  const plan = m.plan ?? {};
  const hasAny = (plan.warnings?.length ?? 0) > 0 || (m.irreversible?.length ?? 0) > 0;
  if (!hasAny) return null;
  return h('section.card', {},
    h('h2', {}, icon('alert'), '경고'),
    plan.warnings?.length
      ? h('div.notice.notice-warn', {}, icon('alert'),
        h('div', {}, h('strong', {}, '계획 생성 중 발견된 문제'),
          h('ul.note-list', {}, plan.warnings.map((w) => h('li', {}, w)))))
      : null,
    m.irreversible?.length
      ? h('div.notice.notice-danger', {}, icon('alert'),
        h('div', {},
          h('strong', {}, '되돌릴 수 없는 변경'),
          h('p', {}, '롤백하면 구조는 복구되지만 데이터는 복구되지 않습니다.'),
          h('ul.note-list', {}, m.irreversible.map((w) => h('li', {}, w)))))
      : null,
  );
}

function changesPanel(m) {
  const changes = m.diff?.changes ?? [];
  return h('section.card', {},
    h('h2', {}, `변경 ${changes.length}건`,
      m.destructiveCount > 0 ? badge(`파괴적 ${m.destructiveCount}건`, 'danger') : null),
    changes.length === 0
      ? h('p.muted', {}, '변경이 없습니다')
      : h('div.table-wrap', {},
        h('table.table', {},
          h('thead', {}, h('tr', {},
            h('th', {}, '종류'), h('th', {}, '대상'), h('th', {}, '내용'))),
          h('tbody', {}, changes.map((c) => h('tr', { class: c.destructive ? 'is-destructive' : '' },
            h('td.nowrap', {}, badge(c.kind, c.destructive ? 'danger' : 'neutral')),
            h('td.nowrap', {}, c.table ?? '—'),
            h('td', {}, c.summary,
              c.lossyDetail ? h('p.mig-lossy', {}, icon('alert', 12), c.lossyDetail) : null),
          ))))),
  );
}

function sqlPanel(m) {
  const plan = m.plan ?? {};
  const upSQL = (plan.up ?? []).map((s) => sqlText(s)).join('\n\n');
  const downSQL = (plan.down ?? []).map((s) => sqlText(s)).join('\n\n');
  return h('section.card', {},
    h('h2', {}, 'SQL'),
    h('div.mig-sql-grid', {},
      sqlBlock('적용 (up)', upSQL, plan.up?.length ?? 0),
      sqlBlock('롤백 (down)', downSQL, plan.down?.length ?? 0),
    ),
  );
}

function sqlText(stmt) {
  const note = stmt.note ? `-- ${stmt.note}\n` : '';
  return `${note}${stmt.sql};`;
}

function sqlBlock(title, sql, count) {
  return h('div.mig-sql-col', {},
    h('div.panel-head', {},
      h('strong', {}, `${title} · ${count}문장`),
      sql
        ? h('button.btn.btn-small', {
          type: 'button', onclick: () => copyToClipboard(sql),
        }, icon('copy'), '복사')
        : null,
    ),
    sql
      ? codeBlock(sql, 'sql', { className: 'sql-block' })
      : h('p.muted', {}, '없음'),
  );
}

function reviewsPanel(m, res, reload) {
  const reviews = m.reviews ?? [];
  return h('section.card', {},
    h('h2', {}, '리뷰',
      badge(`승인 ${res.approvals}/${res.requiredApprovals}`,
        res.approvals >= res.requiredApprovals ? 'success' : 'neutral')),
    h('p.field-help', {},
      '승인·반려는 ', h('b', {}, '리뷰어로 지정된 사람'), '만 남길 수 있습니다. ',
      '의견은 누구나 남길 수 있습니다.',
      ' 담당자는 자기가 맡은 계획을 승인할 수 없습니다.',
      ' 실행 전이라면 결정을 바꿀 수 있습니다.',
      res.requiredApprovals > 1
        ? ' 운영 DB이거나 파괴적 변경이 포함되어 2명의 승인이 필요합니다.'
        : ''),
    pendingReviewers(m),
    reviews.length === 0
      ? h('p.muted', {}, '아직 리뷰가 없습니다')
      : h('div.mig-reviews', {}, reviews.map((r) => h('div.mig-review', {},
        h('div.mig-review-head', {},
          h('strong', {}, r.reviewerName || '알 수 없음'),
          reviewBadge(r.decision),
          h('span.muted', {}, relativeTime(r.createdAt)),
        ),
        r.comment ? h('p.mig-review-comment', {}, r.comment) : null,
      ))),
    m.status === 'in_review'
      ? h('button.btn.btn-primary', {
        type: 'button', onclick: () => openReviewDialog(m, res, reload),
      }, icon('check'), '검토 의견 남기기')
      : null,
  );
}

function reviewBadge(decision) {
  const map = {
    approved: ['승인', 'success'],
    rejected: ['반려', 'danger'],
    comment: ['의견', 'neutral'],
  };
  const [label, kind] = map[decision] ?? [decision, 'neutral'];
  return badge(label, kind);
}

function executionPanel(m) {
  const log = m.executionLog ?? [];
  if (log.length === 0) return null;
  const failed = log.filter((s) => s.error).length;
  return h('section.card', {},
    h('h2', {}, '실행 기록',
      h('span.muted', {}, `${m.appliedStatements}문장 적용`),
      failed ? badge(`실패 ${failed}`, 'danger') : null),
    h('div.table-wrap', {},
      h('table.table.mig-exec', {},
        h('thead', {}, h('tr', {},
          h('th', {}, '#'), h('th', {}, 'SQL'), h('th', {}, '소요'), h('th', {}, '결과'))),
        h('tbody', {}, log.map((s) => h('tr', { class: s.error ? 'is-destructive' : '' },
          h('td.nowrap', {}, String(s.index + 1)),
          h('td', {}, codeBlock(s.sql, 'sql', { className: 'mig-exec-sql' })),
          h('td.nowrap', {}, `${s.durationMs}ms`),
          h('td', {}, s.error ? h('span.text-danger', {}, s.error) : badge('성공', 'success')),
        ))))),
  );
}

// ---------- 대화상자 ----------

function openReviewDialog(m, res, reload) {
  // 승인·반려는 지정된 리뷰어의 것이다. 서버가 막지만, 화면에서도 눌리지 않아야
  // 한다 — 누를 수 있는 버튼이 403으로 돌아오는 것은 설명이 아니라 사고다.
  //
  // 슈퍼 어드민은 지정과 무관하게 결정할 수 있다(서버도 같은 예외를 둔다). 지정한
  // 리뷰어가 자리를 비운 사이 계획이 멈춰 있으면 사람들은 이 흐름을 우회하는 다른
  // 길을 찾는다 — 막다른 길을 만들지 않는 것이 규칙을 지키게 하는 방법이다.
  const me = state.user?.id;
  const designated = (m.reviewers ?? []).some((r) => r.userId === me);
  const superadmin = state.user?.role === 'superadmin';
  // 담당자는 자기가 맡은 계획을 승인할 수 없다. 지정 규칙이 생기기 전에 저장된
  // 자료에서는 담당자가 리뷰어로 남아 있을 수 있어, 여기서도 한 번 더 본다.
  const isAssignee = Boolean(me) && m.assigneeId === me;
  const isReviewer = superadmin || (designated && !isAssignee);
  const commentInput = textarea({
    placeholder: isReviewer
      ? '검토 의견 (반려 시 필수는 아니지만 남겨주세요)'
      : '의견 (예: 이 인덱스는 트래픽이 적은 시간에 거는 편이 좋겠습니다)',
  });
  const decide = async (decision, close) => {
    try {
      const out = await api.post(`/migrations/${encodeURIComponent(m.id)}/review`, {
        decision, comment: commentInput.value,
      });
      close();
      toast(`검토를 기록했습니다 (승인 ${out.approvals}/${out.requiredApprovals})`, 'success');
      reload();
    } catch (err) {
      toastError(err);
    }
  };

  const mine = myDecision(m);
  const mineLabel = { approved: '승인', rejected: '반려' }[mine] ?? '';

  openModal({
    title: mine ? '검토 바꾸기' : '마이그레이션 검토',
    width: 560,
    body: () => [
      h('p.modal-message', {}, `"${m.title}" 을 ${isReviewer ? '검토합니다' : '봅니다'}.`),
      // 지금 내가 무엇으로 남겨 두었는지 먼저 말한다. 그것을 모르면 "바꾸는 것"인지
      // "처음 남기는 것"인지 알 수 없고, 같은 결정을 한 번 더 누르게 된다.
      mine
        ? h('p.notice.notice-info', {}, icon('activity'),
          h('span', {},
            '지금 이 계획에 ', h('b', {}, mineLabel), '으로 남겨 두었습니다. ',
            '다시 고르면 그 결정을 대신합니다.'))
        : null,
      h('dl.mig-meta', {},
        metaRow('변경', `${m.diff?.changes?.length ?? 0}건`),
        metaRow('파괴적 변경', `${m.destructiveCount}건`),
        metaRow('SQL', `${m.plan?.up?.length ?? 0}문장`),
        metaRow('현재 승인', `${res.approvals}/${res.requiredApprovals}명`),
      ),
      m.destructiveCount > 0
        ? h('p.notice.notice-warn', {}, icon('alert'),
          '데이터 손실이 발생할 수 있는 변경이 포함되어 있습니다. SQL을 직접 확인하세요.')
        : null,
      isReviewer
        ? null
        : h('p.notice.notice-info', {}, icon('activity'),
          isAssignee
            ? h('span', {},
              '이 계획의 ', h('b', {}, '담당자'), ' 라 승인·반려할 수 없습니다. ',
              '의견은 남길 수 있고, 결정은 리뷰어에게 부탁하세요.')
            : h('span', {},
              '리뷰어로 지정되지 않아 ', h('b', {}, '의견만'), ' 남길 수 있습니다. ',
              '승인·반려가 필요하면 담당자에게 리뷰어 지정을 요청하세요.')),
      // 대신 결정하는 것임을 스스로도 알고 있어야 한다. 리뷰 기록에는 이름이 남는다.
      !designated && superadmin
        ? h('p.notice.notice-warn', {}, icon('alert'),
          h('span', {},
            '리뷰어로 지정되지는 않았지만 ', h('b', {}, '슈퍼 어드민'),
            ' 이라 승인·반려할 수 있습니다. 누가 결정했는지는 기록에 남습니다.'))
        : null,
      commentInput,
    ],
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn', {
        type: 'button', onclick: () => decide('comment', close),
      }, isReviewer ? '의견만 남기기' : '의견 남기기'),
      isReviewer
        ? h('button.btn.btn-danger', {
          type: 'button', onclick: () => decide('rejected', close),
        }, '반려')
        : null,
      isReviewer
        ? h('button.btn.btn-primary', {
          type: 'button', onclick: () => decide('approved', close),
        }, '승인')
        : null,
    ],
  });
}

function openApplyDialog(m, res, reload) {
  const needConfirm = res.connection?.environment === 'prod' || m.destructiveCount > 0;
  const confirmInput = input({ placeholder: res.connection?.name, autocomplete: 'off' });
  const box = h('div');

  openModal({
    title: '마이그레이션 실행',
    width: 640,
    body: () => [
      h('p.modal-message', {},
        `${res.connection?.name} 에 ${m.plan?.up?.length ?? 0}개 문장을 실행합니다.`),
      res.connection?.environment === 'prod'
        ? h('div.notice.notice-danger', {}, icon('alert'),
          h('div', {}, h('strong', {}, '운영 데이터베이스입니다'),
            h('p', {}, res.backupConfigured
              ? '실행 전 백업 훅이 자동으로 실행됩니다. 실패하면 마이그레이션이 중단됩니다.'
              : '백업 훅이 설정되지 않았습니다. 백업 없이 진행됩니다.')))
        : null,
      m.destructiveCount > 0
        ? h('div.notice.notice-warn', {}, icon('alert'),
          `데이터 손실이 발생할 수 있는 변경 ${m.destructiveCount}건이 포함되어 있습니다.`)
        : null,
      needConfirm
        ? h('label.field', {},
          h('span.field-label', {}, `계속하려면 커넥션 이름 "${res.connection?.name}" 을 입력하세요`),
          confirmInput)
        : null,
      box,
    ],
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-danger', {
        type: 'button',
        onclick: async (e) => {
          const btn = e.currentTarget;
          btn.disabled = true;
          mount(box, spinner('실행 중… 이 창을 닫지 마세요'));
          try {
            const out = await api.post(`/migrations/${encodeURIComponent(m.id)}/apply`, {
              confirm: confirmInput.value,
            });
            close();
            showResult('실행 결과', out.result);
            reload();
          } catch (err) {
            btn.disabled = false;
            mount(box, blockersView(err));
            if (!err.blockers) toastError(err);
          }
        },
      }, icon('play'), '실행'),
    ],
  });
}

function openRollbackDialog(m, res, reload) {
  const isProd = res.connection?.environment === 'prod';
  const confirmInput = input({ placeholder: res.connection?.name, autocomplete: 'off' });
  const continueBox = h('input', { type: 'checkbox' });
  const box = h('div');

  openModal({
    title: '롤백',
    width: 620,
    body: () => [
      h('p.modal-message', {},
        `저장된 롤백 SQL ${m.plan?.down?.length ?? 0}개 문장을 실행합니다.`),
      m.irreversible?.length
        ? h('div.notice.notice-danger', {}, icon('alert'),
          h('div', {}, h('strong', {}, '데이터는 복구되지 않습니다'),
            h('p', {}, '구조는 되돌아가지만 삭제된 데이터는 복구할 수 없습니다.')))
        : null,
      m.status === 'failed'
        ? h('div.notice.notice-warn', {}, icon('alert'),
          h('div', {},
            h('strong', {}, '실패한 마이그레이션 정리'),
            h('p', {}, `${m.appliedStatements}개 문장이 적용된 상태입니다. ` +
              '적용되지 않은 부분의 롤백 SQL은 실패할 수 있으므로 ' +
              '"오류를 무시하고 계속"을 켜는 것이 보통입니다.'),
            h('label.checkbox', {}, continueBox, h('span', {}, '오류를 무시하고 계속 실행'))))
        : null,
      isProd
        ? h('label.field', {},
          h('span.field-label', {}, `운영 DB입니다. 커넥션 이름 "${res.connection?.name}" 을 입력하세요`),
          confirmInput)
        : null,
      box,
    ],
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-danger', {
        type: 'button',
        onclick: async (e) => {
          const btn = e.currentTarget;
          btn.disabled = true;
          mount(box, spinner('롤백 중…'));
          try {
            const out = await api.post(`/migrations/${encodeURIComponent(m.id)}/rollback`, {
              confirm: confirmInput.value,
              continueOnError: continueBox.checked,
            });
            close();
            showResult('롤백 결과', out.result);
            reload();
          } catch (err) {
            btn.disabled = false;
            mount(box, blockersView(err));
            if (!err.blockers) toastError(err);
          }
        },
      }, icon('refresh'), '롤백 실행'),
    ],
  });
}

// blockersView는 서버가 돌려준 차단 사유를 보여준다.
//
// 일반 오류 토스트로 처리하면 여러 사유가 한 줄로 뭉개져 읽을 수 없다.
// 사유 목록은 message가 아니라 오류 본문(payload)에 담겨 오므로 거기서 읽는다.
function blockersView(err) {
  const blockers = err?.payload?.blockers ?? err?.detail;
  if (Array.isArray(blockers)) {
    return h('div.notice.notice-danger', {}, icon('alert'),
      h('div', {}, h('strong', {}, '실행할 수 없습니다'),
        h('ul.note-list', {}, blockers.map((b) => h('li', {}, b)))));
  }
  return h('div.notice.notice-danger', {}, icon('alert'),
    h('div', {}, err?.message ?? '알 수 없는 오류', blockers ? h('p.muted', {}, blockers) : null));
}

function showResult(title, result) {
  const failed = result.status !== 'applied' && result.status !== 'rolled_back';
  openModal({
    title,
    width: 680,
    body: () => [
      h(failed ? 'div.notice.notice-danger' : 'div.notice.notice-success', {},
        icon(failed ? 'alert' : 'check'),
        h('div', {},
          h('strong', {}, failed ? '실패했습니다' : '완료했습니다'),
          result.error ? h('p', {}, result.error) : null,
          h('p.muted', {}, `${result.report?.applied ?? 0}개 문장 적용` +
            (result.report?.transactionUsed
              ? (result.report?.rolledBack ? ' · 트랜잭션으로 전부 되돌림' : ' · 트랜잭션 사용')
              : ' · 트랜잭션 미사용')),
        )),
      result.version
        ? h('p', {}, `새 버전 v${result.version.versionNo} 이 등록되었습니다.`)
        : null,
      result.warnings?.length
        ? h('div.notice.notice-warn', {}, icon('alert'),
          h('ul.note-list', {}, result.warnings.map((w) => h('li', {}, w))))
        : null,
      result.postDiff?.length
        ? h('details', {}, h('summary', {}, `목표와 남은 차이 ${result.postDiff.length}건`),
          h('ul.note-list', {}, result.postDiff.map((d) => h('li', {}, d))))
        : null,
      result.backupOutput
        ? h('details', {}, h('summary', {}, '백업 훅 출력'), h('pre.sql-block', {}, result.backupOutput))
        : null,
      result.report?.steps?.length
        ? h('details', {}, h('summary', {}, `실행 기록 ${result.report.steps.length}건`),
          h('table.table.mig-exec', {},
            h('tbody', {}, result.report.steps.map((s) => h('tr', { class: s.error ? 'is-destructive' : '' },
              h('td.nowrap', {}, String(s.index + 1)),
              h('td', {}, codeBlock(s.sql, 'sql', { className: 'mig-exec-sql' })),
              h('td.nowrap', {}, `${s.durationMs}ms`),
              h('td', {}, s.error ? h('span.text-danger', {}, s.error) : '✓'),
            )))))
        : null,
    ],
  });
}

// ---------- 버전 이력 ----------

export async function renderVersions(outlet, params, query) {
  mount(outlet, spinner('커넥션 목록을 불러오는 중…'));

  let conns;
  let servers;
  try {
    // 서버 목록을 함께 받는 이유: 커넥션을 평평하게 늘어놓으면 같은 서버의 DB가
    // 이름만 다른 항목으로 반복된다. 데이터·스키마 화면과 같은 두 단계 고르개를
    // 쓰려면 서버 정보가 필요하다.
    [conns, servers] = await Promise.all([
      api.get('/connections/'),
      api.get('/servers/'),
    ]);
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }
  const usable = conns.items.filter((i) => {
    if (!i.accessible) return false;
    const info = state.meta?.dbKinds?.find((k) => k.kind === i.connection.kind);
    return info?.capabilities?.migrate;
  });
  if (usable.length === 0) {
    mount(outlet,
      pageHeader('스키마 버전', '확정된 스키마 이력'),
      emptyState('버전을 관리할 수 있는 커넥션이 없습니다.'));
    return;
  }

  const selectedId = query.get('conn') || usable[0].connection.id;
  const current = usable.find((i) => i.connection.id === selectedId) ?? usable[0];
  const picker = serverDbPicker({
    usable,
    servers: servers.items ?? [],
    currentId: current.connection.id,
    onPick: (id) => navigate(`/versions?conn=${encodeURIComponent(id)}`),
  });

  const body = h('div', {}, spinner('버전 이력을 불러오는 중…'));
  const canCapture = current.level === 'erd' || current.level === 'migrate';

  mount(outlet,
    pageHeader('스키마 버전', '확정된 스키마 이력과 외부 편집 기록', [
      canCapture
        ? h('button.btn.btn-primary', {
          type: 'button',
          onclick: () => captureVersion(current.connection.id, () => renderVersions(outlet, params, query)),
        }, icon('plus'), '현재 상태를 버전으로 등록')
        : null,
    ]),
    h('div.card.filter-bar', {},
      ...picker.nodes,
      h('p.field-help', {},
        '앱 밖에서 스키마가 바뀌면 "외부 편집"으로 등록해 이력에 남길 수 있습니다.'),
    ),
    body,
  );

  try {
    const res = await api.get(`/connections/${encodeURIComponent(current.connection.id)}/versions`);
    mount(body, versionTimeline(res.versions, current.connection, current.level === 'migrate'));
  } catch (err) {
    mount(body, errorPanel(err));
  }
}

const VERSION_SOURCE = {
  initial_import: ['최초 등록', 'neutral'],
  migration: ['마이그레이션', 'accent'],
  external_edit: ['외부 편집', 'warn'],
  rollback: ['롤백', 'info'],
};

function versionTimeline(versions, conn, canMigrate) {
  if (!versions || versions.length === 0) {
    return emptyState('아직 확정된 버전이 없습니다. 현재 상태를 버전으로 등록해 기준선을 만드세요.');
  }
  // 최신 버전에도 되돌리기를 둔다.
  //
  // 앱 밖에서 스키마를 고치면(외부 편집) 현재 DB가 최신 버전과 달라진다. 그때
  // 필요한 동작이 바로 "최신 버전으로 되돌리기"다 — 손으로 넣은 인덱스나 컬럼을
  // 확정된 구조로 되돌리는 길이 이력 화면에 없으면, 사람은 그 차이를 SQL로 직접
  // 지우게 된다. 차이가 없을 때는 미리보기가 "되돌릴 것이 없습니다"로 막는다.
  const latest = versions[0]?.versionNo;
  // 한 줄에 담기는 정보를 세로로 쌓으면 카드 하나가 화면 높이의 5분의 1을 먹는다.
  // 버전 이력은 "훑어서 언제 무엇이 바뀌었나"를 보는 화면이므로, 한 화면에 들어가는
  // 버전 수가 카드 하나의 정보량보다 중요하다.
  return h('div.version-timeline', {}, versions.map((v, i) => {
    const [label, kind] = VERSION_SOURCE[v.source] ?? [v.source, 'neutral'];
    // 기본 비교 대상은 바로 이전 버전이다. 가장 오래된 버전에는 이전이 없으므로
    // 그때만 최신 버전을 기본으로 잡는다.
    const other = versions[i + 1] ?? versions.find((x) => x.id !== v.id) ?? null;

    return h('article.card.version-item', {},
      h('div.version-head', {},
        h('strong.version-no', {}, `v${v.versionNo}`),
        badge(label, kind),
        h('span.muted', {}, formatDate(v.createdAt)),
        v.authorName ? h('span.muted', {}, v.authorName) : null,
        // 다섯 개를 이름까지 붙여 늘어놓으면 한 줄을 넘겨 버전 하나가 두 줄이
        // 된다. 이 화면은 "훑어서 언제 무엇이 바뀌었나"를 보는 곳이라 한 화면에
        // 들어가는 버전 수가 버튼 이름보다 중요하다. 이름은 올렸을 때 뜬다.
        h('div.version-actions', {},
          // 비교보다 앞에 둔다. "그때 구조가 어땠나"는 "무엇이 달라졌나"보다 먼저
          // 오는 질문이고, 비교 화면은 그 답을 주지 않는다.
          h('button.icon-btn.btn-tip', {
            type: 'button',
            'data-tip': '구조 보기',
            'aria-label': `v${v.versionNo} 의 구조를 ERD로 봅니다`,
            onclick: () => navigate(
              `/structure?conn=${encodeURIComponent(conn.id)}&version=${v.id}`),
          }, icon('workflow')),
          h('button.icon-btn.btn-tip', {
            type: 'button',
            'data-tip': 'SQL 보기',
            'aria-label': `v${v.versionNo} 의 CREATE 스크립트를 봅니다`,
            onclick: () => showVersionSQL(conn, v),
          }, icon('code')),
          h('button.icon-btn.btn-tip', {
            type: 'button',
            'data-tip': '현재 DB와 비교',
            'aria-label': `v${v.versionNo} 을 현재 DB 구조와 비교합니다`,
            onclick: () => showVersionDiff(conn.id, v, null, versions),
          }, icon('database')),
          other
            ? h('button.icon-btn.btn-tip', {
              type: 'button',
              'data-tip': '버전 비교',
              'aria-label': `v${v.versionNo} 을 다른 버전과 비교합니다`,
              onclick: () => showVersionDiff(conn.id, v, other, versions),
            }, icon('history'))
            : null,
          // 되돌리기만 붉게 둔다. 나머지는 보기만 하는 버튼이고 이것만 DB를
          // 바꾼다 — 아이콘만 남은 줄에서 그 차이가 모양으로 드러나야 한다.
          canMigrate
            ? h('button.icon-btn.danger.btn-tip', {
              type: 'button',
              // 최신 버전에서는 "왜 여기에 되돌리기가 있는가"가 바로 보여야 한다.
              'data-tip': v.versionNo === latest
                ? '롤백 — 현재 DB가 이 버전과 다를 때 되돌립니다'
                : '롤백 — 이 구조로 되돌립니다',
              'aria-label': v.versionNo === latest
                ? `현재 DB를 최신 버전(v${v.versionNo})의 구조로 되돌립니다`
                : `이 버전(v${v.versionNo})의 구조로 되돌립니다`,
              onclick: () => openVersionRollbackDialog(conn, v),
            }, icon('undo'))
            : null,
        ),
      ),
      h('div.version-body', {},
        v.note
          // 메모는 길 수 있어 한 줄로 자른다. 전체는 툴팁으로 남긴다 —
          // 이 줄의 역할은 "무엇 때문에 만든 버전인가"를 알아보는 것까지다.
          ? h('p.version-note', { title: v.note }, v.note)
          : h('p.version-note.is-empty', {}, '메모 없음'),
        h('dl.version-stats', {},
          versionStat('테이블', v.stats?.tables),
          versionStat('컬럼', v.stats?.columns),
          versionStat('인덱스', v.stats?.indexes),
          versionStat('외래키', v.stats?.foreignKeys),
        ),
      ),
      v.changeSummary?.length
        ? h('details.version-changes', {},
          h('summary', {}, `변경 ${v.changeSummary.length}건`),
          h('ul.note-list', {}, v.changeSummary.slice(0, 50).map((c) => h('li', {}, c))))
        : null,
    );
  }));
}

function versionStat(label, value) {
  return h('div', {}, h('dt', {}, label), h('dd', {}, String(value ?? 0)));
}

// openVersionRollbackDialog는 되돌렸을 때 무엇이 일어나는지 먼저 보여준다.
//
// 바로 계획을 만들지 않는 이유: 되돌리기는 대개 급한 상황에서 눌린다. 그때야말로
// "이 버튼이 무엇을 지우는가"가 보여야 한다 — 되돌리기라고 해서 안전한 것이 아니다.
// 나중에 추가된 테이블은 되돌리면 DROP된다.
function openVersionRollbackDialog(conn, version) {
  const body = h('div', {}, spinner('되돌렸을 때의 변경을 계산하는 중…'));
  const footer = h('div.rollback-footer');

  // body와 footer를 미리 만들어 두고 나중에 채운다. 계산이 끝날 때까지 모달은
  // 열려 있어야 하고(사용자는 이미 눌렀다), openModal이 돌려주는 close가 필요하다.
  const close = openModal({
    title: `v${version.versionNo} 으로 롤백`,
    width: 780,
    body: () => body,
    footer: () => footer,
  });

  (async () => {
    let res;
    try {
      res = await api.get(
        `/connections/${encodeURIComponent(conn.id)}/versions/${version.id}/rollback`);
    } catch (err) {
      mount(body, errorPanel(err));
      return;
    }

    const changes = res.diff?.changes ?? [];
    const destructive = res.diff?.destructiveCount ?? 0;
    const noteInput = input({ placeholder: '예: 인덱스 추가로 성능 저하, 되돌림' });

    mount(body,
      h('p.modal-message', {},
        '현재 구조를 이 버전의 구조로 되돌리는 ',
        h('b', {}, '마이그레이션 계획'),
        '을 만듭니다. 계획은 기존과 같은 흐름(검토 → 승인 → 사전 검사 → 실행)을 거치며, ' +
        '적용이 끝나면 새 버전이 "롤백" 유형으로 등록됩니다.'),
      res.empty
        ? h('p.notice.notice-success', {}, icon('check'),
          '현재 구조가 이미 이 버전과 같습니다. 되돌릴 것이 없습니다.')
        : null,
      destructive > 0
        ? h('p.notice.notice-danger', {}, icon('alert'),
          h('span', {},
            h('b', {}, `파괴적 변경 ${destructive}건. `),
            '되돌리기는 안전한 동작이 아닙니다 — 이 버전 이후에 만들어진 테이블·컬럼은 삭제되고 ' +
            '그 안의 데이터는 복구되지 않습니다.'))
        : null,
      changes.length
        ? h('div.table-scroll', {},
          h('table.table', {},
            h('thead', {}, h('tr', {}, h('th', {}, '종류'), h('th', {}, '내용'))),
            h('tbody', {}, changes.map((ch) => h('tr', { class: ch.destructive ? 'is-destructive' : '' },
              h('td.nowrap', {}, badge(ch.kind, ch.destructive ? 'danger' : 'neutral')),
              h('td', {}, ch.summary))))))
        : null,
      res.plan?.up?.length
        ? h('details', {},
          h('summary', {}, `실행될 SQL ${res.plan.up.length}문장`),
          codeBlock(res.plan.up.map((s) => `${s.sql};`).join('\n\n'), 'sql', { className: 'sql-block' }))
        : null,
      res.plan?.warnings?.length
        ? h('p.notice.notice-warn', {}, icon('alert'), res.plan.warnings.join(' / '))
        : null,
      res.empty ? null : h('label.field', {}, h('span.field-label', {}, '메모'), noteInput),
    );

    mount(footer,
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      res.empty ? null : h('button.btn.btn-danger', {
        type: 'button',
        onclick: async (e) => {
          e.currentTarget.disabled = true;
          try {
            const created = await api.post(
              `/connections/${encodeURIComponent(conn.id)}/versions/${version.id}/rollback`,
              { note: noteInput.value.trim() });
            close();
            toast(created.message ?? '롤백 계획을 만들었습니다', 'success', 6000);
            navigate(`/migrations/${encodeURIComponent(created.migration.id)}`);
          } catch (err) {
            toastError(err);
            e.currentTarget.disabled = false;
          }
        },
      }, icon('refresh'), '롤백 계획 만들기'),
    );
  })();
}

async function captureVersion(connID, reload) {
  const noteInput = input({ placeholder: '예: 운영 긴급 패치로 인덱스 추가' });
  openModal({
    title: '현재 상태를 버전으로 등록',
    width: 520,
    body: () => [
      h('p.modal-message', {},
        '대상 DB의 현재 스키마를 읽어 새 버전으로 확정합니다. ' +
        '이전 버전과 구조가 같으면 새로 만들지 않습니다.'),
      h('label.field', {}, h('span.field-label', {}, '메모'), noteInput),
    ],
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: async (e) => {
          e.currentTarget.disabled = true;
          try {
            const res = await api.post(`/connections/${encodeURIComponent(connID)}/versions`, {
              note: noteInput.value,
            });
            close();
            if (res.created) {
              toast(`v${res.version.versionNo} 을 등록했습니다 (변경 ${res.changes?.length ?? 0}건)`, 'success');
            } else {
              toast(res.message ?? '변경이 없어 새 버전을 만들지 않았습니다', 'info');
            }
            reload();
          } catch (err) {
            toastError(err);
          }
        },
      }, '등록'),
    ],
  });
}

// showVersionSQL은 그 버전의 구조를 만드는 SQL을 보여준다.
//
// 비교 없이 단독으로 보는 길이 필요한 이유: "지금과 무엇이 다른가"와 "그때 구조가
// 어땠나"는 다른 질문이다. 후자는 옛 구조를 다른 곳에 다시 만들 때(재현 환경, 사고
// 조사) 필요하고, 그때 쓸 수 있는 것은 차이 목록이 아니라 완전한 CREATE 스크립트다.
//
// 스냅샷으로 만드는 이유: 그 시점의 DB는 이미 없다. 저장된 버전 본문이 유일한 출처다.
async function showVersionSQL(conn, v) {
  const body = h('div', {}, spinner('SQL을 만드는 중…'));
  openModal({
    title: `v${v.versionNo} 의 SQL — ${conn.name}`,
    width: 900,
    body: () => body,
    footer: (close) => [h('button.btn', { type: 'button', onclick: close }, '닫기')],
  });

  try {
    const res = await api.get(
      `/connections/${encodeURIComponent(conn.id)}/schema/ddl?version=${v.id}`);
    const sql = res.upSql ?? '';
    const warnings = res.plan?.warnings ?? [];
    mount(body,
      h('p.field-help', {},
        `${formatDate(v.createdAt)} 에 기록된 구조입니다`,
        v.note ? ` — ${v.note}` : '',
        `. ${res.dialect ?? conn.kind} 문법으로 만들었습니다.`),
      warnings.length
        ? h('div.notice.notice-warn', {}, icon('alert'),
          h('div', {}, warnings.map((w) => h('div', {}, w))))
        : null,
      sqlBlock('CREATE 스크립트', sql, res.plan?.up?.length ?? 0),
      sql
        ? h('div.node-actions', {},
          h('button.btn.btn-small', {
            type: 'button',
            onclick: () => downloadText(`${conn.name}_v${v.versionNo}.sql`, sql),
          }, icon('save'), '파일로 저장'),
        )
        : null,
    );
  } catch (err) {
    mount(body, errorPanel(err));
  }
}

// downloadText는 만든 문자열을 파일로 내려준다.
//
// 서버에 파일을 만들지 않는 이유: 이 스크립트는 이미 브라우저에 다 와 있다. 내려받기
// 경로를 서버에 두면 같은 것을 두 번 만들고, 그 파일을 지울 책임도 새로 생긴다.
function downloadText(name, text) {
  const url = URL.createObjectURL(new Blob([text], { type: 'text/plain;charset=utf-8' }));
  const a = h('a', { href: url, download: name });
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}

// showVersionDiff는 한 버전을 기준으로 무엇이 다른지 보여준다.
//
// 비교 대상을 대화상자 안에서 바꿀 수 있게 둔 이유: "현재 DB와 다른가"와
// "저 버전과 무엇이 다른가"는 같은 질문의 두 형태다. 창을 닫고 다른 버튼을
// 찾아 다시 여는 동안 방금 본 변경 목록이 사라져 비교가 끊긴다.
function showVersionDiff(connID, from, to, versions = []) {
  // 기준도 고를 수 있어야 한다. 지금까지는 목록에서 누른 버전이 기준으로 고정되어,
  // "v3와 v5를 비교"하려면 창을 닫고 v3부터 다시 눌러야 했다.
  const all = versions.length ? versions : [from];
  const baseSelect = select(
    all.map((v) => ({
      value: String(v.id),
      label: `v${v.versionNo} · ${formatDate(v.createdAt)}`,
    })),
    { value: String(from.id) },
  );
  const targetSelect = select([], { value: to ? String(to.id) : '' });

  // 대상 목록은 기준을 뺀 나머지다. 자기 자신과의 비교는 언제나 "같음"이라
  // 고를 수 있게 두면 잘못 고른 것으로만 보인다.
  const fillTargets = () => {
    const baseID = baseSelect.value;
    const keep = targetSelect.value;
    const options = [
      { value: '', label: '현재 DB' },
      ...all.filter((v) => String(v.id) !== baseID).map((v) => ({
        value: String(v.id),
        label: `v${v.versionNo} · ${formatDate(v.createdAt)}`,
      })),
    ];
    targetSelect.replaceChildren(...options.map((o) => h('option', {
      value: o.value, selected: o.value === keep,
    }, o.label)));
    if (targetSelect.value !== keep) targetSelect.value = '';
  };
  fillTargets();

  const title = h('span', {});
  const body = h('div', {}, spinner('비교하는 중…'));
  openModal({
    title: '버전 비교',
    width: 760,
    body: () => [
      h('div.diff-picker', {},
        h('span.field-label', {}, '기준'),
        baseSelect,
        h('span.diff-arrow', {}, '→'),
        h('span.field-label', {}, '대상'),
        targetSelect,
        title,
      ),
      body,
    ],
  });

  const load = async () => {
    mount(body, spinner('비교하는 중…'));
    try {
      const q = new URLSearchParams({ from: baseSelect.value });
      if (targetSelect.value) q.set('to', targetSelect.value);
      const res = await api.get(`/connections/${encodeURIComponent(connID)}/versions/diff?${q}`);
      const changes = res.diff?.changes ?? [];
      mount(body,
        h('p.erd-diff-summary', {},
          `${res.from.label} → ${res.to.label}`,
          badge(`${changes.length}건`, changes.length ? 'info' : 'success'),
          res.diff?.destructiveCount ? badge(`파괴적 ${res.diff.destructiveCount}`, 'danger') : null),
        changes.length === 0
          ? h('p.notice.notice-success', {}, icon('check'), '구조가 같습니다')
          : h('table.table', {},
            h('thead', {}, h('tr', {}, h('th', {}, '종류'), h('th', {}, '내용'))),
            h('tbody', {}, changes.map((c) => h('tr', { class: c.destructive ? 'is-destructive' : '' },
              h('td.nowrap', {}, badge(c.kind, c.destructive ? 'danger' : 'neutral')),
              h('td', {}, c.summary),
            )))),
      );
    } catch (err) {
      mount(body, errorPanel(err));
    }
  };

  baseSelect.addEventListener('change', () => { fillTargets(); load(); });
  targetSelect.addEventListener('change', load);
  load();
}
