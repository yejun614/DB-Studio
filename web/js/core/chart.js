// 의존성 없는 SVG 시계열 차트.
//
// 라이브러리를 쓰지 않는 이유는 요구사항(프레임워크 없음)이지만, 실제로 필요한 것은
// 선/영역 + min-max 밴드 + 호버 툴팁 정도이고 이는 SVG로 직접 그리는 편이
// 번들 크기와 테마 연동 면에서 유리하다.

const NS = 'http://www.w3.org/2000/svg';

function el(name, attrs = {}) {
  const node = document.createElementNS(NS, name);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === null || v === undefined) continue;
    node.setAttribute(k, String(v));
  }
  return node;
}

// ---------- 값 포맷 ----------

export function formatMetricValue(value, unit) {
  if (value === null || value === undefined || Number.isNaN(value)) return '—';
  switch (unit) {
    case 'percent':
      return `${value.toFixed(1)}%`;
    case 'bytes':
      return formatBytes(value);
    // 초당 바이트는 "얼마"가 아니라 "얼마씩"이다. /s를 빼면 누적량으로 읽힌다.
    case 'bytes_per_sec':
      return `${formatBytes(value)}/s`;
    case 'ms':
      return value >= 1000 ? `${(value / 1000).toFixed(2)}s` : `${value.toFixed(0)}ms`;
    case 's':
      return formatDuration(value);
    case 'per_sec':
      return `${formatNumber(value)}/s`;
    case 'ratio':
      return value.toFixed(2);
    default:
      return formatNumber(value);
  }
}

export function formatBytes(v) {
  if (!v) return '0B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  let i = 0;
  let n = Math.abs(v);
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i += 1;
  }
  return `${i === 0 ? n.toFixed(0) : n.toFixed(1)}${units[i]}`;
}

export function formatDuration(sec) {
  if (sec < 1) return `${(sec * 1000).toFixed(0)}ms`;
  if (sec < 60) return `${sec.toFixed(0)}초`;
  if (sec < 3600) return `${(sec / 60).toFixed(1)}분`;
  if (sec < 86400) return `${(sec / 3600).toFixed(1)}시간`;
  return `${(sec / 86400).toFixed(1)}일`;
}

export function formatNumber(v) {
  if (v === null || v === undefined) return '—';
  const abs = Math.abs(v);
  if (abs >= 1e9) return `${(v / 1e9).toFixed(1)}B`;
  if (abs >= 1e6) return `${(v / 1e6).toFixed(1)}M`;
  if (abs >= 1e4) return `${(v / 1e3).toFixed(1)}K`;
  if (Number.isInteger(v)) return String(v);
  if (abs >= 10) return v.toFixed(1);
  if (abs >= 1) return v.toFixed(2);
  return v.toFixed(3);
}

function formatClock(d) {
  const pad = (n) => String(n).padStart(2, '0');
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function formatClockFull(d) {
  const pad = (n) => String(n).padStart(2, '0');
  return `${d.getMonth() + 1}/${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

// ---------- 눈금 계산 ----------

// niceTicks는 사람이 읽기 좋은 축 값을 만든다.
// 1, 2, 5의 배수만 쓰면 0.5, 1, 2, 5, 10, 20, 50 같은 눈금이 나온다.
function niceTicks(min, max, count = 4) {
  if (min === max) {
    // 값이 일정한 구간(예: 항상 0)에서도 축이 생기도록 폭을 만든다.
    const pad = Math.abs(min) > 1 ? Math.abs(min) * 0.1 : 1;
    min -= pad;
    max += pad;
  }
  const span = max - min;
  const rawStep = span / count;
  const mag = 10 ** Math.floor(Math.log10(rawStep));
  const norm = rawStep / mag;
  const step = (norm >= 5 ? 5 : norm >= 2 ? 2 : 1) * mag;
  const start = Math.floor(min / step) * step;
  const end = Math.ceil(max / step) * step;

  const ticks = [];
  for (let v = start; v <= end + step * 1e-6; v += step) {
    // 부동소수 누적 오차로 -0 이나 1e-17 같은 값이 생기는 것을 막는다.
    ticks.push(Math.abs(v) < step * 1e-6 ? 0 : v);
  }
  return { ticks, min: start, max: end };
}

// ---------- 차트 ----------

/**
 * 시계열 차트를 그린다.
 * @param {object} opts
 * @param {Array<{ts: string, avg: number, min: number, max: number}>} opts.points
 * @param {string} opts.unit  값 단위
 * @param {string} opts.label 지표 라벨
 * @param {number} opts.height  픽셀 높이 (기본 150)
 * @param {number|null} opts.threshold  임계선 (있으면 점선으로 표시)
 * @param {boolean} opts.higherIsBetter  색상 방향
 */
export function lineChart(opts) {
  const {
    unit = 'count', label = '', height = 150,
    threshold = null, showBand = true,
  } = opts;

  const wrap = document.createElement('div');
  wrap.className = 'chart';

  // viewBox 좌표계로 그리고 CSS로 늘려 반응형을 만든다.
  const W = 800;
  const H = height;
  const padL = 52;
  const padR = 12;
  const padT = 10;
  const padB = 22;
  const plotW = W - padL - padR;
  const plotH = H - padT - padB;

  // 아래 것들은 갱신할 때마다 다시 계산된다. 호버 처리기가 최신 값을 봐야 하므로
  // 클로저 변수로 둔다.
  let points = [];
  let times = [];
  let tMin = 0;
  let tSpan = 1;
  let x = (t) => t;
  let y = (v) => v;

  const svg = el('svg', {
    viewBox: `0 0 ${W} ${H}`,
    preserveAspectRatio: 'none',
    class: 'chart-svg',
    role: 'img',
    'aria-label': `${label} 시계열 차트`,
  });

  // 값 축(가로 격자·Y 라벨)과 시간 축을 나눈다.
  // 갱신할 때 시간 쪽만 왼쪽으로 밀어야 하는데, 한 덩어리면 격자까지 함께 움직인다.
  const axisLayer = el('g', { class: 'chart-axis-layer' });
  const plotLayer = el('g', { class: 'chart-plot-layer' });
  svg.appendChild(axisLayer);
  svg.appendChild(plotLayer);

  // draw는 주어진 점들로 축과 선을 다시 그린다.
  //
  // 카드를 통째로 다시 만들지 않는 이유: 15초마다 화면이 사라졌다 나타나면
  // 읽는 도중에 시선이 끊긴다. 같은 SVG 안의 내용만 바꾸면 깜빡임이 없다.
  function draw(next) {
    points = next ?? [];
    axisLayer.replaceChildren();
    plotLayer.replaceChildren();
    emptyNote.style.display = points.length === 0 ? '' : 'none';
    if (points.length === 0) return;

    times = points.map((p) => new Date(p.ts).getTime());
    tMin = times[0];
    const tMax = times[times.length - 1];
    tSpan = Math.max(1, tMax - tMin);

    let vMin = Infinity;
    let vMax = -Infinity;
    for (const p of points) {
      vMin = Math.min(vMin, showBand ? p.min : p.avg);
      vMax = Math.max(vMax, showBand ? p.max : p.avg);
    }
    if (threshold !== null && Number.isFinite(threshold)) {
      vMin = Math.min(vMin, threshold);
      vMax = Math.max(vMax, threshold);
    }
    // 비율/퍼센트는 0을 기준으로 보는 것이 직관적이다.
    if (unit === 'percent' || unit === 'per_sec' || unit === 'count') {
      vMin = Math.min(vMin, 0);
    }

    const scale = niceTicks(vMin, vMax, 4);
    x = (t) => padL + ((t - tMin) / tSpan) * plotW;
    y = (v) => padT + plotH - ((v - scale.min) / (scale.max - scale.min || 1)) * plotH;

    // 가로 격자 + Y축 라벨 (값 축이므로 밀지 않는다)
    for (const tick of scale.ticks) {
      const yy = y(tick);
      if (yy < padT - 1 || yy > padT + plotH + 1) continue;
      axisLayer.appendChild(el('line', {
        x1: padL, y1: yy, x2: W - padR, y2: yy, class: 'chart-grid',
      }));
      const text = el('text', {
        x: padL - 6, y: yy + 3, class: 'chart-axis-label', 'text-anchor': 'end',
      });
      text.textContent = formatMetricValue(tick, unit);
      axisLayer.appendChild(text);
    }

    // 임계선도 값 축이다.
    if (threshold !== null && Number.isFinite(threshold)) {
      const ty = y(threshold);
      if (ty >= padT && ty <= padT + plotH) {
        axisLayer.appendChild(el('line', {
          x1: padL, y1: ty, x2: W - padR, y2: ty,
          class: 'chart-threshold', 'vector-effect': 'non-scaling-stroke',
        }));
      }
    }

    // min-max 밴드: 버킷 집계로 뭉개진 변동 폭을 보여준다.
    // 평균만 보면 순간 스파이크가 사라져 "문제 없었다"고 오해하게 된다.
    if (showBand && points.some((p) => p.max > p.min)) {
      const top = points.map((p, i) => `${x(times[i])},${y(p.max)}`);
      const bottom = points.map((p, i) => `${x(times[i])},${y(p.min)}`).reverse();
      plotLayer.appendChild(el('polygon', {
        points: [...top, ...bottom].join(' '),
        class: 'chart-band',
      }));
    }

    // 영역 + 선
    const linePoints = points.map((p, i) => `${x(times[i])},${y(p.avg)}`);
    plotLayer.appendChild(el('polygon', {
      points: [
        `${padL},${padT + plotH}`,
        ...linePoints,
        `${x(times[times.length - 1])},${padT + plotH}`,
      ].join(' '),
      class: 'chart-area',
    }));
    plotLayer.appendChild(el('polyline', {
      points: linePoints.join(' '),
      class: 'chart-line',
      'vector-effect': 'non-scaling-stroke',
    }));

    // X축 라벨 (시작/중간/끝) — 시간 축이므로 함께 민다.
    const xLabels = [0, Math.floor((points.length - 1) / 2), points.length - 1];
    const seen = new Set();
    for (const i of xLabels) {
      if (i < 0 || seen.has(i)) continue;
      seen.add(i);
      const t = new Date(points[i].ts);
      const text = el('text', {
        x: x(t.getTime()), y: H - 6, class: 'chart-axis-label',
        'text-anchor': i === 0 ? 'start' : i === points.length - 1 ? 'end' : 'middle',
      });
      text.textContent = formatClock(t);
      plotLayer.appendChild(text);
    }
  }

  // 호버 표시
  const hoverLine = el('line', { class: 'chart-hover-line', y1: padT, y2: padT + plotH, style: 'display:none' });
  const hoverDot = el('circle', { r: 3.5, class: 'chart-hover-dot', style: 'display:none' });
  svg.appendChild(hoverLine);
  svg.appendChild(hoverDot);

  const tooltip = document.createElement('div');
  tooltip.className = 'chart-tooltip';
  tooltip.style.display = 'none';

  // 빈 구간 안내. 갱신으로 데이터가 들어오면 사라져야 하므로 차트 위에 겹쳐 둔다.
  const emptyNote = document.createElement('div');
  emptyNote.className = 'chart-empty';
  emptyNote.textContent = '데이터 없음';

  // 마우스 X를 뷰박스 좌표로 환산해 가장 가까운 점을 찾는다.
  //
  // 표시는 점 단위로만 바뀌므로, 같은 점 위에서 움직이는 동안에는 아무것도 하지
  // 않는다. 매 mousemove마다 툴팁 폭을 재면 강제 레이아웃이 반복된다.
  let shown = -1;
  const onMove = (e) => {
    const rect = svg.getBoundingClientRect();
    if (rect.width === 0) return;
    const vx = ((e.clientX - rect.left) / rect.width) * W;
    let best = 0;
    let bestDist = Infinity;
    for (let i = 0; i < points.length; i += 1) {
      const d = Math.abs(x(times[i]) - vx);
      if (d < bestDist) {
        bestDist = d;
        best = i;
      }
    }
    if (best === shown) return;
    shown = best;

    const p = points[best];
    const px = x(times[best]);
    const py = y(p.avg);

    hoverLine.setAttribute('x1', px);
    hoverLine.setAttribute('x2', px);
    hoverLine.style.display = '';
    hoverDot.setAttribute('cx', px);
    hoverDot.setAttribute('cy', py);
    hoverDot.style.display = '';

    const parts = [formatClockFull(new Date(p.ts)), `${label}: ${formatMetricValue(p.avg, unit)}`];
    if (p.max > p.min) {
      parts.push(`범위 ${formatMetricValue(p.min, unit)} ~ ${formatMetricValue(p.max, unit)}`);
    }
    tooltip.textContent = parts.join(' · ');
    tooltip.style.display = '';

    // 툴팁 위치는 뷰박스 비율이 아니라 실제 픽셀로 정하고, 차트 폭 안으로 가둔다.
    //
    // 비율(%)로만 잡으면 툴팁 자신의 폭이 계산에 들어가지 않는다. 오른쪽 절반에서
    // 툴팁이 카드 밖으로 삐져나오면 페이지에 가로 스크롤바가 생기고, 스크롤바가
    // 생기는 순간 본문 폭이 줄어 화면 전체가 다시 흔들린다(그리드가 다시 접힌다).
    tooltip.style.right = 'auto';
    const tipW = tooltip.offsetWidth;
    const cssX = (px / W) * rect.width;
    const maxLeft = Math.max(0, rect.width - tipW);
    tooltip.style.left = `${Math.min(maxLeft, Math.max(0, cssX - tipW / 2))}px`;
  };
  const onLeave = () => {
    shown = -1;
    hoverLine.style.display = 'none';
    hoverDot.style.display = 'none';
    tooltip.style.display = 'none';
  };
  svg.addEventListener('mousemove', onMove);
  svg.addEventListener('mouseleave', onLeave);

  wrap.style.height = `${height}px`;
  wrap.appendChild(svg);
  wrap.appendChild(emptyNote);
  wrap.appendChild(tooltip);

  /**
   * updateSeries는 같은 차트에 새 구간을 그린다.
   *
   * 시간 창이 오른쪽으로 밀린 만큼 그림을 오른쪽에서 시작해 제자리로 되돌린다.
   * 그러면 새 점이 오른쪽 끝에서 들어오고 옛 점이 왼쪽으로 흘러나가는 것처럼 보인다 —
   * 값이 순간이동하면 "언제 것인지" 감각이 끊긴다.
   *
   * Element.animate를 쓰는 이유: transform을 직접 걸었다가 되돌리는 방식은 애니메이션이
   * 돌지 않는 환경(프레임이 멈춘 배경 탭 등)에서 차트가 밀린 채로 남는다.
   * 애니메이션은 끝나면 흔적을 남기지 않으므로 안 돌아도 제자리다.
   */
  wrap.updateSeries = (next) => {
    const prevMin = tMin;
    const prevSpan = tSpan;
    const had = points.length > 0;
    draw(next);
    onLeave(); // 커서 아래 점이 바뀌었으므로 표시를 지운다

    if (!had || !points.length || typeof plotLayer.animate !== 'function') return;
    const shift = ((tMin - prevMin) / prevSpan) * plotW;
    if (!Number.isFinite(shift) || Math.abs(shift) < 0.5) return;
    plotLayer.animate(
      [{ transform: `translateX(${shift}px)` }, { transform: 'translateX(0)' }],
      { duration: 450, easing: 'ease-out' },
    );
  };

  draw(opts.points ?? []);
  return wrap;
}

// sparkline은 값만 보여주는 초소형 차트다. 카드 안에 넣는다.
export function sparkline(points, { width = 120, height = 28 } = {}) {
  const wrap = document.createElement('span');
  wrap.className = 'sparkline';
  if (!points || points.length < 2) return wrap;

  const values = points.map((p) => p.avg);
  const vMin = Math.min(...values, 0);
  const vMax = Math.max(...values);
  const span = vMax - vMin || 1;

  const svg = el('svg', {
    viewBox: `0 0 ${width} ${height}`,
    preserveAspectRatio: 'none',
    class: 'sparkline-svg',
  });
  const pts = values.map((v, i) => {
    const px = (i / (values.length - 1)) * width;
    const py = height - 2 - ((v - vMin) / span) * (height - 4);
    return `${px.toFixed(1)},${py.toFixed(1)}`;
  });
  svg.appendChild(el('polyline', {
    points: pts.join(' '), class: 'sparkline-line',
    'vector-effect': 'non-scaling-stroke',
  }));
  wrap.appendChild(svg);
  return wrap;
}
