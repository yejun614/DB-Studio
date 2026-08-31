// 논리명과 물리명.
//
// 물리명은 DB에 실제로 만들어지는 이름(`ic_users`, `user_id`)이고, 논리명은 그것이
// 무엇을 뜻하는지를 사람 말로 적은 것(`회원`, `회원 번호`)이다. 설계 회의에서는
// 논리명으로 이야기하고 코드에서는 물리명으로 쓴다 — 둘 다 있어야 그 사이를 오간다.
//
// 논리명은 레이아웃(Box)에 담긴다. DB에 만들어지는 무엇이 아니므로 구조 지문에
// 들어가서는 안 된다 — 들어가면 이름을 한국어로 적는 순간 대상 DB와 다르다고
// 잡힌다(드리프트). 주석과도 다르다: 주석은 DB에 COMMENT로 실려 가는 설명이고,
// 논리명은 그 이름 자체다.

// NAME_MODES는 카드에 어느 이름을 보일지다.
export const NAME_MODES = [
  { value: 'physical', label: '물리명' },
  { value: 'logical', label: '논리명' },
  { value: 'both', label: '둘 다' },
];

// logicalOf는 레이아웃에서 컬럼의 논리명을 찾는다(없으면 빈 문자열).
export function logicalOf(layout, columnName) {
  const map = layout?.columnLogical;
  if (!map || !columnName) return '';
  return map[String(columnName).toLowerCase()] ?? '';
}

// tableLabel은 카드 제목에 쓸 이름을 고른다.
//
// main은 크게, sub는 작게 아래에 붙는다. 논리명을 적지 않은 테이블에서는 어느
// 방식이든 물리명 하나만 나온다 — 빈 자리를 남겨 두면 "논리명 모드에서는 이름이
// 사라진다"가 되고, 그것은 기능이 아니라 고장으로 읽힌다.
export function tableLabel(table, layout, mode) {
  const physical = table.namespace ? `${table.namespace}.${table.name}` : table.name;
  const logical = (layout?.logical ?? '').trim();
  if (!logical || mode === 'physical' || !mode) return { main: physical, sub: '' };
  if (mode === 'logical') return { main: logical, sub: '' };
  return { main: logical, sub: physical };
}

// columnLabel은 컬럼 줄에 쓸 이름을 고른다.
export function columnLabel(column, layout, mode) {
  const physical = column.name;
  const logical = logicalOf(layout, column.name).trim();
  if (!logical || mode === 'physical' || !mode) return { main: physical, sub: '' };
  if (mode === 'logical') return { main: logical, sub: '' };
  return { main: logical, sub: physical };
}
