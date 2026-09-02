// ERD 안에서 찾기.
//
// 왜 필요한가: 도면이 서른 장을 넘으면 눈으로 훑는 것이 가장 느린 방법이 된다.
// "그 컬럼 어느 표에 있었지"를 확인하려고 카드를 하나씩 열어 보게 되고, 그러다
// 같은 뜻의 컬럼을 또 만든다.
//
// 이름만이 아니라 **주석과 논리명까지** 뒤지는 이유: 물리명을 기억하지 못해서
// 찾는 경우가 대부분이다. "탈퇴"라고 쳤을 때 withdrawn_at 이 나와야 쓸모가 있다.

// MAX_HITS는 한 번에 보여 줄 결과 수다.
//
// 상한을 두는 이유: 두 글자만 쳐도 수백 줄이 나오는 도면이 있고, 그 목록은
// 아무도 읽지 않는다. 더 좁히라는 신호가 목록 길이보다 낫다.
import { cardHeight } from './erdcanvas.js';

const MAX_HITS = 60;

function norm(v) {
  return String(v ?? '').toLowerCase();
}

// score는 얼마나 잘 맞았는지다. 큰 쪽이 먼저 온다.
//
// 이름이 주석보다 앞서고, 앞에서부터 맞은 것이 가운데서 맞은 것보다 앞선다.
// 정확히 같은 이름은 맨 앞이다 — 사람이 이름을 그대로 쳤다면 그것을 찾는 것이다.
function score(needle, name, extra) {
  const n = norm(name);
  if (n === needle) return 100;
  if (n.startsWith(needle)) return 80;
  if (n.includes(needle)) return 60;
  return norm(extra).includes(needle) ? 30 : 0;
}

/**
 * findInDocument는 문서 안에서 말과 맞는 것들을 찾는다.
 *
 * 결과 항목: { kind, key, label, sub, where, score }
 *   kind  — table | column | domain | note | group
 *   key   — 고르기와 화면 옮기기에 쓰는 식별자(테이블 키, 메모 id 등)
 *   where — 어디에 있는지(컬럼이면 표 이름). 목록에서 같은 이름을 가르는 값이다.
 */
export function findInDocument(doc, query) {
  const needle = norm(query).trim();
  if (needle.length < 1) return [];

  const hits = [];
  const push = (item) => {
    if (item.score > 0) hits.push(item);
  };

  const layout = doc?.layout ?? {};
  for (const t of doc?.schema?.tables ?? []) {
    const key = t.namespace ? `${t.namespace}.${t.name}` : t.name;
    const box = layout[key] ?? layout[String(key).toLowerCase()] ?? {};
    const logical = (box.logical ?? '').trim();
    const comment = (t.comment ?? '').trim();
    push({
      kind: 'table', key, tableKey: key,
      label: t.name, sub: logical || comment, where: '',
      score: Math.max(score(needle, t.name, ''), score(needle, logical, comment)),
    });

    const colLogical = box.columnLogical ?? {};
    for (const c of t.columns ?? []) {
      const cl = (colLogical[norm(c.name)] ?? '').trim();
      const cc = (c.comment ?? '').trim();
      const type = c.domain || c.rawType || c.type?.base || '';
      push({
        kind: 'column', key: `${key}.${c.name}`, tableKey: key, column: c.name,
        label: c.name, sub: cl || cc || type, where: t.name,
        score: Math.max(
          score(needle, c.name, ''),
          score(needle, cl, cc),
          // 타입으로도 찾을 수 있어야 한다. "decimal" 로 금액 컬럼을 모아 보는 일은
          // 설계를 정리할 때 자주 한다.
          norm(type).includes(needle) ? 20 : 0,
        ),
      });
    }
  }

  for (const d of doc?.domains ?? []) {
    push({
      kind: 'domain', key: d.name, label: d.name,
      sub: d.type, where: '',
      score: Math.max(score(needle, d.name, d.comment), norm(d.type).includes(needle) ? 20 : 0),
    });
  }

  for (const n of doc?.notes ?? []) {
    const text = (n.text ?? '').trim();
    push({
      kind: 'note', key: n.id, label: text.split('\n')[0] || '(빈 메모)',
      sub: '', where: '',
      score: norm(text).includes(needle) ? 40 : 0,
    });
  }

  for (const g of doc?.groups ?? []) {
    push({
      kind: 'group', key: g.id, label: g.label || '(이름 없는 묶음)',
      sub: '', where: '',
      score: score(needle, g.label, ''),
    });
  }

  hits.sort((a, b) => b.score - a.score
    || a.label.localeCompare(b.label, 'ko')
    || a.kind.localeCompare(b.kind));
  return hits.slice(0, MAX_HITS);
}

// KIND_LABEL은 결과 줄에 붙이는 종류 이름이다.
export const KIND_LABEL = {
  table: '표', column: '컬럼', domain: '도메인', note: '메모', group: '묶음',
};

/**
 * hitCenter는 이 결과가 도면의 어느 자리인지다. 자리를 모르면 null.
 *
 * 컬럼도 그 표의 자리를 쓴다. 컬럼 한 줄로 화면을 옮기면 표의 머리가 화면 밖으로
 * 나가, 정작 어느 표인지 알 수 없게 된다.
 */
export function hitCenter(doc, hit) {
  const layout = doc?.layout ?? {};
  if (hit.kind === 'table' || hit.kind === 'column') {
    const box = layout[hit.tableKey] ?? layout[norm(hit.tableKey)];
    if (!box) return null;
    const rows = (doc.schema?.tables ?? [])
      .find((t) => (t.namespace ? `${t.namespace}.${t.name}` : t.name) === hit.tableKey)
      ?.columns?.length ?? 0;
    const w = box.w || 260;
    return { x: box.x + w / 2, y: box.y + cardHeight(rows, box.collapsed) / 2 };
  }
  if (hit.kind === 'note') {
    const n = (doc.notes ?? []).find((x) => x.id === hit.key);
    return n ? { x: n.x + (n.w || 200) / 2, y: n.y + (n.h || 80) / 2 } : null;
  }
  if (hit.kind === 'group') {
    const g = (doc.groups ?? []).find((x) => x.id === hit.key);
    return g ? { x: g.x + (g.w || 320) / 2, y: g.y + (g.h || 240) / 2 } : null;
  }
  // 도메인은 도면 위에 자리가 없다. 화면을 옮기지 않고 고르기만 한다.
  return null;
}
