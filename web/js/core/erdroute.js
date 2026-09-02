// 관계선이 남의 카드 밑으로 숨지 않게 길을 고른다.
//
// 예전에는 두 카드의 중심을 이은 방향만 보고 붙을 변을 정한 뒤 그대로 곡선을 그었다.
// 사이에 표가 하나 끼면 선이 그 카드 밑으로 들어가 **사라졌다** — 카드가 선보다 위에
// 그려지기 때문이다. 순서를 뒤집으면 선이 컬럼 이름을 가로지르므로 그럴 수도 없다.
//
// 그래서 세 단계로 고른다.
//   1. 붙을 변을 고를 때 **사이에 무엇이 있는지** 본다. 막히지 않는 조합이 있으면
//      그것을 쓴다. 이미 깨끗한 도면은 예전과 똑같이 나온다(아래 curveD 참고).
//   2. 어느 조합도 막히면 카드를 **돌아가는** 길을 찾는다(직각 + 둥근 모서리).
//   3. 그래도 지날 데가 없으면 막혔다고 알린다. 그 선은 카드 **위로** 그리고
//      배경색 테두리를 깔아 "덮고 지나간다"로 읽히게 한다(erdcanvas.js).
//      안 보이는 선보다는 낫다.

const OUT = 18; // 카드에서 일단 빠져나오는 거리
const PAD = 12; // 남의 카드를 피할 때 남길 틈
const SAMPLES = 16; // 곡선이 카드를 지나는지 볼 때 쪼개는 수
const NEAR = 200; // 두 카드를 감싼 범위에서 이만큼 밖까지만 장애물로 본다

const SIDES = {
  right: { nx: 1, ny: 0 },
  left: { nx: -1, ny: 0 },
  bottom: { nx: 0, ny: 1 },
  top: { nx: 0, ny: -1 },
};
const ORDER = ['right', 'left', 'bottom', 'top'];
const FACING = { right: 'left', left: 'right', bottom: 'top', top: 'bottom' };

function n1(v) {
  return Math.round(v * 10) / 10;
}

function center(b) {
  return { x: b.x + b.w / 2, y: b.y + b.h / 2 };
}

// sidePoint는 그 변의 가운데다. 예전과 같은 자리라 붙는 위치가 달라지지 않는다.
function sidePoint(b, side) {
  switch (side) {
    case 'right': return { x: b.x + b.w, y: b.y + b.h / 2 };
    case 'left': return { x: b.x, y: b.y + b.h / 2 };
    case 'bottom': return { x: b.x + b.w / 2, y: b.y + b.h };
    default: return { x: b.x + b.w / 2, y: b.y };
  }
}

function step(p, side, by) {
  return { x: p.x + SIDES[side].nx * by, y: p.y + SIDES[side].ny * by };
}

// naturalSides는 예전 방식으로 고른 변이다. 막히지 않았다면 이것을 쓴다.
function naturalSides(from, to) {
  const fc = center(from);
  const tc = center(to);
  if (Math.abs(tc.x - fc.x) > Math.abs(tc.y - fc.y)) {
    return tc.x > fc.x ? ['right', 'left'] : ['left', 'right'];
  }
  return tc.y > fc.y ? ['bottom', 'top'] : ['top', 'bottom'];
}

// segHitsRect는 선분이 사각형 안을 지나는지 본다(Liang-Barsky 슬랩 자르기).
function segHitsRect(p, q, r) {
  const d = [q.x - p.x, q.y - p.y];
  const s = [p.x, p.y];
  const lo = [r.x, r.y];
  const hi = [r.x + r.w, r.y + r.h];
  let t0 = 0;
  let t1 = 1;
  for (let i = 0; i < 2; i += 1) {
    if (Math.abs(d[i]) < 1e-9) {
      if (s[i] < lo[i] || s[i] > hi[i]) return false;
      continue;
    }
    let a = (lo[i] - s[i]) / d[i];
    let b = (hi[i] - s[i]) / d[i];
    if (a > b) {
      const t = a;
      a = b;
      b = t;
    }
    if (a > t0) t0 = a;
    if (b < t1) t1 = b;
    if (t0 > t1) return false;
  }
  return true;
}

// hits는 이 길이 **몇 장의 카드**를 지나는지다. 선분 수가 아니라 카드 수로 세는
// 이유: 한 카드를 길게 지나는 것과 짧게 스치는 것은 똑같이 "가려진다"이고,
// 두 장을 지나는 길보다 한 장을 지나는 길이 낫다.
function hits(pts, rects) {
  let count = 0;
  for (const r of rects) {
    for (let i = 1; i < pts.length; i += 1) {
      if (segHitsRect(pts[i - 1], pts[i], r)) {
        count += 1;
        break;
      }
    }
  }
  return count;
}

function polyLen(pts) {
  let len = 0;
  for (let i = 1; i < pts.length; i += 1) {
    len += Math.hypot(pts[i].x - pts[i - 1].x, pts[i].y - pts[i - 1].y);
  }
  return len;
}

function sampleCubic(a, c1, c2, b) {
  const out = [];
  for (let i = 0; i <= SAMPLES; i += 1) {
    const t = i / SAMPLES;
    const u = 1 - t;
    out.push({
      x: u * u * u * a.x + 3 * u * u * t * c1.x + 3 * u * t * t * c2.x + t * t * t * b.x,
      y: u * u * u * a.y + 3 * u * u * t * c1.y + 3 * u * t * t * c2.y + t * t * t * b.y,
    });
  }
  return out;
}

// curveD는 곡선 하나다.
//
// 좌우로 붙는 경우의 식은 예전 그대로 둔다. 이미 잘 보이는 도면의 그림이 이유 없이
// 바뀌면 쓰던 사람은 개선이 아니라 고장으로 읽는다.
function curveD(a, b, sa, sb) {
  const na = SIDES[sa];
  const nb = SIDES[sb];
  let c1;
  let c2;
  if (na.ny === 0 && nb.ny === 0) {
    const mx = (a.x + b.x) / 2;
    c1 = { x: mx, y: a.y };
    c2 = { x: mx, y: b.y };
  } else {
    // 위아래로 붙을 때는 접선도 위아래여야 한다. 예전에는 이 경우에도 좌우로
    // 휘어서, 카드 아래에서 나온 선이 옆으로 빠지며 제 카드 밑을 스쳤다.
    const reach = Math.max(36, Math.min(180, Math.hypot(b.x - a.x, b.y - a.y) / 2));
    c1 = { x: a.x + na.nx * reach, y: a.y + na.ny * reach };
    c2 = { x: b.x + nb.nx * reach, y: b.y + nb.ny * reach };
  }
  const d = `M${n1(a.x)},${n1(a.y)} C${n1(c1.x)},${n1(c1.y)} `
    + `${n1(c2.x)},${n1(c2.y)} ${n1(b.x)},${n1(b.y)}`;
  return { d, pts: sampleCubic(a, c1, c2, b) };
}

// roundPath는 꺾인 점들을 둥근 모서리로 잇는다. 각진 직각선은 카드 테두리와
// 뒤섞여 어디까지가 선인지 헷갈린다.
function roundPath(raw, r = 10) {
  const p = [];
  for (const q of raw) {
    const last = p[p.length - 1];
    if (!last || Math.abs(last.x - q.x) > 0.5 || Math.abs(last.y - q.y) > 0.5) p.push(q);
  }
  if (p.length < 2) return '';
  if (p.length === 2) return `M${n1(p[0].x)},${n1(p[0].y)} L${n1(p[1].x)},${n1(p[1].y)}`;

  let d = `M${n1(p[0].x)},${n1(p[0].y)}`;
  for (let i = 1; i < p.length - 1; i += 1) {
    const prev = p[i - 1];
    const cur = p[i];
    const next = p[i + 1];
    const back = Math.hypot(cur.x - prev.x, cur.y - prev.y);
    const fwd = Math.hypot(next.x - cur.x, next.y - cur.y);
    const rr = Math.min(r, back / 2, fwd / 2);
    if (rr < 1) {
      d += ` L${n1(cur.x)},${n1(cur.y)}`;
      continue;
    }
    const inP = {
      x: cur.x + ((prev.x - cur.x) / back) * rr,
      y: cur.y + ((prev.y - cur.y) / back) * rr,
    };
    const outP = {
      x: cur.x + ((next.x - cur.x) / fwd) * rr,
      y: cur.y + ((next.y - cur.y) / fwd) * rr,
    };
    d += ` L${n1(inP.x)},${n1(inP.y)} Q${n1(cur.x)},${n1(cur.y)} ${n1(outP.x)},${n1(outP.y)}`;
  }
  const end = p[p.length - 1];
  return `${d} L${n1(end.x)},${n1(end.y)}`;
}

// channels는 돌아갈 만한 통로다. 장애물 카드의 바로 위·아래·왼쪽·오른쪽을 지나는
// 선을 후보로 만든다 — 사람이 손으로 선을 옮길 때 하는 것과 같다.
function channels(near, pa, pb) {
  const out = [
    { axis: 'x', v: (pa.x + pb.x) / 2 },
    { axis: 'y', v: (pa.y + pb.y) / 2 },
  ];
  for (const r of near) {
    out.push({ axis: 'y', v: r.y - 2 }, { axis: 'y', v: r.y + r.h + 2 });
    out.push({ axis: 'x', v: r.x - 2 }, { axis: 'x', v: r.x + r.w + 2 });
    if (out.length >= 80) break;
  }
  return out;
}

function elbow(a, pa, pb, b, ch) {
  if (ch.axis === 'x') {
    return [a, pa, { x: ch.v, y: pa.y }, { x: ch.v, y: pb.y }, pb, b];
  }
  return [a, pa, { x: pa.x, y: ch.v }, { x: pb.x, y: ch.v }, pb, b];
}

function detour(from, to, near, nat) {
  const pairs = [nat, ['right', 'left'], ['left', 'right'], ['bottom', 'top'], ['top', 'bottom']];
  const seen = new Set();
  let best = null;
  for (const [sa, sb] of pairs) {
    if (seen.has(sa + sb)) continue;
    seen.add(sa + sb);
    const a = sidePoint(from, sa);
    const b = sidePoint(to, sb);
    const pa = step(a, sa, OUT);
    const pb = step(b, sb, OUT);
    for (const ch of channels(near, pa, pb)) {
      const pts = elbow(a, pa, pb, b, ch);
      const crossings = hits(pts, near);
      // 꺾인 길에는 값을 얹는다. 똑같이 안 막히면 곧은 길이 읽기 쉽다.
      const score = crossings * 10000 + polyLen(pts) + 24;
      if (!best || score < best.score) {
        best = { pts, crossings, score, a, b, sa, sb };
      }
    }
  }
  if (!best) return null;
  return {
    d: roundPath(best.pts),
    a: best.a,
    b: best.b,
    sa: best.sa,
    sb: best.sb,
    crossings: best.crossings,
  };
}

/**
 * makeRouter는 이 배치에 대한 길찾기다.
 *
 * 카드 기하가 그대로면 결과도 그대로여서 erdcanvas 쪽에서 캐시해 쓴다 —
 * 고르기만 바뀐 다시 그리기에서 길을 다시 찾을 이유가 없다.
 */
export function makeRouter(boxes, { quick = false } = {}) {
  const rects = [];
  for (const [key, g] of boxes) {
    rects.push({ key, x: g.x - PAD, y: g.y - PAD, w: g.w + PAD * 2, h: g.h + PAD * 2 });
  }

  return function route(fromKey, toKey) {
    const from = boxes.get(fromKey);
    const to = boxes.get(toKey);
    if (!from || !to) return null;

    // 자기 자신을 가리키는 외래키(부모 아이디 같은 것)는 제 카드로 돌아온다.
    //
    // 예전에는 두 접점이 같은 점이어서 길이 0인 선이 나왔다 — 화면에는 아무것도
    // 없고, 관계가 있다는 사실 자체가 도면에서 사라졌다. 오른쪽으로 나가 위로
    // 돌아 들어오는 고리로 그린다.
    if (fromKey === toKey) {
      const a = sidePoint(from, 'right');
      const b = sidePoint(from, 'top');
      const up = from.y - 30;
      const pts = [a, { x: a.x + 30, y: a.y }, { x: a.x + 30, y: up }, { x: b.x, y: up }, b];
      return {
        d: roundPath(pts), a, b, sa: 'right', sb: 'top', blocked: false,
      };
    }

    // 두 카드를 감싼 범위 근처만 본다. 멀리 있는 카드는 어차피 지나지 않는다.
    const hull = {
      x: Math.min(from.x, to.x) - NEAR,
      y: Math.min(from.y, to.y) - NEAR,
      w: Math.abs(from.x - to.x) + Math.max(from.w, to.w) + NEAR * 2,
      h: Math.abs(from.y - to.y) + Math.max(from.h, to.h) + NEAR * 2,
    };
    const near = rects.filter((r) => r.key !== fromKey && r.key !== toKey
      && r.x < hull.x + hull.w && r.x + r.w > hull.x
      && r.y < hull.y + hull.h && r.y + r.h > hull.y);

    const nat = naturalSides(from, to);
    let best = null;
    for (const sa of ORDER) {
      for (const sb of ORDER) {
        const a = sidePoint(from, sa);
        const b = sidePoint(to, sb);
        const cand = curveD(a, b, sa, sb);
        const crossings = near.length ? hits(cand.pts, near) : 0;
        // 예전에 고르던 조합에 가산점을 준다. 막힘 수가 같다면 익숙한 그림이 낫다.
        let penalty = 140;
        if (sa === nat[0] && sb === nat[1]) penalty = 0;
        else if (FACING[sa] === sb) penalty = 40;
        const score = crossings * 10000 + polyLen(cand.pts) + penalty;
        if (!best || score < best.score) {
          best = {
            ...cand, a, b, sa, sb, crossings, score,
          };
        }
      }
    }
    if (best.crossings === 0) return { ...best, blocked: false };

    // 카드를 끌고 있는 동안에는 돌아가는 길을 찾지 않는다. 프레임마다 통로를
    // 다 훑으면 손이 뻑뻑해지고, 끌고 있는 사람은 최종 경로가 아니라 어디에
    // 놓을지를 보고 있다. 놓는 순간 제대로 다시 찾는다.
    if (quick) return { ...best, blocked: best.crossings > 0 };

    const around = detour(from, to, near, nat);
    if (around && around.crossings === 0) return { ...around, blocked: false };

    // 여기까지 오면 지날 데가 없다. 덜 막히는 쪽을 쓰고 막혔다고 알린다.
    const pick = around && around.crossings < best.crossings ? around : best;
    return { ...pick, blocked: true };
  };
}

// oneMarker는 "하나" 쪽 짧은 막대다. 들어오는 방향과 직각이어야 선의 끝으로 읽힌다.
export function oneMarker(b, side) {
  if (SIDES[side].ny === 0) {
    return {
      x: b.x - 1, y: b.y - 5, width: 2, height: 10, rx: 1,
    };
  }
  return {
    x: b.x - 5, y: b.y - 1, width: 10, height: 2, rx: 1,
  };
}

// endLabelSpot은 선 끝에 붙일 글자의 자리다.
//
// 끝점 그대로에 두면 카드 테두리 위에 글자가 얹혀 읽히지 않는다. 들어오는 방향의
// **반대쪽**(선이 온 쪽)으로 조금 물리고, 선과 겹치지 않게 직각 방향으로도 비킨다.
export function endLabelSpot(p, side, gap = 13, off = 9) {
  const n = SIDES[side] ?? SIDES.right;
  return {
    x: p.x - n.nx * gap - (n.ny === 0 ? 0 : off),
    y: p.y - n.ny * gap - (n.ny === 0 ? off : 0),
    // 가로로 들어오는 선은 글자를 선 위에 얹고, 세로로 들어오는 선은 옆에 둔다.
    anchor: n.ny === 0 ? 'middle' : 'start',
  };
}
