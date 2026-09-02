// 관계선의 카디널리티(1:1, N:1 …)를 스키마에서 끌어낸다.
//
// 지어내지 않는 것이 이 파일의 규칙이다. 도면은 DDL의 그림이고, 도면에만 있는
// 관계는 실행되지 않는다. 그래서 여기서 내는 값은 모두 **스키마에 적혀 있는 것**
// 에서만 나온다.
//
// # 무엇을 알 수 있고 무엇을 알 수 없는가
//
// 외래키 하나는 "자식의 이 컬럼이 부모의 그 컬럼을 가리킨다"를 말한다. 거기서
// 나오는 것은 둘이다.
//
//   부모 쪽 개수 — 자식 한 줄에 부모가 몇 줄인가. FK 컬럼이 NOT NULL 이면 정확히
//                  하나, NULL 을 허용하면 없을 수도 있다(0..1).
//   자식 쪽 개수 — 부모 한 줄에 자식이 몇 줄인가. 보통 여럿(N)이지만, FK 컬럼이
//                  **유일하면**(기본키이거나 UNIQUE 인덱스) 하나뿐이다 → 1:1.
//
// **N:N 은 외래키 하나로 표현되지 않는다.** 그것은 연결 표(junction table)를 두는
// 패턴이고, 그 표에서 나가는 외래키는 각각 N:1 이다. 그래서 두 표 사이에 N:N 선을
// 그리지 않는다 — 그 선은 DDL에 없고, 없는 것을 그리면 도면이 거짓이 된다.
// 대신 연결 표임을 그 표에 적어 준다(isJunction). 읽는 사람이 알고 싶은 것은
// "이 표가 A와 B를 N:N으로 잇는다"이고, 그 사실은 그 표의 성질이다.

function norm(v) {
  return String(v ?? '').toLowerCase();
}

// sameSet은 두 컬럼 이름 목록이 같은 집합인지다(순서는 무시).
function sameSet(a, b) {
  if (!a || !b || a.length !== b.length) return false;
  const left = a.map(norm).sort();
  const right = b.map(norm).sort();
  return left.every((v, i) => v === right[i]);
}

// coversUnique는 이 컬럼들이 유일함을 보장받는지다.
//
// 기본키이거나, 정확히 그 컬럼들로만 이뤄진 UNIQUE 인덱스가 있으면 참이다.
// "그 컬럼들로만"이 중요하다 — UNIQUE(a, b) 는 a 하나의 유일함을 보장하지 않는다.
function coversUnique(table, cols) {
  if (!cols?.length) return false;
  if (sameSet(table?.primaryKey?.columns, cols)) return true;
  for (const idx of table?.indexes ?? []) {
    if (!idx.unique) continue;
    // 부분 인덱스(WHERE)는 조건에 맞는 줄에서만 유일하다. 유일하다고 읽으면
    // 조건 밖의 줄에서 관계가 실제와 달라진다.
    if (idx.where) continue;
    const names = (idx.columns ?? []).map((c) => c.column).filter(Boolean);
    if (names.length !== (idx.columns ?? []).length) continue; // 식 기반 인덱스
    if (sameSet(names, cols)) return true;
  }
  return false;
}

/**
 * cardinality는 이 외래키의 관계를 읽어 낸다.
 *
 * 돌려주는 것:
 *   child   — 부모 한 줄에 딸리는 자식의 수 표기('N' | '1' | '0..1')
 *   parent  — 자식 한 줄이 가리키는 부모의 수 표기('1' | '0..1')
 *   label   — 'N:1' 처럼 자식:부모 순서로 읽는 짧은 표기
 *   optional— FK 컬럼에 NULL 이 허용되는지
 *   oneToOne— 1:1 인지
 */
export function cardinality(childTable, fk) {
  const cols = fk?.columns ?? [];
  const byName = new Map((childTable?.columns ?? []).map((c) => [norm(c.name), c]));
  // 여러 컬럼짜리 외래키는 **하나라도** NULL 을 허용하면 부모가 없을 수 있다.
  const optional = cols.some((name) => byName.get(norm(name))?.nullable !== false);
  const oneToOne = coversUnique(childTable, cols);

  const parent = optional ? '0..1' : '1';
  // 자식 쪽: 유일하면 한 줄뿐이고, 그 한 줄조차 없을 수 있다(부모만 있는 경우).
  const child = oneToOne ? (optional ? '0..1' : '1') : 'N';
  return {
    child, parent, optional, oneToOne,
    label: `${oneToOne ? '1' : 'N'}:1`,
  };
}

/**
 * isJunction은 이 표가 N:N 연결 표인지다.
 *
 * 판정: 외래키가 둘 이상이고, 기본키가 **그 외래키 컬럼들로만** 이뤄져 있다.
 *
 * 기본키까지 보는 이유: 외래키 둘을 가진 표는 흔하다(주문에 회원과 배송지). 그것을
 * 다 연결 표로 부르면 표시가 아무 뜻도 없어진다. 기본키가 곧 두 외래키라는 것은
 * "이 표의 정체가 그 둘의 짝이다"라는 뜻이고, 그때만 N:N 이다.
 */
export function isJunction(table) {
  const fks = table?.foreignKeys ?? [];
  if (fks.length < 2) return false;
  const pk = table?.primaryKey?.columns ?? [];
  if (pk.length < 2) return false;

  const fkCols = new Set();
  for (const fk of fks) for (const c of fk.columns ?? []) fkCols.add(norm(c));
  // 기본키의 모든 컬럼이 외래키 컬럼이어야 한다. 하나라도 아니면(연결 표에 자기
  // 아이디를 둔 경우) 그 표는 짝 이상의 무엇이다.
  if (!pk.every((c) => fkCols.has(norm(c)))) return false;
  // 그리고 외래키가 기본키를 다 덮어야 한다. 덮지 못하면 짝이 유일하지 않다.
  return pk.length >= fks.length;
}

// junctionPartners는 이 연결 표가 잇는 표들의 이름이다.
export function junctionPartners(table) {
  return (table?.foreignKeys ?? []).map((fk) => fk.refTable).filter(Boolean);
}

/**
 * describeRelation은 인스펙터에 쓸 한 문장이다.
 *
 * 기호만 두지 않는 이유: "N:1"이 무엇의 N인지는 처음 보는 사람에게 분명하지 않다.
 * 어느 쪽이 여럿인지를 표 이름으로 말해 주면 헷갈릴 일이 없다.
 */
export function describeRelation(childTable, fk) {
  const c = cardinality(childTable, fk);
  const child = childTable?.name ?? '자식';
  const parent = fk?.refTable ?? '부모';
  if (c.oneToOne) {
    return `1:1 — ${child} 한 줄에 ${parent} ${c.optional ? '0개 또는 1개' : '1개'}가 짝을 이룹니다`
      + `(${fk.columns?.join(', ')}이(가) 유일합니다)`;
  }
  return `N:1 — ${parent} 한 줄에 ${child} 여러 줄이 딸립니다`
    + `. ${child} 한 줄은 ${parent} ${c.optional ? '0개 또는 1개' : '정확히 1개'}를 가리킵니다`;
}
