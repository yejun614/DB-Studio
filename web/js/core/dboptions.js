// 표와 데이터베이스의 저장 설정(엔진·문자셋·테이블스페이스) 입력칸.
//
// 목록은 서버가 준다(/meta 의 dbKinds). 화면이 스스로 목록을 들고 있으면 언젠가
// DDL 을 만드는 쪽과 갈라져서, 화면에서는 고를 수 있는데 스크립트에는 안 나가는
// 설정이 생긴다 — 그 어긋남은 마이그레이션을 실행하는 순간에야 드러난다.
import { h, icon } from './dom.js';
import { state } from './store.js';
import { field, input, select } from './ui.js';

// specsFor는 이 DB 문법에서 정할 수 있는 설정 목록이다.
export function specsFor(dialect, scope = 'table') {
  const info = (state.meta?.dbKinds ?? []).find((k) => k.kind === dialect);
  if (!info) return [];
  return (scope === 'database' ? info.databaseOptions : info.tableOptions) ?? [];
}

// optionEditor는 설정 칸들을 그리고, 바뀐 것만 돌려준다.
//
// **바뀐 것만** 돌려주는 것이 요점이다. 서버의 op 는 패치라서, 손대지 않은 칸까지
// 함께 보내면 다른 사람이 그 사이에 고친 값을 내가 본 옛 값으로 되돌린다.
//
// inherit: 비운 칸이 무엇을 따르는지 적는 글(문서 기본값 또는 DB 기본값).
export function optionEditor(specs, values = {}, { disabled = false, inherit = {} } = {}) {
  const controls = new Map();
  const nodes = [];
  for (const spec of specs) {
    const value = values?.[spec.key] ?? '';
    const from = inherit?.[spec.key] ?? '';
    let el;
    if (spec.kind === 'select' && spec.choices?.length) {
      el = select([
        { value: '', label: from ? `${from} (물려받음)` : '정하지 않음' },
        ...spec.choices.map((c) => ({ value: c, label: c })),
        // 목록에 없는 값이 이미 들어 있으면 그대로 보여 준다. 조용히 지우면
        // 저장하는 순간 남이 정해 둔 값이 사라진다.
        ...(value && !spec.choices.includes(value)
          ? [{ value, label: `${value} (목록에 없음)` }] : []),
      ], { value, disabled });
    } else {
      el = input({
        value, disabled,
        placeholder: from || spec.placeholder || '정하지 않음',
      });
    }
    controls.set(spec.key, el);
    const help = [spec.help, from ? `비우면 ${from} 을(를) 따릅니다.` : '']
      .filter(Boolean).join(' ');
    nodes.push(field(spec.label, el, help || undefined));
  }
  return {
    nodes,
    // read는 지금 칸에 적힌 값 전체다.
    read() {
      const out = {};
      for (const [key, el] of controls) out[key] = (el.value ?? '').trim();
      return out;
    },
    // patch는 처음 값과 달라진 칸만 담은 패치다(빈 문자열 = 그 열쇠를 지운다).
    patch() {
      const out = {};
      for (const [key, el] of controls) {
        const next = (el.value ?? '').trim();
        if (next !== (values?.[key] ?? '')) out[key] = next;
      }
      return out;
    },
    dirty() {
      return Object.keys(this.patch()).length > 0;
    },
  };
}

// optionChips는 정해진 설정을 읽기 전용으로 보여 준다.
//
// 구조·스키마 화면처럼 고칠 수 없는 곳에서 쓴다. 아무것도 정하지 않았으면 아무것도
// 그리지 않는다 — "엔진: (없음)" 같은 줄은 자리만 차지하고 아무 말도 하지 않는다.
export function optionChips(dialect, options, scope = 'table') {
  const specs = specsFor(dialect, scope);
  const chips = [];
  for (const spec of specs) {
    const v = options?.[spec.key];
    if (!v) continue;
    chips.push(h('span.db-option-chip', { title: spec.help ?? '' },
      h('span.muted', {}, spec.label), h('span', {}, v)));
  }
  if (!chips.length) return null;
  return h('div.db-option-chips', {}, chips);
}

// optionLine은 한 줄짜리 요약이다(표 목록처럼 자리가 좁은 곳).
export function optionLine(dialect, options, scope = 'table') {
  const specs = specsFor(dialect, scope);
  return specs
    .map((spec) => (options?.[spec.key] ? `${spec.label} ${options[spec.key]}` : ''))
    .filter(Boolean)
    .join(' · ');
}

// inheritNotice는 "여기서 정하지 않으면 어디를 따르는가"를 적는 안내다.
export function inheritNotice(text) {
  return h('p.notice.notice-info.db-option-notice', {}, icon('alert'), text);
}
