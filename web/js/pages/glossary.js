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
  toast, toastError, openModal, confirmDialog, copyToClipboard,
} from '../core/ui.js';
import { suggestInput } from '../core/searchpick.js';
import { errorPanel } from './users.js';

// 분류 이름의 층. 대·중·소 셋이고, 셋 다 비워 둘 수 있다.
//
// 분류를 따로 표로 두지 않는다. 분류는 이름 그 자체이고 다른 속성이 없어서, 표로
// 두면 "쓰이지 않는 분류를 누가 치우는가"가 새 문제로 생긴다. 목록은 실제로 쓰인
// 값에서 모은다 — 그래서 새 분류는 그냥 적으면 생긴다.
const CAT_LEVELS = [
  { key: 'cat1', label: '대분류', hint: '예: 회원' },
  { key: 'cat2', label: '중분류', hint: '예: 인증' },
  { key: 'cat3', label: '소분류', hint: '예: 비밀번호' },
];

export async function renderGlossary(outlet, params, query) {
  mount(outlet, spinner('용어 사전을 불러오는 중…'));

  const q = query.get('q') ?? '';
  const box = h('div');
  const search = input({
    type: 'search', value: q, placeholder: '용어·물리명·설명·분류에서 찾기',
    autocomplete: 'off',
  });

  let canManage = false;
  // 실제로 쓰인 분류 조합. 새 용어를 넣을 때 이미 쓰던 이름을 다시 쓰게 한다 —
  // "회원"과 "회원관리"가 따로 생기면 분류가 분류 노릇을 못 한다.
  let cats = [];
  const pickCategory = (name) => {
    search.value = name;
    load(name);
  };
  const load = async (term = search.value) => {
    mount(box, spinner('불러오는 중…'));
    try {
      const res = await api.get(`/glossary/?q=${encodeURIComponent(term.trim())}`);
      canManage = res.canManage === true;
      cats = res.categories ?? [];
      mount(box, listView(res.terms ?? [], canManage, load, term.trim(), pickCategory, () => cats));
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
          type: 'button', onclick: () => openTermDialog(null, load, cats),
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

function listView(terms, canManage, reload, q, pickCategory, cats) {
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
        h('th.nowrap', {}, '분류'),
        h('th', {}, '용어'),
        h('th', {}, '물리명'),
        h('th', {}, '설명'),
        canManage ? h('th', {}) : null)),
      h('tbody', {}, terms.map((t) => h('tr', {},
        categoryCell(t, pickCategory),
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
        canManage
          ? h('td.nowrap', {},
            h('button.icon-btn', {
              type: 'button', title: '고치기',
              onclick: () => openTermDialog(t, reload, cats()),
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

// categoryCell은 분류를 대·중·소 순으로 보여주고, 누르면 그 분류로 좁힌다.
//
// 사전을 훑는 사람은 "회원 쪽 용어"를 덩어리로 읽는다. 그때 검색어를 직접 치게
// 하면 그 말을 이미 아는 사람만 좁힐 수 있는데, 사전이 필요한 사람은 대개 그것을
// 모르는 사람이다.
function categoryCell(t, pickCategory) {
  const parts = [t.cat1, t.cat2, t.cat3].filter((x) => x);
  if (parts.length === 0) return h('td.glossary-cat.muted.small', {}, '—');
  return h('td.glossary-cat', {}, parts.map((name, i) => h('span', {},
    i > 0 ? h('span.glossary-cat-sep', {}, '›') : null,
    h('button.glossary-cat-btn', {
      type: 'button', title: `"${name}" 으로 좁히기`,
      onclick: () => pickCategory(name),
    }, name))));
}

function openTermDialog(existing, reload, cats = []) {
  const termInput = input({ value: existing?.term ?? '', placeholder: '예: 회원 번호', autofocus: true });
  const physicalInput = input({ value: existing?.physical ?? '', placeholder: '예: member_no' });
  const noteInput = input({ value: existing?.note ?? '', placeholder: '언제 이 말을 쓰는지 (선택)' });
  const cat = categoryFields(cats, existing);

  openModal({
    title: existing ? '용어 고치기' : '용어 추가',
    width: 520,
    body: () => [
      field('용어', termInput, '사람이 쓰는 말입니다. 논리명에 그대로 적습니다.'),
      field('물리명', physicalInput, 'DB에 적을 이름입니다. 테이블·컬럼 이름에 이 말을 씁니다.'),
      field('설명', noteInput,
        '뜻이 겹치는 말이 있을 때 그 자리를 가릅니다("주문 일시는 결제 완료 시각이 아니다").'),
      cat.node,
    ],
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: async (e) => {
          e.currentTarget.disabled = true;
          const body = {
            term: termInput.value, physical: physicalInput.value, note: noteInput.value,
            ...cat.values(),
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

// categoryFields는 대·중·소 세 칸을 만든다.
//
// 고르개(select)가 아니라 **자유 입력 + 제안**이다. 고르개로만 두면 새 분류를 넣기
// 위해 다른 화면으로 가야 하는데, 분류는 용어를 적는 그 순간에 처음 생긴다. 목록에
// 없는 이름을 그냥 적으면 그것이 새 분류가 된다.
//
// 아래 층은 위 층에 맞춰 제안을 좁힌다 — "회원 › 인증"을 쓰던 팀에게 "주문" 밑에서
// "인증"을 먼저 보여줄 이유가 없다. 좁히는 것은 제안뿐이고, 적는 것은 언제나 자유다.
function categoryFields(cats, existing) {
  const boxes = [];
  const valueAt = (i) => (boxes[i] ? boxes[i].value.trim() : '');

  const suggestFor = (level) => {
    const seen = new Set();
    for (const row of cats) {
      const name = row[level];
      if (!name) continue;
      // 위 층이 정해져 있으면 그 아래에서 쓰인 것만 제안한다.
      let under = true;
      for (let up = 0; up < level; up += 1) {
        const chosen = valueAt(up);
        if (chosen && row[up] !== chosen) under = false;
      }
      if (under) seen.add(name);
    }
    return [...seen].sort((a, b) => a.localeCompare(b, 'ko')).map((value) => ({ value }));
  };

  for (let i = 0; i < CAT_LEVELS.length; i += 1) {
    const level = CAT_LEVELS[i];
    boxes.push(suggestInput({
      items: [],
      value: existing?.[level.key] ?? '',
      placeholder: level.hint,
      code: false,
      iconName: 'list',
      onChange: () => {
        // 위 칸이 바뀌면 아래 칸의 제안을 다시 모은다. 값은 지우지 않는다 —
        // 사람이 적어 둔 것을 화면이 마음대로 지우면 다시 적게 된다.
        for (let below = i + 1; below < boxes.length; below += 1) {
          boxes[below].setItems(suggestFor(below));
        }
      },
    }));
  }
  boxes.forEach((b, i) => b.setItems(suggestFor(i)));

  return {
    node: h('div.field', {},
      h('span.field-label', {}, '분류'),
      h('div.glossary-cats', {}, boxes.map((b, i) => h('label.glossary-cat-field', {},
        h('span.field-label', {}, CAT_LEVELS[i].label), b.node))),
      h('span.field-help', {},
        '셋 다 비워 둬도 됩니다. 목록에 없는 이름을 적으면 그대로 새 분류가 됩니다.'),
    ),
    values: () => ({ cat1: valueAt(0), cat2: valueAt(1), cat3: valueAt(2) }),
  };
}

// openBulkDialog는 쓰던 목록을 한 번에 들인다.
//
// 사전을 처음 들이는 팀은 이미 어딘가에 목록을 갖고 있다(엑셀, 위키). 한 줄씩 옮겨
// 적게 하면 사전은 시작되지 않는다.
function openBulkDialog(reload) {
  const text = textarea({
    rows: 10,
    placeholder: '회원, member\n주문 일시, order_dttm, 결제 시각 아님, 주문\n번호, no',
    autofocus: true,
  });
  const result = h('div');

  openModal({
    title: '여러 줄 올리기',
    width: 620,
    body: () => [
      h('p.modal-message', {},
        '한 줄에 하나씩 적습니다: ',
        h('b', {}, '용어, 물리명, 설명, 대분류, 중분류, 소분류'), '. ',
        '앞의 둘만 있으면 됩니다. 엑셀에서 복사한 탭 구분도 그대로 됩니다.'),
      text,
      h('p.field-help', {},
        '설명에 쉼표가 들어가면 그 뒤가 분류로 잘립니다 — 그런 목록은 엑셀에서 '
        + '복사해 탭 구분으로 넣으세요.'),
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
