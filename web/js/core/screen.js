// 지금 보고 있는 화면을 한 곳에 모아 둔다. AI 어시스턴트가 이것을 함께 보낸다.
//
// 왜 필요한가: 어시스턴트는 팝업으로 떠서 뒤에 있던 화면을 가리지 않는다. 그래서
// 사람은 "이 테이블", "이 쿼리", "이거 왜 느려요"처럼 화면을 가리키며 묻는데, 모델은
// 그 화면을 볼 수 없다. 매번 "지금 운영 shop DB의 orders 테이블을 보고 있고…"를
// 앞에 붙여 적는 것이 대화의 절반이 되었다.
//
// 두 층으로 만든다.
//
//   - 자동: 주소와 화면 이름. 라우터가 아는 것이라 화면마다 손댈 것이 없다.
//   - 화면이 알려 주는 것(setScreenDetail): 고른 DB·테이블·쿼리처럼 주소에 없거나
//     주소에는 아이디로만 있는 것들. 사람이 화면에서 읽는 말 그대로 넘긴다 —
//     모델에게 "abc123 커넥션"은 아무 뜻도 없다.
//
// 화면 이름 표는 메뉴(NAV)에서 받아 온다(setScreenNames). 여기에 따로 적어 두면
// 메뉴 이름을 고칠 때 한쪽만 고쳐지고, 모델은 없는 화면 이름을 사용자에게 말한다.

// path → 화면 이름.
let names = new Map();
// 지금 화면이 알려 준 것들(목록 또는 목록을 만드는 함수). 화면이 바뀌면 라우터가 비운다.
let detail = [];

// setScreenNames는 메뉴 묶음에서 경로별 이름을 받아 둔다.
export function setScreenNames(nav) {
  names = new Map();
  for (const group of nav ?? []) {
    for (const item of group.items ?? []) {
      if (item.path) names.set(item.path, item.label ?? '');
    }
  }
}

// setScreenDetail은 이 화면이 스스로 알려 주는 사실들이다.
//
// 문자열 목록으로 받는다("보고 있는 DB: shop"). 구조를 요구하면 화면마다 다른 모양이
// 되고, 결국 그것을 문장으로 만드는 규칙이 여기와 서버에 두 벌 생긴다.
//
// **함수도 받는다.** 편집기의 글이나 고른 행처럼 계속 바뀌는 것은 목록을 그때그때
// 다시 넘기게 하면 글자 하나마다 이 함수를 부르게 된다. 함수로 두면 어시스턴트가
// 물을 때 한 번만 읽는다.
//
// 화면은 다시 그릴 때마다 불러도 된다(덮어쓴다). 고른 것이 바뀌면 그때 부르는 것이
// 맞다 — 첫 렌더에만 부르면 테이블을 바꾼 뒤에도 처음 것이 보고된다.
export function setScreenDetail(bits) {
  detail = bits;
}

// normalize는 보고할 목록을 정리한다. 여덟 줄로 자르는 이유: 이것은 시스템 프롬프트에
// 들어가는 글이고, 화면 설명이 사용자의 질문보다 길어지면 모델은 질문이 아니라
// 화면을 설명하기 시작한다.
function normalize(bits) {
  return (Array.isArray(bits) ? bits : [bits])
    .map((b) => String(b ?? '').trim())
    .filter(Boolean)
    .slice(0, 8);
}

// clearScreenDetail은 화면이 바뀔 때 라우터가 부른다.
//
// 비우지 않으면 떠난 화면의 사실이 다음 화면의 것으로 보고된다. "모니터링 화면인데
// 테이블은 orders" 같은 문맥은 틀린 정도가 아니라 사람을 오해하게 만드는 답을 낳는다.
export function clearScreenDetail() {
  detail = [];
}

// screenLabel은 이 경로의 화면 이름이다.
//
// 정확히 일치하는 것이 없으면 앞부분이 같은 가장 긴 경로를 쓴다: /erd/<아이디> 는
// 메뉴에 없지만 /erd 는 있다. 그래야 상세 화면도 이름을 갖는다.
export function screenLabel(path) {
  const clean = String(path ?? '').split('?')[0];
  if (names.has(clean)) return names.get(clean);
  let best = '';
  let bestLen = 0;
  for (const [p, label] of names) {
    if (p === '/') continue; // 모든 경로의 앞부분이라 아무 화면이나 "대시보드"가 된다
    if ((clean === p || clean.startsWith(`${p}/`)) && p.length > bestLen) {
      best = label;
      bestLen = p.length;
    }
  }
  return best;
}

// screenConn은 커넥션 하나를 사람이 읽는 한 줄로 만든다.
//
// 화면마다 다르게 적으면 모델이 받는 문맥이 화면마다 다른 모양이 된다. 종류와 환경을
// 함께 넣는 이유는 시스템 프롬프트가 대상 DB를 그렇게 알려 주는 것과 같다 — 방언과
// 위험도를 판단할 근거다.
export function screenConn(connection, label = '보고 있는 DB') {
  if (!connection) return '';
  const db = String(connection.databaseName ?? '').trim();
  const kind = String(connection.kind ?? '').trim();
  const env = connection.environment === 'prod' ? '운영' : '개발';
  const tail = [kind, env].filter(Boolean).join(', ');
  return `${label}: ${connection.name}${db ? ` / ${db}` : ''}${tail ? ` (${tail})` : ''}`;
}

// screenContext는 지금 화면을 한 덩어리로 만든다. 볼 것이 없으면 null 이다.
export function screenContext() {
  const path = `${location.pathname}${location.search}`;
  const label = screenLabel(location.pathname);
  let bits = [];
  try {
    bits = normalize(typeof detail === 'function' ? detail() : detail);
  } catch {
    // 화면 설명을 만들다 실패했다고 질문을 막을 수는 없다.
    bits = [];
  }
  if (!label && !bits.length) return null;
  return { path, label, detail: bits };
}
