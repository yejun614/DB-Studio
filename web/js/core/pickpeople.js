// 검색해서 고르는 여러 명 선택기.
//
// 체크박스를 늘어놓던 자리를 대신한다. 사람이 열 명을 넘어가면 목록을 눈으로 훑어
// 찾는 것이 느려지고, 대화상자 높이도 사람 수만큼 늘어난다.
//
// 메뉴를 띄우지 않고 **흐름 안에서 펼치는** 이유: 이 고르개가 놓이는 자리는 대개
// 대화상자이고, 대화상자 본문은 스크롤 상자다(.modal-body { overflow-y: auto }).
// 절대 위치로 띄운 메뉴는 그 상자 경계에서 잘린다 — 목록의 아래쪽 몇 명이 조용히
// 사라지는 종류의 어긋남이다.
import { h, mount, icon } from './dom.js';

/**
 * peoplePicker는 검색 칸 + 고른 사람 칩 + 후보 목록을 묶어 돌려준다.
 * @param {object} opts
 * @param {Array<{id: string, label: string, sub?: string}>} opts.items 후보
 * @param {string[]} opts.selected 처음 고른 id 목록
 * @param {string} opts.placeholder 검색 칸 안내
 * @param {string} opts.emptyText 후보가 하나도 없을 때 문구
 * @param {(ids: string[]) => void} [opts.onChange] 고른 것이 바뀔 때
 * @returns {{node: HTMLElement, value: string[]}}
 */
export function peoplePicker({
  items, selected = [], placeholder = '이름으로 검색', emptyText = '고를 수 있는 사람이 없습니다',
  onChange,
}) {
  const chosen = new Set(selected);
  const byID = new Map(items.map((p) => [p.id, p]));

  const chips = h('div.pick-chips');
  const list = h('div.pick-list');
  const search = h('input.input.pick-search', {
    type: 'text', placeholder, autocomplete: 'off', spellcheck: false,
  });
  // 검색 칸에 포커스가 있을 때만 후보를 펼친다. 늘 펼쳐 두면 고르고 난 뒤에도
  // 대화상자 절반을 후보 목록이 차지한다.
  let open = false;
  let cursor = 0;

  const box = h('div.pick', {},
    chips,
    h('div.pick-field', {}, icon('users'), search),
    list,
  );

  const matches = () => {
    const q = search.value.trim().toLowerCase();
    return items.filter((p) => {
      if (chosen.has(p.id)) return false;
      if (!q) return true;
      return p.label.toLowerCase().includes(q) || (p.sub ?? '').toLowerCase().includes(q);
    });
  };

  const drawChips = () => {
    const picked = [...chosen].map((id) => byID.get(id)).filter(Boolean);
    mount(chips, picked.length
      ? picked.map((p) => h('span.pick-chip', {},
        h('span', {}, p.label),
        h('button.pick-chip-x', {
          type: 'button', title: `${p.label} 빼기`,
          onclick: () => toggle(p.id, false),
        }, icon('x'))))
      : h('span.muted.small', {}, '아직 고르지 않았습니다'));
  };

  const drawList = () => {
    if (!open) {
      mount(list, []);
      list.classList.remove('is-open');
      return;
    }
    list.classList.add('is-open');
    const found = matches();
    if (found.length === 0) {
      mount(list, h('p.pick-empty', {},
        items.length === 0 ? emptyText : '일치하는 사람이 없습니다'));
      return;
    }
    if (cursor >= found.length) cursor = found.length - 1;
    if (cursor < 0) cursor = 0;
    mount(list, found.map((p, i) => h('button', {
      type: 'button',
      class: `pick-opt${i === cursor ? ' is-cursor' : ''}`,
      // mousedown 으로 처리하는 이유: click 은 blur 뒤에 오고, blur 가 목록을
      // 닫아 버리면 그 클릭은 아무 데도 닿지 않는다.
      onmousedown: (e) => { e.preventDefault(); toggle(p.id, true); },
    }, h('span', {}, p.label), p.sub ? h('span.muted.small', {}, p.sub) : null)));
  };

  const toggle = (id, on) => {
    if (on) chosen.add(id); else chosen.delete(id);
    // 고른 뒤에는 검색어를 비운다. 남겨 두면 방금 고른 이름으로 걸러진 빈 목록이
    // 남아 "더 고를 사람이 없다"처럼 보인다.
    if (on) search.value = '';
    cursor = 0;
    drawChips();
    drawList();
    onChange?.([...chosen]);
  };

  search.addEventListener('focus', () => { open = true; drawList(); });
  search.addEventListener('blur', () => { open = false; drawList(); });
  search.addEventListener('input', () => { cursor = 0; drawList(); });
  search.addEventListener('keydown', (e) => {
    const found = matches();
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      if (!open) { open = true; }
      cursor += e.key === 'ArrowDown' ? 1 : -1;
      if (cursor < 0) cursor = found.length - 1;
      if (cursor >= found.length) cursor = 0;
      drawList();
      return;
    }
    if (e.key === 'Enter') {
      e.preventDefault();
      if (found[cursor]) toggle(found[cursor].id, true);
      return;
    }
    if (e.key === 'Escape' && open) {
      // 대화상자가 함께 닫히지 않게 여기서 멈춘다 — 사용자가 지우려던 것은
      // 펼쳐진 목록이지 대화상자가 아니다.
      e.stopPropagation();
      open = false;
      drawList();
      return;
    }
    // 검색어가 빈 상태의 Backspace는 마지막으로 고른 사람을 뺀다.
    if (e.key === 'Backspace' && search.value === '' && chosen.size > 0) {
      toggle([...chosen].pop(), false);
    }
  });

  drawChips();
  drawList();

  return {
    node: box,
    get value() { return [...chosen]; },
  };
}
