// 매크로 공유 — 공개 범위와 협업자.
//
// 목록 화면과 편집 화면이 같은 대화상자를 쓴다. 공유는 어디서 열든 같은 결정을
// 내리는 일이고, 두 벌로 두면 한쪽에만 새 선택지가 생기는 식으로 갈라진다.
//
// 전역 커스텀 노드에도 같은 것이 필요해서 대상(target)을 인자로 받는다.
// 매크로 전용 노드는 소속 매크로의 설정을 따르므로 여기에 오지 않는다.
import { api } from '../core/api.js';
import {
  h, mount, icon, select, badge, toast, toastError, openModal, field, spinner,
  relativeTime,
} from '../core/ui.js';

// visibilityBadge는 목록과 헤더에서 한눈에 상태를 보여준다.
//
// 공개 매크로에 "수정 허용"까지 함께 표시하는 이유: 공개했다는 사실보다 남이 고칠
// 수 있다는 사실이 훨씬 중요하고, 그것을 모른 채 공개해 두는 경우가 실제로 생긴다.
export function visibilityBadge(item) {
  if (item.visibility !== 'public') return badge('비공개', 'neutral');
  return item.publicAccess === 'edit'
    ? badge('공개 · 수정 허용', 'warn')
    : badge('공개 · 조회', 'info');
}

// accessNote는 "나는 이 매크로에 대해 무엇인가"를 한 단어로 알려준다.
// 남의 매크로를 열었을 때 왜 저장 버튼이 없는지가 여기서 설명된다.
export function accessLabel(access) {
  return {
    owner: '내가 만든 매크로',
    manage: '협업자',
    edit: '수정 가능',
    view: '조회·실행만',
  }[access] ?? '';
}

// openShareDialog는 공유 설정 대화상자를 연다.
//
// target: { kind: 'macro' | 'node', id, name, item }
// onSaved: 설정이 바뀔 때마다 불린다(목록 새로고침 등).
export function openShareDialog(target, onSaved) {
  const base = target.kind === 'macro'
    ? `/macros/${encodeURIComponent(target.id)}`
    : `/macros/nodes/${encodeURIComponent(target.id)}`;

  let item = { ...target.item };
  const listBox = h('div.share-collab');
  const peopleBox = h('div.share-invite');

  const visSelect = select([
    { value: 'private', label: '비공개 — 나와 협업자만' },
    { value: 'public', label: '공개 — 매크로를 쓰는 모든 사람' },
  ], { value: item.visibility ?? 'private' });

  const pubSelect = select([
    { value: 'view', label: '조회 + 실행만' },
    { value: 'edit', label: '수정까지 허용' },
  ], { value: item.publicAccess ?? 'view' });

  const pubField = field('공개했을 때 허용할 것', pubSelect,
    '수정 허용이어도 삭제와 공유 설정, 자동 실행은 만든 사람과 협업자만 할 수 있습니다.');

  // 비공개일 때 공개 권한 선택을 숨긴다. 지우지는 않는다 —
  // 다시 공개로 돌아왔을 때 직전 선택이 남아 있어야 매번 다시 고르지 않는다.
  const syncVisibility = () => {
    pubField.style.display = visSelect.value === 'public' ? '' : 'none';
  };
  syncVisibility();

  const saveVisibility = async () => {
    try {
      const res = await api.put(`${base}/access`, {
        visibility: visSelect.value,
        publicAccess: pubSelect.value,
      });
      item = res.macro ?? res.nodeDef ?? item;
      toast('공유 설정을 저장했습니다', 'success');
      onSaved?.(item);
    } catch (err) {
      toastError(err);
    }
  };
  visSelect.addEventListener('change', () => { syncVisibility(); saveVisibility(); });
  pubSelect.addEventListener('change', saveVisibility);

  const drawCollaborators = (items) => {
    if (!items.length) {
      mount(listBox, h('p.field-help', {}, '아직 협업자가 없습니다.'));
      return;
    }
    mount(listBox, ...items.map((c) => h('div.share-row', {},
      h('div.share-row-main', {},
        h('span.share-name', {}, c.displayName || c.username),
        c.displayName ? h('span.muted', {}, c.username) : null,
        // 권한이 없거나 비활성인 계정은 협업자로 넣어도 아무것도 못 한다.
        // 목록에서 바로 알려주지 않으면 "왜 안 보인대?"로 며칠이 간다.
        c.disabled ? badge('비활성 계정', 'danger')
          : !c.hasMacroPerm ? badge('매크로 권한 없음', 'warn') : null,
      ),
      h('div.share-row-side', {},
        h('span.muted', {}, `${c.addedByName || '알 수 없음'} · ${relativeTime(c.addedAt)}`),
        h('button.btn.btn-small.btn-danger-ghost', {
          type: 'button',
          onclick: async () => {
            try {
              const res = await api.del(`${base}/collaborators/${encodeURIComponent(c.userId)}`);
              drawCollaborators(res.items);
              onSaved?.(item);
            } catch (err) {
              toastError(err);
            }
          },
        }, icon('trash'), '제외'),
      ),
    )));
  };

  const drawInvite = (people, current) => {
    const taken = new Set(current.map((c) => c.userId));
    const options = people.filter((p) => !taken.has(p.id));
    if (!options.length) {
      mount(peopleBox, h('p.field-help', {},
        '초대할 수 있는 사람이 없습니다. 매크로 권한이 있는 활성 계정만 초대할 수 있습니다.'));
      return;
    }
    const picker = select(
      [{ value: '', label: '사람 선택…' },
        ...options.map((p) => ({
          value: p.id,
          label: p.displayName ? `${p.displayName} (${p.username})` : p.username,
        }))],
      {},
    );
    mount(peopleBox,
      h('div.share-invite-row', {},
        picker,
        h('button.btn.btn-small.btn-primary', {
          type: 'button',
          onclick: async () => {
            if (!picker.value) {
              toast('초대할 사람을 선택하세요', 'error');
              return;
            }
            try {
              const res = await api.post(`${base}/collaborators`, { userId: picker.value });
              drawCollaborators(res.items);
              drawInvite(people, res.items);
              onSaved?.(item);
            } catch (err) {
              toastError(err);
            }
          },
        }, icon('plus'), '협업자 추가'),
      ));
  };

  const close = openModal({
    title: `공유 — ${target.name}`,
    width: 620,
    body: () => [
      field('공개 범위', visSelect,
        '비공개 매크로는 목록에도 나오지 않습니다.'),
      pubField,
      h('div.share-section', {},
        h('h4', {}, '협업자'),
        h('p.field-help', {},
          '협업자는 수정·실행·공유 설정·자동 실행을 할 수 있습니다. 삭제는 만든 사람만 할 수 있습니다.'),
        listBox,
        peopleBox,
      ),
    ],
    footer: (done) => [h('button.btn', { type: 'button', onclick: done }, '닫기')],
  });

  (async () => {
    mount(listBox, spinner('협업자를 불러오는 중…'));
    try {
      const [collab, people] = await Promise.all([
        api.get(`${base}/collaborators`),
        api.get('/macros/people'),
      ]);
      drawCollaborators(collab.items);
      drawInvite(people.items, collab.items);
    } catch (err) {
      mount(listBox, h('p.notice.notice-danger', {}, icon('alert'), err.message ?? '불러오지 못했습니다'));
    }
  })();

  return close;
}
