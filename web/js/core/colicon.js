// 컬럼 아이콘.
//
// 카드에서 컬럼을 읽는 순서는 "이름 → 타입"이 아니라 "이게 뭐지 → 이름"이다.
// 예전에는 기본키에 ●, 외래키에 ◆를 붙였는데, 그 둘이 아닌 컬럼은 아무 표식이
// 없어서 스무 줄짜리 카드가 글자 벽이 됐다. 그래서 모든 컬럼에 아이콘을 두되
// 사람이 하나하나 고르게 하지는 않는다 — 타입과 키 여부만 보면 대부분 정해진다.
//
// 고른 값은 문서 레이아웃(Box.columnIcons)에 담긴다. 스키마 IR이 아니다:
// 아이콘은 사람의 메모이고, IR에 넣으면 아이콘만 바꿔도 대상 DB와 구조가 다르다고
// 잡힌다.

// AUTO는 "고르지 않음"이다. 빈 문자열을 그대로 쓰면 "아이콘 없음"과 구별되지 않아
// 자동으로 되돌릴 방법이 사라진다.
export const AUTO = '';

// 타입 계열 → 아이콘. schema.BaseType 값을 그대로 쓴다.
const BY_BASE = {
  bool: 'check',
  smallint: 'chart', int: 'chart', bigint: 'chart',
  decimal: 'chart', float: 'chart', double: 'chart',
  char: 'tag', varchar: 'tag',
  text: 'file',
  enum: 'list',
  binary: 'box', blob: 'box',
  date: 'calendar', time: 'calendar', timestamp: 'calendar', timestamptz: 'calendar',
  interval: 'history',
  uuid: 'shield', objectid: 'shield',
  json: 'code', document: 'code',
  array: 'list',
  geometry: 'location',
};

// 이름으로 한 번 더 본다. 타입이 varchar 하나로 뭉뚱그려지는 값들(메일 주소,
// 전화번호, 링크)은 타입만 봐서는 갈라지지 않는데, 카드에서 가장 눈에 띄어야 하는
// 것이 바로 그런 컬럼이다. 다만 이름 규칙은 어디까지나 짐작이라 타입 뒤에 둔다.
const BY_NAME = [
  [/(^|_)(email|mail)($|_)/, 'mail'],
  [/(^|_)(password|passwd|secret|token|hash)($|_)/, 'lock'],
  [/(^|_)(price|amount|cost|fee|salary|balance|total)($|_)/, 'money'],
  [/(^|_)(url|uri|link|href)($|_)/, 'link'],
  [/(^|_)(address|addr|city|country|lat|lng|location)($|_)/, 'location'],
  [/(^|_)(status|state|type|kind|category)($|_)/, 'flag'],
  [/(^|_)(count|cnt|qty|quantity|score|rank)($|_)/, 'chart'],
  [/(^|_)(name|title|label)($|_)/, 'tag'],
  [/(^|_)(memo|note|comment|description|content|body)($|_)/, 'chat'],
  [/(^|_)(user|owner|author|member|customer)($|_)?/, 'users'],
];

// autoColumnIcon은 아무도 고르지 않았을 때 쓸 아이콘이다.
//
// 키가 타입을 이긴다: 기본키인 정수는 "숫자"이기 전에 "키"다. 관계를 따라 읽을
// 때 필요한 것도 그쪽이다.
export function autoColumnIcon(col, { isPK = false, isFK = false } = {}) {
  if (isPK) return 'key';
  if (isFK) return 'link';
  const base = (col?.type?.base || '').toLowerCase();
  const byBase = BY_BASE[base];
  const name = (col?.name || '').toLowerCase();
  // 이름 규칙은 타입이 애매할 때(문자·정수)만 쓴다. timestamp 컬럼이 created_at
  // 이라고 해서 달력이 아닌 무엇이 되면 곤란하다.
  if (base === '' || base === 'unknown' || byBase === 'tag' || byBase === 'file'
    || byBase === 'chart') {
    for (const [re, ic] of BY_NAME) {
      if (re.test(name)) return ic;
    }
  }
  return byBase || 'tag';
}

// columnIcon은 실제로 그릴 아이콘이다. 고른 것이 있으면 그것을, 없으면 자동값을.
export function columnIcon(col, flags, chosen) {
  const picked = (chosen || '').trim();
  if (picked === 'none') return '';
  if (picked) return picked;
  return autoColumnIcon(col, flags);
}

// chosenIconFor는 레이아웃에서 이 컬럼에 지정된 값을 꺼낸다. 컬럼 이름은
// 대소문자를 가리지 않는다 — 이름을 Id 에서 id 로 고쳤다고 아이콘이 사라지면
// 사람은 그것을 버그로 읽는다.
export function chosenIconFor(layout, name) {
  const map = layout?.columnIcons ?? {};
  return map[(name || '').toLowerCase()] ?? '';
}

// 고르개에 늘어놓을 아이콘. 'none'은 "표시 안 함"이다.
export const COLUMN_ICONS = [
  'key', 'link', 'tag', 'file', 'list', 'check', 'chart', 'money',
  'calendar', 'history', 'box', 'code', 'lock', 'shield', 'mail', 'chat',
  'bell', 'location', 'truck', 'cart', 'star', 'flag', 'users', 'database',
  'activity', 'settings', 'table', 'terminal',
];
