// 사용자 관리 화면 (슈퍼 어드민 전용).
import { api } from '../core/api.js';
import { state } from '../core/store.js';
import {
  h, mount, icon, field, input, select, spinner, emptyState, pageHeader,
  openModal, confirmDialog, toast, toastError, badge, roleBadge, formatDate,
  relativeTime, copyToClipboard,
} from '../core/ui.js';
import { navigate } from '../core/router.js';
import { avatarNode } from '../core/avatars.js';

export async function renderUsers(outlet) {
  mount(outlet, spinner());
  let users;
  try {
    ({ users } = await api.get('/users/'));
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  const reload = () => renderUsers(outlet);

  mount(outlet,
    pageHeader('사용자 관리', `${users.length}명의 계정`, [
      h('button.btn.btn-primary', { type: 'button', onclick: () => openUserForm(null, reload) },
        icon('plus'), '사용자 추가'),
    ]),
    users.length === 0
      ? emptyState('등록된 사용자가 없습니다')
      : h('div.card', {}, userTable(users, reload)),
  );
}

function userTable(users, reload) {
  return h('table.table', {},
    h('thead', {}, h('tr', {},
      h('th', {}, '아이디'),
      h('th', {}, '이름'),
      h('th', {}, '역할'),
      h('th', {}, '상태'),
      h('th', {}, '2단계 인증'),
      h('th', {}, '최근 로그인 · IP'),
      h('th', {}, '생성'),
      h('th.col-actions', {}, ''),
    )),
    h('tbody', {}, users.map((u) => userRow(u, reload))),
  );
}

function userRow(u, reload) {
  const isSelf = u.id === state.user?.id;
  return h('tr', {},
    h('td', {},
      h('div.cell-main', {},
        avatarNode(u, { size: 24 }),
        h('a', { href: `/users/${u.id}/access` }, u.username),
        isSelf ? badge('나', 'accent') : null,
        u.mustChangePassword ? badge('비밀번호 변경 대기', 'warn') : null,
      ),
      u.email ? h('div.cell-sub', {}, u.email) : null,
    ),
    h('td', {}, u.displayName || '—'),
    h('td', {}, roleBadge(u.role)),
    h('td', {}, u.status === 'active' ? badge('활성', 'success') : badge('비활성', 'neutral')),
    // 의무화되어 있으면 미설정을 경고로 보여준다. 자율일 때는 사실만 적는다 —
    // 켜지 않은 것이 아직 잘못은 아니기 때문이다.
    h('td', {}, u.totpEnabled
      ? badge('사용 중', 'success')
      : badge('미설정', state.permissions?.totpRequired ? 'warn' : 'neutral')),
    h('td', {}, lastLoginCell(u)),
    h('td', {}, formatDate(u.createdAt)),
    h('td.col-actions', {},
      h('div.row-actions', {},
        h('button.icon-btn', {
          type: 'button', title: '권한 설정',
          onclick: () => navigate(`/users/${u.id}/access`),
        }, icon('shield')),
        h('button.icon-btn', {
          type: 'button', title: '수정',
          onclick: () => openUserForm(u, reload),
        }, icon('edit')),
        h('button.icon-btn', {
          type: 'button', title: '비밀번호 재설정',
          onclick: () => resetPassword(u, reload),
        }, icon('key')),
        // 인증 앱을 잃고 복구 코드도 없는 사람을 위한 경로다.
        // 설정하지 않은 계정에는 누를 것이 없으므로 비활성으로 둔다.
        h('button.icon-btn', {
          type: 'button', disabled: !u.totpEnabled,
          title: u.totpEnabled ? '2단계 인증 초기화' : '2단계 인증을 설정하지 않은 계정입니다',
          onclick: () => resetTOTP(u, reload),
        }, icon('shield')),
        h('button.icon-btn.danger', {
          type: 'button', title: '삭제', disabled: isSelf,
          onclick: () => deleteUser(u, reload),
        }, icon('trash')),
      ),
    ),
  );
}

// lastLoginCell은 마지막 로그인 시각과 그때의 접속 IP를 함께 보여준다.
//
// 시각만으로는 "그 접속이 정상인가"를 판단할 수 없다. 평소와 다른 IP에서 들어온
// 로그인을 알아채는 것이 이 열의 목적이므로 둘을 같은 칸에 둔다.
function lastLoginCell(u) {
  if (!u.lastLoginAt) return h('span.muted', {}, '기록 없음');
  return h('div', {},
    h('div', { title: formatDate(u.lastLoginAt) }, relativeTime(u.lastLoginAt)),
    u.lastLoginIp
      ? h('code.cell-ip', { title: `접속 IP ${u.lastLoginIp}` }, u.lastLoginIp)
      // 이 컬럼이 생기기 전에 로그인한 기록은 IP를 알 수 없다.
      // 빈칸으로 두면 "IP가 없는 접속"으로 오해하므로 이유를 적는다.
      : h('span.cell-sub', {}, 'IP 기록 없음'),
  );
}

function openUserForm(existing, reload) {
  const isEdit = Boolean(existing);
  const roles = state.meta?.roles ?? [];

  const username = input({
    value: existing?.username ?? '',
    placeholder: 'developer1',
    disabled: isEdit, // 아이디는 감사 추적성을 위해 변경하지 않는다
    autocomplete: 'off',
  });
  const displayName = input({ value: existing?.displayName ?? '', placeholder: '홍길동' });
  const email = input({ type: 'email', value: existing?.email ?? '', placeholder: 'dev@example.com' });
  const role = select(roles.map((r) => ({ value: r.value, label: r.label })), {
    value: existing?.role ?? 'member',
  });
  const status = select(
    [{ value: 'active', label: '활성' }, { value: 'disabled', label: '비활성' }],
    { value: existing?.status ?? 'active' },
  );
  const password = input({
    type: 'password', autocomplete: 'new-password',
    placeholder: '비워두면 임시 비밀번호를 자동 생성합니다',
  });

  const roleHelp = h('span.field-help', {});
  const syncRoleHelp = () => {
    roleHelp.textContent = roles.find((r) => r.value === role.value)?.help ?? '';
  };
  role.addEventListener('change', syncRoleHelp);
  syncRoleHelp();

  const submit = async (close) => {
    try {
      if (isEdit) {
        await api.patch(`/users/${existing.id}`, {
          email: email.value.trim(),
          displayName: displayName.value.trim(),
          role: role.value,
          status: status.value,
        });
        toast('사용자 정보를 수정했습니다', 'success');
      } else {
        const res = await api.post('/users/', {
          username: username.value.trim(),
          email: email.value.trim(),
          displayName: displayName.value.trim(),
          role: role.value,
          password: password.value || undefined,
        });
        if (res.temporaryPassword) {
          showTemporaryPassword(res.user.username, res.temporaryPassword);
        } else {
          toast('사용자를 생성했습니다', 'success');
        }
      }
      close();
      reload();
    } catch (err) {
      toastError(err);
    }
  };

  openModal({
    title: isEdit ? `사용자 수정 — ${existing.username}` : '사용자 추가',
    body: () => [
      field('아이디', username, isEdit ? '아이디는 변경할 수 없습니다' : '영문/숫자/._- 조합 3~32자'),
      field('이름', displayName),
      field('이메일', email),
      h('label.field', {}, h('span.field-label', {}, '역할'), role, roleHelp),
      isEdit ? field('상태', status, '비활성화하면 즉시 모든 세션이 종료됩니다') : null,
      isEdit ? null : field('초기 비밀번호', password, '첫 로그인 시 변경이 강제됩니다'),
      // 권한은 여기서 다루지 않는다. 전역 권한만 이 창에 있으면 권한을 바꿀 때마다
      // 두 화면을 오가야 하고, 어느 쪽이 최신인지 알 수 없게 된다.
      // 역할(role)은 계정의 신분이라 남기고, 부여하는 권한은 모두 권한 설정 화면에 모은다.
      h('p.field-help', {},
        '전역 권한(매크로·셸·외부 API)과 DB별 접근 권한은 ',
        h('strong', {}, '권한 설정'),
        ' 화면에서 함께 설정합니다.'),
    ],
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-primary', { type: 'button', onclick: () => submit(close) },
        isEdit ? '저장' : '생성'),
    ],
  });
}

// showTemporaryPassword는 생성된 비밀번호를 한 번만 보여준다.
function showTemporaryPassword(username, password) {
  openModal({
    title: '임시 비밀번호',
    width: 520,
    body: () => [
      h('p.modal-message', {}, `${username} 계정의 임시 비밀번호입니다. 이 창을 닫으면 다시 볼 수 없습니다.`),
      h('div.secret-box', {},
        h('code', {}, password),
        h('button.btn.btn-small', { type: 'button', onclick: () => copyToClipboard(password) },
          icon('copy'), '복사'),
      ),
      h('p.field-help', {}, '해당 사용자는 첫 로그인 시 비밀번호 변경이 요구됩니다.'),
    ],
    footer: (close) => [h('button.btn.btn-primary', { type: 'button', onclick: close }, '확인')],
  });
}

async function resetPassword(u, reload) {
  const ok = await confirmDialog({
    title: '비밀번호 재설정',
    message: `${u.username} 계정의 비밀번호를 임시 비밀번호로 재설정합니다. 해당 사용자의 모든 세션이 종료됩니다.`,
    confirmLabel: '재설정',
  });
  if (!ok) return;
  try {
    const res = await api.post(`/users/${u.id}/password`, {});
    if (res.temporaryPassword) showTemporaryPassword(u.username, res.temporaryPassword);
    reload();
  } catch (err) {
    toastError(err);
  }
}

// resetTOTP는 대상의 2단계 인증을 지운다.
//
// 확인 문구에 "그 사람의 세션이 끊긴다"와 "비밀번호만으로 로그인하게 된다"를
// 함께 적는다. 초기화는 계정을 되찾아 주는 동작이면서 동시에 방어를 한 겹 내리는
// 동작이고, 누르는 사람이 그 둘을 모두 알아야 한다.
async function resetTOTP(u, reload) {
  const ok = await confirmDialog({
    title: '2단계 인증 초기화',
    message: `${u.username} 계정의 2단계 인증과 복구 코드를 지웁니다. ` +
      '해당 사용자의 모든 세션이 종료되고, 다시 설정하기 전까지는 비밀번호만으로 로그인합니다.',
    confirmLabel: '초기화',
    danger: true,
    details: h('p.field-help', {},
      '인증 앱을 잃어버렸다는 사실을 본인에게 직접 확인한 뒤에 사용하세요. ' +
      '이 작업은 감사 로그에 남습니다.'),
  });
  if (!ok) return;
  try {
    await api.post(`/users/${u.id}/totp/reset`);
    toast('2단계 인증을 초기화했습니다', 'success');
    reload();
  } catch (err) {
    toastError(err);
  }
}

async function deleteUser(u, reload) {
  const ok = await confirmDialog({
    title: '사용자 삭제',
    message: `${u.username} 계정을 삭제합니다. 이 작업은 되돌릴 수 없습니다.`,
    confirmLabel: '삭제',
    danger: true,
    requireText: u.username,
  });
  if (!ok) return;
  try {
    await api.del(`/users/${u.id}`);
    toast('사용자를 삭제했습니다', 'success');
    reload();
  } catch (err) {
    toastError(err);
  }
}

// errorPanel은 실패를 화면에 남긴다.
//
// actions에는 여기서 빠져나갈 수단을 넣는다. 오류만 남기고 원래 화면을 덮어 버리면
// 사용자는 무엇을 되돌려야 할지 모른 채 새로고침밖에 할 수 없다.
export function errorPanel(err, actions = null) {
  return h('div.card.error-panel', {},
    icon('alert', 24),
    h('div', {},
      h('strong', {}, err?.message ?? '오류가 발생했습니다'),
      err?.detail ? h('p', {}, err.detail) : null,
      actions ? h('div.error-actions', {}, actions) : null,
    ),
  );
}
