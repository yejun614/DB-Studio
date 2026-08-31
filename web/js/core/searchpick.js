// 검색해서 고르는 고르개 두 가지.
//
//   - searchPicker : 하나만 고른다(타입 고르기처럼 후보가 수십 개인 자리)
//   - peoplePicker : 여럿 고른다(리뷰어 지정처럼)
//
// 목록이 길어지면 select 는 고르기 어렵다. 스크롤로 훑어야 하고, 항목 글자가 길면
// 잘리며, 키보드로는 첫 글자 점프밖에 안 된다. 그래서 입력으로 걸러 고른다.
//
// 후보 목록은 **화면에 띄운다**(position: fixed). 흐름 안에서 펼치면 열 때마다
// 대화상자 높이가 출렁이고, absolute 로 띄우면 대화상자 본문(.modal-body 는 스크롤
// 상자다)의 경계에서 잘려 아래쪽 몇 개가 조용히 사라진다. fixed 는 뷰포트를 기준으로
// 놓이므로 둘 다 피한다 — 대신 자리를 스스로 계산한다.
import { h, mount, icon } from './dom.js';

// 목록 높이 상한. 이보다 길면 목록 안에서 스크롤한다.
const MAX_LIST_HEIGHT = 220;

// createList는 띄우는 목록 하나와 그 자리 잡기를 만든다.
function createList(anchorEl) {
  const list = h('div.pick-list');

  const place = () => {
    const r = anchorEl.getBoundingClientRect();
    const margin = 12;
    const gap = 4;
    const below = window.innerHeight - r.bottom - margin;
    const above = r.top - margin;
    // 아래가 좁고 위가 더 넓으면 위로 뒤집는다. 화면 아래쪽에서 열면 목록이
    // 두세 줄만 보이는 일이 생긴다.
    const flip = below < 140 && above > below;
    const height = Math.max(80, Math.min(MAX_LIST_HEIGHT, flip ? above : below));
    list.style.left = `${Math.round(r.left)}px`;
    list.style.width = `${Math.round(r.width)}px`;
    list.style.maxHeight = `${Math.round(height)}px`;
    if (flip) {
      list.style.top = 'auto';
      list.style.bottom = `${Math.round(window.innerHeight - r.top + gap)}px`;
    } else {
      list.style.bottom = 'auto';
      list.style.top = `${Math.round(r.bottom + gap)}px`;
    }
  };

  // 스크롤·리사이즈를 따라 자리만 다시 잡는다. 목록을 닫아 버리면 대화상자를
  // 조금 스크롤했다고 고르던 것이 사라진다.
  const follow = () => {
    if (!anchorEl.isConnected) {
      detach();
      return;
    }
    place();
  };
  const attach = () => {
    window.addEventListener('resize', follow);
    // capture: 대화상자 본문처럼 중간에 있는 스크롤 상자의 스크롤도 잡아야 한다.
    document.addEventListener('scroll', follow, true);
  };
  const detach = () => {
    window.removeEventListener('resize', follow);
    document.removeEventListener('scroll', follow, true);
  };
  return { list, place, attach, detach };
}

// exprNodes는 식을 조각으로 나눠 함수 이름과 괄호를 따로 칠한다.
//
// 색을 나누는 이유: 기본값 칸에 들어가는 것은 값(0, '')과 식(now(), NEXTVAL(...))
// 두 종류인데, 목록에서 둘이 같은 색이면 어느 것이 함수인지 읽어서 판단해야 한다.
// 인자가 있는 함수는 예시 인자까지 들어 있어 줄이 길어지므로 더 그렇다.
function exprNodes(expr) {
  const out = [];
  // 함수 호출(이름 + 여는 괄호), 괄호, 문자열, 나머지로 자른다.
  const re = /([A-Za-z_][A-Za-z0-9_.]*)\s*(?=\()|([()])|('[^']*')/g;
  let last = 0;
  let m = re.exec(expr);
  while (m) {
    if (m.index > last) out.push(h('span', {}, expr.slice(last, m.index)));
    if (m[1]) out.push(h('span.pick-fn', {}, m[1]));
    else if (m[2]) out.push(h('span.pick-paren', {}, m[2]));
    else if (m[3]) out.push(h('span.pick-str', {}, m[3]));
    last = m.index + m[0].length;
    m = re.exec(expr);
  }
  if (last < expr.length) out.push(h('span', {}, expr.slice(last)));
  return out;
}

// optionRow는 후보 한 줄이다. 이름과 곁말(hint)을 한 줄에 둔다.
function optionRow(item, active, onPick) {
  return h('button', {
    type: 'button',
    class: `pick-opt${active ? ' is-cursor' : ''}`,
    // mousedown 으로 처리하는 이유: click 은 blur 뒤에 오고, blur 가 목록을 닫아
    // 버리면 그 클릭은 아무 데도 닿지 않는다.
    onmousedown: (e) => { e.preventDefault(); onPick(); },
  },
  item.code ? h('code.pick-expr', {}, ...exprNodes(item.label)) : h('span', {}, item.label),
  item.hint ? h('span.muted.small', {}, item.hint) : null);
}

// renderOptions는 후보를 그리되 묶음이 바뀌는 자리에 머리말을 끼운다.
//
// 머리말이 필요한 이유: 이 타입에 어울리는 것과 그 밖의 것을 한 목록에 함께 두기
// 때문이다. 구분이 없으면 "왜 문자 컬럼에 now() 가 있지"가 되고, 나누어 두면
// 아래쪽은 "찾으면 있다"는 뜻이 된다.
function renderOptions(list, found, cursor, onPick) {
  const nodes = [];
  let group = null;
  found.forEach((it, i) => {
    if (it.group && it.group !== group) {
      group = it.group;
      nodes.push(h('div.pick-group', {}, group));
    }
    nodes.push(optionRow(it, i === cursor, () => onPick(it.value)));
  });
  mount(list, nodes);
}

// matches는 검색어로 후보를 거른다. 이름·곁말·묶음 어디에 걸려도 통과시킨다 —
// 사람은 "int" 로도 "정수" 로도 찾는다.
function matches(items, q, skip = () => false) {
  const needle = q.trim().toLowerCase();
  return items.filter((it) => {
    if (skip(it)) return false;
    if (!needle) return true;
    return `${it.label} ${it.hint ?? ''} ${it.group ?? ''}`.toLowerCase().includes(needle);
  });
}

/**
 * searchPicker는 하나만 고르는 검색 고르개다. select 를 대신한다.
 * @param {object} opts
 * @param {Array<{value: string, label: string, hint?: string, group?: string}>} opts.items
 * @param {string} [opts.value] 처음 고른 값
 * @param {string} [opts.placeholder]
 * @param {string} [opts.emptyLabel] 아무것도 고르지 않은 상태의 표시
 * @param {(value: string) => void} [opts.onPick]
 * @returns {{node: HTMLElement, value: string, disabled: boolean, focus: () => void}}
 */
export function searchPicker({
  items, value = '', placeholder = '이름으로 검색', emptyLabel = '(고르기)', onPick,
}) {
  const byValue = new Map(items.map((it) => [it.value, it]));
  let current = value;
  let open = false;
  let cursor = 0;

  const box = h('input.input.pick-input', {
    type: 'text', placeholder, autocomplete: 'off', spellcheck: false,
  });
  const field = h('div.pick-field', {}, icon('list'), box);
  const { list, place, attach, detach } = createList(box);
  const node = h('div.pick.is-single', {}, field, list);

  const labelOf = (v) => byValue.get(v)?.label ?? '';
  const showCurrent = () => { box.value = current ? labelOf(current) : ''; };

  const draw = () => {
    if (!open) {
      mount(list, []);
      list.classList.remove('is-open');
      detach();
      return;
    }
    list.classList.add('is-open');
    // 열자마자는 고른 것을 그대로 보여주므로 검색어로 치지 않는다.
    const q = box.value === labelOf(current) ? '' : box.value;
    const found = matches(items, q);
    if (found.length === 0) {
      mount(list, h('p.pick-empty', {}, '일치하는 것이 없습니다'));
      place();
      attach();
      return;
    }
    if (cursor >= found.length) cursor = found.length - 1;
    if (cursor < 0) cursor = 0;
    place();
    attach();
    renderOptions(list, found, cursor, pick);
    // 고른 줄이 보이도록 맞춘다. 스무 개 넘는 목록에서 지금 값이 어디인지 모르면
    // 다시 찾아 내려가야 한다.
    list.querySelector('.pick-opt.is-cursor')?.scrollIntoView({ block: 'nearest' });
  };

  const pick = (next) => {
    current = next;
    open = false;
    showCurrent();
    draw();
    box.blur();
    onPick?.(current);
  };

  box.addEventListener('focus', () => {
    open = true;
    // 고른 값을 그대로 두고 전체를 고른다. 그대로 타자하면 걸러지고,
    // 그냥 두면 지금 값이 무엇인지 보인다.
    box.select();
    cursor = Math.max(0, matches(items, '').findIndex((it) => it.value === current));
    draw();
  });
  box.addEventListener('blur', () => {
    open = false;
    showCurrent();
    draw();
  });
  box.addEventListener('input', () => { cursor = 0; draw(); });
  box.addEventListener('keydown', (e) => {
    const q = box.value === labelOf(current) ? '' : box.value;
    const found = matches(items, q);
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      open = true;
      cursor += e.key === 'ArrowDown' ? 1 : -1;
      if (cursor < 0) cursor = found.length - 1;
      if (cursor >= found.length) cursor = 0;
      draw();
      return;
    }
    if (e.key === 'Enter') {
      e.preventDefault();
      if (found[cursor]) pick(found[cursor].value);
      return;
    }
    if (e.key === 'Escape' && open) {
      // 대화상자가 함께 닫히지 않게 여기서 멈춘다 — 지우려던 것은 목록이다.
      e.stopPropagation();
      open = false;
      showCurrent();
      draw();
    }
  });

  showCurrent();
  box.placeholder = current ? placeholder : emptyLabel;

  return {
    node,
    focus() { box.focus(); },
    get value() { return current; },
    set value(v) {
      current = v ?? '';
      showCurrent();
    },
    get disabled() { return box.disabled; },
    set disabled(v) {
      box.disabled = Boolean(v);
      node.classList.toggle('is-disabled', Boolean(v));
      if (v) {
        open = false;
        draw();
      }
    },
  };
}

/**
 * suggestInput은 **자유 입력**에 제안 목록을 붙인다.
 *
 * searchPicker 와 다른 점: 값이 목록에 매이지 않는다. 기본값 칸이 그런 자리다 —
 * now() 같은 흔한 식은 골라 넣되, DB마다 다른 표현이나 우리가 모르는 함수도 그대로
 * 적을 수 있어야 한다. 목록에 없는 것을 못 적게 하면 그 칸은 쓸 수 없게 된다.
 *
 * @param {object} opts
 * @param {Array<{value: string, label?: string, hint?: string}>} opts.items 제안
 * @param {string} [opts.value] 처음 값
 * @param {string} [opts.placeholder]
 * @param {(value: string) => void} [opts.onChange] 값이 바뀔 때
 * @param {boolean} [opts.code] 제안을 식으로 칠할지 (기본 true)
 * @param {string} [opts.iconName] 칸 앞 아이콘
 * @returns {{node: HTMLElement, input: HTMLInputElement, value: string, setItems: Function}}
 */
export function suggestInput({
  items = [], value = '', placeholder = '', onChange,
  code = true, iconName = 'sparkles',
}) {
  const toRow = (it) => ({
    value: it.value,
    label: it.label ?? it.value,
    hint: it.hint ?? '',
    group: it.group ?? '',
    // 기본값은 식이라 함수와 괄호를 따로 칠한다. 사람 말이 들어오는 칸(분류
    // 이름 같은)에서는 끄야 한다 — 한글을 코드처럼 칠하면 읽기 어렵다.
    code,
  });
  let rows = items.map(toRow);
  let open = false;
  let cursor = 0;

  const box = h('input.input.pick-input.is-free', {
    type: 'text', value, placeholder, autocomplete: 'off', spellcheck: false,
  });
  const field = h('div.pick-field', {}, icon(iconName), box);
  const { list, place, attach, detach } = createList(box);
  const node = h('div.pick.is-single', {}, field, list);

  // 제안은 "지금 적은 것으로 시작하는가"까지 본다. 빈 칸에서는 전부 보여준다 —
  // 무엇을 적을 수 있는지 모르는 것이 이 칸의 어려움이다.
  const visible = () => matches(rows, box.value);

  const draw = () => {
    if (!open || rows.length === 0) {
      mount(list, []);
      list.classList.remove('is-open');
      detach();
      return;
    }
    const found = visible();
    if (found.length === 0) {
      mount(list, []);
      list.classList.remove('is-open');
      detach();
      return;
    }
    list.classList.add('is-open');
    if (cursor >= found.length) cursor = found.length - 1;
    if (cursor < 0) cursor = 0;
    place();
    attach();
    renderOptions(list, found, cursor, pick);
  };

  const pick = (next) => {
    box.value = next;
    open = false;
    draw();
    onChange?.(next);
  };

  box.addEventListener('focus', () => { open = true; cursor = 0; draw(); });
  box.addEventListener('blur', () => { open = false; draw(); onChange?.(box.value); });
  box.addEventListener('input', () => { open = true; cursor = 0; draw(); onChange?.(box.value); });
  box.addEventListener('keydown', (e) => {
    const found = visible();
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      open = true;
      cursor += e.key === 'ArrowDown' ? 1 : -1;
      if (cursor < 0) cursor = found.length - 1;
      if (cursor >= found.length) cursor = 0;
      draw();
      return;
    }
    // Enter는 목록이 펼쳐져 있을 때만 고른다. 접혀 있으면 적은 것이 답이므로
    // 대화상자의 기본 동작(저장)을 막지 않는다.
    if (e.key === 'Enter' && open && found[cursor]) {
      e.preventDefault();
      pick(found[cursor].value);
      return;
    }
    if (e.key === 'Escape' && open) {
      e.stopPropagation();
      open = false;
      draw();
    }
  });

  return {
    node,
    input: box,
    get value() { return box.value; },
    set value(v) { box.value = v ?? ''; },
    // setItems는 제안 목록을 갈아 끼운다. 타입을 바꾸면 제안도 달라진다.
    setItems(next) {
      rows = (next ?? []).map(toRow);
      cursor = 0;
      draw();
    },
  };
}

/**
 * peoplePicker는 검색 칸 + 고른 사람 칩 + 후보 목록을 묶어 돌려준다(여럿 고르기).
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
  const rows = items.map((p) => ({ value: p.id, label: p.label, hint: p.sub }));
  // 뺄 사람. 고를 수도 없고, 이미 골라 두었다면 빠진다.
  //
  // 고르개 바깥의 사정(예: 담당자로 정한 사람은 리뷰어가 될 수 없다)을 여기서 알
  // 필요는 없다. 그것을 아는 쪽이 목록만 넘겨 준다.
  let excluded = new Set();

  const chips = h('div.pick-chips');
  const search = h('input.input.pick-input', {
    type: 'text', placeholder, autocomplete: 'off', spellcheck: false,
  });
  const field = h('div.pick-field', {}, icon('users'), search);
  const { list, place, attach, detach } = createList(search);
  const box = h('div.pick', {}, chips, field, list);

  // 검색 칸에 포커스가 있을 때만 후보를 펼친다. 늘 펼쳐 두면 고르고 난 뒤에도
  // 대화상자 절반을 후보 목록이 차지한다.
  let open = false;
  let cursor = 0;

  const visible = () => matches(rows, search.value,
    (it) => chosen.has(it.value) || excluded.has(it.value));

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
      detach();
      return;
    }
    list.classList.add('is-open');
    const found = visible();
    if (found.length === 0) {
      mount(list, h('p.pick-empty', {},
        items.length === 0 ? emptyText : '일치하는 사람이 없습니다'));
      place();
      attach();
      return;
    }
    if (cursor >= found.length) cursor = found.length - 1;
    if (cursor < 0) cursor = 0;
    place();
    attach();
    renderOptions(list, found, cursor, (v) => toggle(v, true));
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
    const found = visible();
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      open = true;
      cursor += e.key === 'ArrowDown' ? 1 : -1;
      if (cursor < 0) cursor = found.length - 1;
      if (cursor >= found.length) cursor = 0;
      drawList();
      return;
    }
    if (e.key === 'Enter') {
      e.preventDefault();
      if (found[cursor]) toggle(found[cursor].value, true);
      return;
    }
    if (e.key === 'Escape' && open) {
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
    // setExcluded는 고를 수 없는 사람을 정한다. 이미 골라 둔 사람이 그 목록에 들면
    // 조용히 빠진다 — 고를 수 없게 만들면서 이미 고른 것을 남겨 두면, 저장할 때에야
    // 거절당한다.
    setExcluded(ids) {
      excluded = new Set(ids ?? []);
      let dropped = false;
      for (const id of [...chosen]) {
        if (!excluded.has(id)) continue;
        chosen.delete(id);
        dropped = true;
      }
      drawChips();
      drawList();
      if (dropped) onChange?.([...chosen]);
      return dropped;
    },
  };
}
