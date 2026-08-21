// DB 종류별 타입 목록과, 그 목록으로 타입 문자열을 짓고 다시 읽는 도구.
//
// 목록 자체는 서버가 쥐고 있다(internal/schema/catalog.go). 화면이 따로 들고 있으면
// DB 종류가 늘 때마다 두 곳을 고쳐야 하고, 어긋나면 화면에서 고른 타입을 서버가
// 모르는 상태가 된다. 여기서는 받아서 캐시하고, 문자열을 짓고 읽는 일만 한다.
import { api } from './api.js';

const cache = new Map(); // dialect → Promise<catalog>

// loadTypeCatalog는 dialect의 타입 목록을 가져온다(한 번만 받아 캐시한다).
export function loadTypeCatalog(dialect) {
  const key = String(dialect || '').toLowerCase();
  if (!cache.has(key)) {
    cache.set(key, api.get(`/erd/types?dialect=${encodeURIComponent(key)}`)
      .catch((err) => {
        // 실패한 약속을 캐시에 남기면 다음 시도도 같은 실패를 재생한다.
        cache.delete(key);
        throw err;
      }));
  }
  return cache.get(key);
}

// buildType은 고른 타입과 인자로 DDL 타입 문자열을 만든다.
//
// 서버가 이 문자열을 다시 파싱하므로(ParseType), 화면이 만드는 모양은 사람이 손으로
// 적는 모양과 같아야 한다 — 그래야 "고르기"와 "직접 입력"이 같은 결과를 낸다.
export function buildType(def, params = {}) {
  if (!def) return '';
  let out = def.name;
  const arg = String(params.arg ?? '').trim();
  switch (def.param) {
    case 'length':
    case 'fraction':
      if (arg) out += `(${arg})`;
      break;
    case 'precision': {
      // "10,2" 또는 "10". 소수 자릿수를 비우면 정수 자릿수만 쓴다.
      const [p, s] = arg.split(',').map((v) => v.trim());
      if (p && s) out += `(${p},${s})`;
      else if (p) out += `(${p})`;
      break;
    }
    case 'values': {
      const values = splitValues(arg);
      if (values.length) out += `(${values.map(quote).join(',')})`;
      break;
    }
    default:
      break;
  }
  if (params.unsigned && def.unsigned) out += ' UNSIGNED';
  if (params.array) out += '[]';
  return out;
}

// parseType은 타입 문자열을 카탈로그의 항목과 인자로 되돌린다.
//
// 완벽한 파서가 아니라 **고르개의 초기값을 맞추는 용도**다. 알아보지 못하면 null을
// 돌려주고, 화면은 그때 직접 입력 모드로 연다 — 억지로 비슷한 타입을 고르면
// 사용자가 적어 둔 타입이 조용히 바뀐다.
export function parseType(raw, catalog) {
  const text = String(raw || '').trim();
  if (!text || !catalog?.types) return null;

  let rest = text;
  const params = { arg: '', unsigned: false, array: false };
  if (rest.endsWith('[]')) {
    params.array = true;
    rest = rest.slice(0, -2).trim();
  }
  const unsignedRE = /\s+unsigned$/i;
  if (unsignedRE.test(rest)) {
    params.unsigned = true;
    rest = rest.replace(unsignedRE, '').trim();
  }

  let name = rest;
  const open = rest.indexOf('(');
  if (open >= 0 && rest.endsWith(')')) {
    name = rest.slice(0, open).trim();
    params.arg = rest.slice(open + 1, -1).trim();
  }

  // 이름이 정확히 같은 것을 먼저 찾는다. VARCHAR(MAX)처럼 괄호까지가 이름인
  // 타입이 있으므로 원문 전체로도 한 번 찾는다.
  const byName = (want) => catalog.types.find(
    (t) => t.name.toLowerCase() === String(want).toLowerCase());
  const def = byName(rest) ?? byName(name);
  if (!def) return null;
  if (def.param === 'values') params.arg = splitValues(params.arg).join(', ');
  return { def, params };
}

// splitValues는 "a, b" 또는 "'a','b'" 를 값 목록으로 만든다.
export function splitValues(text) {
  return String(text || '')
    .split(',')
    .map((v) => v.trim().replace(/^['"]|['"]$/g, ''))
    .filter(Boolean);
}

function quote(value) {
  return `'${String(value).replace(/'/g, "''")}'`;
}

// categories는 타입을 화면에 묶어 보여줄 순서대로 정리한다.
export function categories(catalog) {
  const order = [];
  const groups = new Map();
  for (const def of catalog?.types ?? []) {
    if (!groups.has(def.category)) {
      groups.set(def.category, []);
      order.push(def.category);
    }
    groups.get(def.category).push(def);
  }
  return order.map((name) => ({ name, types: groups.get(name) }));
}

// paramLabel은 인자 입력칸에 붙일 이름이다. 타입마다 인자의 뜻이 다르므로
// "값"처럼 뭉뚱그리면 무엇을 넣어야 할지 알 수 없다.
export function paramLabel(def) {
  switch (def?.param) {
    case 'length': return '길이';
    case 'precision': return '자릿수 (전체, 소수)';
    case 'values': return '값 목록 (쉼표로 구분)';
    case 'fraction': return '초의 소수 자릿수';
    default: return '';
  }
}

export function paramPlaceholder(def) {
  switch (def?.param) {
    case 'length': return def.max ? `1 ~ ${def.max}` : '예: 255';
    case 'precision': return '예: 10,2';
    case 'values': return '예: draft, sent, paid';
    case 'fraction': return def.max ? `0 ~ ${def.max}` : '예: 6';
    default: return '';
  }
}
