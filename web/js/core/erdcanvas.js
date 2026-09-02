// ERD 캔버스: 스키마와 레이아웃을 SVG로 그리고, 이동·확대·선택을 다룬다.
//
// 편집기(협업 op-log)와 구조 화면(읽기 전용 + 개인 배치)이 같은 그림을 보여줘야
// 한다. 두 곳에 같은 코드를 두면 언젠가 갈라지고, 그러면 "설계 화면과 구조 화면의
// 같은 테이블이 다르게 보인다"가 된다 — 그 순간 둘 다 못 믿게 된다.
//
// 그래서 이 파일은 **그리는 일과 손으로 옮기는 일만** 안다. 옮긴 결과를 어디에
// 저장할지(서버 op / 개인 뷰)는 콜백으로 바깥이 정한다.
//
// 렌더링 전략: 구조가 바뀌면 통째로 다시 그린다. 부분 갱신은 변경 종류마다 다른
// 경로가 필요하고, 그 경로가 데이터와 어긋나면 화면마다 다른 그림이 나온다.
// 예외는 드래그 중 좌표뿐이다 — 왕복이나 재렌더를 기다리면 카드가 마우스를 못 따라온다.
import { h, mount, icon } from './dom.js';
import { columnIcon, chosenIconFor } from './colicon.js';
import { NAME_MODES, tableLabel, columnLabel } from './logical.js';
import { fitText, measure, cssFont, forgetFonts } from './textfit.js';
import { makeRouter, endMarker, endLabelSpot } from './erdroute.js';
import { cardinality, isJunction, junctionPartners } from './erdrel.js';

const SVG_NS = 'http://www.w3.org/2000/svg';

// 카드 치수. 서버의 자동 배치 격자(320×260)와 어긋나면 초기 화면에서 겹친다.
//
// CARD_W는 **기본값**이다. 카드마다 Box.w로 폭을 따로 정할 수 있다(오른쪽 아래
// 손잡이를 끌면 바뀐다) — 이름이 긴 표가 하나 있다고 모든 카드를 넓힐 이유는 없다.
export const CARD_W = 260;
export const CARD_MIN_W = 180;
export const CARD_MAX_W = 720;

// 카드 안쪽 여백. 이름이 시작하는 자리(26)와 오른쪽 끝(10), 그리고 이름과 타입
// 사이에 반드시 남길 틈(12)이다. 이 틈이 없으면 두 글자가 서로 닿아 한 낱말처럼 읽힌다.
const NAME_X = 26;
const PAD_R = 10;
const GAP = 12;
const HEAD_H = 30;
const ROW_H = 20;
const CARD_PAD = 8;

// 컬럼은 전부 그린다.
//
// 예전에는 14줄에서 끊고 "…N개 더"로 접었다. 카드 높이를 고르게 유지하려던 것인데,
// 도면을 보는 이유가 "이 표에 무엇이 있는가"라서 그 접힘은 정작 알고 싶은 것을
// 가렸다 — 열다섯 번째 컬럼을 확인하려면 속성 창을 따로 열어야 했고, 그러면 도면은
// 목차 노릇밖에 하지 않는다.
//
// 카드 하나가 길어지는 것은 카드를 접어 두는 것(collapsed)으로 사람이 정한다.

export class ErdCanvas {
  // wrap은 캔버스를 담을 요소다. 이 클래스가 그 안에 svg를 만든다.
  constructor(wrap, opts = {}) {
    this.wrap = wrap;
    this.opts = opts;
    this.doc = { schema: { tables: [] }, layout: {}, notes: [], groups: [] };
    this.marks = [];
    this.participants = [];
    this.youClientId = null;
    this.remoteCursors = new Map();
    this.view = { x: 0, y: 0, w: 1200, h: 800 };
    this.drag = null;
    // tool은 **빈 곳을 끌 때** 무엇을 할지다: 'pan'이면 화면 이동, 'select'면 범위 선택.
    // 기본은 pan이다 — 이 값을 바꾸지 않는 화면(구조)은 지금까지와 똑같이 움직인다.
    this.tool = 'pan';
    // hasToolPicker는 "이 화면에 마우스 도구 단추가 있는가"다.
    //
    // 없는 화면(구조)에서 tool은 그냥 기본값 'pan'일 뿐, 사람이 고른 것이 아니다.
    // 그 둘을 구분하지 않으면 도구를 고른 적도 없는 화면에서 카드가 움직이지 않게
    // 된다 — 구조 화면은 배치를 손으로 정리하는 곳이다.
    this.hasToolPicker = false;
    // spaceHeld는 스페이스바를 누르고 있는 동안 켜진다. 그동안은 도구와 무관하게
    // 화면 이동이다 — 그림 도구들의 공통 관례이고, 도구를 바꾸고 되돌리는 두 번의
    // 클릭보다 손이 덜 움직인다.
    this.spaceHeld = false;
    // nameMode는 카드에 어느 이름을 보일지다: 'physical' | 'logical' | 'both'.
    // 기본은 물리명 — 논리명을 적지 않은 문서가 지금까지와 똑같이 보인다.
    this.nameMode = 'physical';

    this.svg = document.createElementNS(SVG_NS, 'svg');
    this.svg.classList.add('erd-canvas');
    this.svg.setAttribute('preserveAspectRatio', 'xMidYMid meet');
    mount(wrap, this.svg);

    this.bind();
    this.watchFonts();
  }

  // watchFonts는 글꼴이 늦게 도착했을 때 다시 재고 다시 그린다.
  //
  // 왜 필요한가: 글자를 자를 자리는 **폭을 재서** 정한다(textfit). 그 잣대는 지금
  // 그려지는 글꼴이고, 웹폰트는 첫 그리기보다 늦게 도착하는 일이 흔하다. 그러면
  // 대체 글꼴의 폭으로 자른 자리가 그대로 남는다 — 글꼴이 바뀌었으니 그 자리는
  // 이제 틀렸고, 화면에는 "이유 없이 일찍 잘린 이름"이나 넘치는 글자가 남는다.
  //
  // D2Coding에서 특히 두드러진다. 한글을 라틴 두 칸 폭으로 그리므로, 시스템
  // 고정폭으로 잰 값과 크게 다르다.
  //
  // ready 와 loadingdone 을 함께 듣는 이유: ready 는 처음 요청한 글꼴이 끝나면
  // 한 번 풀리는데, 도면의 고정폭은 카드가 처음 그려질 때 비로소 요청된다 —
  // 그때는 ready 가 이미 풀린 뒤일 수 있다.
  watchFonts() {
    const fonts = document.fonts;
    if (!fonts) return;
    const refresh = () => {
      forgetFonts();
      if (this.svg?.isConnected) this.render();
    };
    fonts.ready?.then(refresh).catch(() => {});
    fonts.addEventListener?.('loadingdone', refresh);
  }

  // canEdit은 "지금 손으로 옮길 수 있는가"다. 구조 화면에서도 배치는 옮길 수
  // 있으므로 스키마 편집 권한과 다른 축이다.
  get canEdit() {
    return this.opts.canEdit ? this.opts.canEdit() !== false : true;
  }

  setDoc(doc) {
    this.doc = doc ?? { schema: { tables: [] }, layout: {}, notes: [], groups: [] };
    if (!this.doc.layout) this.doc.layout = {};
    if (!this.doc.notes) this.doc.notes = [];
    if (!this.doc.groups) this.doc.groups = [];
  }

  // 선택은 **목록**이다(marks). 여러 개를 함께 고르면 함께 옮기고, 바깥 화면이
  // 함께 지우거나 정렬할 수 있다.
  //
  // 마지막에 고른 것을 selection 으로 따로 보는 이유: 오른쪽 인스펙터는 언제나 하나를
  // 보여줘야 한다. 여러 개를 고른 상태에서 "지금 무엇을 편집 중인가"가 정해지지 않으면
  // 패널이 고를 때마다 다른 것을 보여주거나 아무것도 못 보여준다.
  get selection() {
    return this.marks.length ? this.marks[this.marks.length - 1] : null;
  }

  set selection(sel) {
    this.marks = sel ? [normalizeMark(sel)] : [];
  }

  // setSelection은 지금 고른 것을 정한다(하나만).
  //
  // 테이블 키(문자열)와 `{kind, id}` 둘 다 받는다. 구조 화면은 테이블만 고르므로
  // 예전처럼 문자열을 넘기고, 편집기는 간선·메모·그룹까지 고르므로 객체를 넘긴다.
  // 두 화면이 같은 캔버스를 쓰는 이상, 덜 쓰는 쪽에 더 많은 것을 요구하지 않는다.
  setSelection(sel) {
    this.selection = sel ?? null;
  }

  // setTool은 빈 곳 드래그의 뜻을 정한다.
  //
  // 도구를 캔버스가 기억하지 않고 화면이 정해 주는 이유: 어느 도구가 켜져 있는지를
  // 도구 막대가 보여줘야 하고, 그 선택은 사람마다 저장된다(localStorage).
  // setNameMode는 카드에 보일 이름을 바꾼다. 문서를 고치지 않는 **보기** 설정이다.
  setNameMode(mode) {
    this.nameMode = NAME_MODES.some((m) => m.value === mode) ? mode : 'physical';
  }

  setTool(tool) {
    // 이 함수를 부른다는 것은 화면에 도구 단추가 있다는 뜻이다.
    this.hasToolPicker = true;
    this.tool = tool === 'select' ? 'select' : 'pan';
    // 커서가 도구를 말해 주지 않으면, 끌어 보고서야 무엇이 일어나는지 알게 된다.
    this.svg.classList.toggle('is-select', this.tool === 'select');
    this.svg.classList.toggle('is-pan', this.tool === 'pan');
  }

  // setMarks는 화면 쪽이 들고 있는 선택 목록을 그대로 받는다.
  //
  // 선택의 주인이 화면인 이유: 다시 그릴 때마다 화면이 자기 상태를 캔버스에 밀어
  // 넣는다(renderCanvas). 캔버스만 목록을 들고 있으면 그 한 번의 밀어 넣기에
  // 다중 선택이 매번 사라진다.
  setMarks(list) {
    this.marks = (list ?? []).filter(Boolean).map(normalizeMark);
  }

  // selectedKey는 선택된 테이블 키다(테이블이 아니면 null).
  get selectedKey() {
    return this.selection?.kind === 'table' ? this.selection.id : null;
  }

  isSelected(kind, id) {
    return this.marks.some((m) => m.kind === kind && m.id === id);
  }

  // toggleMark는 하나를 목록에 더하거나 뺀다.
  toggleMark(kind, id) {
    // 관계선은 함께 고르지 않는다. 옮길 수도 지울 수도 없어서, 목록에 들어가면
    // "N개 선택됨"의 N만 늘고 함께 할 수 있는 일은 오히려 줄어든다.
    if (kind === 'link') {
      this.selection = { kind, id };
      return;
    }
    const at = this.marks.findIndex((m) => m.kind === kind && m.id === id);
    if (at >= 0) this.marks.splice(at, 1);
    else this.marks.push({ kind, id });
  }

  setParticipants(list, youClientId) {
    this.participants = list ?? [];
    this.youClientId = youClientId ?? null;
    // 떠난 사람의 커서를 남겨두면 유령 커서가 화면에 붙어 있는다.
    const alive = new Set(this.participants.map((p) => p.clientId));
    for (const id of [...this.remoteCursors.keys()]) {
      if (!alive.has(id)) this.remoteCursors.delete(id);
    }
  }

  setCursor(clientId, cursor) {
    // cursor가 없으면 지운다. 다른 시점으로 옮겨 간 사람의 커서를 남겨 두면 그
    // 자리에 유령이 붙어 있고, null을 그대로 넣으면 그리는 쪽에서 터진다.
    if (!cursor) this.remoteCursors.delete(clientId);
    else this.remoteCursors.set(clientId, cursor);
    this.renderCursors();
  }

  destroy() {
    this.unbind?.();
    // 화면을 떠나는 순간에도 예약된 프레임이 남아 있을 수 있다.
    // 그 콜백은 이미 버려진 레이어를 만진다.
    if (this.linkFrame) cancelAnimationFrame(this.linkFrame);
    this.linkFrame = 0;
  }

  // ---------- 그리기 ----------

  render() {
    // 가리키던 요소가 사라졌는데 설명만 남으면, 그 설명은 이제 아무것도 가리키지
    // 않는 거짓말이다.
    this.hideTip();
    const svg = this.svg;
    while (svg.firstChild) svg.removeChild(svg.firstChild);
    svg.setAttribute('viewBox', `${this.view.x} ${this.view.y} ${this.view.w} ${this.view.h}`);

    const layers = {
      // 그룹은 맨 뒤에 깔린다. 카드 위에 있으면 카드를 고를 수 없다.
      groups: svgEl('g', { class: 'erd-layer-groups' }),
      links: svgEl('g', { class: 'erd-layer-links' }),
      cards: svgEl('g', { class: 'erd-layer-cards' }),
      // 카드 위로 올려 그리는 관계선. 두 경우에 쓴다.
      //   - 돌아갈 길이 없어 카드를 지나가야 하는 선. 배경색 테두리를 깔아
      //     "덮고 지나간다"로 읽히게 한다 — 안 보이는 선보다는 낫다.
      //   - 고른 선, 고른 카드에 붙은 선, 마우스를 올린 선. 어디로 가는지
      //     따라가려는 순간에는 카드보다 선이 중요하다.
      linksTop: svgEl('g', { class: 'erd-layer-links-top' }),
      notes: svgEl('g', { class: 'erd-layer-notes' }),
      cursors: svgEl('g', { class: 'erd-layer-cursors' }),
      // 범위 선택 사각형은 맨 위에 그린다. 카드 밑에 깔리면 무엇을 훑고 있는지
      // 보이지 않는다.
      band: svgEl('g', { class: 'erd-layer-band' }),
    };
    svg.appendChild(layers.groups);
    svg.appendChild(layers.links);
    svg.appendChild(layers.notes);
    svg.appendChild(layers.cards);
    svg.appendChild(layers.linksTop);
    svg.appendChild(layers.cursors);
    svg.appendChild(layers.band);
    this.layers = layers;

    const boxes = this.boxes();
    for (const group of this.doc.groups ?? []) layers.groups.appendChild(this.groupEl(group));
    this.links(boxes);
    for (const note of this.doc.notes ?? []) layers.notes.appendChild(this.noteEl(note));
    for (const tbl of this.doc.schema?.tables ?? []) {
      const geom = boxes.get(tableKey(tbl));
      if (geom) layers.cards.appendChild(this.cardEl(tbl, geom));
    }
    this.renderCursors();

    if ((this.doc.schema?.tables ?? []).length === 0 && this.opts.emptyHint) {
      svg.appendChild(svgEl('text', {
        x: this.view.x + this.view.w / 2, y: this.view.y + this.view.h / 2,
        class: 'erd-hint', 'text-anchor': 'middle',
      }, this.opts.emptyHint));
    }
  }

  // boxes는 테이블 키 → 화면 기하 정보를 만든다.
  boxes() {
    const out = new Map();
    for (const tbl of this.doc.schema?.tables ?? []) {
      const key = tableKey(tbl);
      const layout = this.doc.layout?.[key] ?? { x: 80, y: 80 };
      const rows = layout.collapsed ? 0 : (tbl.columns?.length ?? 0);
      out.set(key, {
        x: layout.x, y: layout.y, w: cardWidth(layout),
        h: HEAD_H + rows * ROW_H + (layout.collapsed ? 0 : CARD_PAD),
        rows, layout, table: tbl,
      });
    }
    return out;
  }

  // routes는 관계선의 길을 찾아 캐시한다.
  //
  // 캐시하는 이유: 길찾기는 카드 기하만 보고 정해지는데, 다시 그리기는 고르기가
  // 바뀔 때마다 일어난다. 카드를 건드리지도 않았는데 매번 다시 찾을 이유가 없다.
  routes(boxes, quick = false) {
    let sig = `${quick ? 'q' : 'f'}|${this.nameMode ?? ''}`;
    for (const [key, g] of boxes) {
      sig += `|${key}:${round1(g.x)},${round1(g.y)},${round1(g.w)},${round1(g.h)}`;
    }
    if (this.routeCache?.sig === sig) return this.routeCache.map;

    const route = makeRouter(boxes, { quick });
    const map = new Map();
    for (const tbl of this.doc.schema?.tables ?? []) {
      const fromKey = tableKey(tbl);
      if (!boxes.has(fromKey)) continue;
      for (const fk of tbl.foreignKeys ?? []) {
        const toKey = refKey(tbl, fk);
        if (!boxes.has(toKey)) continue;
        const r = route(fromKey, toKey);
        if (r) {
          map.set(`${fromKey}.${fk.name}`, {
            ...r, fromKey, toKey, card: cardinality(tbl, fk),
          });
        }
      }
    }
    this.routeCache = { sig, map };
    return map;
  }

  // links는 외래키 관계선을 두 레이어에 그린다.
  links(boxes, quick = false) {
    const layer = this.layers?.links;
    if (!layer) return;
    while (layer.firstChild) layer.removeChild(layer.firstChild);

    const routed = this.routes(boxes, quick);
    this.linkSpots = new Map();
    for (const [fkID, r] of routed) {
      const selected = this.isSelected('link', fkID);
      const fkClass = `erd-link${selected ? ' is-selected' : ''}`;

      // 선을 누를 수 있게 만든다. 보이는 선은 1.4px이라 마우스로 맞히기 어려우므로,
      // 같은 경로를 투명한 굵은 선으로 한 겹 더 깔아 그것으로 받는다.
      // 보이는 선을 굵게 하면 그림이 지저분해진다.
      const hit = svgEl('path', { class: 'erd-link-hit', d: r.d, 'data-fk': fkID });
      hit.addEventListener('pointerdown', (e) => {
        e.stopPropagation();
        this.selection = { kind: 'link', id: fkID };
        this.opts.onSelectLink?.(fkID);
        this.render();
      });
      // 마우스를 올린 선은 카드 위로 올린다. 카드 밑으로 들어가 사라지는 선을
      // 따라가는 방법이 "카드를 옮겨 본다"뿐이면 그건 도구가 아니다.
      hit.addEventListener('pointerenter', () => this.setHoverLink(fkID));
      hit.addEventListener('pointerleave', () => this.setHoverLink(null));
      this.layers.links.appendChild(hit);

      this.layers.links.appendChild(svgEl('path', {
        class: fkClass, d: r.d, 'data-fk': fkID,
      }));
      // 양 끝 표식(까마귀발). 세 갈래는 여럿, 막대는 하나, 동그라미는 "없을
      // 수도 있다"다 — erdroute.endMarker 의 주석에 표기법을 적어 두었다.
      //
      // 끝 표식에도 data-fk 를 단다. 그림을 범위만 골라 내보낼 때 어느 선의
      // 조각인지 알아야 함께 빠진다 — 선만 빠지고 표식이 남으면 아무 데도 닿지
      // 않는 도형이 그림에 남는다.
      //
      // card 가 없는 경우(표를 못 찾은 선)에는 예전 모양인 N:1 로 그린다.
      // 아무 표식도 없는 끝은 "덜 그려진 그림"으로 보인다.
      for (const [at, side, spec] of endSpecs(r)) {
        const mark = endMarker(at, side, spec);
        this.layers.links.appendChild(svgEl('path', {
          class: 'erd-link-mark', 'data-fk': fkID, d: mark.d,
        }));
        if (!mark.ring) continue;
        this.layers.links.appendChild(svgEl('circle', {
          class: 'erd-link-ring', 'data-fk': fkID, ...mark.ring,
        }));
      }
      this.linkSpots.set(fkID, r);
    }
    this.syncLiftedLinks();
  }

  // liftedLinks는 지금 카드 위로 올려 그려야 하는 관계선들이다.
  liftedLinks() {
    const out = new Set();
    for (const [fkID, r] of this.linkSpots ?? []) {
      if (r.blocked) out.add(fkID);
      if (this.hoverLink === fkID) out.add(fkID);
      if (this.isSelected('link', fkID)) out.add(fkID);
      // 고른 카드에 붙은 선도 함께 올린다. 표 하나를 눌러 "이 표가 무엇과
      // 엮여 있나"를 보는 것이 관계선을 보는 가장 흔한 이유다.
      if (this.isSelected('table', r.fromKey) || this.isSelected('table', r.toKey)) out.add(fkID);
    }
    return out;
  }

  // syncLiftedLinks는 위 레이어만 다시 만든다. 마우스를 올릴 때마다 도면 전체를
  // 다시 그리면 큰 문서에서 손이 뻑뻑해진다.
  syncLiftedLinks() {
    const layer = this.layers?.linksTop;
    if (!layer) return;
    while (layer.firstChild) layer.removeChild(layer.firstChild);

    for (const fkID of this.liftedLinks()) {
      const r = this.linkSpots.get(fkID);
      if (!r) continue;
      // 잠깐 올린 선(마우스를 올렸거나 고른 것)은 **그림 파일에서 뺀다**.
      // 아래 레이어에 같은 선이 이미 있어서, 내보낸 그림에 두 번 그려진다.
      // 지날 데가 없어 올린 선(blocked)은 그것이 유일한 선이므로 남긴다.
      const temp = r.blocked ? '' : ' erd-link-temp';
      // 배경색 테두리를 먼저 깐다. 이것이 없으면 카드 글자 위에 선이 겹쳐
      // 둘 다 못 읽는다.
      layer.appendChild(svgEl('path', {
        class: `erd-link-halo${temp}`, 'data-fk': fkID, d: r.d,
      }));
      layer.appendChild(svgEl('path', {
        class: `erd-link is-lifted${r.blocked ? ' is-over' : ''}`
          + `${this.isSelected('link', fkID) ? ' is-selected' : ''}${temp}`,
        d: r.d, 'data-fk': fkID,
      }));

      // 개수 글자(N · 1 · 0..1)는 **가리킨 선에만** 붙인다.
      //
      // 기호(까마귀발)는 늘 있다. 거기에 글자까지 늘 붙이면 표 스무 개짜리 도면의
      // 끝점마다 기호와 글자가 겹쳐 쌓이고, 카드가 가까이 붙은 곳에서는 양쪽
      // 글자가 서로 부딪친다. 반대로 선에 손을 올린 순간은 "이 선이 무엇인가"가
      // 궁금한 순간이라, 그때는 배울 것 없는 글자가 기호보다 낫다.
      //
      // 표 하나를 고르면 그 표의 선이 모두 올라오므로, 한 표의 관계를 한 번에
      // 글자로 읽을 수 있다.
      if (!r.card || !this.pointedLink(fkID, r)) continue;
      for (const [at, side, spec, text] of endSpecs(r)) {
        const spot = endLabelSpot(at, side, endMarker(at, side, spec).reach + 9);
        layer.appendChild(svgEl('text', {
          // 언제나 erd-link-temp 다. 그림 파일에는 손을 올린 상태가 남아서는
          // 안 된다 — 내보낸 그림에 왜 이 선만 글자가 있는지 설명할 길이 없다.
          class: 'erd-link-card erd-link-temp', 'data-fk': fkID,
          x: spot.x, y: spot.y, 'text-anchor': spot.anchor,
        }, text));
      }
    }
  }

  // pointedLink는 지금 사람이 가리키고 있는 선인지다.
  //
  // 막혀서 올라온 선(blocked)은 여기에 들지 않는다. 그것은 사람이 가리킨 것이
  // 아니라 그릴 데가 없어 올라온 것이라, 글자까지 붙으면 그 선만 이유 없이
  // 시끄러워진다.
  pointedLink(fkID, r) {
    return this.hoverLink === fkID
      || this.isSelected('link', fkID)
      || this.isSelected('table', r.fromKey)
      || this.isSelected('table', r.toKey);
  }

  setHoverLink(fkID) {
    if (this.hoverLink === fkID) return;
    this.hoverLink = fkID;
    this.syncLiftedLinks();
  }

  cardEl(tbl, geom) {
    const key = tableKey(tbl);
    const selected = this.isSelected('table', key);
    // 폭 손잡이에 손이 닿았거나 지금 끌고 있으면 테두리 전체가 달라진다.
    //
    // 둘을 나누는 이유: 손이 닿은 것은 중립적으로 밝아지기만 하고, 끄는 동안에만
    // 강조색을 쓴다. 호버에 강조색을 쓰면 "고른 카드"(같은 색 테두리)와 구별되지
    // 않아, 지나가기만 해도 고른 것처럼 보인다.
    //
    // 다시 그려도 그대로여야 하므로(끄는 동안 카드를 매 프레임 새로 만든다)
    // 클래스를 여기서 붙인다.
    const resizing = this.drag?.mode === 'card-resize' && this.drag.key === key;
    const gripHover = !resizing && this.gripHover === key;
    const g = svgEl('g', {
      class: `erd-card-g${selected ? ' is-selected' : ''}`
        + `${this.isPrimary('table', key) ? ' is-primary' : ''}`
        + `${resizing ? ' is-resizing' : ''}${gripHover ? ' is-grip-hover' : ''}`,
      transform: `translate(${geom.x},${geom.y})`,
      'data-key': key,
      // 색은 **묶음(g)**에 싣는다. 안쪽 사각형에 실으면 그 사각형 자신에게만
      // 정의되어, 머리띠(erd-card-head)처럼 형제 요소는 값을 물려받지 못한다.
      // CSS 규칙도 g를 기준으로 쓰여 있어 아무 일도 일어나지 않았다.
      style: geom.layout.color ? `--card-accent:${geom.layout.color}` : null,
    });

    // 다른 참여자가 선택 중인 테이블은 그 사람의 색으로 테두리를 표시한다.
    const holder = this.participants.find(
      (p) => p.selection === key && p.clientId !== this.youClientId);

    g.appendChild(svgEl('rect', {
      class: 'erd-card-bg', width: geom.w, height: geom.h, rx: 6,
    }));
    g.appendChild(svgEl('rect', { class: 'erd-card-head', width: geom.w, height: HEAD_H, rx: 6 }));

    // 아이콘이 지정되어 있으면 이름 왼쪽에 붙이고 제목을 그만큼 민다.
    const iconName = geom.layout.icon;
    let titleX = 10;
    if (iconName) {
      const mark = icon(iconName, 13);
      mark.setAttribute('x', 9);
      mark.setAttribute('y', 9);
      mark.classList.add('erd-card-icon');
      g.appendChild(mark);
      titleX = 28;
    }
    const { main, sub } = tableLabel(tbl, geom.layout, this.nameMode);
    // 제목 자리를 덮는 투명한 판. 글자 위에만 손을 올려야 뜨면, 짧은 이름에서는
    // 겨냥할 곳이 몇 픽셀뿐이다.
    const headHit = svgEl('rect', {
      class: 'erd-hit', x: 0, y: 0, width: geom.w, height: HEAD_H,
    });
    // 논리명일 때는 조금 작게 쓴다. 같은 크기로 두면 한글이 라틴 문자보다 크게
    // 보여서(글자당 차지하는 면적이 넓다) 제목만 유독 큼직하게 튄다.
    const isLogical = main !== (tbl.namespace ? `${tbl.namespace}.${tbl.name}` : tbl.name);
    // 제목이 오른쪽 컬럼 수 위로 올라가지 않게 그 자리를 빼고 맞춘다.
    const titleMax = geom.w - titleX - PAD_R - 22;
    g.appendChild(svgEl('text', {
      class: `erd-card-name${isLogical ? ' is-logical' : ''}`,
      x: titleX, y: sub ? 17 : 20,
    }, fitText(main, titleMax, cssFont(`erd-card-name${isLogical ? ' is-logical' : ''}`))));
    // 연결 표(N:N)임을 제목 줄에 적는다.
    //
    // 두 표 사이에 N:N 선을 그리지 않는 이유: 그런 외래키는 없다. N:N 은 이 표를
    // 두는 **패턴**이고, 그 표에서 나가는 외래키는 각각 N:1 이다. 없는 선을 그리면
    // 도면이 DDL과 달라진다 — 이 앱에서 도면은 DDL의 그림이다.
    //
    // 대신 그 사실을 이 표에 적는다. 읽는 사람이 알고 싶은 것은 "이 표가 A와 B를
    // N:N 으로 잇는다"이고, 그것은 이 표의 성질이다.
    if (isJunction(tbl)) {
      g.appendChild(svgEl('text', {
        class: 'erd-card-nn', x: geom.w - PAD_R - 14, y: sub ? 17 : 20,
        'text-anchor': 'end',
      }, 'N:N'));
    }
    // 제목의 주석·잘린 이름을 손 올렸을 때 보여 준다.
    this.bindTip(headHit, () => tipForTable(tbl, geom.layout, main, titleMax));
    g.appendChild(headHit);
    // 둘 다 보기에서는 물리명을 작은 글씨로 아래에 둔다. 나란히 쓰면 어느 쪽이
    // 진짜 이름인지 알 수 없고, 긴 한국어 이름에서 물리명이 먼저 잘린다.
    if (sub) {
      g.appendChild(svgEl('text', {
        class: 'erd-card-sub', x: titleX, y: 28,
      }, fitText(sub, titleMax, cssFont('erd-card-sub'))));
    }

    const count = tbl.columns?.length ?? 0;
    g.appendChild(svgEl('text', {
      class: 'erd-card-count', x: geom.w - 10, y: 20, 'text-anchor': 'end',
    }, `${count}`));

    if (!geom.layout.collapsed) {
      const cols = tbl.columns ?? [];
      cols.forEach((col, i) => {
        const y = HEAD_H + i * ROW_H + 14;
        const isPK = (tbl.primaryKey?.columns ?? []).some((c) => eqName(c, col.name));
        const isFK = (tbl.foreignKeys ?? []).some((fk) =>
          (fk.columns ?? []).some((c) => eqName(c, col.name)));
        // 아이콘은 ●/◆ 를 대신한다. 표식이 붙던 컬럼만 달라 보이던 것과 달리
        // 모든 줄이 같은 자리에서 시작하므로, 이름은 아이콘이 없을 때도 밀어 둔다.
        const ic = columnIcon(col, { isPK, isFK }, chosenIconFor(geom.layout, col.name));
        if (ic) {
          const mark = icon(ic, 12);
          mark.setAttribute('x', 9);
          mark.setAttribute('y', y - 10);
          mark.classList.add('erd-col-icon');
          if (isPK) mark.classList.add('is-pk');
          else if (isFK) mark.classList.add('is-fk');
          g.appendChild(mark);
        }
        const label = columnLabel(col, geom.layout, this.nameMode);
        const nameText = svgEl('text', {
          class: `erd-col${isPK ? ' is-pk' : ''}`, x: NAME_X, y,
        });

        // 자리를 나눈다: 타입이 먼저 자기 폭을 가져가고, 남는 것이 이름 몫이다.
        //
        // 글자 수로 자르면(예전) 한글에서 넘친다 — 한글 한 자는 라틴 한 자보다
        // 두 배 가까이 넓어서, 같은 "20자"가 영문에서는 남고 한국어에서는 타입
        // 위로 올라탔다. 논리명과 물리명을 함께 보여 줄 때는 늘 겹쳤다.
        const domain = (col.domain ?? '').trim();
        const rawType = domain || (col.rawType || col.type?.base || '');
        const typeFont = cssFont(`erd-col-type${domain ? ' is-domain' : ''}`);
        // 타입은 카드의 절반을 넘지 않는다. 이름이 무엇인지 모르게 되면 도면이
        // 아니라 타입 목록이 된다.
        const typeStr = fitText(rawType, (geom.w - NAME_X - PAD_R) * 0.5, typeFont)
          + (col.nullable ? '' : ' *');
        const room = geom.w - NAME_X - PAD_R - measure(typeStr, typeFont) - GAP;

        const nameFont = cssFont(`erd-col${isPK ? ' is-pk' : ''}`);
        if (label.sub) {
          // 논리명과 물리명을 함께 보여 줄 때: 남는 폭을 6:4로 나눈다. 물리명은
          // 작은 글씨라 그 비율에서 대개 다 들어간다.
          const subFont = cssFont('erd-col-sub');
          const subStr = fitText(label.sub, room * 0.4, subFont);
          const mainRoom = room - measure(subStr, subFont) - 6;
          nameText.appendChild(svgEl('tspan', {}, fitText(label.main, mainRoom, nameFont)));
          // 같은 <text> 안의 tspan 으로 잇는다. x를 따로 주면 한글에서 어긋난다.
          nameText.appendChild(svgEl('tspan', { class: 'erd-col-sub', dx: 6 }, subStr));
        } else {
          nameText.appendChild(svgEl('tspan', {}, fitText(label.main, room, nameFont)));
        }
        g.appendChild(nameText);
        // 도메인이 걸린 컬럼은 도메인 이름을 보여준다.
        //
        // 도메인을 쓴다는 것은 "이 컬럼은 이메일이다"라고 말한 것이고, 도면에서
        // 알고 싶은 것도 그것이다 — VARCHAR(255) 는 그 이메일이 지금 어떻게
        // 구현돼 있는가일 뿐이고, 그 값은 속성 창에 그대로 있다. 도메인을 정리해
        // 놓고도 도면에는 원시 타입만 보이면, 도메인은 아무도 보지 않는 목록이 된다.
        //
        // 실제 타입과 헷갈리지 않게 다른 색으로 그린다(erd-col-domain).
        g.appendChild(svgEl('text', {
          class: `erd-col-type${domain ? ' is-domain' : ''}`,
          x: geom.w - PAD_R, y, 'text-anchor': 'end',
        }, typeStr));

        // 줄 전체를 덮는 투명한 판. 이름 글자에만 손을 올려야 뜨면 짧은 컬럼에서
        // 겨냥할 곳이 거의 없고, 잘려서 "…"이 된 이름은 더 짧아진다.
        const rowHit = svgEl('rect', {
          class: 'erd-hit', x: 0, y: y - ROW_H + 5, width: geom.w, height: ROW_H,
        });
        this.bindTip(rowHit, () => tipForColumn(col, geom.layout, label, rawType, domain, room));
        g.appendChild(rowHit);
      });
    }

    // 테두리는 **맨 위에** 얹는다.
    //
    // 카드 배경(erd-card-bg)에 테두리를 그리면 그 위에 덮이는 머리띠(erd-card-head)가
    // 테두리의 안쪽 절반을 가린다. 선은 가장자리를 가운데 두고 그려지기 때문이다.
    // 그래서 고른 카드의 하이라이트가 제목 줄에서만 얇아 보였다.
    //
    // 언제나 그려 두고 색은 CSS가 정한다(고르지 않았으면 stroke:none). 고른 순간에만
    // 요소를 만들면, 그림으로 내보낼 때 클래스를 떼어 원래 모습을 계산하는 방법이
    // 통하지 않는다 — 그 요소는 여전히 남아 색이 박힌다.
    g.appendChild(svgEl('rect', {
      class: 'erd-card-outline', width: geom.w, height: geom.h, rx: 6,
    }));
    if (holder) {
      // 다른 참여자가 고른 표시도 같은 이유로 맨 위다.
      g.appendChild(svgEl('rect', {
        class: 'erd-card-holder', width: geom.w, height: geom.h, rx: 6,
        stroke: holder.color,
      }));
    }

    // 폭 손잡이. 좌우 가장자리를 잡아 끈다.
    //
    // 그룹·메모는 모서리에서 가로세로를 함께 잡지만 카드는 높이가 컬럼 수로 정해진다.
    // 모서리에 두면 세로도 바뀔 것처럼 보이고, 끌어 본 사람은 높이가 안 변하는 것을
    // 고장으로 읽는다. 좌우 변이 "여기서 폭만 바뀐다"를 말해 준다.
    //
    // 왼쪽에도 두는 이유: 카드가 화면 오른쪽에 있으면 오른쪽 변을 잡으려고 화면을
    // 먼저 밀어야 한다. 그리고 왼쪽으로 넓히는 것과 오른쪽으로 넓히는 것은 결과가
    // 다르다 — 왼쪽 변을 끌면 오른쪽 변이 제자리에 있으므로, 오른쪽 이웃과의
    // 간격을 지키면서 넓힐 수 있다.
    if (this.canEdit && !geom.layout.collapsed) {
      const gripH = Math.max(16, geom.h - HEAD_H);
      for (const side of ['left', 'right']) {
        const grip = svgEl('rect', {
          class: `erd-card-grip is-${side}`,
          x: side === 'right' ? geom.w - 4 : -4,
          y: HEAD_H, width: 8, height: gripH,
        });
        grip.addEventListener('pointerdown', (e) => this.onCardResizeDown(e, key, geom, side));
        grip.addEventListener('pointerenter', () => this.setGripHover(key));
        grip.addEventListener('pointerleave', () => this.setGripHover(null));
        g.appendChild(grip);
      }
    }

    // 끄는 동안에는 폭을 숫자로도 적는다. 두 카드를 같은 폭으로 맞추려는 사람에게
    // 글자가 어디까지 보이는지만으로는 부족하다. 카드 밖 오른쪽 아래에 두어
    // 내용을 가리지 않는다.
    if (this.drag?.mode === 'card-resize' && this.drag.key === key) {
      g.appendChild(svgEl('text', {
        class: 'erd-card-wnote', x: geom.w - 2, y: geom.h + 13, 'text-anchor': 'end',
      }, `${Math.round(geom.w)}px`));
    }

    g.addEventListener('pointerdown', (e) => {
      if (e.target.classList.contains('erd-card-grip')) return;
      this.onCardPointerDown(e, key, geom);
    });
    g.addEventListener('dblclick', () => {
      this.opts.onToggleCollapse?.(key, geom);
    });
    return g;
  }

  // groupEl은 테이블 묶음을 감싸는 반투명 사각형이다.
  //
  // 크기를 자동으로 맞추지 않는 이유: 어느 테이블이 이 묶음인지 데이터로 정하지
  // 않았기 때문이다. 사람이 눈으로 묶고 손으로 크기를 정하는 편이 규칙이 없어 단순하다.
  groupEl(group) {
    const g = svgEl('g', {
      class: `erd-group-g${this.isSelected('group', group.id) ? ' is-selected' : ''}`,
      transform: `translate(${group.x},${group.y})`,
      'data-group': group.id,
      style: group.color ? `--group-accent:${group.color}` : null,
    });
    g.appendChild(svgEl('rect', {
      class: 'erd-group-bg', width: group.w || 320, height: group.h || 240, rx: 10,
    }));
    if (group.label) {
      g.appendChild(svgEl('text', { class: 'erd-group-label', x: 12, y: 22 }, group.label));
    }
    if (this.canEdit) {
      const grip = svgEl('rect', {
        class: 'erd-group-grip', x: (group.w || 320) - 16, y: (group.h || 240) - 16,
        width: 12, height: 12, rx: 3,
      });
      grip.addEventListener('pointerdown', (e) => this.onGroupPointerDown(e, group, 'resize'));
      g.appendChild(grip);
    }
    g.addEventListener('pointerdown', (e) => {
      if (e.target.classList.contains('erd-group-grip')) return;
      this.onGroupPointerDown(e, group, 'move');
    });
    g.addEventListener('dblclick', () => this.opts.onEditGroup?.(group));
    return g;
  }

  noteEl(note) {
    const g = svgEl('g', {
      class: `erd-note-g${this.isSelected('note', note.id) ? ' is-selected' : ''}`,
      transform: `translate(${note.x},${note.y})`,
      'data-note': note.id,
      style: note.color ? `--note-accent:${note.color}` : null,
    });
    // 크기는 사람이 정한 값이 있으면 그것을 쓰고, 없으면 글에 맞춘다.
    // 글자 폭은 대략 7px이라, 상자 폭에서 여백을 뺀 만큼이 한 줄에 들어간다.
    const width = note.w || NOTE_W;
    const perLine = Math.max(8, Math.floor((width - 16) / 7));
    const lines = wrapText(note.text || '(빈 메모)', perLine);
    const height = note.h || (16 + lines.length * 16);
    g.appendChild(svgEl('rect', {
      class: 'erd-note-bg', width, height, rx: 4,
    }));
    lines.forEach((line, i) => {
      g.appendChild(svgEl('text', { class: 'erd-note-text', x: 8, y: 18 + i * 16 }, line));
    });
    // 메모도 그룹처럼 손으로 크기를 정한다. 자동으로만 늘어나면 긴 메모가
    // 캔버스를 가로지르고, 그것을 줄일 방법이 없다.
    if (this.canEdit) {
      const grip = svgEl('rect', {
        class: 'erd-note-grip', x: width - 12, y: height - 12,
        width: 9, height: 9, rx: 2,
      });
      grip.addEventListener('pointerdown', (e) => this.onNotePointerDown(e, note, 'resize'));
      g.appendChild(grip);
    }
    g.addEventListener('pointerdown', (e) => this.onNotePointerDown(e, note));
    g.addEventListener('dblclick', () => this.opts.onEditNote?.(note));
    return g;
  }

  renderCursors() {
    if (!this.layers) return;
    const layer = this.layers.cursors;
    while (layer.firstChild) layer.removeChild(layer.firstChild);
    for (const [, c] of this.remoteCursors) {
      const g = svgEl('g', { class: 'erd-cursor', transform: `translate(${c.x},${c.y})` });
      g.appendChild(svgEl('path', {
        d: 'M0,0 L0,14 L4,10 L7,16 L10,14 L7,8 L12,8 Z', fill: c.color,
      }));
      if (c.name) {
        g.appendChild(svgEl('rect', {
          class: 'erd-cursor-label-bg', x: 12, y: 2, width: c.name.length * 8 + 10, height: 16, rx: 3,
          fill: c.color,
        }));
        g.appendChild(svgEl('text', { class: 'erd-cursor-label', x: 17, y: 14 }, c.name));
      }
      layer.appendChild(g);
    }
  }

  // ---------- 상호작용 ----------

  bind() {
    const svg = this.svg;

    const onWheel = (e) => {
      e.preventDefault();
      // 화면이 움직이면 설명이 가리키던 자리도 움직인다. 제자리에 남은 설명은
      // 엉뚱한 것을 가리킨다.
      this.hideTip();
      const p = this.toCanvas(e.clientX, e.clientY);
      this.zoomAt(e.deltaY > 0 ? 1.12 : 0.89, p);
    };
    const onPointerDown = (e) => {
      // 오른쪽 버튼은 우리 것이 아니다(브라우저 메뉴). 잡으면 메뉴가 뜬 채로
      // 드래그가 시작된 상태가 남는다.
      if (e.button === 2) return;
      if (e.target.closest('.erd-card-g') || e.target.closest('.erd-note-g')) return;
      // 빈 곳을 끌면 무엇이 되는가 — 지금 켜진 도구가 정한다.
      //
      //            plain      Shift        Ctrl(⌘)     가운데 버튼
      //   pan      화면 이동   범위(더하기)  범위(새로)   화면 이동
      //   select   범위(새로)  범위(더하기)  화면 이동    화면 이동
      //
      // 가운데 버튼이 언제나 화면 이동인 이유: 도구를 바꾸지 않고 잠깐 옮겨 보는
      // 일이 가장 잦다. 이 캔버스에는 스크롤바가 없어서 끌기 말고는 화면을 옮길
      // 방법이 없으므로, 어떤 도구에서도 이동으로 가는 길이 하나는 열려 있어야 한다.
      const middle = e.button === 1;
      const wantBand = this.canPick && !middle && !this.spaceHeld && (this.tool === 'select'
        ? !(e.ctrlKey || e.metaKey)
        : (e.shiftKey || e.ctrlKey || e.metaKey));
      if (wantBand) {
        const at = this.toCanvas(e.clientX, e.clientY);
        this.drag = { mode: 'band', start: at, keep: e.shiftKey ? this.marks.slice() : [] };
        if (!e.shiftKey && this.marks.length) {
          this.marks = [];
          this.render();
        }
        this.drawBand(at, at);
        svg.setPointerCapture?.(e.pointerId);
        return;
      }
      // 빈 곳을 끌면 화면 이동. 빈 곳을 눌렀으니 선택도 놓는다 — 스페이스바로
      // 옮기는 중이라면 놓지 않는다(보는 자리를 바꾸는 일이지 고른 것을 놓는 일이
      // 아니다. 골라 둔 열 장을 옮겨 보려고 화면을 밀었더니 선택이 풀리면, 그 뒤의
      // 정렬·복제를 다시 골라야 한다).
      this.startPan(e, { clearSelection: !this.spaceHeld });
    };
    const onPointerMove = (e) => {
      const point = this.toCanvas(e.clientX, e.clientY);
      this.opts.onCursorMove?.({ x: round1(point.x), y: round1(point.y) });

      if (!this.drag) return;
      if (this.drag.mode === 'band') {
        this.drag.end = point;
        this.drawBand(this.drag.start, point);
        return;
      }
      if (this.drag.mode === 'multi') {
        const dx = round1(point.x - this.drag.start.x);
        const dy = round1(point.y - this.drag.start.y);
        this.drag.delta = { x: dx, y: dy };
        for (const it of this.drag.items) {
          const el = this.svg.querySelector(it.selector);
          el?.setAttribute('transform', `translate(${it.x + dx},${it.y + dy})`);
          // 관계선은 카드 좌표에서 계산되므로 레이아웃도 함께 옮겨 둔다.
          if (it.kind !== 'table') continue;
          this.doc.layout[it.id] = { ...(this.doc.layout[it.id] ?? {}), x: it.x + dx, y: it.y + dy };
        }
        this.refreshLinks();
        return;
      }
      if (this.drag.mode === 'pan') {
        const scale = this.view.w / svg.clientWidth;
        this.view.x = this.drag.view.x - (e.clientX - this.drag.startClient.x) * scale;
        this.view.y = this.drag.view.y - (e.clientY - this.drag.startClient.y) * scale;
        this.applyViewBox();
        return;
      }
      if (this.drag.mode === 'note-resize') {
        const w = Math.max(120, round1(this.drag.size.w + (point.x - this.drag.start.x)));
        const hh = Math.max(60, round1(this.drag.size.h + (point.y - this.drag.start.y)));
        this.drag.lastSize = { w, h: hh };
        const el = this.dragEl();
        if (!el) return;
        const rect = el.querySelector('.erd-note-bg');
        const grip = el.querySelector('.erd-note-grip');
        if (rect) {
          rect.setAttribute('width', w);
          rect.setAttribute('height', hh);
        }
        if (grip) {
          grip.setAttribute('x', w - 12);
          grip.setAttribute('y', hh - 12);
        }
        return;
      }
      if (this.drag.mode === 'card-resize') {
        const d = this.drag;
        const moved = point.x - d.start.x;
        // 왼쪽 변은 끄는 방향과 폭의 방향이 반대다(오른쪽으로 끌면 좁아진다).
        const raw = d.side === 'left' ? d.width - moved : d.width + moved;
        const w = Math.max(CARD_MIN_W, Math.min(CARD_MAX_W, round1(raw)));
        // 상한·하한에 닿아도 오른쪽 변은 제자리를 지켜야 한다. x를 끌던 자리로
        // 계산하면 폭이 멈춘 뒤에도 카드가 계속 미끄러진다.
        const x = d.side === 'left' ? round1(d.right - w) : d.x;
        this.liveResize(d.key, w, x);
        return;
      }
      if (this.drag.mode === 'group-resize') {
        const w = Math.max(80, round1(this.drag.size.w + (point.x - this.drag.start.x)));
        const hh = Math.max(60, round1(this.drag.size.h + (point.y - this.drag.start.y)));
        this.drag.lastSize = { w, h: hh };
        const el = this.dragEl();
        if (!el) return;
        const rect = el.querySelector('.erd-group-bg');
        const grip = el.querySelector('.erd-group-grip');
        if (rect) {
          rect.setAttribute('width', w);
          rect.setAttribute('height', hh);
        }
        if (grip) {
          grip.setAttribute('x', w - 16);
          grip.setAttribute('y', hh - 16);
        }
        return;
      }
      if (this.drag.mode === 'card' || this.drag.mode === 'note' || this.drag.mode === 'group') {
        const nx = round1(point.x - this.drag.grab.x);
        const ny = round1(point.y - this.drag.grab.y);
        this.drag.last = { x: nx, y: ny };
        // 드래그 중에는 저장을 기다리지 않고 transform만 바꾼다.
        this.dragEl()?.setAttribute('transform', `translate(${nx},${ny})`);
        // 관계선은 두 카드의 위치에서 계산되므로 카드만 옮기면 선이 제자리에 남는다.
        // 카드가 선에서 떨어져 나온 그림은 "연결이 끊겼나"로 읽힌다.
        if (this.drag.mode === 'card') this.moveLinks(this.drag.key, nx, ny);
      }
    };
    const onPointerUp = () => {
      const drag = this.drag;
      this.drag = null;
      if (drag?.mode === 'band') {
        this.clearBand();
        const merged = drag.keep.slice();
        for (const m of this.bandHits(drag.start, drag.end ?? drag.start)) {
          if (!merged.some((x) => x.kind === m.kind && x.id === m.id)) merged.push(m);
        }
        this.marks = merged;
        this.render();
        this.opts.onMarks?.(this.marks.slice());
        return;
      }
      if (drag?.mode === 'multi') {
        // 움직이지 않았으면 아무것도 보내지 않는다. 고른 것을 확인하려고 한 번
        // 누른 것까지 편집으로 남으면 되돌리기 스택이 제자리 이동으로 채워진다.
        if (!drag.delta || (!drag.delta.x && !drag.delta.y)) return;
        const moves = [];
        for (const it of drag.items) {
          const x = round1(it.x + drag.delta.x);
          const y = round1(it.y + drag.delta.y);
          this.placeMark(it, x, y);
          moves.push({ kind: it.kind, id: it.id, x, y });
        }
        this.opts.onMultiMove?.(moves);
        this.render();
        return;
      }
      if (drag?.mode === 'note-resize' && drag.lastSize) {
        const note = (this.doc.notes ?? []).find((n) => n.id === drag.id);
        if (note) {
          note.w = drag.lastSize.w;
          note.h = drag.lastSize.h;
        }
        this.opts.onNoteResize?.(drag.id, drag.lastSize.w, drag.lastSize.h);
        this.render();
        return;
      }
      if (drag?.mode === 'card-resize' && drag.lastWidth) {
        // 로컬 상태는 끄는 동안 이미 갱신됐다(liveResize). 저장 결과가 오기 전에
        // 다시 그려도 카드가 원래 폭으로 튀지 않는다.
        //
        // 왼쪽을 끌었으면 좌표도 바뀌어 있다. 따로 넘기지 않는 이유: 화면 쪽은
        // 이미 같은 doc.layout 을 읽어 x·y 를 op 에 담는다(setDoc 이 같은 객체를
        // 넘긴다). 여기서 또 넘기면 같은 값을 두 갈래로 전하게 되고, 둘이 어긋나는
        // 날이 온다.
        this.opts.onTableResize?.(drag.key, drag.lastWidth);
        this.render();
        return;
      }
      if (drag?.mode === 'group-resize' && drag.lastSize) {
        const group = (this.doc.groups ?? []).find((x) => x.id === drag.id);
        if (group) {
          group.w = drag.lastSize.w;
          group.h = drag.lastSize.h;
        }
        this.opts.onGroupResize?.(drag.id, drag.lastSize.w, drag.lastSize.h);
        this.render();
        return;
      }
      if (!drag || !drag.last) return;
      if (drag.mode === 'card') {
        // 로컬 상태를 먼저 갱신해 두어, 저장 결과가 오기 전에 다시 그려도
        // 카드가 원래 자리로 튀지 않게 한다.
        this.doc.layout[drag.key] = {
          ...(this.doc.layout[drag.key] ?? {}), x: drag.last.x, y: drag.last.y,
        };
        this.opts.onTableMove?.(drag.key, drag.last.x, drag.last.y);
        this.render();
      } else if (drag.mode === 'group') {
        const group = (this.doc.groups ?? []).find((x) => x.id === drag.id);
        if (group) {
          group.x = drag.last.x;
          group.y = drag.last.y;
        }
        this.opts.onGroupMove?.(drag.id, drag.last.x, drag.last.y);
        this.render();
      } else if (drag.mode === 'note') {
        const note = (this.doc.notes ?? []).find((n) => n.id === drag.id);
        if (note) {
          note.x = drag.last.x;
          note.y = drag.last.y;
        }
        this.opts.onNoteMove?.(drag.id, drag.last.x, drag.last.y);
        this.render();
      }
    };

    // 스페이스바: 누르고 있는 동안만 화면 이동.
    //
    // 스페이스는 **캔버스를 보고 있을 때만** 우리 것이다.
    //
    // 입력 칸만 걸러서는 부족하다. 스페이스는 단추·체크박스를 누르는 키이기도 하고
    // 모달이 열리면 포커스는 그 대화상자에 있다. 그 자리에서 우리가 삼키면 키보드로
    // 단추를 누를 수 없게 된다 — 그래서 "포커스가 캔버스 쪽에 있는가"로 판단한다.
    const canTakeSpace = () => {
      const el = document.activeElement;
      if (!el || el === document.body) return true;
      if (el.isContentEditable) return false;
      return this.wrap.contains(el);
    };
    const onKeyDown = (e) => {
      if (e.code !== 'Space' && e.key !== ' ') return;
      if (!canTakeSpace()) return;
      // 기본 동작(페이지 스크롤)을 막는다. 캔버스를 보고 있는데 페이지가 튀면
      // 지금 무엇을 보고 있었는지 잃는다.
      e.preventDefault();
      this.setSpaceHeld(true);
    };
    const onKeyUp = (e) => {
      if (e.code !== 'Space' && e.key !== ' ') return;
      this.setSpaceHeld(false);
    };
    // 창을 벗어나면(다른 탭·다른 창) keyup을 못 받는다. 그대로 두면 돌아왔을 때
    // 계속 스페이스를 누르고 있는 상태로 남아, 카드를 끌 수 없게 된다.
    const onBlur = () => this.setSpaceHeld(false);

    svg.addEventListener('wheel', onWheel, { passive: false });
    svg.addEventListener('pointerdown', onPointerDown);
    window.addEventListener('pointermove', onPointerMove);
    window.addEventListener('pointerup', onPointerUp);
    window.addEventListener('keydown', onKeyDown);
    window.addEventListener('keyup', onKeyUp);
    window.addEventListener('blur', onBlur);

    this.unbind = () => {
      svg.removeEventListener('wheel', onWheel);
      svg.removeEventListener('pointerdown', onPointerDown);
      window.removeEventListener('pointermove', onPointerMove);
      window.removeEventListener('pointerup', onPointerUp);
      window.removeEventListener('keydown', onKeyDown);
      window.removeEventListener('keyup', onKeyUp);
      window.removeEventListener('blur', onBlur);
    };
  }

  // panDrag는 "지금 무엇을 잡든 화면이 움직이는가"다.
  //
  // 화면 이동 도구를 고른 사람은 "당분간 배치는 건드리지 않겠다"고 말한 것이다.
  // 카드를 잡아도 카드가 아니라 화면이 움직인다 — 고르는 것(속성 보기)은 그대로
  // 되므로, 읽는 동안 실수로 배치가 흐트러지는 일이 없어진다.
  get panDrag() {
    return this.spaceHeld || (this.hasToolPicker && this.tool === 'pan');
  }

  // canPick은 "여럿을 고를 수 있는가"다. 고르는 것은 편집이 아니므로 읽기 전용
  // 참여자도 할 수 있다 — 읽는 사람도 "이 넷이 한 덩어리"를 짚어 볼 수 있어야 한다.
  get canPick() {
    return this.opts.onMarks != null;
  }

  isPrimary(kind, id) {
    if (this.marks.length < 2) return false;
    const last = this.marks[this.marks.length - 1];
    return last.kind === kind && last.id === id;
  }

  // pickToggle은 Shift·Ctrl 클릭으로 선택에 더하거나 뺀다. 처리했으면 true.
  //
  // 이때 드래그를 시작하지 않는 이유: 고르려고 누른 손이 몇 픽셀 흔들리면 방금
  // 고른 것이 딸려 움직인다. 더 고르는 동작과 옮기는 동작은 나눠 둔다.
  pickToggle(e, kind, id) {
    if (!this.canPick) return false;
    if (!(e.shiftKey || e.ctrlKey || e.metaKey)) return false;
    this.toggleMark(kind, id);
    this.render();
    this.opts.onMarks?.(this.marks.slice());
    return true;
  }

  // markPos는 고른 것의 현재 좌표다.
  markPos(mark) {
    if (mark.kind === 'table') {
      const box = this.doc.layout?.[mark.id];
      return box ? { x: box.x ?? 80, y: box.y ?? 80 } : null;
    }
    const list = mark.kind === 'note' ? this.doc.notes : this.doc.groups;
    const found = (list ?? []).find((x) => x.id === mark.id);
    return found ? { x: found.x ?? 0, y: found.y ?? 0 } : null;
  }

  // placeMark는 로컬 상태를 먼저 옮겨 둔다. 저장 결과가 오기 전에 다시 그려도
  // 카드가 원래 자리로 튀지 않게 하기 위해서다.
  placeMark(mark, x, y) {
    if (mark.kind === 'table') {
      this.doc.layout[mark.id] = { ...(this.doc.layout[mark.id] ?? {}), x, y };
      return;
    }
    const list = mark.kind === 'note' ? this.doc.notes : this.doc.groups;
    const found = (list ?? []).find((n) => n.id === mark.id);
    if (found) {
      found.x = x;
      found.y = y;
    }
  }

  // startMultiDrag는 고른 것들을 함께 끌기 시작한다. 시작했으면 true.
  //
  // 여럿을 골라 둔 채 그중 하나를 잡으면 나머지도 따라와야 한다. 그러지 않으면
  // 다중 선택으로 할 수 있는 일이 "함께 보기"밖에 없다.
  startMultiDrag(e) {
    if (this.marks.length < 2) return false;
    const items = [];
    for (const m of this.marks) {
      if (m.kind === 'link') continue;
      const at = this.markPos(m);
      if (!at) continue;
      items.push({ kind: m.kind, id: m.id, selector: markSelector(m), x: at.x, y: at.y });
    }
    if (items.length < 2) return false;
    this.drag = { mode: 'multi', items, start: this.toCanvas(e.clientX, e.clientY) };
    return true;
  }

  // drawBand는 범위 선택 사각형을 그린다. 선택 자체는 손을 뗄 때 정해진다.
  drawBand(a, b) {
    if (!this.layers?.band) return;
    if (!this.band || !this.band.isConnected) {
      this.band = svgEl('rect', { class: 'erd-band' });
      this.layers.band.appendChild(this.band);
    }
    this.band.setAttribute('x', Math.min(a.x, b.x));
    this.band.setAttribute('y', Math.min(a.y, b.y));
    this.band.setAttribute('width', Math.abs(b.x - a.x));
    this.band.setAttribute('height', Math.abs(b.y - a.y));
  }

  clearBand() {
    this.band?.remove();
    this.band = null;
  }

  // bandHits는 사각형에 **닿은** 것들이다.
  //
  // 완전히 품은 것만 고르지 않는 이유: 카드가 큰 화면에서는 넷을 고르려면 화면
  // 밖까지 끌어야 한다. 닿기만 해도 골리면 훑는 동작 하나로 줄 단위 선택이 된다.
  bandHits(a, b) {
    const box = {
      x: Math.min(a.x, b.x), y: Math.min(a.y, b.y),
      w: Math.abs(b.x - a.x), h: Math.abs(b.y - a.y),
    };
    const hit = (x, y, w, hh) => x < box.x + box.w && x + w > box.x
      && y < box.y + box.h && y + hh > box.y;
    const out = [];
    for (const [key, geom] of this.boxes()) {
      if (hit(geom.x, geom.y, geom.w, geom.h)) out.push({ kind: 'table', id: key });
    }
    for (const note of this.doc.notes ?? []) {
      if (hit(note.x, note.y, note.w || NOTE_W, note.h || noteHeight(note))) {
        out.push({ kind: 'note', id: note.id });
      }
    }
    for (const group of this.doc.groups ?? []) {
      if (hit(group.x, group.y, group.w || 320, group.h || 240)) {
        out.push({ kind: 'group', id: group.id });
      }
    }
    return out;
  }

  // startPan은 화면 이동 드래그를 시작한다.
  //
  // clearSelection은 "빈 곳을 눌렀다"는 뜻일 때만 켠다. 스페이스바로 옮기는 것은
  // 보는 자리를 바꾸는 일이지 고른 것을 놓는 일이 아니다 — 골라 둔 열 장을 옮겨
  // 보려고 화면을 밀었더니 선택이 풀리면, 그 뒤의 정렬·복제를 다시 골라야 한다.
  // otherButton은 왼쪽이 아닌 버튼을 처리한다. 처리했으면 true.
  //
  // 빈 곳에서는 이미 이 규칙이었다(가운데는 화면 이동, 오른쪽은 우리 것이 아니다).
  // 그런데 카드·메모·묶음은 각자 pointerdown 을 잡으면서 버튼을 보지 않아, 그 위에서만
  // 규칙이 달랐다 — 가운데 버튼으로 끌면 화면이 아니라 **그것이** 움직였다. 묶음은
  // 넓어서 화면을 옮기려다 그 위를 누르는 일이 잦으니, 옮기려다 배치를 흐트러뜨리게
  // 된다. 오른쪽 버튼도 마찬가지로 막는다: 브라우저 메뉴가 뜬 채 드래그가 시작된
  // 상태가 남는다.
  otherButton(e) {
    if (e.button === 2) return true;
    if (e.button !== 1) return false;
    // 브라우저의 자동 스크롤(가운데 버튼을 누르면 나오는 사방 화살표)을 막는다.
    // 그것이 켜지면 우리 이동과 두 힘이 동시에 당겨 화면이 튄다.
    e.preventDefault();
    // 고른 것은 그대로 둔다. 화면을 잠깐 옮기는 일이 "고른 것을 놓는 일"이어서는
    // 안 된다 — 속성 창을 보면서 다른 곳을 살피는 것이 이 버튼을 쓰는 이유다.
    this.startPan(e, { clearSelection: false });
    return true;
  }

  startPan(e, { clearSelection = false } = {}) {
    this.drag = { mode: 'pan', startClient: { x: e.clientX, y: e.clientY }, view: { ...this.view } };
    // 화면을 손으로 옮기기 시작했다. 따라가기와 두 힘이 동시에 당기면
    // 어느 쪽도 원하는 곳을 볼 수 없다.
    this.opts.onManualPan?.();
    if (clearSelection && this.marks.length) {
      const many = this.marks.length > 1;
      this.marks = [];
      if (many) this.opts.onMarks?.([]);
      else this.opts.onSelect?.(null);
      this.render();
    }
    this.svg.setPointerCapture?.(e.pointerId);
  }

  // setSpaceHeld는 스페이스바 상태를 반영한다(커서 포함).
  setSpaceHeld(on) {
    if (this.spaceHeld === on) return;
    this.spaceHeld = on;
    this.svg.classList.toggle('is-space-pan', on);
  }

  // isPanTool은 화면 이동 도구가 켜져 있는지다(CSS가 커서를 정할 때 쓴다).
  get isPanTool() {
    return this.hasToolPicker && this.tool === 'pan';
  }

  // moveLinks는 끌고 있는 카드의 좌표를 반영해 관계선만 다시 그린다.
  //
  // 선 레이어만 갈아 끼우는 이유: 카드까지 다시 그리면 지금 잡고 있는 요소가 버려져
  // 드래그가 끊긴다(onCardPointerDown의 주석과 같은 이유다).
  //
  // 프레임당 한 번으로 묶는다. pointermove는 화면 갱신보다 훨씬 자주 오므로
  // 그대로 두면 같은 그림을 프레임마다 여러 번 만든다.
  moveLinks(key, x, y) {
    if (!this.layers?.links) return;
    const prev = this.doc.layout[key] ?? {};
    this.doc.layout[key] = { ...prev, x, y };
    this.refreshLinks();
  }

  // refreshLinks는 지금 레이아웃으로 관계선만 다시 그린다.
  refreshLinks() {
    if (!this.layers?.links) return;
    if (this.linkFrame) return;
    this.linkFrame = requestAnimationFrame(() => {
      this.linkFrame = 0;
      if (!this.layers?.links) return;
      this.links(this.boxes(), Boolean(this.drag));
    });
  }

  onCardPointerDown(e, key, geom) {
    e.stopPropagation();
    if (this.otherButton(e)) return;
    // 스페이스바를 누르고 있으면 무엇을 눌렀든 화면 이동이다. 카드 위에서만 안 되면
    // "빈 곳을 찾아 눌러야 하는" 도구가 되는데, 카드가 화면을 덮은 상태에서 옮기려는
    // 순간이 바로 그 도구가 필요한 순간이다.
    if (this.spaceHeld) {
      this.startPan(e);
      return;
    }
    if (this.pickToggle(e, 'table', key)) return;
    // 이미 여럿을 고른 상태에서 그중 하나를 잡은 것이라면 선택을 그대로 둔다.
    // 여기서 하나로 줄이면 함께 옮기려고 잡은 순간 나머지가 풀린다.
    if (!this.isSelected('table', key)) {
      // 선택은 언제나 {kind, id} 형태로 둔다. 여기서만 문자열을 넣으면
      // isSelected가 어긋나 카드에 선택 표시가 그려지지 않는다 — 구조 화면이
      // 그랬다(그쪽은 setSelection을 다시 부르지 않아 문자열이 그대로 남았다).
      this.selection = { kind: 'table', id: key };
      this.opts.onSelect?.(key);
    }
    // 선택 표시를 즉시 반영한다. **다시 그린 뒤에** 드래그 대상을 찾아야 한다 —
    // 지금 손에 쥔 요소는 재렌더로 버려지고, 버려진 요소를 옮기면 화면에서
    // 아무 일도 일어나지 않는다.
    this.render();
    if (!this.canEdit) return;
    // 화면 이동 도구에서는 여기까지(고르기)만 하고 끌기는 화면 이동이다.
    if (this.panDrag) {
      this.startPan(e);
      return;
    }
    if (this.startMultiDrag(e)) return;
    const p = this.toCanvas(e.clientX, e.clientY);
    const el = this.svg.querySelector(`.erd-card-g[data-key="${cssEscape(key)}"]`);
    if (!el) return;
    this.drag = {
      mode: 'card', key, el,
      selector: `.erd-card-g[data-key="${cssEscape(key)}"]`,
      grab: { x: p.x - geom.x, y: p.y - geom.y },
    };
  }

  onNotePointerDown(e, note, mode = 'move') {
    e.stopPropagation();
    if (this.otherButton(e)) return;
    if (this.spaceHeld) {
      this.startPan(e);
      return;
    }
    if (mode !== 'resize' && this.pickToggle(e, 'note', note.id)) return;
    // 고르는 것과 옮기는 것은 다른 권한이다. 읽기 전용 참여자도 메모를 골라
    // 내용을 인스펙터에서 읽을 수 있어야 한다.
    if (!this.isSelected('note', note.id)) {
      this.selection = { kind: 'note', id: note.id };
      this.opts.onSelectNote?.(note.id);
    }
    this.render();
    if (!this.canEdit) return;
    // 크기 조절도 배치를 바꾸는 일이라 화면 이동 도구에서는 하지 않는다.
    if (this.panDrag) {
      this.startPan(e);
      return;
    }
    if (mode !== 'resize' && this.startMultiDrag(e)) return;
    const p = this.toCanvas(e.clientX, e.clientY);
    const el = this.svg.querySelector(`.erd-note-g[data-note="${cssEscape(note.id)}"]`);
    if (!el) return;
    const selector = `.erd-note-g[data-note="${cssEscape(note.id)}"]`;
    this.drag = mode === 'resize'
      ? {
        mode: 'note-resize', id: note.id, el, selector, note,
        start: p, size: { w: note.w || NOTE_W, h: note.h || noteHeight(note) },
      }
      : {
        mode: 'note', id: note.id, el, selector,
        grab: { x: p.x - note.x, y: p.y - note.y },
      };
  }

  onGroupPointerDown(e, group, mode) {
    e.stopPropagation();
    if (this.otherButton(e)) return;
    if (this.spaceHeld) {
      this.startPan(e);
      return;
    }
    if (mode !== 'resize' && this.pickToggle(e, 'group', group.id)) return;
    if (!this.isSelected('group', group.id)) {
      this.selection = { kind: 'group', id: group.id };
      this.opts.onSelectGroup?.(group.id);
    }
    this.render();
    if (!this.canEdit) return;
    if (this.panDrag) {
      this.startPan(e);
      return;
    }
    if (mode !== 'resize' && this.startMultiDrag(e)) return;
    const p = this.toCanvas(e.clientX, e.clientY);
    const el = this.svg.querySelector(`.erd-group-g[data-group="${cssEscape(group.id)}"]`);
    if (!el) return;
    const selector = `.erd-group-g[data-group="${cssEscape(group.id)}"]`;
    this.drag = mode === 'resize'
      ? {
        mode: 'group-resize', id: group.id, el, selector, group,
        start: p, size: { w: group.w || 320, h: group.h || 240 },
      }
      : { mode: 'group', id: group.id, el, selector, grab: { x: p.x - group.x, y: p.y - group.y } };
  }

  // onCardResizeDown은 카드 폭 조절을 시작한다.
  //
  // side가 'left'면 왼쪽 변을 끄는 것이다. 그때 움직이는 것은 폭만이 아니라
  // 카드의 x이기도 하다 — 오른쪽 변을 제자리에 두려면 왼쪽으로 넓힌 만큼 x가
  // 왼쪽으로 가야 한다.
  onCardResizeDown(e, key, geom, side = 'right') {
    e.stopPropagation();
    if (this.otherButton(e)) return;
    if (this.spaceHeld || this.panDrag) {
      this.startPan(e);
      return;
    }
    if (!this.isSelected('table', key)) {
      this.selection = { kind: 'table', id: key };
      this.opts.onSelect?.(key);
      this.render();
    }
    if (!this.canEdit) return;
    const p = this.toCanvas(e.clientX, e.clientY);
    const selector = `.erd-card-g[data-key="${cssEscape(key)}"]`;
    this.drag = {
      mode: 'card-resize', key, el: this.svg.querySelector(selector), selector,
      start: p, width: geom.w, side,
      // 왼쪽을 끌 때 붙잡아 둘 오른쪽 변의 자리.
      right: geom.x + geom.w, x: geom.x, y: geom.y,
    };
  }

  // bindTip은 이 요소에 손을 올리면 잠깐 뒤 설명을 띄우도록 붙인다.
  //
  // 잠깐 기다리는 이유: 도면 위에서 마우스는 늘 지나다닌다. 곧바로 뜨면 카드를
  // 가로지를 때마다 팝오버가 깜박이며 따라다녀서, 정작 읽으려는 것을 가린다.
  bindTip(el, build) {
    el.addEventListener('pointerenter', (e) => {
      // 무언가를 끌고 있는 동안에는 띄우지 않는다. 옮기는 손끝을 설명이 가린다.
      if (this.drag) return;
      const text = build();
      if (!text) return;
      clearTimeout(this.tipTimer);
      this.tipTimer = setTimeout(() => this.showTip(text, e.clientX, e.clientY), TIP_DELAY);
    });
    el.addEventListener('pointerleave', () => this.hideTip());
    // 누르면 그 자리에서 치운다. 고르거나 끌기 시작하는 순간에 남아 있으면
    // 방금 무엇을 눌렀는지가 가려진다.
    el.addEventListener('pointerdown', () => this.hideTip());
  }

  // showTip은 설명 팝오버를 띄운다.
  showTip(lines, clientX, clientY) {
    if (!this.wrap) return;
    if (!this.tipEl) {
      this.tipEl = h('div.erd-tip', { role: 'tooltip' });
      this.wrap.appendChild(this.tipEl);
    }
    mount(this.tipEl, lines.map(([label, value]) => h('div.erd-tip-row', {},
      label ? h('span.erd-tip-label', {}, label) : null,
      h('span.erd-tip-value', {}, value))));

    // 자리를 잡는다. 마우스 오른쪽 아래가 기본이고, 넘치면 반대쪽으로 접는다 —
    // 화면 밖으로 나간 설명은 없는 것과 같다.
    const box = this.wrap.getBoundingClientRect();
    this.tipEl.style.visibility = 'hidden';
    this.tipEl.hidden = false;
    const tip = this.tipEl.getBoundingClientRect();
    let x = clientX - box.left + 14;
    let y = clientY - box.top + 16;
    if (x + tip.width > box.width - 8) x = Math.max(8, clientX - box.left - tip.width - 14);
    if (y + tip.height > box.height - 8) y = Math.max(8, clientY - box.top - tip.height - 12);
    this.tipEl.style.left = `${Math.round(x)}px`;
    this.tipEl.style.top = `${Math.round(y)}px`;
    this.tipEl.style.visibility = '';
  }

  hideTip() {
    clearTimeout(this.tipTimer);
    this.tipTimer = 0;
    if (this.tipEl) this.tipEl.hidden = true;
  }

  // setGripHover는 폭 손잡이에 손이 닿았는지를 표시한다.
  //
  // 다시 그리지 않고 클래스만 갈아 끼운다. 마우스가 지나갈 때마다 도면 전체를
  // 다시 그리면 큰 문서에서 손이 뻑뻑해진다.
  setGripHover(key) {
    if (this.gripHover === key) return;
    const mark = (k, on) => {
      if (!k) return;
      this.svg.querySelector(`.erd-card-g[data-key="${cssEscape(k)}"]`)
        ?.classList.toggle('is-grip-hover', on);
    };
    mark(this.gripHover, false);
    this.gripHover = key;
    mark(key, true);
  }

  // liveResize는 끄는 동안의 폭을 그대로 화면에 반영한다.
  //
  // 예전에는 사각형의 width 속성만 바꿨다. 그러면 카드는 넓어지는데 컬럼 이름은
  // 아까 그 폭에 맞춰 자른 그대로여서, 넓히는 내내 "…"이 그대로 남아 있었다 —
  // 넓히는 이유가 바로 그 "…"을 없애려는 것인데, 놓아 봐야 결과를 알 수 있었다.
  //
  // 카드 한 장만 새로 만들어 갈아 끼운다. 프레임당 한 번으로 묶으므로, 컬럼이
  // 수십 개라도 글자를 다시 재는 일은 한 프레임에 한 번이다.
  liveResize(key, w, x) {
    this.drag.lastWidth = w;
    this.drag.lastX = x;
    const prev = this.doc.layout[key] ?? {};
    this.doc.layout[key] = { ...prev, w, x: x ?? prev.x };
    if (this.resizeFrame) return;
    this.resizeFrame = requestAnimationFrame(() => {
      this.resizeFrame = 0;
      // 끄는 동안에는 이 카드를 맨 위로 올린다. 넓히다 옆 카드 밑으로 들어가면
      // 정작 늘어나는 부분이 가려져, 실시간으로 보여 주는 뜻이 없어진다.
      // 놓으면 render()가 원래 순서로 다시 쌓는다.
      this.redrawCard(key, { toTop: true });
      // 관계선도 따라와야 한다. 오른쪽 변이 움직이는데 선이 제자리에 있으면
      // 선이 카드 안쪽에서 시작하거나 허공에서 시작한다.
      this.refreshLinks();
    });
  }

  // redrawCard는 카드 한 장만 새로 그려 갈아 끼운다.
  redrawCard(key, { toTop = false } = {}) {
    const old = this.svg?.querySelector(`.erd-card-g[data-key="${cssEscape(key)}"]`);
    if (!old) return;
    const tbl = (this.doc.schema?.tables ?? []).find((t) => tableKey(t) === key);
    const geom = this.boxes().get(key);
    if (!tbl || !geom) return;
    const fresh = this.cardEl(tbl, geom);
    old.replaceWith(fresh);
    if (toTop) this.layers?.cards?.appendChild(fresh);
  }

  // dragEl은 지금 끌고 있는 요소를 **살아 있는 것으로** 돌려준다.
  //
  // 누르는 순간 잡아 둔 요소는 곧 버려질 수 있다. 선택이 바뀌면 화면 쪽에서
  // 다시 그리기 때문이다(onSelect → renderCanvas). 버려진 요소를 옮기면 아무 일도
  // 일어나지 않는데, 관계선은 매번 새로 찾으므로 선만 따라 움직인다 —
  // "고르지 않은 테이블을 끌면 간선만 움직인다"가 그 증상이었다.
  dragEl() {
    const d = this.drag;
    if (!d) return null;
    if (d.el?.isConnected) return d.el;
    if (!d.selector) return null;
    d.el = this.svg.querySelector(d.selector);
    return d.el;
  }

  // ---------- 뷰포트 ----------

  applyViewBox() {
    this.svg.setAttribute('viewBox',
      `${this.view.x} ${this.view.y} ${this.view.w} ${this.view.h}`);
  }

  toCanvas(clientX, clientY) {
    const ctm = this.svg.getScreenCTM();
    if (!ctm) return { x: 0, y: 0 };
    const pt = this.svg.createSVGPoint();
    pt.x = clientX;
    pt.y = clientY;
    const p = pt.matrixTransform(ctm.inverse());
    return { x: p.x, y: p.y };
  }

  zoom(factor) {
    this.zoomAt(factor, {
      x: this.view.x + this.view.w / 2,
      y: this.view.y + this.view.h / 2,
    });
  }

  // centerOn은 캔버스 좌표 한 점을 화면 가운데로 옮긴다.
  //
  // 확대율은 건드리지 않는다. 따라가기에서 배율까지 맞추면 상대가 줌을 만질 때마다
  // 내 화면이 튀고, 그러면 무엇을 보고 있는지가 오히려 흐려진다.
  centerOn(x, y) {
    this.view.x = x - this.view.w / 2;
    this.view.y = y - this.view.h / 2;
    this.applyViewBox();
  }

  // zoomAt은 지정한 캔버스 좌표를 고정한 채 확대/축소한다.
  // 화면 중앙 기준으로만 확대하면 마우스가 가리키는 곳이 화면에서 벗어난다.
  zoomAt(factor, focus) {
    const nw = clamp(this.view.w * factor, 200, 12000);
    const nh = this.view.h * (nw / this.view.w);
    this.view.x = focus.x - (focus.x - this.view.x) * (nw / this.view.w);
    this.view.y = focus.y - (focus.y - this.view.y) * (nh / this.view.h);
    this.view.w = nw;
    this.view.h = nh;
    this.applyViewBox();
  }

  // fitView는 모든 요소가 보이도록 뷰포트를 맞춘다.
  fitView() {
    const aspect = this.wrap.clientHeight > 0
      ? this.wrap.clientWidth / this.wrap.clientHeight : 1.5;
    const boxes = [...this.boxes().values()];
    const notes = this.doc.notes ?? [];
    if (boxes.length === 0 && notes.length === 0) {
      this.view = { x: 0, y: 0, w: 1200, h: 1200 / aspect };
      return;
    }
    let minX = Infinity; let minY = Infinity; let maxX = -Infinity; let maxY = -Infinity;
    for (const b of boxes) {
      minX = Math.min(minX, b.x); minY = Math.min(minY, b.y);
      maxX = Math.max(maxX, b.x + b.w); maxY = Math.max(maxY, b.y + b.h);
    }
    for (const n of notes) {
      minX = Math.min(minX, n.x); minY = Math.min(minY, n.y);
      maxX = Math.max(maxX, n.x + 200); maxY = Math.max(maxY, n.y + 80);
    }
    const pad = 60;
    const w = Math.max(maxX - minX + pad * 2, 400);
    const hh = Math.max(maxY - minY + pad * 2, 300);
    // 가로세로 비율을 캔버스에 맞춰야 preserveAspectRatio 때문에 여백이 치우치지 않는다.
    const fitW = Math.max(w, hh * aspect);
    const fitH = fitW / aspect;
    this.view = {
      x: minX - pad - (fitW - w) / 2,
      y: minY - pad - (fitH - hh) / 2,
      w: fitW, h: fitH,
    };
  }

  // 화면 중앙의 캔버스 좌표. 새 요소를 "지금 보고 있는 곳"에 놓기 위해 쓴다.
  center(offsetX = 0, offsetY = 0) {
    return {
      x: round1(this.view.x + this.view.w / 2 - offsetX),
      y: round1(this.view.y + this.view.h / 2 - offsetY),
    };
  }
}

// ---------- 공용 헬퍼 ----------

// normalizeMark는 선택 항목을 {kind, id} 로 맞춘다(문자열은 테이블 키로 본다).
function normalizeMark(mark) {
  return typeof mark === 'string' ? { kind: 'table', id: mark } : mark;
}

// markSelector는 고른 것의 SVG 요소를 찾는 선택자다.
function markSelector(mark) {
  if (mark.kind === 'table') return `.erd-card-g[data-key="${cssEscape(mark.id)}"]`;
  if (mark.kind === 'note') return `.erd-note-g[data-note="${cssEscape(mark.id)}"]`;
  return `.erd-group-g[data-group="${cssEscape(mark.id)}"]`;
}

// NOTE_W는 메모의 기본 폭이다. 크기를 정하지 않은 메모가 이 폭으로 그려진다.
export const NOTE_W = 200;

// noteHeight는 크기를 정하지 않은 메모의 높이다(글 줄 수에 맞춘다).
export function noteHeight(note) {
  const perLine = Math.max(8, Math.floor(((note.w || NOTE_W) - 16) / 7));
  return 16 + wrapText(note.text || '(빈 메모)', perLine).length * 16;
}

export function tableKey(tbl) {
  return (tbl.namespace ? `${tbl.namespace}.${tbl.name}` : tbl.name).toLowerCase();
}

export function tableDisplay(tbl) {
  return tbl.namespace ? `${tbl.namespace}.${tbl.name}` : tbl.name;
}

export function refKey(tbl, fk) {
  const ns = fk.refNamespace || tbl.namespace || '';
  return (ns ? `${ns}.${fk.refTable}` : fk.refTable).toLowerCase();
}

export function eqName(a, b) {
  return String(a).toLowerCase() === String(b).toLowerCase();
}

export function svgEl(tag, attrs = {}, text) {
  const el = document.createElementNS(SVG_NS, tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === null || v === undefined || v === false) continue;
    el.setAttribute(k, v);
  }
  if (text !== undefined) el.textContent = text;
  return el;
}

// cardWidth는 그 카드의 폭이다. 정해 둔 것이 없으면 기본값을 쓴다.
//
// 여기 한 곳에서 상한·하한을 건다. 서버도 같은 값으로 자르지만(erd.ops), 화면이
// 먼저 걸러 주지 않으면 끌고 있는 동안 카드가 화면 밖으로 자라거나 글자 폭보다
// 좁아져 아무것도 읽을 수 없게 된다.
export function cardWidth(layout) {
  const w = Number(layout?.w) || 0;
  if (!w) return CARD_W;
  return Math.max(CARD_MIN_W, Math.min(CARD_MAX_W, w));
}

export function truncate(text, max) {
  const s = String(text ?? '');
  return s.length > max ? `${s.slice(0, max - 1)}…` : s;
}

function wrapText(text, width) {
  const out = [];
  for (const para of String(text).split('\n')) {
    let line = '';
    for (const word of para.split(/\s+/)) {
      if (!line) {
        line = word;
      } else if ((line + ' ' + word).length <= width) {
        line += ` ${word}`;
      } else {
        out.push(line);
        line = word;
      }
      if (out.length >= 8) return out;
    }
    out.push(line);
  }
  return out.slice(0, 8);
}

function clamp(v, min, max) {
  return Math.min(Math.max(v, min), max);
}

export function round1(v) {
  return Math.round(v * 10) / 10;
}

// cssEscape는 속성 선택자에 쓸 값을 안전하게 만든다.
// 테이블 키에는 점과 따옴표가 들어갈 수 있다.
function cssEscape(value) {
  return String(value).replace(/["\\]/g, '\\$&');
}

// newLocalID는 메모·그룹처럼 클라이언트가 만드는 요소의 식별자다.
export function newLocalID(prefix) {
  return `${prefix}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 6)}`;
}

// 캔버스 위에 아무것도 없을 때 쓰는 안내 요소.
export function canvasWrap() {
  return h('div.erd-canvas-wrap');
}

// TIP_DELAY는 손을 올리고 설명이 뜨기까지의 시간이다.
//
// 도면 위에서 마우스는 늘 지나다닌다. 곧바로 뜨면 카드를 가로지를 때마다
// 팝오버가 깜박이며 따라다니고, 정작 읽으려는 것을 가린다. 반대로 너무 길면
// 사람이 먼저 포기한다.
const TIP_DELAY = 420;

// tipForTable은 표 제목에 띄울 줄들이다. 띄울 것이 없으면 null.
function tipForTable(tbl, layout, shown, maxWidth) {
  const rows = [];
  const full = tbl.namespace ? `${tbl.namespace}.${tbl.name}` : tbl.name;
  const logical = (layout?.logical ?? '').trim();

  // 잘린 이름은 그것만으로도 띄울 이유가 된다. "…"으로 끝난 이름을 확인할 방법이
  // 속성 창을 여는 것뿐이면, 도면을 훑는 일이 그때마다 끊긴다.
  const cut = fitText(shown, maxWidth, cssFont('erd-card-name')) !== shown;
  if (cut || logical) rows.push(['', full]);
  if (logical) rows.push(['논리명', logical]);
  const comment = (tbl.comment ?? '').trim();
  if (comment) rows.push(['주석', comment]);
  // 연결 표라면 무엇과 무엇을 잇는지 적는다. 카드에는 "N:N"만 보이는데, 그것만으로는
  // 무엇의 N:N 인지 알 수 없다.
  if (isJunction(tbl)) {
    rows.push(['N:N', `${junctionPartners(tbl).join(' ↔ ')} 를 잇는 연결 표`]);
  }
  return rows.length ? rows : null;
}

// endSpecs는 관계선 양 끝의 [자리, 변, 표식 종류, 글자]다.
//
// 한곳에서 만드는 이유: 표식과 글자는 같은 것을 말하므로 같은 값에서 나와야 한다.
// 따로 계산하면 한쪽만 고쳐 기호와 글자가 다른 말을 하는 날이 온다.
//
// card 가 없는 선(표를 못 찾은 경우)에는 예전 모양인 N:1 로 그린다. 아무 표식도
// 없는 끝은 덜 그려진 그림으로 보인다.
function endSpecs(r) {
  const child = r.card?.childMark ?? { many: true, optional: true };
  const parent = r.card?.parentMark ?? { many: false, optional: false };
  return [
    [r.a, r.sa, child, r.card?.child ?? 'N'],
    [r.b, r.sb, parent, r.card?.parent ?? '1'],
  ];
}

// tipForColumn은 컬럼 줄에 띄울 줄들이다. 띄울 것이 없으면 null.
function tipForColumn(col, layout, label, rawType, domain, room) {
  const rows = [];
  const logical = (layout?.columnLogical ?? {})[String(col.name).toLowerCase()] ?? '';

  // 이름이 잘렸는지 본다. 논리명과 물리명을 함께 보일 때는 둘 다 잘릴 수 있다.
  const nameFont = cssFont('erd-col');
  const cut = fitText(label.main, room, nameFont) !== label.main;
  if (cut || logical || label.sub) rows.push(['', col.name]);
  if (logical) rows.push(['논리명', logical]);

  // 타입은 늘 적는다. 도메인이 걸린 컬럼은 도면에 도메인 이름만 보이는데,
  // 그때 실제 타입이 무엇인지가 가장 자주 궁금해지는 것이다.
  const actual = col.rawType || col.type?.base || '';
  if (domain) rows.push(['도메인', `${domain}${actual ? ` (${actual})` : ''}`]);
  else if (actual) rows.push(['타입', actual + (col.nullable ? '' : ' NOT NULL')]);

  if ((col.default ?? '') !== '') rows.push(['기본값', String(col.default)]);
  const comment = (col.comment ?? '').trim();
  if (comment) rows.push(['주석', comment]);
  // 이름도 안 잘리고 주석도 없으면 띄울 이유가 없다. 타입은 이미 줄에 보인다.
  if (!comment && !logical && !cut && !domain) return null;
  return rows.length ? rows : null;
}
