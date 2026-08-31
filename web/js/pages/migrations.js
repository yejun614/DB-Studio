// 마이그레이션: 목록 → 상세(변경 목록·SQL·리뷰) → 사전 검사 → 실행/롤백.
import { api } from '../core/api.js';
import { withProject } from '../core/project.js';
import { state, kindLabel } from '../core/store.js';
import {
  h, mount, icon, select, input, textarea, spinner, emptyState, pageHeader,
  badge, envBadge, toast, toastError, relativeTime, formatDate, openModal,
  copyToClipboard, field, confirmDialog,
} from '../core/ui.js';
import { navigate } from '../core/router.js';
import { serverDbPicker } from '../core/connpick.js';
import { peoplePicker } from '../core/searchpick.js';
import { codeBlock } from '../core/highlight.js';
import { runDryRun } from '../core/dryrun.js';
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
    res = await api.get(withProject(
      `/migrations/${status ? `?status=${encodeURIComponent(status)}` : ''}`));
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
      applyGate(m, res, reload),
    ),
    precheckBox,
    warningsPanel(m),
    changesPanel(m),
    sqlPanel(m),
    reviewsPanel(m, res, reload),
    executionPanel(m),
    activityPanel(m),
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
  // 의견만 남긴 사람도 기다리는 사람이다. 의견을 결정으로 세면 "다 봤다"로 표시된
  // 채 승인 수는 차지 않아, 무엇을 기다리는지 알 수 없게 된다.
  const waiting = pendingList(m);
  if (waiting.length === 0) return null;
  return h('p.field-help', {},
    '기다리는 리뷰어: ',
    ...waiting.map((name, i) => h('span', {}, i ? ', ' : '', h('b', {}, name))));
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
    //
    // 슈퍼 어드민은 예외다(서버도 같은 예외를 둔다). 슈퍼 어드민은 자기가 맡은
    // 계획도 승인할 수 있으므로, 여기서만 막으면 "승인은 되는데 리뷰어로는 못 넣는"
    // 어긋난 상태가 된다. 대신 그것이 예외라는 것을 문장으로 말해 둔다.
    const syncExclusion = () => {
      const who = people.find((p) => p.id === assignee.value);
      const superadmin = who?.role === 'superadmin';
      const dropped = reviewers.setExcluded(
        assignee.value && !superadmin ? [assignee.value] : []);
      selfNote.textContent = '';
      if (assignee.value) {
        selfNote.textContent = superadmin
          ? `담당자(${who ? label(who) : assignee.value})는 슈퍼 어드민이라 리뷰어로도 고를 수 있습니다.`
            + ' 자기 계획을 자기가 승인한 기록은 활동 기록에 남습니다.'
          : `담당자(${who ? label(who) : assignee.value})는 리뷰어로 고를 수 없습니다.`;
      }
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

// canReviewNow는 지금 결정을 남기거나 바꿀 수 있는 상태인지다(서버의 reviewableStatus와 같다).
//
// 두 곳(도구 줄, 리뷰 칸)이 같은 판단을 하도록 함수로 둔다. 한쪽만 고치면 "여기서는
// 보이는데 저기서는 안 보인다"가 되고, 그것이 바로 반려 뒤에 일어났던 일이다.
function canReviewNow(status) {
  return status === 'in_review' || status === 'approved' || status === 'rejected';
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

// hasRun은 이 계획이 이미 실행된 적이 있는지다.
//
// 적용된 뒤 닫은 계획도 실행된 계획이다 — 닫혔다는 이유로 "아직 안 한 일"처럼
// 다루면 미리 검사 같은 것이 다시 나타난다. 그 SQL은 이미 이력이다.
function hasRun(m) {
  switch (m.status) {
    case 'applied':
    case 'rolled_back':
    case 'failed':
      return true;
    case 'closed':
      return m.closedFrom === 'applied';
  }
  return false;
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
  if (canReviewNow(m.status)) {
    const mine = myDecision(m);
    buttons.push(h('button.btn', {
      type: 'button',
      onclick: () => openReviewDialog(m, res, reload),
    }, icon('check'), mine ? '검토 바꾸기' : '검토하기'));
  }
  // 미리 검사는 승인 전에도 쓸 수 있어야 한다. SQL이 깨진 것을 승인이 끝난 뒤에
  // 알면 리뷰를 처음부터 다시 받아야 하고, 그 한 바퀴가 이 기능이 없애려던 것이다.
  //
  // 사전 검사와 다른 일을 한다. 사전 검사는 "지금 실행해도 되는 조건인가"(승인 수,
  // 드리프트)를 보고, 미리 검사는 "이 SQL이 이 DB에서 도는가"를 본다. 둘 다 필요해서
  // 둘 다 둔다 — 조건은 맞는데 SQL이 깨진 경우가 바로 이 기능이 생긴 까닭이다.
  if (!hasRun(m)) {
    buttons.push(h('button.btn', {
      type: 'button',
      onclick: (e) => runDryRun({
        path: `/migrations/${encodeURIComponent(m.id)}/dryrun`,
        box: precheckBox,
        button: e.currentTarget,
      }),
    }, icon('shield'), '미리 검사'));
  }
  if (m.status === 'approved') {
    buttons.push(h('button.btn', {
      type: 'button',
      onclick: () => runPrecheck(m, precheckBox),
    }, icon('activity'), '사전 검사'));
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
  // 롤백은 "이 변경을 물린다"이지 "이 계획을 버린다"가 아니다. 원인을 고친 뒤 같은
  // 변경을 다시 넣는 일이 흔한데, 그때마다 계획을 새로 만들어야 한다면 사람들은
  // 롤백을 누르기를 망설이게 된다 — 되돌리기가 비싸지면 아무도 되돌리지 않는다.
  if (m.status === 'rolled_back') {
    buttons.push(h('button.btn.btn-primary', {
      type: 'button',
      onclick: () => reopenForRerun(m, reload),
    }, icon('play'), '다시 실행'));
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
  //
  // 적용됨·롤백됨도 닫는다. 끝난 계획이 목록에 영원히 남으면 "지금 볼 것"과 "끝난
  // 것"이 섞여, 목록은 훑을수록 무거워진다.
  //
  // 닫아도 사실이 사라지지 않는다. 닫기 전 상태를 기억했다가 다시 열 때 그 자리로
  // 돌려보내므로, 적용된 계획은 적용됨으로 돌아오고 롤백할 길도 그대로다.
  if (m.status !== 'closed') {
    buttons.push(h('button.btn', {
      type: 'button',
      onclick: () => changeStatus(m.id, 'closed', reload),
    }, icon('x'), '닫기'));
  }
  if (m.status === 'closed') {
    // 닫혀 있는 동안 대상 DB가 바뀌었을 수 있으므로 초안으로 돌아간다.
    // 그때의 승인은 지금 구조를 본 것이 아니다.
    //
    // 다만 적용된 상태에서 닫은 계획은 적용됨으로 돌아간다. 그것을 초안으로
    // 보내면 DB에 들어가 있는 변경이 "아직 실행하지 않은 초안"으로 보이고,
    // 롤백 버튼도 사라져 되돌릴 방법이 화면에서 없어진다.
    //
    // 먼저 묻는 이유: 이 버튼은 남아 있는 리뷰를 지운다. "다시 열기"라는 이름만
    // 보면 되돌리는 일 같지만, 실제로는 승인 기록이 사라지는 일이다. 무엇이
    // 사라지는지 말하지 않고 지우면, 사라진 것을 나중에야 알게 된다.
    buttons.push(h('button.btn.btn-primary', {
      type: 'button',
      onclick: () => reopen(m, reload),
    }, icon('refresh'), '다시 열기'));
  }

  return h('div.mig-actions', {}, buttons);
}

// applyGate는 실행 버튼이 없는 까닭과 다음에 할 일을 한 줄로 말한다.
//
// 버튼을 그냥 감추면 화면은 "실행할 수 없다"까지만 말하고 "왜"와 "그래서 뭘 해야
// 하는가"를 빠뜨린다. 승인 1/2 이라는 숫자는 옆에 있지만, 그것이 실행 버튼이 없는
// 이유라는 것은 이 흐름을 아는 사람만 안다. 승인이 모자란 계획에서 "실행 버튼이
// 안 보이네요?" 라는 물음이 실제로 나왔다.
//
// 비활성 버튼을 두는 대신 문장을 쓰는 이유: 눌리지 않는 버튼은 이유를 말해 주지
// 않으면서 자리만 차지한다. 여기서 필요한 것은 누를 것이 아니라 설명이다.
function applyGate(m, res, reload) {
  // 실행 버튼이 이미 있거나, 실행이 화제가 아닌 상태에서는 아무 말도 하지 않는다.
  if (m.status === 'approved') return null;
  const short = Math.max(0, res.requiredApprovals - res.approvals);
  // 필요한 수가 1을 넘는 경우에만 그 숫자를 말한다. 규칙이 바뀌어도 문장은 참이다.
  const why = res.requiredApprovals > 1
    ? ` 이 계획은 승인 ${res.requiredApprovals}명이 필요합니다.`
    : '';

  if (m.status === 'draft') {
    return gateNote('info',
      h('span', {},
        '아직 ', h('b', {}, '초안'), ' 이라 실행할 수 없습니다. ',
        '리뷰어를 지정하면 리뷰가 시작되고, 승인이 차면 실행 버튼이 나타납니다.', why),
      h('button.btn.btn-small', {
        type: 'button', onclick: () => openAssignDialog(m, reload),
      }, icon('users'), '리뷰어 지정'));
  }
  if (m.status === 'in_review') {
    // 세 가지를 가른다. "기다리는 사람이 없다"와 "부탁받은 사람이 아예 없다"는
    // 화면에서는 비슷해 보이지만 할 일이 다르다 — 앞은 리뷰어를 더 부르는 것이고,
    // 뒤는 아직 아무에게도 부탁하지 않은 것이다. 한 문장으로 합치면 지정된
    // 리뷰어가 이미 결정한 계획에서 "리뷰어가 없습니다"라는 거짓말이 된다.
    const waiting = pendingList(m);
    const none = (m.reviewers ?? []).length === 0;
    let tail = ` 기다리는 리뷰어: ${waiting.join(', ')}.`;
    if (none) tail = ' 아직 아무에게도 부탁하지 않았습니다 — 리뷰어를 지정하세요.';
    else if (waiting.length === 0) {
      tail = ' 지정된 리뷰어는 모두 결정했습니다. 승인이 더 필요하니 리뷰어를 더 지정하세요.';
    }
    return gateNote('info',
      h('span', {},
        '승인이 ', h('b', {}, `${short}명`), ' 더 필요해서 실행 버튼이 아직 없습니다',
        ` (지금 ${res.approvals}/${res.requiredApprovals}).`, why, tail),
      waiting.length
        ? null
        : h('button.btn.btn-small', {
          type: 'button', onclick: () => openAssignDialog(m, reload),
        }, icon('users'), '리뷰어 지정'));
  }
  if (m.status === 'rejected') {
    return gateNote('warn',
      h('span', {},
        h('b', {}, '반려된'), ' 계획이라 실행할 수 없습니다. ',
        '반려를 남긴 사람이 결정을 바꾸거나, 지적을 반영한 새 계획이 필요합니다.'));
  }
  if (m.status === 'closed') {
    return gateNote('info', m.closedFrom === 'applied'
      ? h('span', {},
        h('b', {}, '닫힌'), ' 계획입니다. 이미 적용된 뒤에 닫혔으므로, 다시 열면 ',
        h('b', {}, '적용됨'), ' 으로 돌아가고 롤백할 수 있습니다.')
      : h('span', {},
        h('b', {}, '닫힌'), ' 계획입니다. 다시 열면 초안으로 돌아가고, 승인을 다시 받아야 합니다.'));
  }
  if (m.status === 'rolled_back') {
    return gateNote('info',
      h('span', {},
        '롤백해서 이 변경은 물러 있습니다. ', h('b', {}, '다시 실행'),
        ' 을 누르면 같은 계획을 다시 실행할 수 있습니다 — 승인 기록은 그대로 유지됩니다.'));
  }
  // 적용됨·실패는 실행이 이미 지나간 일이다. 실행 이력 칸이 그것을 말한다.
  return null;
}

// gateNote는 도구 줄 아래 한 줄 안내다. 오른쪽에 바로 할 일을 놓을 수 있다.
function gateNote(kind, message, action = null) {
  return h(`p.notice.notice-${kind}.mig-gate`, {},
    icon(kind === 'warn' ? 'alert' : 'activity'), message, action);
}

// pendingList는 아직 결정하지 않은 리뷰어 이름이다.
function pendingList(m) {
  const decided = new Set((m.reviews ?? [])
    .filter((r) => r.decision !== 'comment')
    .map((r) => r.reviewerId)
    .filter(Boolean));
  return (m.reviewers ?? [])
    .filter((r) => !decided.has(r.userId))
    .map((r) => r.name || r.userId);
}

async function changeStatus(id, status, reload, message = '상태를 변경했습니다') {
  try {
    await api.post(`/migrations/${encodeURIComponent(id)}/status`, { status });
    toast(message, 'success');
    reload();
  } catch (err) {
    toastError(err);
  }
}

// reopenForRerun은 롤백된 계획을 다시 실행 대기(승인됨)로 되돌린다.
//
// 닫기 후 다시 열기와 다른 점: 승인 기록을 지우지 않는다. 계획의 내용이 바뀌지
// 않았고 롤백으로 DB가 실행 전 구조로 돌아왔으므로, 그때의 승인은 여전히 "지금 이
// 구조에 이 변경을 해도 좋다"는 뜻이다. 구조가 실제로 그런지는 사전 검사가 기준
// 지문으로 다시 확인한다.
async function reopenForRerun(m, reload) {
  const ok = await confirmDialog({
    title: '다시 실행',
    message: '이 계획을 다시 실행할 수 있는 상태(승인됨)로 되돌립니다. '
      + '승인 기록은 그대로 두므로 다시 승인받을 필요가 없습니다.',
    confirmLabel: '다시 실행 준비',
    details: h('p.notice.notice-info', {}, icon('activity'),
      h('span', {},
        '롤백된 뒤 DB가 다른 곳에서 또 바뀌었을 수 있습니다. 실행 전에 ',
        h('b', {}, '사전 검사'), ' 로 지금 구조가 계획의 기준과 같은지 확인하세요.')),
  });
  if (!ok) return;
  changeStatus(m.id, 'approved', reload, '다시 실행할 수 있습니다. 사전 검사 후 실행하세요');
}

// reopen은 닫은 계획을 초안으로 되돌린다.
//
// 승인을 그대로 두고 열 수는 없다. 닫혀 있는 동안 대상 DB가 바뀌었을 수 있고,
// 그때의 승인은 지금 구조를 본 것이 아니다 — 그것을 인정하면 아무도 지금 상태를
// 보지 않은 채 실행할 수 있다. 서버도 draft 전이에서 리뷰를 지운다.
async function reopen(m, reload) {
  // 적용된 뒤 닫은 계획은 적용됨으로 돌아간다. 리뷰도 그대로다 — 지울 이유가 없다.
  // 그 계획은 이미 실행됐고, 다시 여는 것은 "끝난 일을 다시 보이게 하는 것"뿐이다.
  if (m.closedFrom === 'applied') {
    const ok = await confirmDialog({
      title: '계획 다시 열기',
      message: '이 계획은 적용된 뒤에 닫혔습니다. 적용됨 상태로 되돌립니다 — '
        + '실행 기록과 리뷰는 그대로이고, 다시 롤백할 수 있게 됩니다.',
      confirmLabel: '다시 열기',
    });
    if (!ok) return;
    changeStatus(m.id, 'applied', reload, '적용됨 상태로 되돌렸습니다');
    return;
  }
  const left = (m.reviews ?? []).length;
  const ok = await confirmDialog({
    title: '계획 다시 열기',
    message: '초안으로 돌아갑니다. 닫혀 있는 동안 대상 DB가 바뀌었을 수 있어 '
      + '그때의 승인은 인정하지 않습니다 — 리뷰어를 다시 지정해 승인을 새로 받아야 합니다.',
    confirmLabel: '다시 열기',
    details: left
      ? h('p.notice.notice-warn', {}, icon('alert'),
        h('span', {},
          '남아 있는 리뷰 ', h('b', {}, `${left}건`), ' 이 지워집니다. ',
          '누가 무엇을 결정했는지는 아래 ', h('b', {}, '활동 기록'), ' 에 남습니다.'))
      : null,
  });
  if (!ok) return;
  changeStatus(m.id, 'draft', reload, '계획을 다시 열었습니다. 승인을 다시 받아야 합니다');
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
      ' 담당자는 자기가 맡은 계획을 승인할 수 없습니다(슈퍼 어드민은 예외).',
      ' 실행 전이라면 결정을 바꿀 수 있고, 남긴 기록을 눌러 고치거나 지울 수 있습니다.',
      res.requiredApprovals > 1
        ? ` 이 계획은 승인 ${res.requiredApprovals}명이 필요합니다.`
        : ' 승인 1명이면 실행할 수 있습니다.'),
    pendingReviewers(m),
    reviews.length === 0
      ? h('p.muted', {}, '아직 리뷰가 없습니다')
      : h('div.mig-reviews', {}, reviews.map((r) => reviewRow(m, r, reload))),
    // 반려된 뒤에도 남아 있어야 한다. 여기가 리뷰를 보다가 바로 누르는 자리이고,
    // 마음을 바꾸는 일은 대개 남의 결정을 읽은 직후에 일어난다.
    canReviewNow(m.status)
      ? h('button.btn.btn-primary', {
        type: 'button', onclick: () => openReviewDialog(m, res, reload),
      }, icon('check'), myDecision(m) ? '검토 바꾸기' : '검토 의견 남기기')
      : null,
  );
}

// reviewRow는 리뷰 한 건을 그린다. 내가 손댈 수 있는 것이면 눌러서 고칠 수 있다.
//
// 누를 수 있는 줄과 그냥 읽는 줄을 겉모습으로 구분한다(is-editable). 눌러도 아무
// 일도 일어나지 않는 자리를 손가락 모양으로 가리키면, 다음부터는 아무 줄도 누르지
// 않게 된다.
function reviewRow(m, r, reload) {
  const rights = reviewRights(m, r);
  const open = () => openReviewEditDialog(m, r, reload);
  const canOpen = rights.canEdit || rights.canDelete;
  const body = [
    h('div.mig-review-head', {},
      h('strong', {}, r.reviewerName || '알 수 없음'),
      reviewBadge(r.decision),
      h('span.muted', {}, relativeTime(r.createdAt)),
      canOpen ? h('span.mig-review-edit', {}, icon('edit'), '고치기') : null,
    ),
    r.comment ? h('p.mig-review-comment', {}, r.comment) : null,
  ];
  if (!canOpen) return h('div.mig-review', {}, ...body);
  return h('div.mig-review.is-editable', {
    role: 'button',
    tabindex: '0',
    title: rights.canEdit ? '눌러서 고치거나 지웁니다' : '눌러서 지웁니다',
    onclick: open,
    onkeydown: (e) => {
      if (e.key !== 'Enter' && e.key !== ' ') return;
      e.preventDefault();
      open();
    },
  }, ...body);
}

// reviewRights는 이 리뷰 한 건을 내가 고칠 수 있는지·지울 수 있는지다.
//
// 서버와 같은 규칙을 둔다. 고치기는 본인만(남의 입에 말을 넣을 수는 없다),
// 지우기는 본인 또는 슈퍼 어드민(아무도 치울 수 없는 부스러기를 남기지 않는다).
// 실행된 계획의 승인·반려는 실행을 허락한 근거이므로 지울 수 없다.
function reviewRights(m, r) {
  const me = state.user?.id;
  const mine = Boolean(me) && r.reviewerId === me;
  const superadmin = state.user?.role === 'superadmin';
  const isDecision = r.decision !== 'comment';
  return {
    mine,
    canEdit: mine,
    canDelete: (mine || superadmin) && (!isDecision || canReviewNow(m.status)),
    isDecision,
  };
}

// openReviewEditDialog는 리뷰 한 건을 고치거나 지우는 창이다.
//
// 결정 자체(승인·반려)는 여기서 바꾸지 않는다. 그것은 승인 수와 상태를 움직이는
// 일이라 "검토 바꾸기"를 지나야 하고, 여기서 하는 일은 적어 둔 말을 고치거나
// 기록을 거두는 것이다. 두 창을 섞으면 오타를 고치려다 상태를 바꾸게 된다.
function openReviewEditDialog(m, r, reload) {
  const rights = reviewRights(m, r);
  const label = { approved: '승인', rejected: '반려', comment: '의견' }[r.decision] ?? r.decision;
  const commentInput = textarea({
    value: r.comment ?? '',
    placeholder: rights.isDecision ? '이 결정에 남길 말' : '의견',
    autofocus: rights.canEdit ? '' : null,
  });

  const save = async (close) => {
    try {
      await api.patch(
        `/migrations/${encodeURIComponent(m.id)}/review/${r.id}`,
        { comment: commentInput.value },
      );
      close();
      toast('리뷰를 고쳤습니다', 'success');
      reload();
    } catch (err) {
      toastError(err);
    }
  };

  // 이름에 조사를 붙이면 "슈퍼 어드민 의 의견" 처럼 어긋난다. "남긴" 을 끼워
  // 문장을 만들고, 내 것이면 이름을 부르지 않는다.
  const who = rights.mine
    ? '내가 남긴'
    : `${r.reviewerName || '알 수 없음'} 님이 남긴`;

  const remove = async (close) => {
    // 결정을 거두면 승인 수가 줄고 상태가 따라 움직인다. 그것까지 말해 준다 —
    // "리뷰 하나 지우기"로 보이는 일이 실제로는 승인됨을 리뷰 중으로 되돌린다.
    const ok = await confirmDialog({
      title: '리뷰 지우기',
      message: `${who} ${rights.isDecision
        ? `${label} 기록을 지웁니다. 승인 수가 다시 계산되어 계획 상태가 바뀔 수 있습니다.`
        : '의견을 지웁니다. 되돌릴 수 없습니다.'}`,
      confirmLabel: '지우기',
      danger: true,
    });
    if (!ok) return;
    try {
      const out = await api.del(`/migrations/${encodeURIComponent(m.id)}/review/${r.id}`);
      close();
      toast(rights.isDecision
        ? `기록을 지웠습니다 (승인 ${out.approvals}/${out.requiredApprovals})`
        : '의견을 지웠습니다', 'success');
      reload();
    } catch (err) {
      toastError(err);
    }
  };

  openModal({
    title: rights.isDecision ? `${label} 기록` : '의견',
    width: 520,
    body: () => [
      h('div.mig-review-head', {},
        h('strong', {}, r.reviewerName || '알 수 없음'),
        reviewBadge(r.decision),
        h('span.muted', {}, formatDate(r.createdAt)),
      ),
      rights.canEdit
        ? field(rights.isDecision ? '이 결정에 남긴 말' : '의견', commentInput,
          rights.isDecision
            ? '결정을 바꾸려면 리뷰 칸의 "검토 바꾸기" 를 쓰세요. 여기서는 말만 고쳐집니다.'
            : '남긴 말을 고칩니다.')
        : h('p.mig-review-comment', {}, r.comment || '(내용 없음)'),
      rights.canEdit
        ? null
        : h('p.notice.notice-info', {}, icon('activity'),
          h('span', {}, '남이 남긴 기록이라 ', h('b', {}, '고칠 수는 없습니다'),
            '. 슈퍼 어드민은 지울 수 있습니다.')),
      rights.isDecision && !canReviewNow(m.status)
        ? h('p.notice.notice-warn', {}, icon('alert'),
          h('span', {}, '이미 실행된 계획입니다. 이 ', h('b', {}, label),
            ' 은 실행을 허락한 근거이므로 지울 수 없습니다.'))
        : null,
    ],
    footer: (close) => [
      rights.canDelete
        ? h('button.btn.btn-danger', { type: 'button', onclick: () => remove(close) },
          icon('trash'), '지우기')
        : null,
      h('span.spacer'),
      h('button.btn', { type: 'button', onclick: close }, '닫기'),
      rights.canEdit
        ? h('button.btn.btn-primary', { type: 'button', onclick: () => save(close) }, '저장')
        : null,
    ],
  });
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

// activityPanel은 이 계획에 일어난 일을 시간 순으로 모두 보여준다.
//
// 예전에는 Git 푸시 이력만 칸으로 있었다. 그래서 "누가 언제 등록했고 누가 승인했고
// 누가 닫았는가"는 화면 어디에도 없었다 — 리뷰 칸은 지금 남아 있는 결정만 보여주고,
// 초안으로 되돌리면 그 결정마저 지워진다. 그러면 나중에 "이거 누가 승인했었죠?"에
// 아무도 답할 수 없다. 감사 로그에는 남아 있었지만 그것은 슈퍼 어드민만 볼 수 있다.
//
// 늦게 불러오는 이유: 이력이 없다고 계획을 못 보는 것은 아니다. 본문이 먼저 뜨고
// 이 칸만 나중에 채워진다.
function activityPanel(m) {
  const list = h('p.muted', {}, '불러오는 중…');
  const card = h('section.card.mig-activity', {},
    h('h2', {}, icon('list'), '활동 기록'),
    h('p.field-help', {},
      '등록·지정·승인·반려·의견·실행·롤백·닫기·다시 열기·Git 푸시를 모두 남깁니다. ',
      '리뷰 칸의 결정이 지워진 뒤에도 여기에는 남습니다.'),
    list);

  (async () => {
    try {
      const res = await api.get(`/migrations/${encodeURIComponent(m.id)}/activity`);
      const items = res.activity ?? [];
      if (items.length === 0) {
        mount(list, h('p.muted', {}, '기록이 없습니다'));
        return;
      }
      mount(list, activityList(items, res.total ?? items.length));
    } catch (err) {
      mount(list, h('p.muted', {}, `이력을 불러오지 못했습니다: ${err.message ?? err}`));
    }
  })();

  return card;
}

// activityList는 줄이 많을 때 오래된 것을 접어 둔다.
//
// 최근 것부터 눈에 들어와야 한다. 오래 살아 있는 계획은 수십 줄이 되는데, 그 전부를
// 펼쳐 두면 정작 방금 무슨 일이 있었는지가 화면 밖으로 밀려난다. 접되 감추지는
// 않는다 — 몇 건이 접혀 있는지 적고, 한 번 누르면 모두 펼쳐진다.
const ACTIVITY_HEAD = 25;

function activityList(items, total) {
  const hidden = Math.max(0, items.length - ACTIVITY_HEAD);
  const rows = h('div.act-list', {}, items.slice(hidden).map(activityRow));
  const more = hidden
    ? h('button.btn.btn-small', {
      type: 'button',
      onclick: (ev) => {
        ev.currentTarget.remove();
        mount(rows, items.map(activityRow));
      },
    }, icon('list'), `이전 기록 ${hidden}건 더 보기`)
    : null;
  // 서버가 잘라 온 경우까지 말해 준다. 잘린 줄 모르면 "이게 전부"로 읽힌다.
  const cut = total > items.length
    ? h('p.muted', {}, `오래된 기록 ${total - items.length}건은 담지 않았습니다. 전체는 감사 로그에 있습니다.`)
    : null;
  return h('div', {}, cut, more, rows);
}

function activityRow(e) {
  const d = e.detail ?? {};
  return h('div.act-row', {},
    h('span.act-dot', {}),
    h('div.act-body', {},
      h('div.act-head', {},
        h('strong', {}, e.actorName || '알 수 없음'),
        h('span.act-what', {}, activityText(e, d)),
        e.result && e.result !== 'ok' ? badge(RESULT_WORD[e.result] ?? e.result, 'danger') : null,
        d.via === 'ai' ? badge('AI', 'info') : null,
      ),
      h('span.act-time', { title: formatDate(e.at) }, relativeTime(e.at)),
    ),
  );
}

const RESULT_WORD = { denied: '막힘', error: '실패' };

const DECISION_WORD = { approved: '승인', rejected: '반려', comment: '의견' };

// activityText는 기록 한 줄을 사람 말로 옮긴다.
//
// 모르는 동작도 줄을 남긴다(동작 이름을 그대로 적는다). 옮길 말이 없다고 감추면
// 이력에 구멍이 생기는데, 이력의 값어치는 "빠진 것이 없다"에서 나온다.
function activityText(e, d) {
  switch (e.action) {
    case 'migration.create':
      return '계획을 등록했습니다' + countTail(d);
    case 'schema.comments.plan':
      return '설명을 고쳐 계획을 만들었습니다' + countTail(d);
    case 'migration.assigned':
      return `담당자·리뷰어를 지정했습니다 (담당자 ${d.assignee || '없음'}, 리뷰어 ${d.reviewers ?? 0}명)`;
    case 'migration.status':
      return statusText(d);
    case 'migration.review': {
      const word = DECISION_WORD[d.decision] ?? d.decision;
      if (d.decision === 'comment') return '의견을 남겼습니다';
      return `${word}했습니다 (승인 ${d.approvals ?? '?'}/${d.required ?? '?'})`;
    }
    case 'migration.dryrun':
      if (d.skipped) return '미리 검사를 하지 못했습니다';
      return d.ok
        ? `미리 검사를 통과했습니다 (${d.statements ?? '?'}문장)`
        : '미리 검사에서 SQL이 막혔습니다';
    case 'migration.review.update':
      return '남긴 리뷰의 내용을 고쳤습니다';
    case 'migration.review.delete': {
      const word = DECISION_WORD[d.decision] ?? d.decision;
      const who = d.author ? `${d.author} 님이 남긴 ` : '';
      return `${who}${word} 기록을 지웠습니다`;
    }
    case 'migration.apply':
      if (e.result && e.result !== 'ok') {
        return `실행하지 못했습니다${d.error ? ` — ${d.error}` : ''}`;
      }
      return `실행했습니다 (문장 ${d.applied ?? d.statements ?? '?'}개)`;
    case 'migration.rollback':
      return `롤백했습니다 (문장 ${d.applied ?? '?'}개)`;
    case 'vcs.push':
      return `Git에 올렸습니다${pushTail(d)}`;
    case 'migration.delete':
      return '계획을 지웠습니다';
    default:
      return e.action;
  }
}

function countTail(d) {
  const parts = [];
  if (d.changes != null) parts.push(`변경 ${d.changes}건`);
  if (d.destructive) parts.push(`파괴적 ${d.destructive}건`);
  if (d.statements != null) parts.push(`SQL ${d.statements}문장`);
  return parts.length ? ` (${parts.join(' · ')})` : '';
}

function pushTail(d) {
  const parts = [];
  if (d.branch) parts.push(d.branch);
  if (d.commit) parts.push(String(d.commit).slice(0, 7));
  if (d.files != null) parts.push(`파일 ${d.files}개`);
  return parts.length ? ` (${parts.join(' · ')})` : '';
}

// statusText는 상태가 어디서 어디로 갔는지를 사람 말로 옮긴다.
//
// "draft → in_review" 를 그대로 보여주지 않는 이유: 이 줄을 읽는 사람이 알고 싶은
// 것은 상태 이름이 아니라 무슨 일이 있었는가다. 되돌아간 경우에는 그 대가(승인이
// 지워졌다)까지 적는다 — 그것이 나중에 "승인 기록이 왜 없지?"의 답이다.
function statusText(d) {
  const from = STATUS[d.from]?.[0] ?? d.from;
  const to = STATUS[d.to]?.[0] ?? d.to;
  if (d.to === 'closed') return '계획을 닫았습니다';
  if (d.from === 'closed' && d.to === 'draft') {
    return '닫은 계획을 다시 열었습니다 (초안으로 돌아가 승인을 다시 받아야 합니다)';
  }
  if (d.from === 'closed' && d.to === 'applied') {
    return '닫은 계획을 다시 열었습니다 (적용됨으로 돌아왔습니다)';
  }
  if (d.to === 'draft') return '초안으로 되돌렸습니다 (그때까지의 승인·반려가 지워졌습니다)';
  if (d.to === 'in_review') return '리뷰를 시작했습니다';
  return `상태를 ${from} 에서 ${to} 로 바꿨습니다`;
}

// execRow는 실행 기록 한 줄이다.
//
// 오류 메시지를 SQL **아래**에 둔다. 예전에는 오른쪽 칸에 넣었는데, 그 칸은 표에서
// 가장 좁은 자리라 긴 메시지가 세로로 길게 눌러 담기고(한 줄에 서너 글자), 정작
// 읽어야 할 SQL은 그 옆에서 잘려 있었다. 실패한 줄에서 사람이 하는 일은 문장과
// 메시지를 나란히 읽는 것이므로, 둘을 위아래로 놓아야 한다.
//
// 오른쪽 칸에는 성공·실패만 남긴다. 표를 훑을 때 필요한 것은 어느 줄이 실패했는가
// 하나이고, 무엇이 잘못됐는지는 그 줄을 찾은 다음의 일이다.
function execRow(s, okMark) {
  // 되돌리기 문장은 적용과 같은 표에 이어 붙는다. 표시가 없으면 "적용된 문장"으로
  // 읽히는데, 실제로는 그 반대를 한 문장이다.
  const cls = [s.error ? 'is-destructive' : '', s.undo ? 'is-undo' : ''].filter(Boolean).join(' ');
  return h('tr', { class: cls },
    h('td.nowrap', {}, String(s.index + 1)),
    h('td', {},
      s.undo ? h('span.exec-undo-tag', {}, icon('refresh'), '되돌리기') : null,
      codeBlock(s.sql, 'sql', { className: 'mig-exec-sql' }),
      s.error ? h('p.mig-exec-error', {}, icon('alert'), h('span', {}, s.error)) : null),
    h('td.nowrap', {}, `${s.durationMs}ms`),
    h('td.nowrap', {}, s.error ? badge('실패', 'danger') : okMark),
  );
}

function executionPanel(m) {
  const log = m.executionLog ?? [];
  if (log.length === 0) return null;
  const failed = log.filter((s) => s.error).length;
  const undone = log.filter((s) => s.undo).length;
  return h('section.card.mig-execlog', {},
    h('h2', {}, '실행 기록',
      h('span.muted', {}, `${m.appliedStatements}문장 적용`),
      failed ? badge(`실패 ${failed}`, 'danger') : null,
      // 되돌린 문장이 있으면 그 사실이 제목에 있어야 한다. 표를 펴 보기 전에
      // "지금 DB에 무엇이 남았는가"를 알 수 있어야 하기 때문이다.
      undone ? badge(`되돌림 ${undone}`, 'warn') : null),
    h('div.table-wrap', {},
      h('table.table.mig-exec', {},
        h('thead', {}, h('tr', {},
          h('th', {}, '#'), h('th', {}, 'SQL'), h('th.nowrap', {}, '소요'), h('th.nowrap', {}, '결과'))),
        h('tbody', {}, log.map((s) => execRow(s, badge('성공', 'success')))))),
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
  // 결정할 수 있는 상태인지도 함께 본다. 실행된 계획에서는 서버가 거부하므로,
  // 눌리는 버튼을 내놓으면 403이 설명을 대신하게 된다.
  const isReviewer = (superadmin || (designated && !isAssignee)) && canReviewNow(m.status);
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
            h('tbody', {}, result.report.steps.map((s) => execRow(s, '✓')))))
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
      api.get(withProject('/connections/')),
      api.get(withProject('/servers/')),
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

// versionSourceLabel은 버전이 어디서 왔는지를 한 낱말로 말한다.
//
// ERD 설계의 기준 고르개도 같은 이름을 쓴다. 두 곳에 따로 적어 두면 한쪽이 값을
// 놓쳐 화면에 'initial_import' 같은 날것이 뜬다 — 실제로 그럴 뻔했다.
export function versionSourceLabel(source) {
  return (VERSION_SOURCE[source] ?? [source])[0];
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
