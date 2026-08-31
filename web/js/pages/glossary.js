// 용어 사전.
//
// 논리명과 물리명을 팀이 같은 규칙으로 쓰기 위한 표다. "회원 번호"를 누구는
// member_no 로, 누구는 mbr_num 으로 적으면 같은 것이 두 이름을 갖고, 그 뒤로는
// 조인 한 번에 두 이름을 다 외워야 한다.
//
// 앱 전체에 하나만 둔다(ERD 문서마다 두지 않는다). 문서마다 사전이 따로 있으면
// 문서마다 다른 약속이 생기는데, 그러면 사전이 있는 것이 없는 것보다 나쁘다.
import { api } from '../core/api.js';
import {
  h, mount, icon, input, textarea, field, spinner, badge, emptyState, pageHeader,
  toast, toastError, openModal, confirmDialog, copyToClipboard, formatDate,
} from '../core/ui.js';
import { errorPanel } from './users.js';

export async function renderGlossary(outlet, params, query) {
  mount(outlet, spinner('용어 사전을 불러오는 중…'));

  const q = query.get('q') ?? '';
  const box = h('div');
  const search = input({
    type: 'search', value: q, placeholder: '용어·물리명·설명에서 찾기',
    autocomplete: 'off',
  });

  let canManage = false;
  const load = async (term = search.value) => {
    mount(box, spinner('불러오는 중…'));
    try {
      const res = await api.get(`/glossary/?q=${encodeURIComponent(term.trim())}`);
      canManage = res.canManage === true;
      mount(box, listView(res.terms ?? [], canManage, load, term.trim()));
      head();
    } catch (err) {
      mount(box, errorPanel(err));
    }
  };

  // 검색은 타자를 멈추면 곧 돈다. 누를 단추를 두면 그 한 번이 매번 늘어나는데,
  // 사전은 "잠깐 찾아보는" 곳이라 그 한 번이 무겁다.
  let timer = null;
  search.addEventListener('input', () => {
    if (timer) clearTimeout(timer);
    timer = setTimeout(() => load(), 250);
  });

  const actions = h('div.filter-actions');
  const head = () => {
    mount(actions,
      canManage
        ? h('button.btn', {
          type: 'button', onclick: () => openBulkDialog(load),
        }, icon('database'), '여러 줄 올리기')
        : null,
      canManage
        ? h('button.btn.btn-primary', {
          type: 'button', onclick: () => openTermDialog(null, load),
        }, icon('plus'), '용어 추가')
        : null,
    );
  };

  mount(outlet,
    pageHeader('용어 사전', '논리명과 물리명을 같은 규칙으로 쓰기 위한 팀의 약속입니다.'),
    h('div.card', {},
      h('div.filter-bar', {},
        h('label.field.field-inline', {}, h('span.field-label', {}, '찾기'), search),
        actions),
      box,
    ),
  );
  head();
  await load(q);
}

function listView(terms, canManage, reload, q) {
  if (terms.length === 0) {
    return emptyState(q
      ? `"${q}" 에 해당하는 용어가 없습니다.`
      : '아직 등록된 용어가 없습니다. 쓰던 목록이 있으면 "여러 줄 올리기"로 한 번에 넣으세요.');
  }

  // 같은 물리명을 쓰는 용어를 표시한다.
  //
  // 막지는 않는다 — 뜻이 다른 두 말이 같은 약어를 쓰는 일은 실제로 있고, 그것을
  // 정하는 것은 팀의 일이다. 다만 모르고 지나가면 나중에 컬럼 이름이 겹치고,
  // 그때는 이미 표가 만들어져 있다.
  const seen = new Map();
  for (const t of terms) {
    const key = t.physical.toLowerCase();
    seen.set(key, (seen.get(key) ?? 0) + 1);
  }

  return h('div.table-wrap', {},
    h('table.table.glossary', {},
      h('thead', {}, h('tr', {},
        h('th', {}, '용어'),
        h('th', {}, '물리명'),
        h('th', {}, '설명'),
        h('th.nowrap', {}, '등록'),
        canManage ? h('th', {}) : null)),
      h('tbody', {}, terms.map((t) => h('tr', {},
        h('td', {}, h('strong', {}, t.term)),
        h('td', {},
          h('button.glossary-copy', {
            type: 'button',
            title: '복사',
            onclick: () => copyToClipboard(t.physical),
          }, h('code', {}, t.physical), icon('copy', 12)),
          seen.get(t.physical.toLowerCase()) > 1
            ? badge('겹침', 'warn')
            : null),
        h('td.muted', {}, t.note || '—'),
        h('td.nowrap.muted.small', {},
          t.createdName || '—',
          h('div', {}, formatDate(t.createdAt))),
        canManage
          ? h('td.nowrap', {},
            h('button.icon-btn', {
              type: 'button', title: '고치기',
              onclick: () => openTermDialog(t, reload),
            }, icon('edit', 13)),
            h('button.icon-btn.danger', {
              type: 'button', title: '지우기',
              onclick: () => removeTerm(t, reload),
            }, icon('trash', 13)))
          : null,
      ))),
    ),
  );
}

function openTermDialog(existing, reload) {
  const termInput = input({ value: existing?.term ?? '', placeholder: '예: 회원 번호', autofocus: true });
  const physicalInput = input({ value: existing?.physical ?? '', placeholder: '예: member_no' });
  const noteInput = input({ value: existing?.note ?? '', placeholder: '언제 이 말을 쓰는지 (선택)' });

  openModal({
    title: existing ? '용어 고치기' : '용어 추가',
    width: 520,
    body: () => [
      field('용어', termInput, '사람이 쓰는 말입니다. 논리명에 그대로 적습니다.'),
      field('물리명', physicalInput, 'DB에 적을 이름입니다. 테이블·컬럼 이름에 이 말을 씁니다.'),
      field('설명', noteInput,
        '뜻이 겹치는 말이 있을 때 그 자리를 가릅니다("주문 일시는 결제 완료 시각이 아니다").'),
    ],
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: async (e) => {
          e.currentTarget.disabled = true;
          const body = {
            term: termInput.value, physical: physicalInput.value, note: noteInput.value,
          };
          try {
            if (existing) await api.put(`/glossary/${existing.id}`, body);
            else await api.post('/glossary/', body);
            close();
            toast(existing ? '용어를 고쳤습니다' : '용어를 더했습니다', 'success');
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

// openBulkDialog는 쓰던 목록을 한 번에 들인다.
//
// 사전을 처음 들이는 팀은 이미 어딘가에 목록을 갖고 있다(엑셀, 위키). 한 줄씩 옮겨
// 적게 하면 사전은 시작되지 않는다.
function openBulkDialog(reload) {
  const text = textarea({
    rows: 10,
    placeholder: '회원, member\n주문 일시, order_dttm, 결제 완료 시각이 아니다\n번호, no',
    autofocus: true,
  });
  const result = h('div');

  openModal({
    title: '여러 줄 올리기',
    width: 620,
    body: () => [
      h('p.modal-message', {},
        '한 줄에 하나씩 적습니다: ', h('b', {}, '용어, 물리명, 설명(선택)'), '. ',
        '엑셀에서 복사한 탭 구분도 그대로 됩니다.'),
      text,
      h('p.field-help', {},
        '이미 사전에 있는 말은 건너뜁니다 — 목록의 절반이 이미 들어 있는 것이 보통이고, '
        + '거기서 멈추면 그 줄을 지우고 다시 붙여넣는 일을 반복하게 됩니다.'),
      result,
    ],
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '닫기'),
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: async (e) => {
          e.currentTarget.disabled = true;
          try {
            const res = await api.post('/glossary/bulk', { text: text.value });
            mount(result, bulkResult(res));
            reload();
          } catch (err) {
            toastError(err);
          } finally {
            e.currentTarget.disabled = false;
          }
        },
      }, icon('check'), '올리기'),
    ],
  });
}

function bulkResult(res) {
  const added = (res.added ?? []).length;
  const skipped = res.skipped ?? [];
  const invalid = res.invalid ?? [];
  return h('div.notice.notice-info', {}, icon('activity'),
    h('div', {},
      h('strong', {}, `${added}개를 더했습니다`),
      skipped.length
        ? h('p.muted', {}, `이미 있어 건너뜀 ${skipped.length}개: ${skipped.slice(0, 8).join(', ')}`
          + (skipped.length > 8 ? ' …' : ''))
        : null,
      invalid.length
        ? h('p.muted', {}, `형식이 맞지 않아 넘어감 ${invalid.length}줄: ${invalid.slice(0, 3).join(' / ')}`)
        : null));
}

async function removeTerm(t, reload) {
  const ok = await confirmDialog({
    title: '용어 지우기',
    message: `"${t.term} → ${t.physical}" 을 사전에서 지웁니다. `
      + '이미 이 규칙으로 만든 이름은 그대로 남습니다 — 사전에서만 사라집니다.',
    confirmLabel: '지우기', danger: true,
  });
  if (!ok) return;
  try {
    await api.del(`/glossary/${t.id}`);
    toast('용어를 지웠습니다', 'success');
    reload();
  } catch (err) { toastError(err); }
}
