// 프로젝트 관리.
//
// 프로젝트는 자원의 울타리다. 커넥션과 ERD 초안과 용어 사전이 프로젝트에 속하고,
// 그 아래 달린 것들(마이그레이션·버전·백업·구조 문서)은 커넥션을 따라 함께 간다.
//
// 서버 컴퓨터·매크로·클러스터·사용자는 프로젝트 밖이다. 한 대의 서버가 여러
// 프로젝트의 DB를 담고, 매크로 하나가 두 프로젝트를 오갈 수 있다.
import { api } from '../core/api.js';
import {
  h, mount, icon, input, textarea, field, spinner, emptyState, pageHeader,
  toast, toastError, openModal, confirmDialog, formatDate,
} from '../core/ui.js';
import { peoplePicker } from '../core/searchpick.js';
import { state } from '../core/store.js';
import {
  loadProjects, projects, currentProjectID, setCurrentProject, canManageProjects,
} from '../core/project.js';
import * as router from '../core/router.js';
import { errorPanel } from './users.js';

export async function renderProjects(outlet) {
  mount(outlet, spinner('프로젝트를 불러오는 중…'));

  const reload = async () => {
    try {
      await loadProjects();
      draw();
    } catch (err) {
      mount(outlet, errorPanel(err));
    }
  };

  const draw = () => {
    const list = projects();
    const canManage = canManageProjects();
    mount(outlet,
      pageHeader('프로젝트', '자원은 모두 프로젝트 안에 있습니다. DB 커넥션·ERD·마이그레이션·버전·용어 사전이 여기에 딸립니다.',
        canManage
          ? h('button.btn.btn-primary', {
            type: 'button', onclick: () => openProjectDialog(null, reload),
          }, icon('plus'), '프로젝트 만들기')
          : null),
      list.length === 0 ? firstRun(canManage, reload) : h('div.project-grid', {},
        list.map((p) => projectCard(p, canManage, reload))),
    );
  };

  await reload();
}

// firstRun은 프로젝트가 하나도 없을 때의 화면이다.
//
// 빈 목록만 보여주면 "여기서 무엇을 해야 하는가"에 답하지 않는다. 자원을 만들려면
// 먼저 프로젝트가 있어야 한다는 것이 이 앱의 새 규칙이고, 그 사실을 처음 만나는
// 자리가 여기다.
function firstRun(canManage, reload) {
  if (!canManage) {
    return emptyState('참여 중인 프로젝트가 없습니다. 관리자에게 프로젝트 참여를 요청하세요.');
  }
  return h('div.card.empty', {},
    icon('box', 28),
    h('h2', {}, '아직 프로젝트가 없습니다'),
    h('p.muted', {},
      'DB 커넥션도 ERD도 프로젝트 안에서 만듭니다. 팀이나 제품 단위로 하나씩 두면 '
      + '목록이 남의 것으로 채워지지 않고, 참여자만 그 안을 볼 수 있습니다.'),
    h('button.btn.btn-primary', {
      type: 'button', onclick: () => openProjectDialog(null, reload),
    }, icon('plus'), '첫 프로젝트 만들기'),
  );
}

function projectCard(p, canManage, reload) {
  const isCurrent = p.id === currentProjectID();
  return h(`div.card.project-card${isCurrent ? '.is-current' : ''}`, {},
    h('div.project-head', {},
      h('div', {},
        h('h3', {}, p.name),
        p.note ? h('p.muted.small', {}, p.note) : null),
      isCurrent
        ? h('span.badge.badge-ok', {}, '보는 중')
        : h('button.btn.btn-small', {
          type: 'button',
          onclick: () => {
            // 프로젝트를 바꾸면 모든 화면의 내용이 바뀐다. 목록에 남아 있으면
            // 방금 무엇이 바뀌었는지 보이지 않으므로 커넥션 화면으로 옮긴다.
            setCurrentProject(p.id);
            toast(`${p.name} 프로젝트로 옮겼습니다`, 'success');
            router.navigate('/connections');
          },
        }, '이 프로젝트 보기'),
    ),
    h('div.project-stats', {},
      stat('DB 커넥션', p.connections),
      stat('ERD 초안', p.documents),
      stat('참여자', p.members)),
    h('div.project-foot', {},
      h('span.muted.small', {}, `${p.createdName || '알 수 없음'} · ${formatDate(p.createdAt)}`),
      canManage
        ? h('div.project-actions', {},
          h('button.btn.btn-small', {
            type: 'button', onclick: () => openMembersDialog(p, reload),
          }, icon('users', 13), '참여자'),
          h('button.btn.btn-small', {
            type: 'button', onclick: () => openProjectDialog(p, reload),
          }, icon('edit', 13), '고치기'),
          h('button.btn.btn-small.danger', {
            type: 'button', onclick: () => removeProject(p, reload),
          }, icon('trash', 13), '지우기'))
        : null),
  );
}

function stat(label, value) {
  return h('div.project-stat', {},
    h('strong', {}, String(value ?? 0)),
    h('span.muted.small', {}, label));
}

function openProjectDialog(existing, reload) {
  const nameInput = input({
    value: existing?.name ?? '', placeholder: '예: 결제, 물류, 사내 포털', autofocus: true,
  });
  const noteInput = textarea({ rows: 3, value: existing?.note ?? '' });

  openModal({
    title: existing ? '프로젝트 고치기' : '프로젝트 만들기',
    width: 520,
    body: () => [
      field('이름', nameInput, '무엇을 담는 울타리인지 한눈에 알 수 있는 이름이 좋습니다.'),
      field('설명', noteInput, '이 프로젝트에 무엇이 들어가는지 (선택)'),
    ],
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: async (e) => {
          e.currentTarget.disabled = true;
          const body = { name: nameInput.value, note: noteInput.value };
          try {
            if (existing) await api.put(`/projects/${existing.id}`, body);
            else await api.post('/projects/', body);
            close();
            toast(existing ? '프로젝트를 고쳤습니다' : '프로젝트를 만들었습니다', 'success');
            reload();
          } catch (err) {
            toastError(err);
            e.currentTarget.disabled = false;
          }
        },
      }, icon('check'), '저장'),
    ],
  });
}

// openMembersDialog는 참여자 명단을 통째로 고친다.
//
// 참여는 등급보다 앞선 관문이다. 여기서 뺀 사람은 그 프로젝트의 DB를 등급이 무엇으로
// 적혀 있든 볼 수 없게 된다 — 그래서 그 사실을 창 안에 적어 둔다.
async function openMembersDialog(p, reload) {
  let detail;
  try {
    detail = await api.get(`/projects/${p.id}`);
  } catch (err) {
    toastError(err);
    return;
  }
  if (detail.canManageMembers !== true) {
    // 참여자를 고치려면 사용자 목록을 볼 수 있어야 한다(슈퍼 어드민).
    openModal({
      title: `${p.name} 참여자`,
      width: 460,
      body: () => [
        h('p.modal-message', {}, '참여자 명단은 슈퍼 어드민이 고칩니다.'),
        h('ul.plain-list', {}, (detail.members ?? []).map((m) => h('li', {},
          h('strong', {}, m.name || m.login), ' ', h('span.muted.small', {}, m.login)))),
      ],
      footer: (close) => [h('button.btn', { type: 'button', onclick: close }, '닫기')],
    });
    return;
  }

  let users = [];
  try {
    ({ users } = await api.get('/users/'));
  } catch (err) {
    toastError(err);
    return;
  }

  const picker = peoplePicker({
    items: (users ?? []).map((u) => ({
      id: u.id,
      label: u.displayName || u.username,
      sub: u.displayName ? u.username : '',
    })),
    selected: (detail.members ?? []).map((m) => m.userId),
    placeholder: '이름 또는 아이디로 검색',
    emptyText: '더 넣을 사람이 없습니다',
  });

  openModal({
    title: `${p.name} 참여자`,
    width: 560,
    body: () => [
      h('p.modal-message', {},
        '참여자만 이 프로젝트의 DB와 설계를 볼 수 있습니다. '
        + '여기서 뺀 사람은 DB별 등급이 무엇으로 적혀 있든 아무것도 보지 못합니다.'),
      picker.node,
      h('p.field-help', {},
        '슈퍼 어드민은 참여와 무관하게 모든 프로젝트를 봅니다.'),
    ],
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: async (e) => {
          e.currentTarget.disabled = true;
          try {
            await api.put(`/projects/${p.id}/members`, { members: picker.value });
            close();
            toast('참여자를 저장했습니다', 'success');
            reload();
          } catch (err) {
            toastError(err);
            e.currentTarget.disabled = false;
          }
        },
      }, icon('check'), '저장'),
    ],
  });
}

// removeProject는 빈 프로젝트만 지운다.
//
// 안에 든 것을 함께 지우지 않는 이유: 단추 하나로 DB 열 개와 그 아래 ERD·
// 마이그레이션이 한꺼번에 사라진다면, 그것은 무엇을 지우는지 말할 수 없는 단추다.
async function removeProject(p, reload) {
  const used = (p.connections ?? 0) + (p.documents ?? 0);
  const ok = await confirmDialog({
    title: '프로젝트 지우기',
    message: used > 0
      ? `"${p.name}" 안에 DB ${p.connections}개와 ERD ${p.documents}개가 있습니다. `
        + '먼저 그것들을 지우거나 옮긴 뒤에 프로젝트를 지울 수 있습니다.'
      : `"${p.name}" 을 지웁니다. 안에 든 것이 없으므로 사라지는 것은 이름과 참여자 명단뿐입니다.`,
    confirmLabel: '지우기',
    danger: true,
  });
  if (!ok) return;
  try {
    await api.del(`/projects/${p.id}`);
    toast('프로젝트를 지웠습니다', 'success');
    reload();
  } catch (err) { toastError(err); }
}

// projectGuard는 프로젝트가 없을 때 자원 화면 대신 보여줄 안내다.
//
// 화면마다 "커넥션이 없습니다"라고만 말하면, 없는 이유가 프로젝트가 없어서라는
// 사실이 어디에도 드러나지 않는다. 자원을 만들려면 프로젝트가 먼저다.
export function projectGuard(what) {
  const canManage = state.permissions?.manageConnections === true;
  return h('div.card.empty', {},
    icon('box', 28),
    h('h2', {}, '먼저 프로젝트가 필요합니다'),
    h('p.muted', {}, `${what}은(는) 프로젝트 안에서 만듭니다. `
      + (canManage
        ? '프로젝트를 하나 만들고 그 안에서 시작하세요.'
        : '참여 중인 프로젝트가 없습니다. 관리자에게 참여를 요청하세요.')),
    h('a.btn.btn-primary', { href: '/projects' }, icon('box'), '프로젝트로 가기'),
  );
}
