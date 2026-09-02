// ERD 문서 목록: 초안 만들기 / 열기 / 상태 전환 / 삭제.
import { api } from '../core/api.js';
import { withProject, currentProjectID, hasProjects } from '../core/project.js';
import { projectGuard } from './projects.js';
import { state, kindLabel } from '../core/store.js';
import {
  h, mount, icon, select, input, textarea, spinner, emptyState,
  pageHeader, badge, envBadge, toast, toastError, relativeTime, openModal, confirmDialog,
} from '../core/ui.js';
import { navigate } from '../core/router.js';
import { errorPanel } from './users.js';
import { serverDbPicker } from '../core/connpick.js';

// 문서 상태 표시. 리뷰/승인 워크플로(P7)가 이 값을 확장한다.
export const STATUS_LABELS = {
  draft: ['초안', 'neutral'],
  in_review: ['리뷰 중', 'info'],
  applied: ['적용됨', 'accent'],
  archived: ['보관', 'neutral'],
};

export function statusBadge(status) {
  const [label, kind] = STATUS_LABELS[status] ?? [status, 'neutral'];
  return badge(label, kind);
}

export async function renderERDList(outlet) {
  if (!hasProjects()) {
    mount(outlet, projectGuard('ERD 초안'));
    return;
  }
  mount(outlet, spinner('ERD 문서를 불러오는 중…'));

  let docs;
  let conns;
  try {
    [docs, conns] = await Promise.all([
      api.get(withProject('/erd/documents/')),
      api.get(withProject('/connections/')),
    ]);
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  // ERD 설계는 관계형 스키마가 있는 DB만 가능하다. MongoDB/Redis는 스키마 개념이
  // 없어 마이그레이션 대상이 아니므로 목록에서 제외한다(서버도 거부한다).
  const targets = conns.items.filter((i) => {
    if (!i.accessible) return false;
    if (i.level !== 'erd' && i.level !== 'migrate') return false;
    const info = state.meta?.dbKinds?.find((k) => k.kind === i.connection.kind);
    return info?.capabilities?.migrate;
  });

  const reload = () => renderERDList(outlet);

  mount(outlet,
    pageHeader('ERD 설계', '스키마 초안을 함께 그리고 리뷰합니다', [
      // 대상 DB가 없어도 만들 수 있으므로 버튼을 잠그지 않는다.
      // 설계는 DB를 만들기 전에 시작되는 일이 더 많다.
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: () => openCreateDialog(targets, reload),
      }, icon('plus'), '새 초안'),
    ]),
    targets.length === 0
      ? h('p.notice.notice-info', {}, icon('alert'),
        'ERD를 설계할 수 있는 커넥션이 없습니다. 대상 DB 없이 새 설계도를 그릴 수는 ' +
        '있으며, 기존 DB에서 가져오려면 ERD 등급 이상의 권한이 있는 관계형 DB 커넥션이 필요합니다.')
      : null,
    docs.items.length === 0
      ? emptyState('아직 ERD 초안이 없습니다. 빈 캔버스에서 시작하거나 기존 DB 스키마를 가져올 수 있습니다.',
        h('button.btn.btn-primary', {
          type: 'button', onclick: () => openCreateDialog(targets, reload),
        }, '새 초안 만들기'))
      : h('div.erd-grid', {}, docs.items.map((item) => docCard(item, reload))),
  );
}

function docCard(item, reload) {
  const doc = item.document;
  const conn = item.connection;
  return h('article.card.erd-card', {},
    h('div.erd-card-head', {},
      h('a.erd-card-title', { href: `/erd/${encodeURIComponent(doc.id)}` }, doc.name),
      statusBadge(doc.status),
    ),
    h('div.erd-card-meta', {},
      conn
        ? h('span', {}, icon('database', 14), ` ${conn.name}`)
        // 대상이 없다는 사실은 눈에 보여야 한다. 이 초안은 마이그레이션을 만들 수
        // 없고 SQL 내보내기로만 결과를 받는데, 카드에서 구분되지 않으면
        // 열어 본 뒤에야 그것을 알게 된다.
        : badge('대상 DB 없음', 'neutral'),
      conn ? envBadge(conn.environment) : null,
      h('span.muted', {}, kindLabel(doc.dialect)),
    ),
    doc.note ? h('p.erd-card-note', {}, doc.note) : null,
    h('dl.erd-card-stats', {},
      h('div', {}, h('dt', {}, '테이블'), h('dd', {}, String(doc.tableCount))),
      h('div', {}, h('dt', {}, '편집 수'), h('dd', {}, String(doc.seq))),
      h('div', {}, h('dt', {}, '수정'), h('dd', {}, relativeTime(doc.updatedAt))),
    ),
    item.activeEditors > 0
      ? h('p.erd-card-live', {}, h('span.live-dot'), `${item.activeEditors}명 편집 중`)
      : null,
    h('div.erd-card-actions', {},
      h('a.btn.btn-small', { href: `/erd/${encodeURIComponent(doc.id)}` },
        icon('edit'), '열기'),
      // 복제는 **볼 수 있으면** 할 수 있다. 담기는 내용은 이미 보고 있는 것이고,
      // 만들어지는 것은 내 새 초안이다 — 원본은 건드리지 않으므로 설정·삭제와 같은
      // 문턱을 세울 이유가 없다.
      h('button.btn.btn-small', {
        type: 'button', onclick: () => openDuplicateDialog(doc, reload),
      }, icon('copy'), '복제'),
      // 설정과 삭제는 되돌릴 수 없거나 남에게 영향을 준다. 대상 DB 없는 초안에서는
      // 만든 사람과 어드민만 할 수 있으므로, 할 수 없는 사람에게는 아예 보이지 않는다.
      !item.canManage ? null : h('button.btn.btn-small', {
        type: 'button', onclick: () => openRenameDialog(doc, reload),
      }, icon('settings'), '설정'),
      !item.canManage ? null : h('button.btn.btn-small.btn-danger-ghost', {
        type: 'button',
        onclick: async () => {
          const ok = await confirmDialog({
            title: '초안 삭제',
            message: `"${doc.name}" 을 삭제합니다. 편집 이력과 대화도 함께 사라집니다.`,
            confirmLabel: '삭제',
            danger: true,
          });
          if (!ok) return;
          try {
            await api.del(`/erd/documents/${encodeURIComponent(doc.id)}`);
            toast('초안을 삭제했습니다', 'success');
            reload();
          } catch (err) {
            toastError(err);
          }
        },
      }, icon('trash'), '삭제'),
    ),
  );
}

// openDuplicateDialog는 초안을 베낀다.
//
// 창을 여는 이유: 사본에서 가장 먼저 정해야 하는 것이 이름이다. 자동 이름으로
// 바로 만들면 "주문 개편 사본"이 목록에 남고, 그것을 고치러 설정을 다시 열어야
// 한다. (테이블 복제도 같은 이유로 창을 띄운다 — erdeditor.openDuplicateDialog)
function openDuplicateDialog(doc, reload) {
  const nameInput = input({ value: `${doc.name} 사본`, autofocus: true });
  let busy = false;

  openModal({
    title: '초안 복제',
    width: 480,
    body: () => [
      h('label.field', {}, h('span.field-label', {}, '새 이름'), nameInput),
      h('p.field-help', {},
        '테이블·컬럼·관계는 물론 배치·색·아이콘·논리명·도메인·메모까지 그대로 베낍니다. '
        + '원본은 바뀌지 않습니다.'),
      // 무엇이 안 따라오는지도 적는다. "복제했는데 대화가 없다"를 나중에 발견하면
      // 그것은 고장으로 읽힌다.
      h('p.field-help', {}, '편집 이력과 대화는 따라오지 않고, 상태는 초안으로 시작합니다.'),
    ],
    footer: (closeFn) => [
      h('button.btn', { type: 'button', onclick: closeFn }, '취소'),
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: async (e) => {
          const name = nameInput.value.trim();
          if (!name) {
            toast('이름을 적으세요', 'error');
            return;
          }
          // 두 번 눌러 두 개가 만들어지는 것을 막는다. 큰 초안은 응답이 한 박자 늦다.
          if (busy) return;
          busy = true;
          e.target.disabled = true;
          try {
            const res = await api.post(
              `/erd/documents/${encodeURIComponent(doc.id)}/duplicate`, { name });
            closeFn();
            toast('복제했습니다', 'success');
            // 사본으로 바로 들어간다. 복제하는 사람은 그 사본을 고치려는 것이고,
            // 목록으로 돌려보내면 방금 만든 것을 다시 찾게 한다.
            navigate(`/erd/${encodeURIComponent(res.document.id)}`);
          } catch (err) {
            busy = false;
            e.target.disabled = false;
            toastError(err);
          }
        },
      }, icon('copy'), '복제'),
    ],
  });
}

// openCreateDialog는 새 초안의 출발점을 고르게 한다.
//
// 출발점이 셋이다: 빈 캔버스 / 기존 DB 스키마 가져오기 / 대상 DB 없는 독립 설계.
// 앞의 둘은 커넥션이 필요하고 마지막은 필요 없으므로, 커넥션 선택을 라디오 아래로
// 접어 두어 "무엇을 먼저 정해야 하는가"가 한 번에 보이게 한다.
function openCreateDialog(targets, reload) {
  const nameInput = input({ placeholder: '예: 주문 도메인 개편', autofocus: true });
  const hasTargets = targets.length > 0;

  const connSelect = hasTargets
    ? serverDbPicker({
      usable: targets,
      currentId: targets[0].connection.id,
      onPick: () => {},
      inline: false,
      help: 'DB 문법과 편집 권한이 이 커넥션을 따릅니다',
    })
    : null;

  // 독립 초안은 대상이 없으므로 어떤 DB 문법으로 그릴지 직접 골라야 한다.
  // 타입 이름과 생성되는 DDL이 여기서 정해진다.
  const dialects = (state.meta?.dbKinds ?? []).filter((k) => k.capabilities?.migrate);
  const dialectSelect = select(
    dialects.map((k) => ({ value: k.kind, label: k.label })),
    { value: dialects[0]?.kind ?? 'postgres' },
  );

  const noteInput = textarea({ placeholder: '이 초안의 목적 (선택)' });

  const modes = [
    hasTargets ? { value: 'import', label: '기존 DB의 현재 스키마를 가져와 시작' } : null,
    hasTargets ? { value: 'blank', label: '대상 DB를 정해 두고 빈 캔버스에서 시작' } : null,
    { value: 'standalone', label: 'DB 연결 없이 새 설계도 그리기' },
  ].filter(Boolean);
  let mode = modes[0].value;

  const connField = h('div', {}, ...(connSelect?.nodes ?? []));
  const dialectField = h('label.field', {},
    h('span.field-label', {}, 'DB 문법'), dialectSelect,
    h('span.field-help', {}, '컬럼 타입과 내보낼 SQL이 이 종류를 따릅니다. 나중에 바꿀 수 없습니다'));
  const hint = h('p.notice.notice-info');

  const syncMode = () => {
    connField.hidden = mode === 'standalone';
    dialectField.hidden = mode !== 'standalone';
    mount(hint, icon('alert'), mode === 'import'
      ? '스키마를 가져오면 그 시점의 스냅샷이 기준으로 기록됩니다. ' +
        '나중에 마이그레이션할 때 "그 사이 DB가 바뀌었는지"를 이 기준으로 판단합니다.'
      : mode === 'blank'
        ? '빈 캔버스에서 시작하고, 대상 DB와의 차이는 언제든 비교할 수 있습니다.'
        : '대상 DB가 없는 초안입니다. 로그인한 사람이면 누구나 함께 편집할 수 있고, ' +
          '삭제와 설정 변경은 만든 사람과 어드민만 할 수 있습니다. ' +
          '마이그레이션 대신 SQL 내보내기로 결과를 받습니다.');
  };

  const modeField = h('div.field', {},
    h('span.field-label', {}, '시작 방법'),
    h('div.radio-list', {}, modes.map((m) => h('label.checkbox', {},
      h('input', {
        type: 'radio', name: 'erd-start', value: m.value, checked: m.value === mode,
        onchange: () => { mode = m.value; syncMode(); },
      }),
      h('span', {}, m.label)))));

  openModal({
    title: '새 ERD 초안',
    width: 560,
    body: () => {
      // hidden 반영은 요소가 만들어진 뒤여야 한다.
      queueMicrotask(syncMode);
      return [
        h('label.field', {}, h('span.field-label', {}, '문서 이름'), nameInput),
        modeField,
        connField,
        dialectField,
        h('label.field', {}, h('span.field-label', {}, '메모'), noteInput),
        hint,
      ];
    },
    footer: (closeFn) => [
      h('button.btn', { type: 'button', onclick: closeFn }, '취소'),
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: async (e) => {
          const btn = e.currentTarget;
          btn.disabled = true;
          try {
            const res = await api.post('/erd/documents/', {
              name: nameInput.value,
              // 대상 DB가 없는 독립 초안에는 프로젝트가 유일한 울타리다.
              // 커넥션이 붙은 초안은 서버가 커넥션 쪽 프로젝트로 맞춘다.
              projectId: currentProjectID(),
              connectionId: mode === 'standalone' ? '' : connSelect.value,
              dialect: mode === 'standalone' ? dialectSelect.value : '',
              note: noteInput.value,
              fromConnection: mode === 'import',
            });
            closeFn();
            toast('초안을 만들었습니다', 'success');
            navigate(`/erd/${encodeURIComponent(res.document.id)}`);
          } catch (err) {
            btn.disabled = false;
            toastError(err);
          }
        },
      }, '만들기'),
    ],
  });
  void reload;
}

function openRenameDialog(doc, reload) {
  const nameInput = input({ value: doc.name });
  const noteInput = textarea({ value: doc.note ?? '' });
  const statusSelect = select(
    Object.entries(STATUS_LABELS).map(([value, [label]]) => ({ value, label })),
    { value: doc.status },
  );

  openModal({
    title: '초안 설정',
    width: 520,
    body: () => [
      h('label.field', {}, h('span.field-label', {}, '이름'), nameInput),
      h('label.field', {}, h('span.field-label', {}, '상태'), statusSelect,
        h('span.field-help', {}, '리뷰 중으로 바꾸면 검토를 요청한 것으로 표시됩니다')),
      h('label.field', {}, h('span.field-label', {}, '메모'), noteInput),
    ],
    footer: (closeFn) => [
      h('button.btn', { type: 'button', onclick: closeFn }, '취소'),
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: async () => {
          try {
            await api.patch(`/erd/documents/${encodeURIComponent(doc.id)}`, {
              name: nameInput.value,
              status: statusSelect.value,
              note: noteInput.value,
            });
            closeFn();
            toast('저장했습니다', 'success');
            reload();
          } catch (err) {
            toastError(err);
          }
        },
      }, '저장'),
    ],
  });
}
