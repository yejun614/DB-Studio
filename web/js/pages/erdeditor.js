// ERD 편집기: SVG 캔버스 + 동시 편집 + 프레즌스 + 채팅.
//
// 렌더링 전략: 구조가 바뀌면 캔버스를 통째로 다시 그린다. 부분 갱신은 op 종류마다
// 다른 경로가 필요하고, 그 경로가 서버의 적용 결과와 어긋나면 사용자마다 다른
// 그림을 보게 된다. 캔버스 전체 재생성은 수십~수백 테이블 규모에서 충분히 빠르다.
//
// 예외는 두 곳이다. (1) 드래그 중 좌표는 op를 기다리지 않고 즉시 반영한다 —
// 왕복 지연이 있으면 카드가 마우스를 따라오지 못한다. (2) 편집 중인 입력에
// 포커스가 있으면 사이드 패널을 다시 그리지 않는다 — 다른 사람의 편집 때문에
// 내가 타이핑하던 내용이 사라지면 안 된다.
import { api } from '../core/api.js';
import { state, kindLabel } from '../core/store.js';
import {
  h, mount, icon, input, select, checkbox, field, spinner, badge, envBadge,
  toast, toastError, relativeTime, formatDate, openModal, confirmDialog, copyToClipboard,
} from '../core/ui.js';
import { ErdSession, throttle } from '../core/erdsocket.js';
import { roomChatView } from '../core/roomchat.js';
import { searchPicker, suggestInput } from '../core/searchpick.js';
import { COLUMN_ICONS, columnIcon, autoColumnIcon, chosenIconFor } from '../core/colicon.js';
import { codeBlock, codeEditor } from '../core/highlight.js';
import { versionSourceLabel } from './migrations.js';
import {
  ErdCanvas, CARD_W, tableKey, tableDisplay, refKey, newLocalID, truncate, NOTE_W, noteHeight,
} from '../core/erdcanvas.js';
import {
  loadTypeCatalog, buildType, parseType, categories, paramLabel, paramPlaceholder,
} from '../core/dbtypes.js';
import { navigate } from '../core/router.js';
import { panelResizeHandle, attachPanelResize } from '../core/panelresize.js';
import { renderMarkdown } from '../core/markdown.js';
import { openImageExportDialog } from '../core/erdimage.js';
import { streamAIChat } from '../core/aistream.js';
import { errorPanel } from './users.js';
import { statusBadge } from './erd.js';

// 테이블에 붙일 수 있는 아이콘. 빈 값은 "없음"이다.
// 종류를 늘리는 것보다 서로 확실히 구분되는 몇 개가 낫다 — 스무 개 중에서 고르면
// 다음에 같은 뜻으로 어느 것을 골랐는지 기억하지 못한다.
const TABLE_ICONS = [
  '', 'users', 'key', 'database', 'activity', 'list', 'lock', 'settings', 'history',
  // 도메인을 눈으로 가르는 데 실제로 필요한 것들. 이름을 읽기 전에 "이건 주문 쪽,
  // 저건 결제 쪽"이 보이려면 종류가 이 정도는 있어야 한다.
  'cart', 'box', 'money', 'chat', 'mail', 'bell', 'calendar', 'chart',
  'file', 'tag', 'location', 'truck', 'star', 'flag', 'shield', 'code', 'link',
];

// 카드 강조색. Box.color에 그대로 담기므로 CSS 변수가 아니라 실제 색을 쓴다.
const TABLE_COLORS = [
  { value: '', label: '기본', className: 'tint-none' },
  { value: '#eab308', label: '노랑', className: 'tint-yellow' },
  { value: '#3b82f6', label: '파랑', className: 'tint-blue' },
  { value: '#22c55e', label: '초록', className: 'tint-green' },
  { value: '#ec4899', label: '분홍', className: 'tint-pink' },
  { value: '#a1a1aa', label: '회색', className: 'tint-gray' },
];

// 새 컬럼의 기본 타입. 방언마다 "가장 흔한 문자열"이 다르다.
function defaultColumnType(dialect) {
  if (dialect === 'postgres') return 'text';
  if (dialect === 'sqlite') return 'text';
  if (dialect === 'oracle') return 'varchar2(255)';
  return 'varchar(255)';
}

export async function renderERDEditor(outlet, params) {
  const docID = params.id;
  mount(outlet, spinner('ERD 문서를 여는 중…'));

  let initial;
  try {
    initial = await api.get(`/erd/documents/${encodeURIComponent(docID)}`);
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  const ui = buildLayout(initial);
  mount(outlet, ui.root);

  const editor = new Editor(docID, initial, ui);
  editor.start();

  // 라우터가 화면을 떠날 때 소켓과 리스너를 정리한다.
  return () => editor.stop();
}

// ---------- 화면 골격 ----------

function buildLayout(initial) {
  const statusChip = h('span.erd-conn-status', {}, '연결 중…');
  const participants = h('div.erd-participants');
  const panel = h('aside.erd-panel');
  const toolbar = h('div.erd-toolbar');
  const canvasWrap = h('div.erd-canvas-wrap');
  // 캔버스 위에 겹쳐 뜨는 것들. 캔버스 자체(SVG)를 건드리지 않으려고 형제로 둔다 —
  // SVG 안에 HTML을 넣으려면 foreignObject가 필요하고, 그 안에서는 스크롤·클릭이
  // 브라우저마다 다르게 동작한다.
  const chatStack = h('div.erd-chat-stack');
  const followBar = h('div.erd-follow-bar', { hidden: true });
  // 인스펙터 폭 손잡이. 컬럼이 많은 테이블에서는 340px에 타입까지 들어가지 않고,
  // 반대로 캔버스를 넓게 보고 싶은 사람에게는 340px도 넓다.
  const panelResize = panelResizeHandle();

  const root = h('div.erd-editor', {},
    h('header.erd-head', {},
      h('div.erd-head-main', {},
        h('a.erd-back', { href: '/erd' }, icon('x', 14), '목록'),
        h('h1.erd-title', {}, initial.document.name),
        statusBadge(initial.document.status),
        initial.connection ? envBadge(initial.connection.environment) : null,
        initial.connection
          ? h('span.muted', {}, `${initial.connection.name} · ${kindLabel(initial.document.dialect)}`)
          // 대상 DB가 없다는 것은 이 화면에서 할 수 있는 일을 바꾼다(마이그레이션 없음).
          // 제목 옆에 두어 열자마자 보이게 한다.
          : h('span.muted', {}, `대상 DB 없음 · ${kindLabel(initial.document.dialect)}`),
        initial.canEdit ? null : badge('읽기 전용', 'warn'),
      ),
      h('div.erd-head-side', {}, participants, statusChip),
    ),
    toolbar,
    h('div.erd-body', {},
      h('div.erd-canvas-area', {}, canvasWrap, chatStack, followBar),
      panelResize,
      panel),
  );

  return {
    root, panel, toolbar, participants, statusChip, canvasWrap, chatStack, followBar, panelResize,
  };
}

// ---------- 편집기 ----------

class Editor {
  constructor(docID, initial, ui) {
    this.docID = docID;
    this.ui = ui;
    this.doc = initial.document;
    this.connection = initial.connection;
    this.canEdit = initial.canEdit;
    // canManage는 문서 자체(이름·메모·삭제)를 바꿀 수 있는가다.
    // 편집 권한(canEdit)과 다른 축이다 — 남의 초안을 고쳐 줄 수는 있어도
    // 그 문서의 이름을 바꾸는 것은 만든 사람의 일이다.
    this.canManage = Boolean(initial.canManage);
    this.panelHidden = false;
    // 메모는 문서(그래프)가 아니라 메타 레코드에 붙어 있어 응답의 최상위로 온다.
    // this.doc.note 를 읽으면 언제나 비어 있다.
    this.note = initial.note ?? '';
    // chatMode는 대화 패널이 무엇을 보여주는가다: 'room'(전체 대화) 또는 AI 세션 id.
    this.chatMode = 'room';
    // following은 화면을 따라갈 참여자의 clientId다.
    this.following = null;
    // 되돌리기 스택은 서버가 사람마다 들고 있다. 여기서는 버튼을 켤지 끌지만 안다 —
    // 화면이 따로 세면 서버의 스택과 어긋나고, 그 어긋남은 "눌러도 아무 일이
    // 없다"로 나타난다.
    this.canUndo = false;
    this.canRedo = false;
    // AI 세션은 대화 탭을 처음 열 때 불러온다 — 편집만 하는 사람에게는
    // 필요 없는 왕복이다.
    this.aiLoaded = false;
    this.aiSessions = [];
    this.aiProviders = [];
    this.aiMessages = [];
    this.aiBusy = false;

    // 선택은 종류를 함께 갖는다: {kind: 'table'|'link'|'note'|'group', id}
    //
    // 예전에는 테이블 키 문자열 하나였다. 간선·메모·그룹까지 인스펙터에서 편집하게
    // 되면서, "무엇을 고른 상태인가"를 문자열 하나로는 표현할 수 없게 되었다.
    this.sel = null;
    // marks는 **함께 고른 것들**이다([{kind, id}]). 마지막 것이 this.sel 이다.
    //
    // 캔버스가 아니라 여기서 들고 있는 이유: 화면은 다시 그릴 때마다 자기 상태를
    // 캔버스에 밀어 넣는다(renderCanvas). 캔버스만 들고 있으면 그 한 번에 다중
    // 선택이 매번 사라진다.
    this.marks = [];
    // tool은 마우스 도구다: 'select'면 빈 곳을 끌어 범위로 고르고, 'pan'이면 화면을
    // 옮긴다. 고른 도구는 사람마다 기억한다 — 한 번 정하면 다음에도 그 손버릇이다.
    this.tool = readTool();
    // renamedSel은 이름 미리보기 때문에 선택 키를 옮겨 둔 기록이다({from, to}).
    // 이름이 거부되면 from으로 되돌린다(previewTableName의 주석 참고).
    this.renamedSel = null;
    this.tab = 'table'; // table | domain | chat
    this.chat = [];
    this.remoteCursors = new Map(); // clientId → {x,y,color,name}
    this.participants = [];
    this.you = null;
    this.panelDirty = false;
    // panelPressed는 "지금 패널에서 무언가를 누르고 있다"는 표시다.
    // 누르고 있는 동안 패널을 다시 그리면 그 클릭이 사라진다.
    this.panelPressed = false;

    // 캔버스는 구조 화면과 공유한다(core/erdcanvas.js). 여기서는 "옮긴 결과를
    // op로 보낸다"만 정하고, 그리는 방법은 캔버스가 안다.
    this.canvas = new ErdCanvas(ui.canvasWrap, {
      canEdit: () => this.canEdit,
      emptyHint: '테이블이 없습니다 — 위의 "＋ 테이블" 로 시작하세요',
      // 캔버스가 선택 표시를 스스로 다시 그린다. 여기서는 프레즌스와 패널만 맞춘다.
      onSelect: (key) => this.select(key ? { kind: 'table', id: key } : null),
      onSelectLink: (id) => this.select({ kind: 'link', id }),
      onSelectNote: (id) => this.select({ kind: 'note', id }),
      onSelectGroup: (id) => this.select({ kind: 'group', id }),
      onMarks: (list) => this.selectMany(list),
      onMultiMove: (moves) => this.moveMany(moves),
      onCursorMove: (point) => this.sendCursor(point),
      onManualPan: () => this.stopFollow(),
      onTableMove: (key, x, y) => this.send('table.move', { key, x, y }),
      onNoteMove: (id, x, y) => this.send('note.update', { id, x, y }),
      onNoteResize: (id, w, h) => this.send('note.update', { id, w, h }),
      onGroupMove: (id, x, y) => this.send('group.update', { id, x, y }),
      onGroupResize: (id, w, h) => this.send('group.update', { id, w, h }),
      onToggleCollapse: (key, geom) => this.toggleCollapse(key, geom),
      // 메모·그룹 편집은 인스펙터에서 한다. 두 번 눌러도 같은 곳이 열리도록
      // 선택만 시킨다 — 편집기가 두 벌이면 한쪽만 고치게 된다.
      onEditNote: (note) => this.select({ kind: 'note', id: note.id }),
      onEditGroup: (group) => this.select({ kind: 'group', id: group.id }),
    });

    this.session = new ErdSession(docID, {
      onStatus: (s, delay) => this.onStatus(s, delay),
      onInit: (msg) => this.onInit(msg),
      onState: (msg) => this.onState(msg),
      onOp: (op, mine, hasDoc) => this.onOp(op, mine, hasDoc),
      onReject: (msg) => this.onReject(msg),
      onPresence: (list) => this.onPresence(list),
      onCursor: (msg) => this.onCursor(msg),
      onChat: (m) => this.onChat(m),
      onUndoState: (canUndo, canRedo) => this.onUndoState(canUndo, canRedo),
      onMeta: (msg) => this.onMeta(msg),
      onClosed: (reason) => this.onClosed(reason),
      onError: (message) => {
        toast(message, 'error');
        this.restoreRenamedSelection();
        // 거부된 편집의 미리보기가 화면에 남지 않게 문서를 다시 받는다.
        //
        // 편집 거부(reject)는 문서를 함께 보내므로 여기 오지 않는다. 여기 오는 것은
        // 권한·저장 실패처럼 문서가 오지 않는 오류이고, 그때 화면에는 낙관적으로
        // 반영한 미리보기가 그대로 남아 그 뒤의 편집이 줄줄이 거부된다.
        this.session.resync(true);
        this.renderCanvas();
        this.renderPanelIfIdle();
      },
    });

    this.sendCursor = throttle((point) => {
      this.session.presence({ cursor: point });
    }, 60);
  }

  start() {
    this.canvas.setDoc(this.doc);
    this.canvas.setTool(this.tool);
    this.canvas.fitView();
    // 타입 목록은 미리 받아 둔다. 고르개를 열 때 받으면 빈 창이 떴다가 채워지고,
    // 컬럼 줄의 "자동 증가" 설명(DB마다 이름이 다르다)도 그때까지 비어 있다.
    this.ensureTypeCatalog().then(() => {
      if (this.tab === 'table') this.renderPanelIfIdle();
    });
    this.renderToolbar();
    this.renderCanvas();
    this.renderPanel();
    this.bindPanel();
    this.bindPanelResize();
    this.bindShortcuts();
    this.bindPageLifecycle();
    this.session.connect();
  }

  stop() {
    this.session.close();
    this.canvas.destroy();
    if (this.unbind) this.unbind();
    if (this.unbindKeys) this.unbindKeys();
    if (this.unbindLifecycle) this.unbindLifecycle();
  }

  // bindPageLifecycle은 페이지를 떠날 때 소켓을 정리한다.
  //
  // 라우터의 정리 콜백은 앱 내부 이동만 잡는다. 브라우저가 다른 문서로 이동하면
  // 페이지가 BFCache에 들어가면서 WebSocket이 열린 채로 남을 수 있고, 그러면 서버는
  // 그 사람을 계속 편집 중으로 본다 — 다른 참여자에게 유령이 보인다.
  // pagehide/pageshow는 그 전환을 알려주는 유일한 신호다.
  // bindShortcuts는 Ctrl+Z / Ctrl+Shift+Z 를 받는다.
  //
  // 되돌리기는 버튼보다 단축키로 더 많이 쓰인다. document에 거는 이유는 캔버스가
  // 포커스를 갖지 않기 때문이다(SVG를 클릭해도 포커스는 body에 남는다).
  bindPanelResize() {
    attachPanelResize({
      root: this.ui.root,
      handle: this.ui.panelResize,
      storageKey: 'dbstudio.erd.panelWidth',
      onResize: () => this.canvas.render(),
    });
  }

  bindShortcuts() {
    const onKey = (e) => {
      // 아래 단축키는 모두 "캔버스를 보고 있을 때"의 것이다. 입력 중이거나 모달이
      // 열려 있으면 사용자의 주의는 그쪽에 있고, 거기서 가로채면 글자를 지우려던
      // 키가 테이블을 지운다.
      const busy = isTyping(document.body) || document.querySelector('.modal-overlay');

      // Esc: 선택 해제. 모달이 열려 있으면 모달이 먼저 닫혀야 한다.
      if (e.key === 'Escape' && !busy) {
        if (!this.marks.length) return;
        e.preventDefault();
        this.select(null);
        return;
      }
      // Delete/Backspace: 고른 것 지우기.
      if ((e.key === 'Delete' || e.key === 'Backspace') && !busy
        && !e.ctrlKey && !e.metaKey && !e.altKey) {
        if (!this.marks.length || !this.canEdit) return;
        e.preventDefault();
        this.deleteMarks();
        return;
      }
      // V / H: 마우스 도구. 그림 도구들이 쓰는 자리와 같게 둔다.
      if (!busy && !e.ctrlKey && !e.metaKey && !e.altKey) {
        const key = e.key.toLowerCase();
        if (key === 'v' || key === 'h') {
          e.preventDefault();
          this.setTool(key === 'v' ? 'select' : 'pan');
          return;
        }
      }
      if (!(e.ctrlKey || e.metaKey) || e.altKey) return;
      // Ctrl+A: 모두 고르기.
      if (e.key.toLowerCase() === 'a' && !busy) {
        e.preventDefault();
        this.selectAll();
        return;
      }
      const key = e.key.toLowerCase();
      const redo = key === 'y' || (key === 'z' && e.shiftKey);
      if (key !== 'z' && key !== 'y') return;
      // 입력 중에는 브라우저의 글자 되돌리기가 맞다. 여기서 가로채면
      // 이름을 고치다 오타를 지우려는 Ctrl+Z가 남의 테이블을 되살린다.
      if (isTyping(document.body)) return;
      // 모달이 열려 있으면 사용자의 주의는 그 안에 있다. 뒤에서 문서가
      // 바뀌면 무엇이 되돌아갔는지 볼 수 없다.
      if (document.querySelector('.modal-overlay')) return;
      if (!this.canEdit) return;
      e.preventDefault();
      if (redo) this.session.redo();
      else this.session.undo();
    };
    document.addEventListener('keydown', onKey);
    this.unbindKeys = () => document.removeEventListener('keydown', onKey);
  }

  bindPageLifecycle() {
    const onHide = () => this.session.suspend();
    const onShow = (e) => {
      if (e.persisted) this.session.resume();
    };
    window.addEventListener('pagehide', onHide);
    window.addEventListener('pageshow', onShow);
    this.unbindLifecycle = () => {
      window.removeEventListener('pagehide', onHide);
      window.removeEventListener('pageshow', onShow);
    };
  }

  // ---------- 소켓 이벤트 ----------

  onStatus(status, delay) {
    const chip = this.ui.statusChip;
    chip.classList.remove('is-ok', 'is-warn', 'is-bad');
    if (status === 'connected') {
      chip.classList.add('is-ok');
      chip.textContent = '실시간 연결됨';
      return;
    }
    if (status === 'reconnecting') {
      chip.classList.add('is-warn');
      chip.textContent = `재연결 중… (${Math.round((delay ?? 0) / 100) / 10}초 후)`;
      return;
    }
    chip.classList.add('is-bad');
    chip.textContent = '연결 끊김 — 편집은 재연결 후 전송됩니다';
  }

  onInit(msg) {
    this.you = msg.you;
    this.doc = msg.document;
    this.chat = msg.chat ?? [];
    this.participants = msg.participants ?? [];
    // 되돌리기 스택은 방보다 오래 산다. 새로고침해서 다시 들어와도 방금 한
    // 편집을 되돌릴 수 있어야 하므로, 첫 메시지에 상태가 실려 온다.
    this.canUndo = Boolean(msg.canUndo);
    this.canRedo = Boolean(msg.canRedo);
    this.renderAll();
  }

  onUndoState(canUndo, canRedo) {
    if (this.canUndo === canUndo && this.canRedo === canRedo) return;
    this.canUndo = canUndo;
    this.canRedo = canRedo;
    this.renderToolbar();
  }

  onState(msg) {
    this.doc = msg.document;
    // 서버 문서가 도착하면 미리보기는 끝났다(이 문서가 이름의 정답이다).
    this.renamedSel = null;
    // 선택한 테이블이 사라졌으면 선택을 해제한다. 없는 대상을 편집하는 패널이
    // 남아 있으면 모든 편집이 "찾을 수 없습니다"로 거부된다.
    this.pruneSelection();
    this.renderCanvas();
    this.renderPanelIfIdle();
  }

  onOp(op, mine, hasDoc) {
    if (hasDoc) return; // 상태가 함께 왔으므로 이미 반영됐다
    // 이동·메모 op는 서버가 op만 보낸다. 좌표/텍스트 대입은 틀릴 여지가 없어
    // 클라이언트가 직접 반영한다.
    applyLightOp(this.doc, op);
    const wasSelected = this.touchesSelection(op);
    // 고른 메모·그룹이 지워졌으면 선택을 놓는다. 없는 대상을 편집하는 패널이
    // 남아 있으면 그 뒤의 모든 편집이 "찾을 수 없습니다"로 거부된다.
    this.pruneSelection();
    this.renderCanvas();
    // 지금 인스펙터에 떠 있는 대상이 바뀌었으면 패널도 다시 그린다.
    //
    // 그리지 않으면 아이콘·색 고르개가 **고르기 전 상태**를 계속 표시한다.
    // 캔버스는 바뀌었는데 고르개에는 여전히 "없음"이 켜져 있으니, 사용자에게는
    // "눌러도 안 먹는다"로 보인다 — 실제로는 반영되어 있는데도.
    if (wasSelected) this.renderPanelIfIdle();
  }

  // touchesSelection은 이 op가 지금 고른 대상을 건드리는지 본다.
  //
  // 남이 저쪽 끝에서 카드를 옮길 때마다 패널을 다시 그리면, 내가 입력하던 칸이
  // 사라지거나 스크롤이 튄다.
  touchesSelection(op) {
    if (!this.sel) return false;
    const p = op.payload ?? {};
    switch (this.sel.kind) {
      case 'table':
        return op.kind === 'table.move' && p.key === this.sel.id;
      case 'note':
        return op.kind.startsWith('note.') && p.id === this.sel.id;
      case 'group':
        return op.kind.startsWith('group.') && p.id === this.sel.id;
      default:
        return false;
    }
  }

  onReject(msg) {
    toast(msg.reason, 'error', 6000);
    this.doc = msg.document;
    // 문서보다 먼저 되돌린다. 거부된 이름이 **다른 테이블의 이름**일 수 있고
    // (이름 충돌이 거부의 대표적인 이유다), 그러면 pruneSelection이 "있는 대상"으로
    // 보고 그대로 두어 남의 테이블을 고르고 있게 된다.
    this.restoreRenamedSelection();
    this.pruneSelection();
    this.renderCanvas();
    // 거절도 다른 갱신과 같은 규칙을 따른다. 여기서 곧바로 다시 그리면, 이름이
    // 겹쳐 거절된 순간 누르고 있던 버튼이 사라져 그 클릭이 사라진다 —
    // "컬럼 이름을 고치다 컬럼 추가를 누르면 추가가 안 되던" 경로가 이것이었다.
    this.renderPanelIfIdle();
  }

  // restoreRenamedSelection은 이름 미리보기 때문에 옮겨 둔 선택을 되돌린다.
  // 되돌릴 것이 없으면 아무 일도 하지 않는다.
  restoreRenamedSelection() {
    const moved = this.renamedSel;
    this.renamedSel = null;
    if (!moved) return;
    if (this.sel?.kind === 'table' && this.sel.id === moved.to) this.sel.id = moved.from;
  }

  onPresence(list) {
    this.participants = list;
    // 떠난 사람의 커서를 남겨두면 유령 커서가 화면에 붙어 있는다.
    const alive = new Set(list.map((p) => p.clientId));
    for (const id of [...this.remoteCursors.keys()]) {
      if (!alive.has(id)) this.remoteCursors.delete(id);
    }
    // 따라가던 사람이 나갔으면 따라가기도 끝난다. 남겨 두면 화면이 마지막
    // 위치에 멈춘 채 "따라가는 중"만 떠 있게 된다.
    if (this.following && !list.some((p) => p.clientId === this.following)) {
      this.following = null;
    }
    this.renderParticipants();
    this.renderFollowBar();
    this.renderCanvas();
  }

  onCursor(msg) {
    if (msg.clientId === this.you?.clientId) return;
    const p = this.participants.find((x) => x.clientId === msg.clientId);
    this.remoteCursors.set(msg.clientId, {
      x: msg.cursor.x, y: msg.cursor.y,
      color: p?.color ?? '#888', name: p?.userName ?? '',
    });
    if (this.following === msg.clientId) {
      this.canvas.centerOn(msg.cursor.x, msg.cursor.y);
    }
    this.renderCursors();
  }

  onChat(message) {
    this.chat.push(message);
    if (this.tab === 'chat' && this.chatMode === 'room') {
      this.renderPanel();
      return;
    }
    // 대화를 보고 있지 않으면 캔버스 위에 잠깐 띄운다.
    //
    // 알림을 아예 두지 않으면 다른 사람이 "이 이름 맞나요?"라고 물어도 아무도
    // 모른 채 편집이 계속된다. 반대로 대화 탭을 강제로 열면 지금 하던 일이 끊긴다.
    // 스택은 그 사이다 — 보이지만 방해하지 않는다.
    this.pushChatToast(message);
  }

  // pushChatToast는 캔버스 오른쪽 위에 메시지 하나를 띄운다.
  pushChatToast(message) {
    if (!this.ui.chatStack) return;
    // 내가 보낸 것은 띄우지 않는다. 방금 친 문장을 다시 읽을 이유가 없다.
    if (message.userId && message.userId === this.you?.userId) return;

    const card = h('button.erd-chat-toast', {
      type: 'button',
      title: '대화 열기',
      onclick: () => {
        card.remove();
        this.tab = 'chat';
        this.chatMode = 'room';
        this.renderToolbar();
        this.renderPanel();
      },
    },
      h('div.erd-chat-toast-head', {},
        h('strong', {}, message.userName || '알 수 없음'),
        message.targetKey ? badge(truncate(message.targetKey, 18), 'neutral') : null,
      ),
      h('p', {}, truncate(message.body ?? '', 140)),
    );
    this.ui.chatStack.appendChild(card);

    // 쌓이면 아래가 캔버스를 다 덮는다. 오래된 것부터 지운다.
    while (this.ui.chatStack.children.length > 4) {
      this.ui.chatStack.firstChild.remove();
    }
    // 읽지 않아도 사라진다. 남겨 두면 결국 "닫기"를 눌러야 하는 일이 늘어난다.
    setTimeout(() => card.remove(), 12000);
  }

  onMeta(msg) {
    this.doc.name = msg.name;
    this.doc.status = msg.status;
    const title = this.ui.root.querySelector('.erd-title');
    if (title) title.textContent = msg.name;
  }

  onClosed(reason) {
    toast(reason || '문서가 닫혔습니다', 'error', 0);
    this.onStatus('disconnected');
  }

  // ---------- op 보내기 ----------

  send(kind, payload, batch = '') {
    if (!this.canEdit) {
      toast('이 문서를 편집할 권한이 없습니다', 'error');
      return null;
    }
    return this.session.send(kind, payload, batch);
  }

  // ---------- 렌더 ----------

  renderAll() {
    this.renderToolbar();
    this.renderParticipants();
    this.renderCanvas();
    this.renderPanel();
  }

  renderToolbar() {
    // 대상 DB가 없는 초안에서는 "지금 DB와 비교"와 "마이그레이션"이 성립하지 않는다.
    // 눌러 보고서야 거부되는 버튼은 왜 안 되는지를 남기지 않으므로 아예 그리지 않고,
    // 대신 SQL 내보내기가 그 자리를 대신한다.
    const hasTarget = Boolean(this.connection);
    mount(this.ui.toolbar,
      // 마우스 도구를 맨 앞에 둔다. "지금 끌면 무엇이 되는가"는 다른 어떤 단추보다
      // 자주 바뀌는 상태이고, 그것을 모르면 캔버스에서 하는 모든 동작이 불안해진다.
      h('div.erd-tool-group', {},
        this.mouseToolBtn('cursor', 'select', '선택 도구 (V) — 빈 곳을 끌어 여러 개 고르기'),
        this.mouseToolBtn('move', 'pan',
          '화면 이동 도구 (H) — 끌면 화면이 움직입니다. 카드는 눌러서 고르기만 됩니다'),
      ),
      // 되돌리기는 그 다음이다. 실수를 되돌리는 손은 급하다.
      h('div.erd-tool-group', {},
        this.toolBtn('undo', '되돌리기 (Ctrl+Z)', () => this.session.undo(),
          { needsEdit: true, disabled: !this.canUndo }),
        this.toolBtn('redo', '다시실행 (Ctrl+Shift+Z)', () => this.session.redo(),
          { needsEdit: true, disabled: !this.canRedo }),
      ),
      h('div.erd-tool-group', {},
        // 확대(＋)와 같은 아이콘이면 도구 막대에서 둘을 구분할 수 없다.
        this.toolBtn('table-plus', '테이블 추가', () => this.addTable(), { needsEdit: true }),
        this.toolBtn('edit', '메모 추가', () => this.addNote(), { needsEdit: true }),
        this.toolBtn('list', '그룹 추가', () => this.addGroup(), { needsEdit: true }),
      ),
      h('div.erd-tool-group', {},
        this.toolBtn('minus', '축소', () => this.zoom(1.25)),
        this.toolBtn('plus', '확대', () => this.zoom(0.8)),
        this.toolBtn('maximize', '화면에 맞추기', () => { this.fitView(); this.renderCanvas(); }),
      ),
      h('div.erd-tool-group', {},
        this.toolBtn('database', 'SQL 불러오기', () => this.importSQL(), { needsEdit: true }),
        this.toolBtn('save', 'SQL 내보내기', () => this.exportSQL()),
        this.toolBtn('image', '사진으로 내보내기',
          () => openImageExportDialog(this.canvas, this.doc.name || '설계')),
      ),
      h('div.erd-tool-group', {},
        !hasTarget ? null : this.toolBtn('activity', '변경 비교', () => this.showDiff()),
        !hasTarget ? null : this.toolBtn('play', '마이그레이션 만들기',
          () => this.createMigration(), { needsEdit: true }),
        this.toolBtn('history', '편집 이력', () => this.showHistory()),
        this.toolBtn('settings', '문서 설정', () => this.openSettings()),
      ),
      h('div.erd-tool-spacer'),
      h('div.erd-tool-group', {},
        h('button.btn.btn-small', {
          type: 'button',
          class: this.tab === 'table' ? 'btn btn-small btn-active' : 'btn btn-small',
          onclick: () => { this.tab = 'table'; this.renderToolbar(); this.renderPanel(); },
        }, '속성'),
        // 도메인은 특정 테이블에 매이지 않는다. 그래서 선택과 무관하게 언제나
        // 열 수 있는 탭이어야 하고, 컬럼을 고치다가 바로 건너갈 수 있어야 한다.
        h('button.btn.btn-small', {
          type: 'button',
          class: this.tab === 'domain' ? 'btn btn-small btn-active' : 'btn btn-small',
          onclick: () => {
            this.tab = 'domain';
            this.renderToolbar();
            this.renderPanel();
            this.ensureTypeCatalog();
          },
        }, '도메인'),
        h('button.btn.btn-small', {
          type: 'button',
          class: this.tab === 'chat' ? 'btn btn-small btn-active' : 'btn btn-small',
          onclick: () => {
            this.tab = 'chat';
            this.renderToolbar();
            this.renderPanel();
            this.loadAISessions();
          },
        }, '대화'),
        // 사이드바를 접으면 캔버스가 340px 넓어진다. 테이블이 스무 개를 넘어가면
        // 그 폭이 "한 화면에 다 보이는가"를 가른다.
        this.toolBtn(this.panelHidden ? 'chevron-left' : 'chevron-right',
          this.panelHidden ? '사이드바 보이기' : '사이드바 숨기기',
          () => this.togglePanel()),
      ),
    );
  }

  // toolBtn은 아이콘만 있는 도구 버튼이다.
  //
  // 이름을 글자로 늘어놓으면 도구 막대가 두 줄이 되고, 캔버스가 그만큼 낮아진다.
  // 대신 이름은 popover로 뜬다(CSS ::after) — 브라우저 기본 툴팁은 1초 가까이
  // 기다려야 하고, 도구 막대에서는 그 사이에 이미 다른 버튼을 찾고 있다.
  // mouseToolBtn은 지금 켜진 마우스 도구를 보여주는 토글 단추다.
  mouseToolBtn(iconName, tool, label) {
    return h('button.icon-btn.btn-tip', {
      type: 'button',
      class: this.tool === tool ? 'is-on' : '',
      'data-tip': label,
      'aria-label': label,
      'aria-pressed': String(this.tool === tool),
      onclick: () => this.setTool(tool),
    }, icon(iconName, 15));
  }

  // setTool은 마우스 도구를 바꾼다.
  setTool(tool) {
    if (this.tool === tool) return;
    this.tool = tool;
    writeTool(tool);
    this.canvas.setTool(tool);
    this.renderToolbar();
  }

  toolBtn(iconName, label, onclick, { needsEdit = false, disabled = false } = {}) {
    return h('button.icon-btn.btn-tip', {
      type: 'button',
      disabled: disabled || (needsEdit && !this.canEdit),
      'data-tip': label,
      'aria-label': label,
      onclick,
    }, icon(iconName, 15));
  }

  // togglePanel은 오른쪽 사이드바를 접었다 편다.
  togglePanel() {
    this.panelHidden = !this.panelHidden;
    this.ui.root.classList.toggle('is-panel-hidden', this.panelHidden);
    this.renderToolbar();
    // 캔버스 폭이 바뀌었으므로 보이는 범위를 다시 잡는다.
    this.canvas.render();
  }

  // openSettings는 문서 자체의 설정을 고친다(이름·메모).
  //
  // 캔버스 위가 아니라 모달인 이유: 자주 바뀌지 않는 값이고, 도구 막대에 늘 펼쳐
  // 두면 정작 자주 쓰는 도구가 밀린다.
  openSettings() {
    const nameInput = input({ value: this.doc.name ?? '', disabled: !this.canManage });
    const noteBox = h('textarea.input.textarea', {
      rows: 4, value: this.note, disabled: !this.canManage,
      placeholder: '이 초안의 목적이나 결정 사항을 적어 두면 나중에 읽는 사람이 덜 묻습니다',
    });

    openModal({
      title: 'ERD 설계 설정',
      width: 520,
      body: () => [
        h('label.field', {}, h('span.field-label', {}, '이름'), nameInput),
        h('label.field', {}, h('span.field-label', {}, '메모'), noteBox),
        h('dl.mig-meta', {},
          h('div.meta-row', {}, h('dt', {}, '대상 DB'),
            h('dd', {}, this.connection ? this.connection.name : '없음 (독립 초안)')),
          h('div.meta-row', {}, h('dt', {}, 'DB 문법'), h('dd', {}, kindLabel(this.doc.dialect))),
          h('div.meta-row', {}, h('dt', {}, '상태'), h('dd', {}, this.doc.status)),
        ),
        this.canManage
          ? null
          : h('p.notice.notice-info', {}, icon('lock'),
            '문서 설정은 만든 사람과 관리 권한이 있는 사람만 바꿀 수 있습니다.'),
      ],
      footer: (close) => [
        h('button.btn', { type: 'button', onclick: close }, this.canManage ? '취소' : '닫기'),
        this.canManage
          ? h('button.btn.btn-primary', {
            type: 'button',
            onclick: async () => {
              try {
                const res = await api.patch(`/erd/documents/${encodeURIComponent(this.docID)}`, {
                  name: nameInput.value.trim(),
                  note: noteBox.value,
                });
                this.doc.name = res.document?.name ?? nameInput.value.trim();
                this.note = res.document?.note ?? noteBox.value;
                const title = this.ui.root.querySelector('.erd-title');
                if (title) mount(title, this.doc.name);
                close();
                toast('문서 설정을 저장했습니다', 'success');
              } catch (err) {
                toastError(err);
              }
            },
          }, '저장')
          : null,
      ].filter(Boolean),
    });
  }

  renderParticipants() {
    // 아바타는 누를 수 있다. 누르면 그 사람의 화면을 따라간다 —
    // "지금 무엇을 고치고 있는지"를 물어보지 않고 볼 수 있게 하는 것이 목적이다.
    //
    // 이름은 popover로 띄운다. 브라우저 기본 툴팁(title)은 1초 가까이 기다려야 하는데,
    // 여기서 알고 싶은 것은 "이 동그라미가 누구인가" 하나뿐이라 그 사이가 길다.
    mount(this.ui.participants, this.participants.map((p) => {
      const isMe = p.clientId === this.you?.clientId;
      const following = this.following === p.clientId;
      const tip = `${p.userName}${isMe ? ' (나)' : ''}${p.canEdit ? '' : ' · 읽기 전용'}`;
      return h('button.erd-avatar', {
        type: 'button',
        style: { background: p.color },
        class: following ? 'is-following' : '',
        'data-tip': isMe ? tip : `${tip} — 눌러서 따라가기`,
        'aria-label': tip,
        disabled: isMe,
        onclick: () => this.toggleFollow(p),
      }, (p.userName || '?').slice(0, 1));
    }));
  }

  // toggleFollow는 참여자 따라가기를 켜고 끈다.
  //
  // 따라가는 동안에는 그 사람의 커서가 움직일 때마다 화면이 그 지점을 가운데로 옮긴다.
  // 내가 캔버스를 직접 끌면 따라가기가 풀린다 — 두 힘이 화면을 동시에 당기면
  // 어느 쪽도 원하는 곳을 볼 수 없다.
  toggleFollow(p) {
    if (p.clientId === this.you?.clientId) return;
    this.following = this.following === p.clientId ? null : p.clientId;
    this.renderParticipants();
    this.renderFollowBar();
    if (this.following) {
      const cursor = this.remoteCursors.get(this.following);
      if (cursor) this.canvas.centerOn(cursor.x, cursor.y);
    }
  }

  stopFollow() {
    if (!this.following) return;
    this.following = null;
    this.renderParticipants();
    this.renderFollowBar();
  }

  renderFollowBar() {
    const bar = this.ui.followBar;
    if (!bar) return;
    const p = this.participants.find((x) => x.clientId === this.following);
    if (!p) {
      bar.hidden = true;
      mount(bar);
      return;
    }
    bar.hidden = false;
    mount(bar,
      h('span.erd-follow-dot', { style: { background: p.color } }),
      h('span', {}, `${p.userName} 따라가는 중`),
      h('button.btn.btn-small', { type: 'button', onclick: () => this.stopFollow() }, '해제'),
    );
  }

  // previewCanvas는 아직 확정하지 않은 편집을 캔버스에만 미리 반영한다.
  //
  // 문서를 직접 고치는 것이 불안해 보이지만, 여기서 바뀐 값은 곧 서버가 보내는
  // 문서로 덮어써진다(확정하면 op가 가고, 취소하면 다음 상태 동기화가 되돌린다).
  // 그 사이 몇 초 동안 화면이 내가 친 글자를 따라오게 하는 것이 이 함수의 전부다.
  previewCanvas(mutate) {
    mutate();
    this.renderCanvas();
  }

  // previewTableName은 이름 미리보기를 **문서 전체에** 반영한다.
  //
  // 이름만 바꾸면 안 되는 이유: 이 문서에서 테이블을 가리키는 것은 이름으로 만든
  // 키(`namespace.name`)다. 배치(layout)와 이 테이블을 참조하는 외래키가 모두 그 키를
  // 쓰므로, 이름만 갈아 두면 그 키로 찾을 배치가 사라진다 — 캔버스는 배치가 없는
  // 카드를 기본 자리(80,80)에 그리므로 **글자를 칠 때마다 카드가 튀어 보였다**.
  // 관계선도 참조하는 테이블을 못 찾아 함께 사라진다.
  //
  // 서버의 table.update가 하는 일(레이아웃 키 이동 + 참조 갱신)과 같은 것을 여기서도
  // 한다. 몇 초 뒤 서버 문서가 이 값을 덮어쓰지만, 그 몇 초가 사용자가 이름을 치는
  // 시간의 전부다.
  //
  // 테이블을 **매번 문서에서 다시 찾는** 이유(ref): 이름을 확정하면 서버가 문서 전체를
  // 보내고 this.doc이 바뀐다. 그 뒤에도 패널이 처음 잡아 둔 객체를 고치면, 이름은
  // 화면에 없는 유령 객체에서 바뀌고 배치 키만 살아 있는 문서에서 옮겨진다 —
  // 백스페이스로 이름을 고칠 때 카드가 다시 튀던 것이 이 어긋남이었다.
  previewTableName(ref, value) {
    const tbl = ref.table();
    if (!tbl) return;
    // 빈 이름은 화면에 반영하지 않는다. 지우고 다시 치는 그 순간에 캔버스에
    // **이름 없는 카드**가 남고, 그 카드는 어느 테이블인지 알 수 없다.
    // 서버로도 보내지 않으므로(빈 이름은 거부된다) 화면만 옛 이름을 지키면 된다.
    if (!value.trim()) return;
    this.previewCanvas(() => {
      const oldKey = tableKey(tbl);
      tbl.name = value;
      const newKey = tableKey(tbl);
      // ref는 "지금 문서에서의 키"를 들고 있어야 다음 미리보기가 이 테이블을 찾는다.
      ref.key = newKey;
      if (newKey === oldKey) return;

      const box = this.doc.layout?.[oldKey];
      if (box) {
        delete this.doc.layout[oldKey];
        this.doc.layout[newKey] = box;
      }
      // 이 테이블을 가리키는 외래키의 대상 이름도 함께 옮긴다.
      for (const other of this.doc.schema?.tables ?? []) {
        for (const fk of other.foreignKeys ?? []) {
          if (refKey(other, fk) !== oldKey) continue;
          fk.refTable = tbl.name;
          if (fk.refNamespace) fk.refNamespace = tbl.namespace ?? '';
        }
      }
      // 선택도 새 키를 가리키게 한다. 그러지 않으면 서버 문서가 도착하는 순간
      // "없는 테이블을 고르고 있다"가 되어 편집 중이던 패널이 닫힌다.
      //
      // 원래 키를 기억해 두는 이유: 이름이 거부될 수 있고(이미 쓰는 이름),
      // 그때 선택이 새 키에 남아 있으면 **그 이름을 쓰던 다른 테이블**을 고른 것이
      // 되어 다음 편집이 엉뚱한 테이블에 간다. 거부되면 여기로 되돌린다.
      if (this.sel?.kind === 'table' && this.sel.id === oldKey) {
        this.renamedSel = { from: this.renamedSel?.to === oldKey ? this.renamedSel.from : oldKey, to: newKey };
        this.sel.id = newKey;
      }
    });
    // 패널 제목만 제자리에서 갈아 끼운다. 패널을 다시 그리면 지금 입력하던 칸의
    // 포커스와 커서 위치가 사라지므로(그래서 renderPanelIfIdle이 있다),
    // 여기서는 글자만 바꾼다.
    const head = this.ui.panel?.querySelector('.erd-panel-head h2');
    if (head) head.textContent = truncate(tableDisplay(tbl), 26);
  }

  // 캔버스 다시 그리기. 데이터 갱신과 그리기를 한 곳에 묶어, 이 메서드만 부르면
  // 화면이 항상 현재 문서와 같아지게 한다.
  renderCanvas() {
    this.canvas.setDoc(this.doc);
    this.canvas.setMarks(this.marks);
    this.canvas.setParticipants(this.participants, this.you?.clientId);
    this.canvas.render();
  }

  renderCursors() {
    this.canvas.renderCursors();
  }

  fitView() {
    this.canvas.setDoc(this.doc);
    this.canvas.fitView();
  }

  zoom(factor) {
    this.canvas.zoom(factor);
  }

  // ---------- 사이드 패널 ----------

  // renderPanelIfIdle은 **타이핑 중일 때만** 패널을 다시 그리지 않는다.
  //
  // 지키려는 것은 "적고 있던 글자"다. 그래서 판정 대상도 글자를 받는 칸으로 좁힌다 —
  // 포커스가 패널 안에 있기만 하면 건너뛰게 두면 버튼을 누른 뒤에도 건너뛴다.
  // 컬럼을 추가하거나 인덱스를 지운 직후가 정확히 그 상황이고(누른 버튼이 패널 안에
  // 있으므로), 그러면 방금 한 일이 화면에 나타나지 않는다. 다시 노드를 눌러야
  // 보이던 증상이 이것이었다.
  renderPanelIfIdle() {
    // 버튼을 누르고 있는 동안에도 다시 그리지 않는다.
    //
    // 이름 칸을 고치다가 패널의 버튼을 누르면 이 순서로 일이 벌어진다:
    // mousedown → 칸이 blur → 이름 op 전송 → (서버 왕복 몇 ms) → 문서가 도착해
    // 패널을 다시 그림 → 누르고 있던 버튼이 사라짐 → mouseup 이 다른 요소에서
    // 일어나 **click 이 끝내 발생하지 않는다.** "타이핑하다 컬럼 추가를 누르면
    // 스크롤만 움직이고 추가가 안 되던" 증상이 이것이다.
    //
    // 그래서 포인터가 눌려 있는 동안은 미뤘다가, 떼는 순간 반영한다.
    if (this.panelPressed || isTyping(this.ui.panel)) {
      this.panelDirty = true;
      return;
    }
    this.renderPanel();
  }

  // tableKey는 지금 고른 테이블의 키다(테이블이 아니면 null).
  // 프레즌스와 채팅 대상은 여전히 테이블 단위다.
  get tableKey() {
    return this.sel?.kind === 'table' ? this.sel.id : null;
  }

  // marksOf는 함께 고른 것 중 그 종류만 골라 낸다.
  marksOf(kind) {
    return this.marks.filter((m) => m.kind === kind).map((m) => m.id);
  }

  // selectMany는 여럿을 함께 고른다(범위 선택·Shift 클릭).
  selectMany(list) {
    this.marks = (list ?? []).slice();
    this.sel = this.marks.length ? this.marks[this.marks.length - 1] : null;
    this.session.presence({ selection: this.tableKey ?? '' });
    this.renderCanvas();
    this.renderPanel();
  }

  // selectAll은 캔버스 위의 것을 모두 고른다(Ctrl+A).
  //
  // 관계선은 넣지 않는다. 옮길 수도 지울 수도 없어서 수만 늘린다.
  selectAll() {
    const marks = [
      ...(this.doc.schema?.tables ?? []).map((t) => ({ kind: 'table', id: tableKey(t) })),
      ...(this.doc.notes ?? []).map((n) => ({ kind: 'note', id: n.id })),
      ...(this.doc.groups ?? []).map((g) => ({ kind: 'group', id: g.id })),
    ];
    if (!marks.length) return;
    this.selectMany(marks);
  }

  // moveMany는 함께 옮긴 결과를 op로 보낸다.
  //
  // 한 묶음(batch)으로 보내는 이유: 되돌리기가 이 이동 전체를 한 번에 되돌려야
  // 한다. 묶지 않으면 다섯 장을 한 번 옮긴 것을 다섯 번 되돌려야 하고, 그동안
  // 화면에는 아무도 만든 적 없는 중간 배치가 남는다.
  moveMany(moves) {
    if (!moves?.length) return;
    const batch = newLocalID();
    for (const m of moves) {
      if (m.kind === 'table') this.send('table.move', { key: m.id, x: m.x, y: m.y }, batch);
      else if (m.kind === 'note') this.send('note.update', { id: m.id, x: m.x, y: m.y }, batch);
      else if (m.kind === 'group') this.send('group.update', { id: m.id, x: m.x, y: m.y }, batch);
    }
  }

  // select는 선택을 바꾸고 화면과 프레즌스를 맞춘다.
  select(sel) {
    this.sel = sel;
    this.marks = sel ? [sel] : [];
    this.session.presence({ selection: this.tableKey ?? '' });
    this.renderCanvas();
    this.renderPanel();
  }

  // pruneSelection은 사라진 대상을 가리키는 선택을 놓는다.
  //
  // 없는 것을 편집하는 패널이 남아 있으면 모든 편집이 "찾을 수 없습니다"로 거부되고,
  // 사용자는 자기 입력이 잘못된 줄 안다.
  pruneSelection() {
    // 함께 고른 것 중 사라진 것도 치운다. 남겨 두면 "3개 선택됨"이라고 해 놓고
    // 지우기·정렬이 조용히 하나를 빠뜨린다.
    if (this.marks.length) {
      this.marks = this.marks.filter((m) => this.markAlive(m));
      if (this.marks.length && !this.marks.some((m) => m.kind === this.sel?.kind && m.id === this.sel?.id)) {
        this.sel = this.marks[this.marks.length - 1];
      }
    }
    if (!this.sel) return;
    const gone = {
      table: () => !this.findTable(this.sel.id),
      link: () => !this.findFK(this.sel.id),
      note: () => !(this.doc.notes ?? []).some((n) => n.id === this.sel.id),
      group: () => !(this.doc.groups ?? []).some((g) => g.id === this.sel.id),
    }[this.sel.kind];
    if (gone && gone()) this.sel = null;
  }

  // markAlive는 고른 대상이 아직 문서에 있는지다.
  markAlive(mark) {
    if (mark.kind === 'table') return Boolean(this.findTable(mark.id));
    if (mark.kind === 'note') return (this.doc.notes ?? []).some((n) => n.id === mark.id);
    if (mark.kind === 'group') return (this.doc.groups ?? []).some((g) => g.id === mark.id);
    return Boolean(this.findFK(mark.id));
  }

  // findFK는 "테이블키.외래키이름" 으로 외래키를 찾는다.
  findFK(id) {
    const at = String(id ?? '').lastIndexOf('.');
    if (at < 0) return null;
    const table = this.findTable(id.slice(0, at));
    if (!table) return null;
    const name = id.slice(at + 1);
    const fk = (table.foreignKeys ?? []).find((f) => f.name === name);
    return fk ? { table, fk } : null;
  }

  renderPanel() {
    this.panelDirty = false;
    if (this.tab === 'domain') {
      mount(this.ui.panel, this.domainView());
      return;
    }
    if (this.tab === 'chat') {
      mount(this.ui.panel, this.chatView());
      const list = this.ui.panel.querySelector('.erd-chat-list');
      if (list) list.scrollTop = list.scrollHeight;
      return;
    }
    // 여럿을 골랐으면 하나를 고치는 대신 **함께 할 수 있는 일**을 보여준다.
    // 이때 하나짜리 편집기를 그대로 두면 "3개 선택됨"이라고 해 놓고 그중 한 개의
    // 이름 칸이 열려, 무엇을 고치고 있는지 알 수 없다.
    if (this.marks.length > 1) {
      mount(this.ui.panel, this.multiView());
      return;
    }
    // 무엇을 골랐느냐에 따라 다른 편집기를 연다. 간선·메모·그룹도 같은 자리에서
    // 고치게 하는 것이 요점이다 — 대상마다 다른 모달을 띄우면 "지금 무엇을 고르고
    // 있었는지"가 화면에서 사라진다.
    if (this.sel?.kind === 'link') {
      const found = this.findFK(this.sel.id);
      if (found) {
        mount(this.ui.panel, this.linkView(found.table, found.fk));
        return;
      }
    }
    if (this.sel?.kind === 'note') {
      const note = (this.doc.notes ?? []).find((n) => n.id === this.sel.id);
      if (note) {
        mount(this.ui.panel, this.noteView(note));
        return;
      }
    }
    if (this.sel?.kind === 'group') {
      const group = (this.doc.groups ?? []).find((g) => g.id === this.sel.id);
      if (group) {
        mount(this.ui.panel, this.groupView(group));
        return;
      }
    }

    const tbl = this.tableKey ? this.findTable(this.tableKey) : null;
    if (!tbl) {
      mount(this.ui.panel, h('div.erd-panel-empty', {},
        icon('database', 24),
        h('p', {}, '테이블을 선택하면 컬럼과 제약을 편집할 수 있습니다.'),
        h('p.muted', {}, '관계선·메모·그룹을 눌러도 여기서 고칠 수 있습니다'),
        h('p.muted', {}, '캔버스를 끌어 이동, 휠로 확대/축소, 카드를 두 번 누르면 접기'),
      ));
      return;
    }
    // 스크롤 위치를 지킨다. 컬럼이 스무 줄인 표에서 한 줄을 고칠 때마다 목록이
    // 맨 위로 튀면, 고치던 줄을 매번 다시 찾아야 한다.
    const keepTop = this.ui.panel.querySelector('.erd-panel-body')?.scrollTop ?? 0;
    mount(this.ui.panel, this.tableView(tbl));
    if (keepTop) {
      const body = this.ui.panel.querySelector('.erd-panel-body');
      if (body) body.scrollTop = keepTop;
    }

    // 방금 만든 컬럼이 있으면 그 이름 칸으로 커서를 옮긴다.
    // 추가 버튼만 누르고 이름을 못 고치면 col_1 이 그대로 남는다.
    if (this.focusColumn) {
      const el = this.ui.panel.querySelector(`.erd-col-name[data-col="${CSS.escape(this.focusColumn)}"]`);
      this.focusColumn = null;
      if (el) {
        el.focus();
        el.select();
      }
    }
  }

  // linkView는 관계선(외래키)을 인스펙터에서 고친다.
  //
  // 여기에는 자주 만지는 것(ON DELETE·ON UPDATE)만 둔다. 이름·컬럼 짝·참조 대상을
  // 바꾸는 것은 창에서 한다 — 그것들은 서로 맞물려 있어서(참조 키가 몇 컬럼인지에
  // 따라 짝의 수가 정해진다) 한 칸씩 바꿀 수 있게 두면 중간에 성립하지 않는 상태를
  // 지나간다.
  linkView(tbl, fk) {
    const key = tableKey(tbl);
    const ro = !this.canEdit;
    const actions = ['', 'NO ACTION', 'RESTRICT', 'CASCADE', 'SET NULL', 'SET DEFAULT'];
    const opts = actions.map((a) => ({ value: a, label: a || '(지정 안 함)' }));

    const onDelete = select(opts, { value: fk.onDelete ?? '', disabled: ro });
    const onUpdate = select(opts, { value: fk.onUpdate ?? '', disabled: ro });
    const patch = () => this.send('fk.update', {
      table: key, name: fk.name,
      onDelete: onDelete.value, onUpdate: onUpdate.value,
    });
    onDelete.addEventListener('change', patch);
    onUpdate.addEventListener('change', patch);

    return [
      this.panelHead(`관계 ${fk.name}`),
      h('div.erd-panel-body', {},
        h('dl.mig-meta', {},
          h('div.meta-row', {}, h('dt', {}, '기준'),
            h('dd', {}, `${tableDisplay(tbl)} (${(fk.columns ?? []).join(', ')})`)),
          h('div.meta-row', {}, h('dt', {}, '참조'),
            h('dd', {}, `${fk.refNamespace ? `${fk.refNamespace}.` : ''}${fk.refTable} (${(fk.refColumns ?? []).join(', ')})`)),
        ),
        h('label.field', {}, h('span.field-label', {}, 'ON DELETE'), onDelete),
        h('label.field', {}, h('span.field-label', {}, 'ON UPDATE'), onUpdate),
        h('p.field-help', {},
          '참조하는 행이 지워지거나 키가 바뀔 때 이 테이블의 행을 어떻게 할지 정합니다. ' +
          '비워 두면 DB 기본값(대개 NO ACTION)을 씁니다.'),
        ro ? null : h('div.erd-panel-actions', {},
          h('button.btn.btn-small', {
            type: 'button',
            onclick: () => this.openFKDialog(this.tableRef(tbl), fk),
          }, icon('edit', 13), ' 이름·컬럼 바꾸기')),
        ro ? null : h('div.erd-panel-danger', {},
          h('button.btn.btn-small.btn-danger', {
            type: 'button',
            onclick: async () => {
              const ok = await confirmDialog({
                title: '관계 삭제',
                message: `${tableDisplay(tbl)} 의 외래키 "${fk.name}" 을 지웁니다.`,
                confirmLabel: '삭제', danger: true,
              });
              if (!ok) return;
              this.send('fk.delete', { table: key, name: fk.name });
              this.select(null);
            },
          }, icon('trash'), '관계 삭제')),
      ),
    ];
  }

  noteView(note) {
    const ro = !this.canEdit;
    const box = h('textarea.input.textarea', { rows: 6, value: note.text ?? '', disabled: ro });
    commitOn(box, () => this.send('note.update', { id: note.id, text: box.value }));

    return [
      this.panelHead('메모'),
      h('div.erd-panel-body', {},
        h('label.field', {}, h('span.field-label', {}, '내용'), box),
        h('p.field-help', {}, '입력을 마치고 다른 곳을 누르면 저장됩니다'),
        h('div.field', {}, h('span.field-label', {}, '색'), this.tintPicker(
          note.color, (value) => this.send('note.update', { id: note.id, color: value }), ro)),
        ro ? null : h('div.erd-panel-danger', {},
          h('button.btn.btn-small.btn-danger', {
            type: 'button',
            onclick: () => { this.send('note.delete', { id: note.id }); this.select(null); },
          }, icon('trash'), '메모 삭제')),
      ),
    ];
  }

  groupView(group) {
    const ro = !this.canEdit;
    const nameInput = input({ value: group.label ?? '', disabled: ro });
    commitOn(nameInput, () =>
      this.send('group.update', { id: group.id, label: nameInput.value.trim() }));

    return [
      this.panelHead('그룹'),
      h('div.erd-panel-body', {},
        h('label.field', {}, h('span.field-label', {}, '이름'), nameInput),
        h('div.field', {}, h('span.field-label', {}, '색'), this.tintPicker(
          group.color, (value) => this.send('group.update', { id: group.id, color: value }), ro)),
        h('p.field-help', {}, '크기는 오른쪽 아래 모서리를 끌어 조절합니다'),
        ro ? null : h('div.erd-panel-danger', {},
          h('button.btn.btn-small.btn-danger', {
            type: 'button',
            onclick: () => { this.send('group.delete', { id: group.id }); this.select(null); },
          }, icon('trash'), '그룹 삭제')),
      ),
    ];
  }

  // panelHead는 인스펙터 머리글이다. 어느 대상을 고르든 같은 자리에 같은 모양으로
  // 제목과 선택 해제 버튼이 있어야 한다.
  panelHead(title) {
    return h('div.erd-panel-head', {},
      h('h2', {}, truncate(title, 26)),
      h('button.icon-btn', {
        type: 'button', title: '선택 해제', onclick: () => this.select(null),
      }, icon('x')),
    );
  }

  // multiView는 여럿을 고른 상태의 인스펙터다.
  multiView() {
    const ro = !this.canEdit;
    const tables = this.marksOf('table');
    const notes = this.marksOf('note');
    const groups = this.marksOf('group');
    const counts = [
      tables.length ? `테이블 ${tables.length}` : null,
      notes.length ? `메모 ${notes.length}` : null,
      groups.length ? `묶음 ${groups.length}` : null,
    ].filter(Boolean);

    const alignBtn = (how, label) => h('button.btn.btn-small', {
      type: 'button', disabled: ro, title: label,
      onclick: () => this.alignMarks(how),
    }, label);

    return [
      this.panelHead(`${this.marks.length}개 선택됨`),
      h('div.erd-panel-body', {},
        h('p.muted', {}, counts.join(' · ')),
        // 무엇을 골랐는지 이름으로 확인할 수 있어야 한다. 카드가 화면 밖에 있으면
        // 캔버스만 봐서는 셋 중 어느 것이 들어왔는지 알 수 없다.
        h('div.erd-mark-list', {}, this.marks.map((m) => h('button.erd-mark-chip', {
          type: 'button',
          title: '이것만 고르기',
          onclick: () => this.select(m),
        }, icon(m.kind === 'table' ? 'table' : m.kind === 'note' ? 'edit' : 'box', 12),
        h('span', {}, this.markLabel(m))))),

        h('h3.erd-sub', {}, '정렬'),
        h('div.erd-align-grid', {},
          alignBtn('left', '왼쪽'), alignBtn('right', '오른쪽'),
          alignBtn('top', '위'), alignBtn('bottom', '아래'),
          alignBtn('column', '세로로 쌓기'), alignBtn('row', '가로로 늘어놓기')),

        h('div.field', {}, h('span.field-label', {}, '색'),
          this.tintPicker(this.markColor(), (value) => this.paintMarks(value), ro),
          h('p.field-help', {}, '고른 것 전체의 색을 한 번에 바꿉니다')),

        ro ? null : h('div.erd-panel-actions', {},
          h('button.btn.btn-small', {
            type: 'button',
            disabled: !tables.length && !notes.length,
            onclick: () => this.groupMarks(),
          }, icon('box', 13), ' 묶음으로 감싸기'),
          h('button.btn.btn-small', {
            type: 'button',
            disabled: !tables.length,
            title: '고른 테이블을 자동 이름(_copy)으로 베낍니다',
            onclick: () => this.duplicateMarks(),
          }, icon('copy', 13), ` 테이블 ${tables.length}개 복제`)),

        ro ? null : h('div.erd-panel-danger', {},
          h('button.btn.btn-small.btn-danger', {
            type: 'button',
            onclick: () => this.deleteMarks(),
          }, '선택한 것 삭제'),
          h('p.field-help', {}, 'Delete 키로도 지웁니다')),

        h('p.field-help', {}, this.tool === 'select'
          ? 'Shift·Ctrl 클릭으로 더 고르거나 빼고, 빈 곳을 끌면 범위로 고릅니다'
            + '(Shift를 누르면 더하기). Ctrl 끌기는 화면 이동. Ctrl+A 모두 고르기, Esc 해제.'
          : 'Shift·Ctrl 클릭으로 더 고르거나 빼고, 빈 곳을 Shift(더하기)·Ctrl(새로 고르기)로 '
            + '끌면 범위로 고릅니다. Ctrl+A 모두 고르기, Esc 해제.'),
      ),
    ];
  }

  // markColor는 고른 것들이 모두 같은 색일 때 그 색이다. 다르면 null.
  markColor() {
    let seen = null;
    for (const m of this.marks) {
      const list = m.kind === 'note' ? this.doc.notes : this.doc.groups;
      const color = m.kind === 'table'
        ? (this.doc.layout?.[m.id]?.color ?? '')
        : ((list ?? []).find((x) => x.id === m.id)?.color ?? '');
      if (seen === null) seen = color;
      else if (seen !== color) return null;
    }
    return seen;
  }

  // markLabel은 고른 것을 한 줄로 부르는 이름이다.
  markLabel(mark) {
    if (mark.kind === 'table') {
      const tbl = this.findTable(mark.id);
      return tbl ? tableDisplay(tbl) : mark.id;
    }
    if (mark.kind === 'note') {
      const note = (this.doc.notes ?? []).find((n) => n.id === mark.id);
      const text = (note?.text ?? '').trim();
      return text ? truncate(text, 18) : '(빈 메모)';
    }
    const group = (this.doc.groups ?? []).find((g) => g.id === mark.id);
    return (group?.label ?? '').trim() || '(이름 없는 묶음)';
  }

  // tintPicker의 current가 null이면 아무 색도 켜지 않는다.
  //
  // 여럿을 골랐을 때 필요하다: 색이 서로 다른데 '기본'에 불이 들어와 있으면 화면이
  // 사실이 아닌 말을 하게 된다("지금 모두 기본색이다").
  tintPicker(current, onPick, ro) {
    return h('div.tint-picker', {}, TABLE_COLORS.map((c) => h('button.tint-swatch', {
      type: 'button',
      class: `${c.className}${current != null && (current || '') === c.value ? ' is-on' : ''}`,
      title: c.label,
      disabled: ro,
      onclick: () => onPick(c.value),
    })));
  }

  // tableRef는 인스펙터가 쓰는 **살아 있는 테이블 참조**다.
  //
  // 패널을 그릴 때 잡은 객체와 키를 그대로 쓰면 안 된다. 구조 편집이 확정되면 서버가
  // 문서 전체를 보내고 this.doc이 통째로 바뀌는데, 그 순간 잡아 둔 객체는 화면에
  // 그려지지 않는 유령이 되고(미리보기가 아무 데도 반영되지 않는다) 잡아 둔 키는
  // 이름이 바뀐 뒤에는 서버가 모르는 이름이 된다. 그래서 **키만** 들고 다니며
  // 필요할 때마다 문서에서 다시 찾는다.
  tableRef(tbl) {
    const key = tableKey(tbl);
    const ref = {
      ns: tbl.namespace ?? '',
      // key는 지금 문서에서의 키다(이름 미리보기 중에는 입력 중인 이름).
      key,
      // serverKey는 서버가 아는 키다. 편집 op는 언제나 이 키로 보낸다.
      serverKey: key,
      table: () => this.findTable(ref.key) ?? this.findTable(ref.serverKey),
      // renamed는 확정된 이름으로 두 키를 함께 옮긴다.
      renamed: (name) => {
        ref.serverKey = ref.ns ? `${ref.ns}.${name}`.toLowerCase() : name.toLowerCase();
        ref.key = ref.serverKey;
      },
    };
    return ref;
  }

  tableView(tbl) {
    const ref = this.tableRef(tbl);
    const ro = !this.canEdit;

    // 서버가 알고 있는 이름·키를 따로 들고 있는다(ref.serverKey).
    //
    // 미리보기가 이름을 글자마다 바꾸므로, 그것을 대상으로 쓰면 두 가지가
    // 어긋난다: 확정 비교가 언제나 "같다"가 되어 아무것도 보내지 않고, 다음
    // 편집은 서버가 모르는 이름을 고치라고 보낸다.
    const nameInput = input({ value: tbl.name, disabled: ro });
    commitOn(nameInput, () => {
      const next = nameInput.value.trim();
      if (!next) return;
      this.send('table.update', { key: ref.serverKey, name: next });
      ref.renamed(next);
    }, (value) => this.previewTableName(ref, value), { required: true });
    const commentInput = input({ value: tbl.comment ?? '', disabled: ro, placeholder: '설명' });
    commitOn(commentInput, () => {
      this.send('table.update', { key: ref.serverKey, comment: commentInput.value });
    });

    return [
      this.panelHead(tbl.name),
      h('div.erd-panel-body', {},
        h('label.field', {}, h('span.field-label', {}, '테이블 이름'), nameInput),
        h('label.field', {}, h('span.field-label', {}, '주석'), commentInput),
        ro ? null : this.appearanceEditor(ref),

        h('h3.erd-sub', {}, '컬럼', h('span.muted', {}, `${tbl.columns?.length ?? 0}개`),
          h('span.erd-sub-hint', {}, '열쇠 = 기본키 · ▲▼ 순서')),
        h('div.erd-col-list', {}, (tbl.columns ?? []).map((col, i, all) =>
          this.columnRow(ref, col, ro, i, all.length))),
        ro ? null : this.addColumnForm(ref),

        h('h3.erd-sub', {}, '인덱스', h('span.muted', {}, `${tbl.indexes?.length ?? 0}개`)),
        h('div.erd-chip-list', {}, (tbl.indexes ?? []).length === 0
          ? h('p.muted', {}, '없음')
          : (tbl.indexes ?? []).map((idx) => h('div.erd-chip', {},
            h('span', {}, `${idx.unique ? 'UNIQUE ' : ''}${idx.name}`),
            // 순서와 정렬 방향까지 보여준다. 복합 인덱스는 앞뒤 순서가 성능을
            // 가르므로, 목록에서 그것이 안 보이면 창을 열어야만 알 수 있다.
            h('span.muted', {}, (idx.columns ?? [])
              .map((c) => `${c.column || c.expression}${c.descending ? ' ↓' : ''}`).join(', ')),
            idx.where ? badge('부분', 'neutral') : null,
            ro ? null : h('button.icon-btn', {
              type: 'button', title: '인덱스 설정',
              onclick: () => this.openIndexDialog(ref, idx),
            }, icon('edit', 13)),
            ro ? null : h('button.icon-btn', {
              type: 'button', title: '삭제',
              onclick: () => this.send('index.delete', { table: ref.serverKey, name: idx.name }),
            }, icon('trash', 13)),
          ))),
        ro ? null : h('button.btn.btn-small', {
          type: 'button', onclick: () => this.openIndexDialog(ref),
        }, icon('plus'), '인덱스 추가'),

        h('h3.erd-sub', {}, '외래키', h('span.muted', {}, `${tbl.foreignKeys?.length ?? 0}개`)),
        h('div.erd-chip-list', {}, (tbl.foreignKeys ?? []).length === 0
          ? h('p.muted', {}, '없음')
          : (tbl.foreignKeys ?? []).map((fk) => h('div.erd-chip', {},
            h('span', {}, fk.name),
            h('span.muted', {}, `${(fk.columns ?? []).join(', ')} → ${fk.refTable}(${(fk.refColumns ?? []).join(', ')})`),
            // NO ACTION 은 배지로 달지 않는다. DB 기본값이고 생성되는 DDL에도 적히지
            // 않으므로, 배지로 두면 아무것도 정하지 않은 외래키가 가장 시끄러워진다.
            ...fkActionBadges(fk),
            ro ? null : h('button.icon-btn', {
              type: 'button', title: '외래키 설정',
              onclick: () => this.openFKDialog(ref, fk),
            }, icon('edit', 13)),
            ro ? null : h('button.icon-btn', {
              type: 'button', title: '삭제',
              onclick: () => this.send('fk.delete', { table: ref.serverKey, name: fk.name }),
            }, icon('trash', 13)),
          ))),
        ro ? null : h('button.btn.btn-small', {
          type: 'button', onclick: () => this.openFKDialog(ref),
        }, icon('plus'), '외래키 추가'),

        h('h3.erd-sub', {}, '체크 제약'),
        h('div.erd-chip-list', {}, (tbl.checks ?? []).length === 0
          ? h('p.muted', {}, '없음')
          : (tbl.checks ?? []).map((ck) => h('div.erd-chip', {},
            h('span', {}, ck.name),
            h('code.muted', {}, truncate(ck.expression, 40)),
            ro ? null : h('button.icon-btn', {
              type: 'button', title: '삭제',
              onclick: () => this.send('check.delete', { table: ref.serverKey, name: ck.name }),
            }, icon('trash', 13)),
          ))),
        ro ? null : h('button.btn.btn-small', {
          type: 'button', onclick: () => this.openCheckDialog(ref),
        }, icon('plus'), '체크 제약 추가'),

        // 복제는 삭제 옆이 아니라 그 위에 둔다. 되돌릴 수 있는 동작과 위험한 동작을
        // 나란히 두면 손이 미끄러지는 자리가 된다.
        ro ? null : h('div.erd-panel-actions', {},
          h('button.btn.btn-small', {
            type: 'button', onclick: () => this.openDuplicateDialog(ref),
          }, icon('copy', 13), ' 테이블 복제')),

        ro ? null : h('div.erd-panel-danger', {},
          h('button.btn.btn-small.btn-danger', {
            type: 'button', onclick: () => this.deleteTable(ref),
          }, icon('trash'), '테이블 삭제'),
        ),
      ),
    ];
  }

  // openDuplicateDialog는 테이블을 베낀다.
  //
  // 창을 여는 이유: 사본에서 가장 먼저 정해야 하는 것이 이름이다. 자동 이름으로
  // 바로 만들면 users_copy 라는 테이블이 생기고, 그것을 고치러 다시 들어가야 한다.
  //
  // 컬럼과 기본키는 고를 수 없다. 그것 없이 베낀 것은 사본이 아니라 빈 테이블이고,
  // 그때 필요한 것은 복제가 아니라 "테이블 추가"다.
  openDuplicateDialog(ref) {
    const tbl = ref.table();
    if (!tbl) return;
    const nameInput = input({ value: this.freeTableName(`${tbl.name}_copy`) });
    const idxBox = checkbox(`인덱스 ${(tbl.indexes ?? []).length}개`,
      { checked: true, disabled: !(tbl.indexes ?? []).length });
    const fkBox = checkbox(`외래키 ${(tbl.foreignKeys ?? []).length}개`,
      { checked: true, disabled: !(tbl.foreignKeys ?? []).length });
    const ckBox = checkbox(`체크 제약 ${(tbl.checks ?? []).length}개`,
      { checked: true, disabled: !(tbl.checks ?? []).length });

    openModal({
      title: `${tableDisplay(tbl)} 복제`,
      width: 480,
      body: () => [
        h('label.field', {}, h('span.field-label', {}, '새 테이블 이름'), nameInput),
        h('div.field', {}, h('span.field-label', {}, '함께 베낄 것'),
          h('div.erd-dup-list', {}, idxBox, fkBox, ckBox),
          h('p.field-help', {},
            `컬럼 ${(tbl.columns ?? []).length}개와 기본키는 언제나 함께 베낍니다. `
            + '제약 이름은 새 이름에 맞춰 바뀝니다(ix_users_email → ix_users_copy_email).')),
        h('p.field-help', {},
          '이 테이블을 **가리키는** 외래키는 베끼지 않습니다 — 다른 테이블에 있는 제약이고, '
          + '사본과 원본 중 어느 쪽을 가리켜야 하는지는 사람만 알 수 있습니다.'),
      ],
      footer: (close) => [
        h('button.btn', { type: 'button', onclick: close }, '취소'),
        h('button.btn.btn-primary', {
          type: 'button',
          onclick: () => {
            const name = nameInput.value.trim();
            if (!name) {
              toast('새 이름을 적으세요', 'error');
              return;
            }
            this.send('table.duplicate', {
              key: ref.serverKey,
              name,
              withIndexes: idxBox.querySelector('input').checked,
              withForeignKeys: fkBox.querySelector('input').checked,
              withChecks: ckBox.querySelector('input').checked,
            });
            // 만들어지는 사본을 바로 고를 수 있게 해 둔다. 서버 응답으로 문서가
            // 오면 그 키가 생기고, 없으면 아무 일도 하지 않는다(pruneSelection).
            const ns = tbl.namespace ? `${tbl.namespace}.` : '';
            this.sel = { kind: 'table', id: `${ns}${name}`.toLowerCase() };
            this.marks = [this.sel];
            close();
          },
        }, '복제'),
      ],
    });
  }

  // freeTableName은 아직 쓰이지 않은 이름을 찾는다(서버의 규칙과 같다).
  //
  // 화면에서 한 번 더 계산하는 이유: 창에 들어갈 기본값이 필요하다. 서버가 정하게
  // 두면 창에는 빈 칸이 뜨고, 사람은 "비워 두면 어떻게 되는가"를 눌러 봐야 안다.
  freeTableName(base) {
    const taken = new Set((this.doc.schema?.tables ?? []).map((t) => t.name.toLowerCase()));
    if (!taken.has(base.toLowerCase())) return base;
    // users_copy 가 이미 있으면 users_copy2. 서버도 같은 규칙을 쓴다.
    const stem = base.replace(/_copy$/, '');
    for (let i = 2; i < 1000; i += 1) {
      const next = `${stem}_copy${i}`;
      if (!taken.has(next.toLowerCase())) return next;
    }
    return base;
  }

  // duplicateMarks는 고른 테이블들을 한 번에 베낀다.
  //
  // 이름을 묻지 않는다: 여러 개를 고른 상태에서 이름을 하나씩 물으면 창이 열 번
  // 열린다. 자동 이름(_copy)으로 만들고, 이름은 만든 뒤에 고치는 편이 빠르다.
  duplicateMarks() {
    const keys = this.marksOf('table');
    if (!keys.length) return;
    const batch = newLocalID();
    for (const key of keys) {
      const tbl = this.findTable(key);
      if (!tbl) continue;
      this.send('table.duplicate', { key }, batch);
    }
    toast(`테이블 ${keys.length}개를 복제했습니다`, 'success');
  }

  columnRow(ref, col, ro, index = 0, total = 1) {
    // 서버가 알고 있는 컬럼 이름. 미리보기가 이름을 바꾸므로 그것을 대상으로
    // 쓰면 이름을 고친 뒤의 모든 편집이 "없는 컬럼"을 향하게 된다.
    let serverName = col.name;
    // shownName은 화면에 그려진 이름이다(미리보기 중에는 입력 중인 이름).
    let shownName = col.name;
    // live는 문서에서 이 줄이 가리키는 컬럼을 다시 찾는다. 테이블과 같은 이유로
    // 처음 잡아 둔 객체를 쓰지 않는다 — 구조 편집이 확정되면 문서가 통째로 바뀐다.
    const live = () => {
      const cols = ref.table()?.columns ?? [];
      return cols.find((c) => c.name === shownName) ?? cols.find((c) => c.name === serverName) ?? null;
    };

    const nameInput = input({ value: col.name, disabled: ro, class: 'input erd-col-name' });
    nameInput.dataset.col = col.name;
    commitOn(nameInput, () => {
      const next = nameInput.value.trim();
      if (!next || next === serverName) return;
      this.send('column.update', { table: ref.serverKey, name: serverName, newName: next });
      serverName = next;
    }, (value) => this.previewCanvas(() => {
      const c = live();
      if (!c || !value.trim()) return;
      c.name = value;
      shownName = value;
    }), { required: true });

    const typeInput = input({
      value: col.rawType || col.type?.base || '', disabled: ro, class: 'input erd-col-type-input',
    });
    commitOn(typeInput, () => {
      const next = typeInput.value.trim();
      if (!next) return;
      this.send('column.update', { table: ref.serverKey, name: serverName, type: next });
    }, (value) => this.previewCanvas(() => {
      const c = live();
      if (c) c.rawType = value;
    }), { required: true });

    // 타입 고르개. 칸에 직접 타자하는 길을 그대로 두고 버튼을 하나 더 두는 이유:
    // 아는 타입은 치는 편이 빠르고, 모르는 타입은 고르는 편이 빠르다. 둘 중 하나만
    // 두면 언제나 한쪽이 불편해진다.
    const pickBtn = h('button.icon-btn.erd-col-type-pick', {
      type: 'button', title: '타입 고르기', disabled: ro,
      onclick: () => this.openTypeDialog(ref, () => serverName),
      // 목록 아이콘이 아니라 편집 아이콘을 쓴다. 이 버튼이 여는 것은 목록이 아니라
      // "이 칸을 고치는 창"이고, 창 안에는 고르기 말고 직접 입력도 있다.
    }, icon('edit', 13));

    const nullBox = checkbox('NULL', {
      checked: col.nullable, disabled: ro,
      onchange: (e) => this.send('column.update', {
        table: ref.serverKey, name: serverName, nullable: e.target.checked,
      }),
    });

    // UNIQUE는 컬럼의 속성이 아니라 **인덱스**다.
    //
    // 그래도 컬럼 줄에 두는 이유: 사람이 스키마를 설계할 때 "이메일은 겹치면 안 된다"는
    // 컬럼을 적는 그 순간에 정해진다. 인덱스 목록까지 내려가 이름을 지어 가며 만들게
    // 하면, 정작 필요한 자리에서 한 걸음 멀어진다.
    //
    // 여기서 보는 것은 "이 컬럼 **하나만으로** 유일한가"다. 복합 유니크는 그 뜻이
    // 아니므로(그것은 조합이 유일하다는 뜻이다) 켜진 것으로 세지 않는다.
    const soleUnique = singleColumnIndex(ref.table(), col.name, true);
    const plainIndex = singleColumnIndex(ref.table(), col.name, false);
    // 기본키가 이 컬럼 하나면 이미 유일하다. (isPK는 아래에서 다시 계산한다 —
    // 여기서 참조하면 선언보다 먼저 쓰는 셈이 된다.)
    const pkCols = (ref.table()?.primaryKey?.columns ?? []).map((c) => c.toLowerCase());
    const pkOnly = pkCols.length === 1 && pkCols[0] === col.name.toLowerCase();
    const uniqueBox = checkbox('UNIQUE', {
      checked: Boolean(soleUnique) || pkOnly,
      disabled: ro || pkOnly,
      onchange: (e) => this.toggleColumnUnique(ref, col, e.target.checked),
    });
    // 무슨 일이 일어나는지 미리 말해 둔다. 인덱스는 이름이 있는 물건이라, 켰다 껐다가
    // 남의 인덱스를 지우는 일이 되면 안 된다.
    uniqueBox.title = pkOnly ? '기본키라 이미 유일합니다'
      : soleUnique ? (soleUnique.name.startsWith('ux_')
        ? `인덱스 ${soleUnique.name} 을 지웁니다`
        : `인덱스 ${soleUnique.name} 에서 UNIQUE 만 풉니다(인덱스는 남습니다)`)
      : plainIndex ? `인덱스 ${plainIndex.name} 을 UNIQUE 로 바꿉니다`
        : `UNIQUE 인덱스 ux_${ref.table()?.name ?? ''}_${col.name} 을 만듭니다`;

    const defInput = input({
      value: col.default ?? '', disabled: ro, placeholder: '기본값', class: 'input erd-col-default',
    });
    commitOn(defInput, () => {
      this.send('column.update', { table: ref.serverKey, name: serverName, default: defInput.value });
    });

    // 기본키를 컬럼 줄에서 바로 켠다.
    //
    // 따로 떨어진 목록에서 고르면 "지금 보고 있는 컬럼이 키인가"를 두 곳을 오가며
    // 맞춰 봐야 한다. 순서는 화면의 컬럼 순서를 그대로 쓴다 — 복합키의 순서는
    // 인덱스 효율에 영향을 주므로 임의로 정렬하지 않는다.
    const isPK = (ref.table()?.primaryKey?.columns ?? [])
      .some((c) => c.toLowerCase() === col.name.toLowerCase());
    const pkBtn = h('button.erd-pk-toggle', {
      type: 'button',
      class: isPK ? 'is-on' : '',
      disabled: ro,
      title: isPK ? '기본키에서 빼기' : '기본키로 지정',
      'aria-pressed': String(isPK),
      onclick: () => {
        // 누르는 순간의 문서를 본다. 그리는 동안 다른 사람이 컬럼을 더했을 수 있고,
        // 그때 그릴 때 읽은 목록을 보내면 그 컬럼이 기본키에서 조용히 빠진다.
        const table = ref.table();
        if (!table) return;
        const next = new Set((table.primaryKey?.columns ?? []).map((c) => c.toLowerCase()));
        if (isPK) next.delete(shownName.toLowerCase());
        else next.add(shownName.toLowerCase());
        const cols = (table.columns ?? [])
          .filter((c) => next.has(c.name.toLowerCase()))
          .map((c) => c.name);
        this.send('pk.set', { table: ref.serverKey, columns: cols });
      },
    }, icon('key', 12));

    // 아이콘 단추. 고른 것이 없으면 자동으로 정해진 아이콘을 그대로 보여준다 —
    // 비어 있는 자리를 보여주면 "여기에 무엇이 들어가는지" 눌러 봐야 알 수 있고,
    // 카드에 이미 그려져 있는 것과도 어긋난다.
    const isFK = (ref.table()?.foreignKeys ?? []).some((fk) =>
      (fk.columns ?? []).some((c) => c.toLowerCase() === col.name.toLowerCase()));
    const chosen = chosenIconFor(this.doc.layout?.[ref.serverKey], col.name);
    const shownIcon = columnIcon(col, { isPK, isFK }, chosen);
    const iconBtn = h('button.erd-col-icon-btn', {
      type: 'button',
      class: chosen ? 'is-set' : '',
      disabled: ro,
      title: chosen ? '아이콘 바꾸기' : '아이콘 (타입·키에서 자동)',
      onclick: () => this.openColumnIconDialog(ref, col, { isPK, isFK }),
    }, shownIcon ? icon(shownIcon, 13) : h('span.erd-icon-none', {}, '—'));

    const moveBtn = (dir, label) => h('button.icon-btn', {
      type: 'button', title: label,
      disabled: ro || (dir < 0 ? index === 0 : index === total - 1),
      onclick: () => this.send('column.move', {
        table: ref.serverKey, name: serverName, to: index + 1 + dir,
      }),
    }, h('span.erd-move-arrow', {}, dir < 0 ? '▲' : '▼'));

    // 자동 증가는 **여기서 켜지 않는다**. 켜고 끄는 곳은 타입 창이다.
    //
    // 타입과 붙어 있는 설정이기 때문이다: 정수 계열에만 붙일 수 있고, 어느 타입까지
    // 되는지는 DB마다 다르다(MS-SQL은 소수 자릿수 0인 DECIMAL에도 되고, SQLite는
    // INTEGER 하나뿐이다). 타입을 고치는 창 밖에 두면 "타입을 바꿨더니 자동 증가가
    // 말이 안 되게 남아 있는" 상태를 사람이 직접 치워야 한다.
    //
    // 대신 배지는 남긴다. 켜져 있다는 사실은 컬럼 목록에서 바로 보여야 한다 —
    // 안 보이면 창을 하나하나 열어 확인하게 된다.
    let autoBadge = null;
    if (col.identity) {
      autoBadge = badge('AUTO', 'info');
      autoBadge.title = `자동 증가 (${this.typeCatalog?.autoIncrement || 'AUTO_INCREMENT'})`
        + ' — 타입 창에서 끕니다';
    }

    // 설명은 **적을 수 있어야** 한다.
    //
    // 지금까지는 있으면 보여주기만 했다. 그런데 설명이 필요해지는 순간은 컬럼을
    // 만드는 그 순간이고("status: 0=대기 1=완료"), 적을 곳이 화면에 없으면 그 지식은
    // 사람 머리에만 남는다. DDL의 COMMENT 로 나가므로 DB에도 함께 실린다.
    const commentInput = input({
      value: col.comment ?? '', placeholder: '설명', class: 'input erd-col-comment-input',
    });
    commitOn(commentInput, () => this.send('column.update', {
      table: ref.serverKey, name: serverName, comment: commentInput.value,
    }));

    // 끌어서 순서를 바꾸는 손잡이.
    //
    // 줄 전체를 끌게 하지 않는 이유: 줄 안에 입력 칸이 셋이라, 글자를 고르려고
    // 드래그하면 그것이 순서 바꾸기가 된다. 그래서 손잡이를 누른 동안에만 줄이
    // 끌리는 것으로 바뀐다(아래 draggable 토글).
    const grip = ro ? null : h('button.erd-col-grip', {
      type: 'button',
      title: '끌어서 순서 바꾸기',
      'aria-label': '순서 바꾸기',
    }, icon('menu', 12));

    const row = h('div.erd-col-row', {},
      h('div.erd-col-row-main', {}, grip, pkBtn, iconBtn, nameInput, typeInput, ro ? null : pickBtn),
      // 둘째 줄은 켜고 끄는 것들, 셋째 줄은 적는 것들.
      //
      // 한 줄에 다 넣으면 좁은 인스펙터에서 줄바꿈이 일어나 순서 단추가 혼자
      // 아래로 떨어진다. 그 모양은 "이 단추가 무엇의 단추인지"를 흐린다.
      h('div.erd-col-row-sub', {},
        nullBox,
        uniqueBox,
        autoBadge,
        // 도메인에서 온 타입이면 그 사실을 줄에 남긴다. 남기지 않으면 타입을 직접
        // 고쳐 놓고 "왜 도메인을 고쳤는데 이 컬럼만 안 바뀌나"를 묻게 된다.
        col.domain ? badge(col.domain, 'neutral') : null,
        ro ? null : h('div.erd-col-order', {}, moveBtn(-1, '위로'), moveBtn(1, '아래로')),
        ro ? null : h('button.icon-btn', {
          type: 'button', title: '컬럼 삭제',
          onclick: () => this.send('column.delete', { table: ref.serverKey, name: serverName }),
        }, icon('trash', 13)),
      ),
      ro
        ? h('div.erd-col-row-fields', {},
          col.default ? h('p.erd-col-note', {}, `기본값 ${col.default}`) : null,
          col.comment ? h('p.erd-col-comment', {}, col.comment) : null)
        : h('div.erd-col-row-fields', {}, defInput, commentInput),
    );
    if (!ro) this.bindColumnDrag(row, grip, ref, col, index);
    return row;
  }

  // bindColumnDrag는 컬럼 줄을 끌어 옮길 수 있게 한다.
  //
  // ▲▼ 단추를 남겨 두는 이유: 한 칸씩 정확히 옮기는 일과 열 칸 위로 던지는 일은
  // 다른 동작이고, 키보드만 쓰는 사람에게는 단추가 유일한 길이다.
  bindColumnDrag(row, grip, ref, col, index) {
    row.dataset.colIndex = String(index);
    row.dataset.colName = col.name;

    // 손잡이를 누른 동안에만 끌 수 있게 한다. 늘 draggable로 두면 입력 칸 안에서
    // 글자를 고르는 드래그가 줄 옮기기로 바뀐다.
    grip.addEventListener('pointerdown', () => {
      row.draggable = true;
      // 손을 떼면 다시 끌 수 없는 줄로 돌아간다. 손잡이의 pointerup 만 듣던 때는
      // 손잡이를 누른 채 밖으로 나가 떼면 그 줄이 계속 끌 수 있는 상태로 남아,
      // 입력 칸에서 글자를 고르는 드래그가 줄 옮기기로 바뀌었다.
      const off = () => {
        row.draggable = false;
        window.removeEventListener('pointerup', off);
      };
      // pointercancel 은 듣지 않는다. 네이티브 드래그가 시작될 때 그것이 오는데,
      // 그때 draggable 을 끄면 시작하려는 드래그를 우리가 취소하는 셈이 된다.
      // 드래그로 끝난 경우는 dragend 가 정리한다.
      window.addEventListener('pointerup', off);
    });

    row.addEventListener('dragstart', (e) => {
      this.colDrag = { table: ref.serverKey, name: col.name, index };
      row.classList.add('is-dragging');
      e.dataTransfer.effectAllowed = 'move';
      // 데이터를 넣지 않으면 파이어폭스에서 드래그가 시작되지 않는다.
      e.dataTransfer.setData('text/plain', col.name);
    });

    row.addEventListener('dragend', () => {
      row.draggable = false;
      row.classList.remove('is-dragging');
      this.colDrag = null;
      this.clearColumnDropMark();
      // 네이티브 드래그는 pointerup 대신 pointercancel 로 끝난다. 잠금을 여기서도
      // 풀어 두지 않으면(브라우저마다 다르다) 그 뒤로 패널이 영영 갱신되지 않는다.
      this.panelPressed = false;
      if (this.panelDirty) setTimeout(() => this.renderPanelIfIdle(), 0);
    });

    row.addEventListener('dragover', (e) => {
      const drag = this.colDrag;
      if (!drag || drag.table !== ref.serverKey || drag.name === col.name) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = 'move';
      const rect = row.getBoundingClientRect();
      const after = e.clientY > rect.top + rect.height / 2;
      this.clearColumnDropMark();
      row.classList.add(after ? 'is-drop-after' : 'is-drop-before');
    });

    row.addEventListener('dragleave', (e) => {
      // 줄 안의 자식으로 들어간 것은 떠난 것이 아니다.
      if (row.contains(e.relatedTarget)) return;
      row.classList.remove('is-drop-before', 'is-drop-after');
    });

    row.addEventListener('drop', (e) => {
      const drag = this.colDrag;
      this.clearColumnDropMark();
      if (!drag || drag.table !== ref.serverKey || drag.name === col.name) return;
      e.preventDefault();
      const rect = row.getBoundingClientRect();
      const after = e.clientY > rect.top + rect.height / 2;
      this.moveColumnTo(ref, drag.name, col.name, after);
    });
  }

  clearColumnDropMark() {
    for (const el of this.ui.panel.querySelectorAll('.is-drop-before, .is-drop-after')) {
      el.classList.remove('is-drop-before', 'is-drop-after');
    }
  }

  // moveColumnTo는 끌어다 놓은 자리를 위치 번호로 바꿔 보낸다.
  //
  // 지금 목록을 다시 읽는 이유: 끄는 동안 다른 사람이 컬럼을 더하거나 지웠을 수 있고,
  // 그리면서 계산해 둔 번호를 그대로 보내면 엉뚱한 자리로 간다.
  moveColumnTo(ref, name, targetName, after) {
    const table = ref.table();
    if (!table) return;
    const names = (table.columns ?? []).map((c) => c.name);
    const from = names.findIndex((n) => n.toLowerCase() === name.toLowerCase());
    const at = names.findIndex((n) => n.toLowerCase() === targetName.toLowerCase());
    if (from < 0 || at < 0) return;

    // 끌고 있는 것을 뺀 목록에서 몇 번째에 끼울지 센다. 서버의 column.move는
    // "옮긴 뒤의 위치(1부터)"를 받으므로 그 값이 곧 답이다.
    const rest = names.filter((_, i) => i !== from);
    let slot = rest.findIndex((n) => n.toLowerCase() === targetName.toLowerCase());
    if (after) slot += 1;
    if (slot === from) return;
    this.send('column.move', { table: ref.serverKey, name, to: slot + 1 });
  }

  // openColumnIconDialog는 컬럼 아이콘을 고른다.
  //
  // 인스펙터 안에 펼치지 않고 창을 여는 이유: 아이콘이 서른 개 가까이 되는데
  // 컬럼 줄마다 그 격자를 둘 자리가 없다. 고르면 바로 보내고 닫는다 — 아이콘은
  // 되돌리기 쉬운 표시 정보라 확인 단추를 한 번 더 누르게 할 이유가 없다.
  openColumnIconDialog(ref, col, flags) {
    const auto = autoColumnIcon(col, flags);
    const chosen = chosenIconFor(this.doc.layout?.[ref.serverKey], col.name);

    const send = (value) => {
      // 좌표는 보내는 순간 다시 읽는다. table.move는 위치가 필수인 패치라,
      // 창을 여는 동안 누가 카드를 옮겼다면 옛 자리로 되돌려 놓게 된다.
      const now = this.doc.layout?.[ref.serverKey] ?? {};
      this.send('table.move', {
        key: ref.serverKey,
        x: now.x ?? 0,
        y: now.y ?? 0,
        // 보낸 컬럼만 바뀐다. 지도를 통째로 보내면 같은 표를 함께 보는 사람의
        // 아이콘을 지우게 된다.
        columnIcons: { [col.name.toLowerCase()]: value },
      });
      close();
    };

    const cell = (value, on, node, label) => h('button.erd-icon-btn', {
      type: 'button',
      class: on ? 'is-on' : '',
      title: label,
      onclick: () => send(value),
    }, node);

    const close = openModal({
      title: `${col.name} 아이콘`,
      width: 420,
      body: () => h('div', {},
        h('div.erd-icon-auto', {},
          cell('', !chosen, h('span.erd-icon-auto-face', {}, icon(auto, 14), h('span', {}, '자동')),
            '타입과 키에 맞춰 고릅니다'),
          cell('none', chosen === 'none', h('span.erd-icon-none', {}, '표시 안 함'),
            '이 컬럼에는 아이콘을 붙이지 않습니다'),
        ),
        h('div.erd-icon-picker.is-wide', {}, COLUMN_ICONS.map((name) =>
          cell(name, chosen === name, icon(name, 15), name))),
        h('p.field-help', {}, '아이콘은 설계 메모입니다. 마이그레이션에는 들어가지 않습니다.'),
      ),
    });
  }

  // appearanceEditor는 카드의 표시 방식(아이콘·색)을 고른다.
  //
  // 스키마가 아니라 주석에 가까운 정보다. 테이블이 서른 개를 넘어가면 이름을 읽기
  // 전에 "이건 사용자 쪽, 저건 주문 쪽"이 눈에 들어와야 전체 구조가 보인다.
  appearanceEditor(ref) {
    const box = this.doc.layout?.[ref.serverKey] ?? {};
    // 좌표는 함께 보내야 한다 — table.move는 위치가 필수인 레이아웃 패치다.
    // 그리고 좌표는 **누르는 순간** 다시 읽는다. 패널이 그려진 뒤에 누군가
    // (또는 내가) 카드를 옮겼다면, 그릴 때 읽어 둔 좌표를 보내는 순간 카드가
    // 옛 자리로 되돌아간다 — 색을 골랐을 뿐인데 카드가 움직인다.
    const patch = (extra) => {
      const now = this.doc.layout?.[ref.serverKey] ?? box;
      return this.send('table.move', {
        key: ref.serverKey, x: now.x ?? 0, y: now.y ?? 0, ...extra,
      });
    };

    const icons = h('div.erd-icon-picker', {}, TABLE_ICONS.map((name) => h('button.erd-icon-btn', {
      type: 'button',
      class: (box.icon || '') === name ? 'is-on' : '',
      title: name === '' ? '아이콘 없음' : name,
      onclick: () => patch({ icon: name }),
    }, name === '' ? h('span.erd-icon-none', {}, '—') : icon(name, 15))));

    const colors = h('div.tint-picker', {}, TABLE_COLORS.map((c) => h('button.tint-swatch', {
      type: 'button',
      class: `${c.className}${(box.color || '') === c.value ? ' is-on' : ''}`,
      title: c.label,
      onclick: () => patch({ color: c.value }),
    })));

    return h('div.field', {},
      h('span.field-label', {}, '표시'),
      h('div.erd-appearance', {}, icons, colors),
      h('p.field-help', {}, '아이콘과 색은 설계 메모입니다. 마이그레이션에는 들어가지 않습니다.'),
    );
  }

  // 컬럼은 먼저 만들고 나중에 이름을 고친다.
  //
  // 이름과 타입을 다 적어야 추가되던 방식은 "지금 몇 개가 필요한가"를 아는 상태에서
  // 하나씩 타자를 치게 만든다. 실제 설계는 자리를 먼저 잡고 이름을 나중에 다듬는
  // 쪽에 가깝고, 이름·타입이 맞는지는 마이그레이션을 만들 때 어차피 검증된다.
  addColumnForm(ref) {
    const add = () => {
      // 누르는 순간의 컬럼 목록을 본다(그리는 동안 늘어났을 수 있다).
      const table = ref.table();
      if (!table) return;
      const used = new Set((table.columns ?? []).map((c) => c.name.toLowerCase()));
      let n = (table.columns ?? []).length + 1;
      let name = `col_${n}`;
      while (used.has(name.toLowerCase())) {
        n += 1;
        name = `col_${n}`;
      }
      this.send('column.add', {
        table: ref.serverKey, name, type: defaultColumnType(this.doc.dialect), nullable: true,
      });
      // 새 줄이 그려지면 이름 칸으로 커서를 옮긴다. 서버 응답으로 패널이 다시
      // 그려지므로 그 뒤에 찾아야 한다.
      this.focusColumn = name;
    };

    return h('div.erd-col-add', {},
      h('button.btn.btn-small.btn-primary', { type: 'button', onclick: add },
        icon('plus'), '컬럼 추가'),
      h('span.muted.small', {}, '추가한 뒤 이름과 타입을 고치세요'),
    );
  }

  // ---------- AI 세션 ----------
  //
  // 전체 대화(방 채팅)와 AI 세션은 같은 패널을 나눠 쓴다. 별도 화면으로 빼지 않는
  // 이유: AI에게 "이 테이블을 이렇게 고쳐 줘"라고 말하는 동안 그 테이블이 보여야
  // 하고, 반영 결과도 같은 캔버스에서 바로 확인해야 한다.
  //
  // AI 세션은 나만 본다. 방 채팅과 달리 브로드캐스트하지 않는 이유는, 서로 다른
  // 사람이 동시에 시킨 지시가 한 줄로 섞이면 누가 무엇을 요청했는지 읽을 수 없기
  // 때문이다. 대신 쓸 만한 답만 골라 전체 대화로 공유한다.

  async loadAISessions() {
    if (this.aiLoaded) return;
    try {
      const res = await api.get(`/erd/documents/${encodeURIComponent(this.docID)}/ai/sessions`);
      this.aiSessions = res.sessions ?? [];
      this.aiProviders = res.providers ?? [];
      this.aiLoaded = true;
    } catch (err) {
      toastError(err);
    }
    if (this.tab === 'chat') this.renderPanel();
  }

  async newAISession() {
    if (this.aiProviders.length === 0) {
      toast('사용할 수 있는 AI 프로바이더가 없습니다. 설정에서 API 키를 등록하세요', 'error', 6000);
      return;
    }
    try {
      const res = await api.post(`/erd/documents/${encodeURIComponent(this.docID)}/ai/sessions`, {
        title: '설계 도우미',
      });
      this.aiSessions = [res.session, ...this.aiSessions];
      this.chatMode = res.session.id;
      this.aiMessages = [];
      this.renderPanel();
    } catch (err) {
      toastError(err);
    }
  }

  async openAISession(id) {
    this.chatMode = id;
    this.aiMessages = [];
    this.renderPanel();
    try {
      const res = await api.get(`/ai/sessions/${encodeURIComponent(id)}`);
      this.aiMessages = res.messages ?? [];
    } catch (err) {
      toastError(err);
    }
    if (this.chatMode === id) this.renderPanel();
  }

  // sendAI는 질문 하나를 보내고 응답을 스트리밍으로 그린다.
  //
  // 스트리밍 중에는 패널 전체를 다시 그리지 않고 이 노드만 갱신한다 — 토큰마다
  // 재렌더하면 입력 포커스가 날아가고 스크롤이 튄다. 툴이 초안을 고치면 그 변화는
  // 방금 그 op가 브로드캐스트되어 캔버스 쪽에서 알아서 들어온다.
  async sendAI(text, node) {
    const id = this.chatMode;
    this.aiBusy = true;
    this.aiMessages.push({ role: 'user', text });
    this.controller = new AbortController();
    try {
      await streamAIChat(id, text, (ev) => {
        if (ev.event === 'text') node.appendText(ev.data.text ?? '');
        else if (ev.event === 'tool_call') node.addTool(ev.data);
        else if (ev.event === 'tool_result') node.setToolResult(ev.data);
        else if (ev.event === 'notice') node.addNotice(ev.data.message ?? '');
        else if (ev.event === 'error') node.setError(ev.data.message ?? '오류가 발생했습니다');
      }, this.controller.signal);
    } catch (err) {
      if (err.name !== 'AbortError') {
        node.setError(err.message);
        toast(err.message, 'error', 6000);
      }
    } finally {
      this.controller = null;
      this.aiBusy = false;
      // 저장된 상태로 다시 읽는다. 스트림 중에는 툴 호출이 축약돼 있고
      // 제목도 첫 질문으로 서버에서 정해진다.
      if (this.chatMode === id) {
        try {
          const res = await api.get(`/ai/sessions/${encodeURIComponent(id)}`);
          this.aiMessages = res.messages ?? [];
          const idx = this.aiSessions.findIndex((s) => s.id === id);
          if (idx >= 0) this.aiSessions[idx] = res.session;
        } catch { /* 다시 읽기 실패는 화면에 남은 스트림으로 충분하다 */ }
        if (this.tab === 'chat') this.renderPanel();
      }
    }
  }

  // shareToRoom은 AI 답변 하나를 전체 대화로 옮긴다.
  //
  // 자동으로 공유하지 않는 이유: AI와의 대화에는 시행착오와 되묻기가 섞여 있다.
  // 그것을 전부 방에 흘리면 사람들의 대화가 묻힌다. 쓸 만한 결론만 고르게 한다.
  shareToRoom(text) {
    const body = String(text ?? '').trim();
    if (!body) return;
    if (body.length > 4000) {
      toast('답변이 너무 길어 공유할 수 없습니다 (4000자 제한)', 'error');
      return;
    }
    this.session.chat(`[AI] ${body}`.slice(0, 4000), this.tableKey ?? '');
    toast('전체 대화로 공유했습니다', 'success');
  }

  chatView() {
    // 어느 대화를 볼지 고르는 자리. 전체 대화와 AI 세션이 한 목록에 있는 이유는
    // 둘이 같은 패널을 차지하기 때문이다 — 탭을 하나 더 만들면 화면이 좁아진다.
    const picker = select([
      { value: 'room', label: `전체 대화 (${this.chat.length})` },
      ...this.aiSessions.map((s) => ({ value: s.id, label: `AI · ${s.title || '새 대화'}` })),
    ], {
      value: this.chatMode,
      onchange: (e) => {
        const v = e.target.value;
        if (v === 'room') {
          this.chatMode = 'room';
          this.renderPanel();
        } else {
          this.openAISession(v);
        }
      },
    });
    const head = h('div.erd-panel-head', {},
      h('h2', {}, '대화'),
      h('div.erd-chat-pick', {}, picker,
        h('button.btn.btn-small', {
          type: 'button', title: '새 AI 세션', onclick: () => this.newAISession(),
        }, icon('plus'), 'AI')),
    );

    if (this.chatMode !== 'room') return [head, ...this.aiChatView()];
    return [head, ...this.roomChatView()];
  }

  aiChatView() {
    const sess = this.aiSessions.find((s) => s.id === this.chatMode);
    const log = h('div.erd-chat-list.is-ai', {}, this.aiMessages.length === 0
      ? h('p.muted.erd-chat-empty', {},
        '테이블을 만들거나 컬럼을 고쳐 달라고 말해보세요. 변경은 초안에 바로 반영됩니다.')
      : this.aiMessages.flatMap((m) => aiMessageNode(m, (t) => this.shareToRoom(t))));

    const box = input({ placeholder: 'AI에게 설계를 요청하세요' });
    const send = () => {
      const text = box.value.trim();
      if (!text || this.aiBusy) return;
      box.value = '';
      const node = erdStreamNode();
      log.querySelector('.erd-chat-empty')?.remove();
      log.append(h('div.erd-chat-msg.is-me', {}, h('p.erd-chat-body', {}, text)), node.el);
      log.scrollTop = log.scrollHeight;
      this.sendAI(text, node);
    };
    box.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') {
        e.preventDefault();
        send();
      }
    });

    return [
      sess && !this.canEdit
        ? h('p.muted.small.erd-chat-note', {}, '읽기 권한만 있어 AI도 초안을 고칠 수 없습니다')
        : null,
      log,
      h('div.erd-chat-input', {}, box,
        h('button.btn.btn-small.btn-primary', { type: 'button', onclick: send }, '보내기')),
    ];
  }

  // 전체 대화는 구조 화면과 같은 뷰를 쓴다(core/roomchat.js). 두 벌로 두면 한쪽만
  // 고쳐지고, 같은 방의 같은 대화가 화면마다 다르게 보인다.
  roomChatView() {
    return roomChatView({
      messages: this.chat,
      participants: this.participants.length,
      placeholder: this.tableKey ? `${this.tableKey} 에 대해…` : '메시지를 입력하세요',
      emptyText: '아직 대화가 없습니다. 설계 의도를 남겨두면 리뷰가 쉬워집니다.',
      onSend: (body) => this.session.chat(body, this.tableKey ?? ''),
    });
  }

  // ---------- 패널 상호작용 ----------
  //
  // 캔버스의 포인터 처리는 ErdCanvas가 맡는다. 여기 남은 것은 사이드 패널 쪽,
  // 즉 "입력 중에는 다시 그리지 않는다"는 규칙을 푸는 시점뿐이다.

  bindPanel() {
    // 패널 입력이 끝나면 미뤄둔 재렌더를 반영한다.
    const onFocusOut = () => {
      if (this.panelDirty) setTimeout(() => this.renderPanelIfIdle(), 0);
    };
    // 누르고 있는 동안은 패널을 그대로 둔다(renderPanelIfIdle의 이유 참고).
    // 창 전체에서 떼는 것을 듣는 이유: 패널 밖으로 끌고 나가 떼는 경우에도
    // 잠금이 풀려야 한다. 그러지 않으면 그 뒤로 패널이 영영 갱신되지 않는다.
    const onDown = () => { this.panelPressed = true; };
    const onUp = () => {
      if (!this.panelPressed) return;
      this.panelPressed = false;
      // click 은 pointerup 다음에 온다. 그 click 이 끝난 뒤에 그리도록 한 틱 미룬다.
      if (this.panelDirty) setTimeout(() => this.renderPanelIfIdle(), 0);
    };
    this.ui.panel.addEventListener('focusout', onFocusOut);
    this.ui.panel.addEventListener('pointerdown', onDown);
    window.addEventListener('pointerup', onUp);
    window.addEventListener('pointercancel', onUp);
    this.unbind = () => {
      this.ui.panel.removeEventListener('focusout', onFocusOut);
      this.ui.panel.removeEventListener('pointerdown', onDown);
      window.removeEventListener('pointerup', onUp);
      window.removeEventListener('pointercancel', onUp);
    };
  }

  // ---------- 편집 동작 ----------

  addTable() {
    const nameInput = input({ placeholder: '테이블 이름', autofocus: true });
    const withID = checkbox('id 기본키 컬럼 함께 만들기', { checked: true });
    openModal({
      title: '테이블 추가',
      width: 460,
      body: () => [
        h('label.field', {}, h('span.field-label', {}, '이름'), nameInput),
        withID,
      ],
      footer: (close) => [
        h('button.btn', { type: 'button', onclick: close }, '취소'),
        h('button.btn.btn-primary', {
          type: 'button',
          onclick: () => {
            const name = nameInput.value.trim();
            if (!name) {
              toast('테이블 이름을 입력하세요', 'error');
              return;
            }
            // 새 테이블은 지금 보이는 화면 중앙에 놓는다. 서버의 빈자리 탐색은
            // 화면을 모르므로, 사용자가 보고 있는 곳에 나타나는 것이 자연스럽다.
            this.send('table.add', {
              name,
              withId: withID.querySelector('input').checked,
              ...this.canvas.center(CARD_W / 2, 60),
            });
            close();
          },
        }, '추가'),
      ],
    });
  }

  addNote() {
    const box = h('textarea.input.textarea', { rows: 3, placeholder: '메모 내용', autofocus: true });
    openModal({
      title: '메모 추가',
      width: 460,
      body: () => box,
      footer: (close) => [
        h('button.btn', { type: 'button', onclick: close }, '취소'),
        h('button.btn.btn-primary', {
          type: 'button',
          onclick: () => {
            this.send('note.add', {
              id: newLocalID('n'),
              text: box.value,
              ...this.canvas.center(100, 40),
            });
            close();
          },
        }, '추가'),
      ],
    });
  }

  addGroup() {
    const nameInput = input({ placeholder: '예: 주문 도메인', autofocus: true });
    let color = '#3b82f6';
    const swatches = h('div.tint-picker', {}, TABLE_COLORS.filter((c) => c.value).map((c) =>
      h('button.tint-swatch', {
        type: 'button',
        class: `${c.className}${c.value === color ? ' is-on' : ''}`,
        title: c.label,
        onclick: (e) => {
          color = c.value;
          for (const b of e.currentTarget.parentElement.children) b.classList.remove('is-on');
          e.currentTarget.classList.add('is-on');
        },
      })));

    openModal({
      title: '그룹 추가',
      width: 460,
      body: () => [
        h('label.field', {}, h('span.field-label', {}, '이름'), nameInput),
        h('div.field', {}, h('span.field-label', {}, '색'), swatches),
        h('p.field-help', {},
          '테이블을 감싸는 사각형입니다. 크기와 위치는 캔버스에서 끌어 정합니다. '
          + '설계 메모이므로 마이그레이션에는 들어가지 않습니다.'),
      ],
      footer: (close) => [
        h('button.btn', { type: 'button', onclick: close }, '취소'),
        h('button.btn.btn-primary', {
          type: 'button',
          onclick: () => {
            this.send('group.add', {
              id: newLocalID('g'),
              label: nameInput.value.trim(),
              ...this.canvas.center(160, 120),
              w: 320, h: 240, color,
            });
            close();
          },
        }, '추가'),
      ],
    });
  }

  toggleCollapse(key, geom) {
    if (!this.canEdit) return;
    this.send('table.move', {
      key, x: geom.x, y: geom.y, collapsed: !geom.layout.collapsed,
    });
  }

  async deleteTable(ref) {
    const tbl = ref.table();
    if (!tbl) return;
    const ok = await confirmDialog({
      title: '테이블 삭제',
      message: `"${tbl.name}" 을 초안에서 지웁니다. 이 테이블을 참조하는 외래키가 있으면 함께 지워집니다.`,
      confirmLabel: '삭제',
      danger: true,
    });
    if (!ok) return;
    // 확인을 기다리는 동안 이름이 바뀌었을 수 있으므로 지금의 키로 보낸다.
    this.send('table.delete', { key: ref.serverKey, cascade: true });
    this.sel = null;
  }

  // toggleColumnUnique는 컬럼 하나짜리 UNIQUE 인덱스를 켜고 끈다.
  //
  // 이미 그 컬럼만 걸린 인덱스가 있으면 그것을 고친다. 새로 만들면 같은 컬럼에
  // 인덱스가 둘이 되는데, 대상 DB에서 그것은 그냥 낭비다.
  //
  // 끌 때는 우리가 만든 이름(ux_)만 지운다. 사람이나 DB가 지어 준 이름의 인덱스는
  // UNIQUE만 풀고 남긴다 — "유일하지 않아도 된다"는 말이 "그 인덱스를 지워라"는
  // 뜻은 아니기 때문이다.
  toggleColumnUnique(ref, col, on) {
    const table = ref.table();
    if (!table) return;
    const unique = singleColumnIndex(table, col.name, true);
    if (!on) {
      if (!unique) return;
      if (unique.name.startsWith('ux_')) {
        this.send('index.delete', { table: ref.serverKey, name: unique.name });
      } else {
        this.send('index.update', { table: ref.serverKey, name: unique.name, unique: false });
      }
      return;
    }
    if (unique) return;
    const plain = singleColumnIndex(table, col.name, false);
    if (plain) {
      this.send('index.update', { table: ref.serverKey, name: plain.name, unique: true });
      return;
    }
    // 이름은 규칙으로 짓되 겹치면 번호를 붙인다. 이름이 겹치면 서버가 거부하고,
    // 사용자는 체크박스를 눌렀는데 아무 일도 안 일어난 것으로 본다.
    const used = new Set((table.indexes ?? []).map((ix) => ix.name.toLowerCase()));
    let name = `ux_${table.name}_${col.name}`;
    let n = 2;
    while (used.has(name.toLowerCase())) {
      name = `ux_${table.name}_${col.name}_${n}`;
      n += 1;
    }
    this.send('index.add', {
      table: ref.serverKey, name, columns: [col.name], unique: true,
    });
  }

  // deleteMarks는 함께 고른 것을 한 번에 지운다.
  //
  // 확인을 한 번만 받는 이유: 대상마다 물으면 열 개를 지울 때 열 번 눌러야 하고,
  // 그 열 번은 아무것도 확인하지 않는 반사 동작이 된다. 대신 무엇이 몇 개
  // 지워지는지를 그 한 번에 다 보여준다.
  async deleteMarks() {
    const tables = this.marksOf('table').map((k) => this.findTable(k)).filter(Boolean);
    const notes = this.marksOf('note');
    const groups = this.marksOf('group');
    if (!tables.length && !notes.length && !groups.length) return;

    const parts = [];
    if (tables.length) parts.push(`테이블 ${tables.length}개`);
    if (notes.length) parts.push(`메모 ${notes.length}개`);
    if (groups.length) parts.push(`묶음 ${groups.length}개`);
    const names = tables.map((t) => t.name).join(', ');
    const ok = await confirmDialog({
      title: '선택한 것 삭제',
      message: `${parts.join(' · ')} 을(를) 초안에서 지웁니다.${
        tables.length ? ` 지워지는 테이블: ${names}. 이 테이블들을 참조하는 외래키도 함께 지워집니다.` : ''}`,
      confirmLabel: '삭제',
      danger: true,
    });
    if (!ok) return;

    const batch = newLocalID();
    // 묶음·메모를 먼저 지운다. 테이블 삭제는 문서를 통째로 다시 받게 하므로,
    // 그 뒤에 오는 id 기반 삭제가 이미 바뀐 문서를 기준으로 판정되지 않게 한다.
    for (const id of groups) this.send('group.delete', { id }, batch);
    for (const id of notes) this.send('note.delete', { id }, batch);
    for (const tbl of tables) this.send('table.delete', { key: tableKey(tbl), cascade: true }, batch);
    this.select(null);
  }

  // alignMarks는 고른 것들을 한 줄로 맞춘다.
  //
  // 배치를 손으로만 맞추면 몇 픽셀씩 어긋나고, 그 어긋남은 카드가 늘어날수록
  // "이 둘이 같은 층인가"를 읽기 어렵게 만든다.
  alignMarks(how) {
    const items = this.markBoxes();
    if (items.length < 2) return;
    const xs = items.map((i) => i.x);
    const ys = items.map((i) => i.y);
    const rights = items.map((i) => i.x + i.w);
    const bottoms = items.map((i) => i.y + i.h);
    const place = {
      left: (i) => ({ x: Math.min(...xs), y: i.y }),
      right: (i) => ({ x: Math.max(...rights) - i.w, y: i.y }),
      top: (i) => ({ x: i.x, y: Math.min(...ys) }),
      bottom: (i) => ({ x: i.x, y: Math.max(...bottoms) - i.h }),
      // 세로로 쌓기: x는 맨 왼쪽에 맞추고 y를 일정한 간격으로 다시 놓는다.
      column: null,
      row: null,
    }[how];

    let moves = [];
    if (place) {
      moves = items.map((i) => ({ kind: i.kind, id: i.id, ...place(i) }));
    } else if (how === 'column') {
      const x = Math.min(...xs);
      let y = Math.min(...ys);
      for (const i of [...items].sort((a, b) => a.y - b.y)) {
        moves.push({ kind: i.kind, id: i.id, x, y });
        y += i.h + 24;
      }
    } else if (how === 'row') {
      const y = Math.min(...ys);
      let x = Math.min(...xs);
      for (const i of [...items].sort((a, b) => a.x - b.x)) {
        moves.push({ kind: i.kind, id: i.id, x, y });
        x += i.w + 32;
      }
    }
    // 제자리인 것은 보내지 않는다. 되돌리기 스택이 "아무 일도 없었던 편집"으로
    // 채워지면 Ctrl+Z 를 눌러도 화면이 변하지 않는다.
    const changed = moves.filter((m) => {
      const at = items.find((i) => i.kind === m.kind && i.id === m.id);
      return at && (Math.round(at.x) !== Math.round(m.x) || Math.round(at.y) !== Math.round(m.y));
    });
    if (!changed.length) return;
    for (const m of changed) this.canvas.placeMark(m, m.x, m.y);
    this.moveMany(changed);
    this.renderCanvas();
  }

  // markBoxes는 고른 것들의 사각형이다(정렬 계산용).
  markBoxes() {
    const boxes = this.canvas.boxes();
    const out = [];
    for (const m of this.marks) {
      if (m.kind === 'table') {
        const geom = boxes.get(m.id);
        if (geom) out.push({ ...m, x: geom.x, y: geom.y, w: geom.w, h: geom.h });
      } else if (m.kind === 'note') {
        const note = (this.doc.notes ?? []).find((n) => n.id === m.id);
        if (note) out.push({ ...m, x: note.x, y: note.y, w: note.w || NOTE_W, h: note.h || noteHeight(note) });
      } else if (m.kind === 'group') {
        const group = (this.doc.groups ?? []).find((g) => g.id === m.id);
        if (group) out.push({ ...m, x: group.x, y: group.y, w: group.w || 320, h: group.h || 240 });
      }
    }
    return out;
  }

  // groupMarks는 고른 것들을 감싸는 묶음을 만든다.
  groupMarks() {
    const items = this.markBoxes().filter((i) => i.kind !== 'group');
    if (!items.length) return;
    const pad = 24;
    const x = Math.min(...items.map((i) => i.x)) - pad;
    const y = Math.min(...items.map((i) => i.y)) - pad - 18;
    const w = Math.max(...items.map((i) => i.x + i.w)) + pad - x;
    const hh = Math.max(...items.map((i) => i.y + i.h)) + pad - y;
    this.send('group.add', { id: newLocalID(), label: '새 묶음', x, y, w, h: hh });
  }

  // paintMarks는 고른 것들의 색을 한 번에 정한다.
  paintMarks(color) {
    const batch = newLocalID();
    for (const m of this.marks) {
      if (m.kind === 'table') {
        const box = this.doc.layout?.[m.id] ?? {};
        this.send('table.move', { key: m.id, x: box.x ?? 0, y: box.y ?? 0, color }, batch);
      } else if (m.kind === 'note') {
        this.send('note.update', { id: m.id, color }, batch);
      } else if (m.kind === 'group') {
        this.send('group.update', { id: m.id, color }, batch);
      }
    }
  }

  // openIndexDialog는 인덱스를 만들거나 고친다.
  //
  // 만들기와 고치기가 같은 창인 이유: 창이 둘이면 "이 설정은 만들 때만 되는 것"이
  // 생기고, 그 사실은 눌러 보고서야 알게 된다. 예전에는 만든 뒤에 고칠 방법이 아예
  // 없어서, 이름 한 글자를 고치려면 지웠다 다시 만들어야 했다.
  openIndexDialog(ref, existing = null) {
    const tbl = ref.table();
    if (!tbl) return;
    const editing = Boolean(existing);
    const nameInput = input({ value: existing?.name ?? `ix_${tbl.name}_` });
    const uniqueBox = checkbox('UNIQUE (값이 겹칠 수 없음)', { checked: Boolean(existing?.unique) });

    // 컬럼은 **순서가 있는 목록**이다.
    //
    // 복합 인덱스의 앞뒤 순서는 어떤 조회가 이 인덱스를 탈 수 있는지를 정한다
    // ((a,b) 인덱스는 a로 찾는 조회에 쓰이지만 b로 찾는 조회에는 쓰이지 않는다).
    // 고를 수만 있고 순서를 못 바꾸면 그 절반은 손댈 수 없는 셈이다.
    let picked = (existing?.columns ?? [])
      .filter((c) => c.column)
      .map((c) => ({ name: c.column, desc: Boolean(c.descending) }));
    // 식 기반 인덱스는 이 창에서 다루지 않는다. 여기서 컬럼만 고쳐 보내면 식이
    // 조용히 사라진다 — 그래서 아예 열지 않고 이유를 말한다.
    const hasExpression = (existing?.columns ?? []).some((c) => !c.column && c.expression);
    if (hasExpression) {
      toast('식으로 만든 인덱스는 이 창에서 고칠 수 없습니다', 'error');
      return;
    }

    const listBox = h('div.erd-idx-cols');
    const drawList = () => {
      const inIndex = new Set(picked.map((c) => c.name.toLowerCase()));
      const rows = [];
      picked.forEach((c, i) => {
        rows.push(h('div.erd-idx-col.is-on', {},
          h('label.checkbox', {},
            h('input', {
              type: 'checkbox',
              checked: true,
              onchange: () => { picked = picked.filter((x) => x.name !== c.name); drawList(); },
            }),
            h('span', {}, c.name)),
          h('span.erd-idx-ord', {}, `${i + 1}`),
          h('button.btn.btn-small', {
            type: 'button',
            title: c.desc ? '내림차순 (DESC)' : '오름차순 (ASC)',
            onclick: () => { c.desc = !c.desc; drawList(); },
          }, c.desc ? '내림 ↓' : '오름 ↑'),
          h('div.erd-col-order', {},
            h('button.icon-btn', {
              type: 'button', title: '앞으로', disabled: i === 0,
              onclick: () => {
                picked.splice(i - 1, 0, picked.splice(i, 1)[0]);
                drawList();
              },
            }, h('span.erd-move-arrow', {}, '▲')),
            h('button.icon-btn', {
              type: 'button', title: '뒤로', disabled: i === picked.length - 1,
              onclick: () => {
                picked.splice(i + 1, 0, picked.splice(i, 1)[0]);
                drawList();
              },
            }, h('span.erd-move-arrow', {}, '▼'))),
        ));
      });
      for (const col of tbl.columns ?? []) {
        if (inIndex.has(col.name.toLowerCase())) continue;
        rows.push(h('div.erd-idx-col', {},
          h('label.checkbox', {},
            h('input', {
              type: 'checkbox',
              onchange: () => { picked.push({ name: col.name, desc: false }); drawList(); },
            }),
            h('span', {}, col.name)),
          h('span.muted.small', {}, col.rawType || col.type?.base || '')));
      }
      mount(listBox, rows);
    };
    drawList();

    // 부분 인덱스는 DB가 받아 주는 곳에서만 보여준다. 적을 수는 있는데 생성되는
    // SQL에는 들어가지 않는 칸은, 적어 둔 사람에게 조용한 거짓말이 된다.
    const partial = ['postgres', 'mssql', 'sqlite'].includes(this.doc.dialect);
    const whereInput = partial
      ? input({ value: existing?.where ?? '', placeholder: '예: deleted_at IS NULL' })
      : null;

    openModal({
      title: editing ? `인덱스 설정 — ${existing.name}` : '인덱스 추가',
      width: 520,
      body: () => [
        h('label.field', {}, h('span.field-label', {}, '이름'), nameInput),
        uniqueBox,
        h('div.field', {}, h('span.field-label', {}, '컬럼'), listBox,
          h('p.field-help', {}, '고른 순서가 곧 인덱스의 컬럼 순서입니다. ▲▼로 바꾸세요.')),
        whereInput
          ? h('label.field', {}, h('span.field-label', {}, '부분 조건 (WHERE)'), whereInput,
            h('p.field-help', {}, '조건에 맞는 행만 인덱스에 넣습니다. 비우면 전체입니다.'))
          : null,
        // 방식은 여기서 고치지 않는다. 생성되는 DDL이 USING 을 적지 않으므로,
        // 고칠 수 있게 두면 바꿔도 대상 DB는 그대로인 상태가 남는다.
        existing?.type
          ? h('p.field-help', {}, `방식: ${existing.type} (DB가 보고한 값이며 여기서 바꾸지 않습니다)`)
          : null,
      ],
      footer: (closeModal) => [
        h('button.btn', { type: 'button', onclick: closeModal }, '취소'),
        h('button.btn.btn-primary', {
          type: 'button',
          onclick: () => {
            const name = nameInput.value.trim();
            if (!name) {
              toast('이름을 적으세요', 'error');
              return;
            }
            if (!picked.length) {
              toast('컬럼을 하나 이상 선택하세요', 'error');
              return;
            }
            const payload = {
              table: ref.serverKey,
              // 고칠 때 name은 **찾는 열쇠**다. 새 이름은 따로 보낸다.
              name: editing ? existing.name : name,
              columns: picked.map((c) => c.name),
              descending: picked.filter((c) => c.desc).map((c) => c.name),
              unique: uniqueBox.querySelector('input').checked,
            };
            if (whereInput) payload.where = whereInput.value.trim();
            if (editing && name !== existing.name) payload.newName = name;
            this.send(editing ? 'index.update' : 'index.add', payload);
            closeModal();
          },
        }, editing ? '저장' : '추가'),
      ],
    });
  }

  // openFKDialog는 외래키를 만들거나 고친다.
  //
  // 만들기와 고치기가 같은 창인 이유: 예전에는 만든 뒤에 이름과 컬럼을 고칠 방법이
  // 아예 없었다(인스펙터에는 ON DELETE·ON UPDATE만 있었다). 컬럼 하나를 잘못 고른
  // 외래키를 지우고 다시 만드는 것은, 관계선이 사라졌다 나타나는 일이기도 하다.
  //
  // 짝을 맞추는 방식이 요점이다. 참조할 **키**를 먼저 고르게 한다(기본키나 고유
  // 인덱스). 그러면 짝의 수가 그 키의 컬럼 수로 정해지므로, 컬럼 수가 서로 다른
  // 외래키(서버가 거부한다)를 만들 수 없다.
  openFKDialog(ref, existing = null) {
    const tbl = ref.table();
    if (!tbl) return;
    const editing = Boolean(existing);
    const others = (this.doc.schema?.tables ?? []);
    const myCols = tbl.columns ?? [];

    const nameInput = input({ value: existing?.name ?? `fk_${tbl.name}_` });
    const refKeyOf = (fk) => `${fk.refNamespace ? `${fk.refNamespace}.` : ''}${fk.refTable}`.toLowerCase();
    // 처음 고를 대상은 **자기 자신이 아닌** 첫 테이블이다.
    //
    // 목록의 첫 항목을 그대로 두면 이 테이블 자신이 골라진다. 자기 참조도 있는
    // 구조지만 드물어서, 그렇게 두면 거의 매번 한 번 더 바꿔야 한다.
    const firstOther = others.find((t) => tableKey(t) !== ref.serverKey) ?? others[0];
    const refSelect = select(others.map((t) => ({ value: tableKey(t), label: tableDisplay(t) })),
      { value: existing ? refKeyOf(existing) : (firstOther ? tableKey(firstOther) : '') });

    const keyWrap = h('div');
    const pairWrap = h('div.erd-fk-pairs');
    const actions = ['', 'NO ACTION', 'RESTRICT', 'CASCADE', 'SET NULL', 'SET DEFAULT']
      .map((a) => ({ value: a, label: a || '(지정 없음)' }));
    const onDeleteSelect = select(actions, { value: existing?.onDelete ?? '' });
    const onUpdateSelect = select(actions, { value: existing?.onUpdate ?? '' });

    // 짝 상태: 참조 키의 컬럼 순서대로, 이 테이블의 어느 컬럼을 붙일지.
    let refCols = [...(existing?.refColumns ?? [])];
    let localCols = [...(existing?.columns ?? [])];

    const target = () => others.find((t) => tableKey(t) === refSelect.value) ?? null;

    // 짝을 처음 채울 때의 짐작. user_id → users(id) 처럼 이름에 단서가 있다.
    const guessLocal = (refCol, used) => {
      const t = target();
      const cands = [
        `${t?.name ?? ''}_${refCol}`,
        `${(t?.name ?? '').replace(/s$/, '')}_${refCol}`,
        refCol,
      ].map((x) => x.toLowerCase());
      for (const want of cands) {
        const hit = myCols.find((c) => c.name.toLowerCase() === want && !used.has(c.name));
        if (hit) return hit.name;
      }
      const free = myCols.find((c) => !used.has(c.name));
      return free?.name ?? '';
    };

    const drawPairs = () => {
      const t = target();
      const used = new Set();
      const rows = refCols.map((refCol, i) => {
        if (!localCols[i] || !myCols.some((c) => c.name === localCols[i])) {
          localCols[i] = guessLocal(refCol, used);
        }
        used.add(localCols[i]);
        const pick = select(myCols.map((c) => ({
          value: c.name,
          label: `${c.name} — ${c.rawType || c.type?.base || ''}`,
        })), { value: localCols[i] });
        pick.addEventListener('change', () => {
          localCols[i] = pick.value;
          drawPairs();
        });
        // 타입이 다르면 대상 DB가 외래키를 거부한다. 막지는 않되(우리가 읽지 못하는
        // 타입도 있다) 실행 전에 알 수 있게 적어 둔다.
        const mine = myCols.find((c) => c.name === localCols[i]);
        const theirs = (t?.columns ?? []).find((c) => c.name.toLowerCase() === refCol.toLowerCase());
        const mismatch = mine && theirs
          && (mine.type?.base ?? '') !== (theirs.type?.base ?? '');
        return h('div.erd-fk-pair', {},
          pick,
          h('span.erd-fk-arrow', {}, '→'),
          h('span.erd-fk-ref', {}, `${t?.name ?? ''}.${refCol}`),
          mismatch ? badge('타입 다름', 'warn') : null);
      });
      mount(pairWrap, rows.length ? rows : h('p.muted.small', {}, '참조할 키를 고르세요'));
    };

    // 참조할 키(기본키·고유 인덱스)를 고른다. 짝의 수가 여기서 정해진다.
    const drawKeys = () => {
      const t = target();
      const sets = t ? uniqueColumnSets(t) : [];
      if (!t || sets.length === 0) {
        refCols = [];
        mount(keyWrap, h('p.notice.notice-warn', {},
          icon('alert'),
          h('span', {}, '이 테이블에는 기본키나 고유 인덱스가 없어 참조할 수 없습니다.')));
        drawPairs();
        return;
      }
      // 지금 참조하고 있는 키가 목록에 있으면 그것을 고른 상태로 둔다.
      const want = refCols.join(',').toLowerCase();
      const match = sets.find((set) => set.join(',').toLowerCase() === want);
      const chosen = match ?? sets[0];
      if (!match) {
        refCols = [...chosen];
        localCols = editing && refCols.length === (existing.columns ?? []).length
          ? [...existing.columns] : [];
      }
      const keySelect = select(sets.map((set) => ({
        value: set.join(','),
        label: set.length > 1 ? `${set.join(', ')} (복합)` : set[0],
      })), { value: chosen.join(',') });
      keySelect.addEventListener('change', () => {
        refCols = keySelect.value.split(',');
        localCols = [];
        drawPairs();
      });
      mount(keyWrap, keySelect);
      drawPairs();
    };

    refSelect.addEventListener('change', () => {
      refCols = [];
      localCols = [];
      drawKeys();
    });
    drawKeys();

    openModal({
      title: editing ? `외래키 설정 — ${existing.name}` : '외래키 추가',
      width: 560,
      body: () => [
        h('label.field', {}, h('span.field-label', {}, '이름'), nameInput),
        h('label.field', {}, h('span.field-label', {}, '참조할 테이블'), refSelect),
        h('div.field', {}, h('span.field-label', {}, '참조할 키'), keyWrap,
          h('p.field-help', {}, '기본키나 고유 인덱스만 참조할 수 있습니다. 고른 키의 컬럼 수만큼 짝이 생깁니다.')),
        h('div.field', {}, h('span.field-label', {}, '컬럼 짝'), pairWrap),
        h('label.field', {}, h('span.field-label', {}, 'ON DELETE'), onDeleteSelect),
        h('label.field', {}, h('span.field-label', {}, 'ON UPDATE'), onUpdateSelect),
        h('p.field-help', {},
          '참조하는 행이 지워지거나 키가 바뀔 때 이 테이블의 행을 어떻게 할지 정합니다.'),
      ],
      footer: (close) => [
        h('button.btn', { type: 'button', onclick: close }, '취소'),
        h('button.btn.btn-primary', {
          type: 'button',
          onclick: () => {
            const t = target();
            const name = nameInput.value.trim();
            if (!name) {
              toast('이름을 적으세요', 'error');
              return;
            }
            if (!t || !refCols.length) {
              toast('참조할 테이블과 키를 고르세요', 'error');
              return;
            }
            if (localCols.length !== refCols.length || localCols.some((c) => !c)) {
              toast('컬럼 짝을 모두 고르세요', 'error');
              return;
            }
            // 같은 컬럼을 두 번 쓰면 복합 외래키가 성립하지 않는다.
            if (new Set(localCols.map((c) => c.toLowerCase())).size !== localCols.length) {
              toast('같은 컬럼을 두 번 쓸 수 없습니다', 'error');
              return;
            }
            const payload = {
              table: ref.serverKey,
              // 고칠 때 name은 **찾는 열쇠**다. 새 이름은 따로 보낸다.
              name: editing ? existing.name : name,
              columns: localCols,
              refTable: t.name,
              refNamespace: t.namespace ?? '',
              refColumns: refCols,
              onDelete: onDeleteSelect.value,
              onUpdate: onUpdateSelect.value,
            };
            if (editing && name !== existing.name) {
              payload.newName = name;
              // 고른 것이 이 관계선이면 선택도 새 이름으로 옮긴다. 그러지 않으면
              // 이름을 바꾼 순간 인스펙터가 없는 대상을 가리켜 비어 버린다.
              const oldID = `${ref.serverKey}.${existing.name}`;
              if (this.sel?.kind === 'link' && this.sel.id === oldID) {
                this.sel = { kind: 'link', id: `${ref.serverKey}.${name}` };
                this.marks = [this.sel];
              }
            }
            this.send(editing ? 'fk.update' : 'fk.add', payload);
            close();
          },
        }, editing ? '저장' : '추가'),
      ],
    });
  }

  // ---------- 도메인 (재사용 타입) ----------

  // domainView는 도메인을 모아 관리하는 사이드바다.
  //
  // 테이블 속성 옆에 탭으로 두는 이유: 도메인은 특정 테이블의 것이 아니라 설계 전체의
  // 어휘다. 테이블을 고른 상태에서만 볼 수 있으면 "먼저 정의부터 정리한다"는 순서가
  // 성립하지 않는다.
  domainView() {
    const ro = !this.canEdit;
    const domains = this.doc.domains ?? [];
    const usage = this.domainUsage();

    return [
      h('div.erd-panel-head', {},
        h('h2', {}, '도메인'),
        ro ? null : h('button.btn.btn-small.btn-primary', {
          type: 'button', onclick: () => this.openDomainDialog(null),
        }, icon('plus'), '추가'),
      ),
      h('div.erd-panel-body', {},
        h('p.field-help', {},
          '자주 쓰는 타입에 이름을 붙여 둡니다. 컬럼의 타입 고르개에서 도메인을 고르면 '
          + '그 정의를 그대로 쓰고, 정의를 고치면 쓰고 있는 컬럼이 함께 바뀝니다.'),
        domains.length === 0
          ? h('p.muted', {}, '아직 도메인이 없습니다.')
          : h('div.erd-domain-list', {}, domains.map((d) => this.domainRow(d, usage.get(d.name.toLowerCase()) ?? [], ro))),
        h('p.field-help', {},
          '도메인은 이 설계 안에서만 쓰는 개념입니다. 마이그레이션에는 도메인이 아니라 '
          + '그 결과인 구체 타입이 들어갑니다.'),
      ),
    ];
  }

  // domainUsage는 도메인 이름 → 쓰는 컬럼 목록이다.
  domainUsage() {
    const out = new Map();
    for (const t of this.doc.schema?.tables ?? []) {
      for (const c of t.columns ?? []) {
        if (!c.domain) continue;
        const key = c.domain.toLowerCase();
        if (!out.has(key)) out.set(key, []);
        out.get(key).push(`${tableDisplay(t)}.${c.name}`);
      }
    }
    return out;
  }

  domainRow(domain, users, ro) {
    return h('div.erd-domain', {},
      h('div.erd-domain-head', {},
        h('span.erd-domain-name', {}, domain.name),
        h('code.erd-domain-type', {}, domain.type),
        ro ? null : h('button.icon-btn', {
          type: 'button', title: '수정', onclick: () => this.openDomainDialog(domain),
        }, icon('edit', 13)),
        ro ? null : h('button.icon-btn', {
          type: 'button',
          title: '삭제',
          onclick: () => this.deleteDomain(domain, users.length),
        }, icon('trash', 13)),
      ),
      h('div.erd-domain-meta', {},
        domain.nullable === false ? badge('NOT NULL', 'neutral') : null,
        domain.nullable === true ? badge('NULL 허용', 'neutral') : null,
        domain.default ? badge(`기본값 ${truncate(domain.default, 20)}`, 'neutral') : null,
        users.length
          ? h('span.muted', {}, `${users.length}개 컬럼에서 사용`)
          : h('span.muted', {}, '사용 중인 컬럼 없음'),
      ),
      domain.comment ? h('p.erd-domain-comment', {}, domain.comment) : null,
      users.length
        ? h('details.erd-domain-users', {},
            h('summary', {}, '사용 중인 컬럼'),
            h('ul.note-list', {}, users.map((u) => h('li', {}, u))))
        : null,
    );
  }

  // openDomainDialog는 도메인을 만들거나 고치는 창이다. domain이 null이면 새로 만든다.
  async openDomainDialog(domain) {
    const cat = await this.ensureTypeCatalog();
    const nameInput = input({ value: domain?.name ?? '', placeholder: '예: email' });
    const typeInput = input({ value: domain?.type ?? '', placeholder: '예: varchar(320)' });
    const commentInput = input({ value: domain?.comment ?? '', placeholder: '이 도메인이 무엇인지' });
    const defaultInput = input({ value: domain?.default ?? '', placeholder: '예: CURRENT_TIMESTAMP' });
    // NULL 여부는 세 가지다: 도메인이 정하지 않음 / 허용 / 불가.
    // "정하지 않음"이 필요한 이유는 같은 뜻의 컬럼이라도 표마다 필수 여부가 다르기 때문이다.
    const nullSelect = select([
      { value: '', label: '컬럼마다 다름 (도메인이 정하지 않음)' },
      { value: 'yes', label: 'NULL 허용' },
      { value: 'no', label: 'NOT NULL' },
    ], { value: domain?.nullable === true ? 'yes' : domain?.nullable === false ? 'no' : '' });

    // 타입은 컬럼과 같은 고르개를 쓴다. 도메인만 다른 방식으로 타입을 정하면
    // "여기서는 왜 안 되지"가 생긴다.
    const typePick = searchPicker({
      items: categories(cat).flatMap((group) => group.types.map((t) => ({
        value: t.name,
        label: `${group.name} · ${t.label}`,
        hint: t.name,
        group: group.name,
      }))),
      value: parseType(domain?.type ?? '', cat)?.def?.name ?? '',
      placeholder: '타입 이름이나 설명으로 검색 (예: int, 정수, 날짜)',
      emptyLabel: '(목록에서 고르기)',
      onPick: () => syncArg(),
    });
    const argWrap = h('div');
    const syncArg = () => {
      const def = (cat.types ?? []).find((t) => t.name === typePick.value);
      mount(argWrap, def?.param
        ? field(paramLabel(def), input({
          value: def.default ?? '',
          placeholder: paramPlaceholder(def),
          oninput: (e) => { typeInput.value = buildType(def, { arg: e.target.value }); },
        }))
        : null);
      if (def) typeInput.value = buildType(def, { arg: def.default ?? '' });
    };
    const save = h('button.btn.btn-primary', { type: 'button' }, domain ? '저장' : '추가');
    const close = openModal({
      title: domain ? `도메인 ${domain.name}` : '도메인 추가',
      width: 560,
      body: [
        field('이름', nameInput, '컬럼 타입 자리에서 이 이름으로 고릅니다.'),
        field('타입 고르기', typePick.node),
        argWrap,
        field('타입', typeInput, '고르개로 채우거나 직접 적을 수 있습니다.'),
        field('NULL 여부', nullSelect),
        field('기본값', defaultInput, '비워 두면 도메인이 기본값에 관여하지 않습니다.'),
        field('설명', commentInput),
      ],
      footer: (dismiss) => [
        h('button.btn', { type: 'button', onclick: dismiss }, '취소'),
        save,
      ],
    });

    save.addEventListener('click', () => {
      const name = nameInput.value.trim();
      const type = typeInput.value.trim();
      if (!name || !type) {
        toast('이름과 타입을 모두 적어야 합니다', 'error');
        return;
      }
      const nullable = nullSelect.value === '' ? null : nullSelect.value === 'yes';
      if (domain) {
        this.send('domain.update', {
          name: domain.name,
          newName: name !== domain.name ? name : undefined,
          type,
          nullable: nullable === null ? undefined : nullable,
          clearNullable: nullable === null,
          default: defaultInput.value.trim(),
          comment: commentInput.value.trim(),
        });
      } else {
        this.send('domain.add', {
          name,
          type,
          nullable: nullable === null ? undefined : nullable,
          default: defaultInput.value.trim(),
          comment: commentInput.value.trim(),
        });
      }
      close();
    });
  }

  async deleteDomain(domain, usedCount) {
    const ok = await confirmDialog({
      title: '도메인 삭제',
      message: usedCount
        ? `"${domain.name}" 을 지웁니다. 이 도메인을 쓰던 ${usedCount}개 컬럼은 지금 타입을 그대로 유지하고 연결만 끊깁니다.`
        : `"${domain.name}" 을 지웁니다.`,
      confirmLabel: '삭제',
      danger: true,
    });
    if (!ok) return;
    this.send('domain.delete', { name: domain.name });
  }

  // ---------- 타입 고르개 ----------

  // ensureTypeCatalog는 이 초안이 향하는 DB의 타입 목록을 한 번 받아 둔다.
  //
  // 화면을 그리기 전에 미리 받는 이유: 타입을 고르려고 버튼을 눌렀을 때 목록이
  // 없으면 빈 창이 떴다가 채워진다. 목록은 DB 종류마다 고정이므로 한 번이면 된다.
  async ensureTypeCatalog() {
    if (this.typeCatalog) return this.typeCatalog;
    try {
      this.typeCatalog = await loadTypeCatalog(this.doc?.dialect || this.doc?.schema?.dialect || '');
    } catch {
      // 목록을 못 받아도 편집은 계속되어야 한다. 그때는 직접 입력만 가능하다.
      this.typeCatalog = { dialect: this.doc?.dialect ?? '', types: [] };
    }
    return this.typeCatalog;
  }

  // openTypeDialog는 컬럼의 타입을 고르는 창을 연다.
  //
  // 창으로 뺀 이유: 타입마다 필요한 입력이 다르다(길이 / 자릿수 / 값 목록 / UNSIGNED /
  // 배열). 그것을 컬럼 한 줄에 모두 밀어 넣으면 줄이 읽히지 않고, 좁은 화면에서는
  // 아예 잘린다. 여기서는 고른 타입에 필요한 칸만 나타난다.
  async openTypeDialog(ref, currentName) {
    const cat = await this.ensureTypeCatalog();
    const colName = typeof currentName === 'function' ? currentName() : currentName;
    const col = (ref.table()?.columns ?? []).find((c) => c.name === colName);
    if (!col) {
      toast('컬럼을 찾을 수 없습니다', 'error');
      return;
    }

    const domains = this.doc.domains ?? [];
    const current = parseType(col.rawType || '', cat);
    const state0 = {
      // 도메인을 고르면 타입 칸은 잠긴다. 도메인이 타입을 정하기 때문이다.
      domain: col.domain || '',
      manual: !current && Boolean(col.rawType),
      def: current?.def ?? null,
      arg: current?.params.arg ?? '',
      unsigned: current?.params.unsigned ?? false,
      array: current?.params.array ?? false,
      raw: col.rawType || '',
    };

    const preview = h('code.erd-type-preview');
    const paramWrap = h('div.erd-type-params');
    // 파라미터 칸(길이·자릿수·값 목록)은 **한 번만** 만든다.
    //
    // 예전에는 refresh()가 이 칸을 매번 새로 그렸다. 그런데 이 칸의 입력이 곧
    // refresh()를 부르므로, 글자를 하나 치면 지금 타이핑 중인 그 요소가 버려지고
    // 포커스가 사라졌다 — 한 글자마다 다시 클릭해야 적을 수 있었다.
    //
    // 다시 그려야 하는 것은 라벨과 안내 문구뿐이고, 그것은 자리에서 고칠 수 있다.
    const paramInput = input({ value: state0.arg });
    const paramLabelEl = h('span.field-label');
    mount(paramWrap, h('label.field', {}, paramLabelEl, paramInput));
    // 어떤 타입의 칸을 보여주고 있는지 기억한다. 타입이 바뀔 때만 값을 그 타입의
    // 기본값으로 되돌린다 — 매번 되돌리면 사람이 지운 값이 되살아난다.
    let paramFor = state0.def?.name ?? '';
    const manualInput = input({ value: state0.raw, placeholder: '예: varchar(255)' });
    const manualWrap = h('label.field', {},
      h('span.field-label', {}, '직접 입력'), manualInput,
      h('span.field-help', {}, '대상 DB의 타입 문자열을 그대로 씁니다. 목록에 없는 타입도 적을 수 있습니다.'));

    // 타입은 수십 개다. select 로 두면 스크롤로 훑어야 하고, "int" 나 "정수" 중
    // 어느 쪽으로 떠오르든 찾는 길이 하나뿐이다. 입력으로 걸러 고른다.
    const typePick = searchPicker({
      items: categories(cat).flatMap((group) => group.types.map((t) => ({
        value: t.name,
        label: `${group.name} · ${t.label}`,
        hint: t.name,
        group: group.name,
      }))),
      value: state0.def?.name ?? '',
      placeholder: '타입 이름이나 설명으로 검색 (예: int, 정수, 날짜)',
      emptyLabel: '(고르기)',
      onPick: (next) => {
        state0.def = (cat.types ?? []).find((t) => t.name === next) ?? null;
        state0.arg = state0.def?.default ?? '';
        refresh();
      },
    });

    const domainSelect = select([
      { value: '', label: '(도메인 없이 타입 직접 지정)' },
      ...domains.map((d) => ({ value: d.name, label: `${d.name} — ${d.type}` })),
    ], { value: state0.domain });

    // 기본값을 여기서도 정한다. 타입을 고르는 순간이 기본값을 떠올리는 순간이고
    // (created_at 을 만들면서 now() 를 같이 정한다), 창을 닫고 컬럼 줄로 돌아가
    // 다시 찾아 적는 것은 같은 일을 두 번 하는 것이다.
    //
    // 제안은 서버 카탈로그에서 온다. 함수 이름이 DB마다 다르므로(now() /
    // GETDATE() / SYSTIMESTAMP) 화면이 짐작하면 DB를 하나 더 지원할 때마다 두 곳을
    // 고쳐야 한다. 고를 수만 있는 것이 아니라 그대로 적을 수도 있다 — 목록에 없는
    // 식을 못 적게 하면 이 칸은 쓸 수 없게 된다.
    const defaultPick = suggestInput({
      items: defaultsFor(cat, state0.def),
      value: col.default ?? '',
      placeholder: '예: 0, \'\', now() — 비우면 기본값 없음',
    });
    const defaultWrap = field('기본값', defaultPick.node,
      '비워 두면 기본값을 두지 않습니다. 함수는 대상 DB의 문법 그대로 적습니다.');

    const unsignedBox = checkbox('UNSIGNED (음수 없음)', { checked: state0.unsigned });
    const arrayBox = checkbox('배열 ([])', { checked: state0.array });
    // 자동 증가를 타입 창에 둔 이유: 붙일 수 있는 타입이 정해져 있고(정수 계열),
    // 그 경계가 DB마다 다르다. 타입을 고르는 자리에서 함께 정하면 "이 타입에는
    // 안 됩니다"를 고른 직후에 알 수 있다.
    const autoBox = checkbox(`자동 증가 (${cat.autoIncrement || 'AUTO_INCREMENT'})`,
      { checked: Boolean(col.identity) });
    const autoNote = h('p.field-help', {}, cat.autoIncrementNote ?? '');
    const autoWrap = h('div', {}, autoBox, autoNote);
    const noteLine = h('p.field-help');

    const compose = () => {
      if (state0.manual) return manualInput.value.trim();
      return buildType(state0.def, {
        arg: paramInput.value,
        unsigned: unsignedBox.querySelector('input').checked,
        array: arrayBox.querySelector('input').checked,
      });
    };

    const refresh = () => {
      const usingDomain = Boolean(domainSelect.value);
      const def = state0.def;
      // 도메인을 골랐으면 타입 칸은 볼 수만 있게 둔다. 두 곳에서 타입을 정하면
      // 어느 쪽이 이겼는지 화면만 보고 알 수 없다.
      typePick.disabled = usingDomain || state0.manual;
      manualInput.disabled = usingDomain;
      manualWrap.style.display = state0.manual ? '' : 'none';
      arrayBox.style.display = cat.arrays && !state0.manual && !usingDomain ? '' : 'none';
      unsignedBox.style.display = def?.unsigned && !state0.manual && !usingDomain ? '' : 'none';
      // 도메인은 여러 컬럼이 함께 쓰는 정의라 자동 증가를 얹을 자리가 아니다.
      // 직접 입력은 우리가 읽을 수 없는 문자열이므로 막지 않는다 — 모르는 것과
      // 안 되는 것은 다르다.
      autoWrap.style.display = usingDomain ? 'none'
        : (state0.manual || identityFits(def, state0.arg) ? '' : 'none');

      const showParam = !usingDomain && !state0.manual && Boolean(def?.param);
      paramWrap.style.display = showParam ? '' : 'none';
      if (showParam) {
        paramLabelEl.textContent = paramLabel(def);
        paramInput.placeholder = paramPlaceholder(def);
        if (paramFor !== def.name) {
          paramFor = def.name;
          state0.arg = state0.arg || def.default || '';
          paramInput.value = state0.arg;
        }
      } else {
        paramFor = '';
      }

      // 도메인을 고르면 기본값도 도메인이 정한다. 두 곳에서 정하면 어느 쪽이
      // 이겼는지 화면만 보고 알 수 없다.
      defaultWrap.style.display = usingDomain ? 'none' : '';
      defaultPick.setItems(defaultsFor(cat, state0.manual ? null : def));

      const dom = domains.find((d) => d.name === domainSelect.value);
      mount(noteLine, usingDomain
        ? `도메인 "${dom?.name}" 의 정의를 씁니다: ${dom?.type}`
        : (def?.note ?? ''));
      preview.textContent = usingDomain ? (dom?.type ?? '') : (compose() || '(타입을 고르세요)');
    };

    domainSelect.addEventListener('change', refresh);
    paramInput.addEventListener('input', () => {
      state0.arg = paramInput.value;
      refresh();
    });
    manualInput.addEventListener('input', refresh);
    unsignedBox.addEventListener('change', refresh);
    arrayBox.addEventListener('change', refresh);

    const manualToggle = checkbox('타입을 직접 입력', { checked: state0.manual });
    manualToggle.addEventListener('change', () => {
      state0.manual = manualToggle.querySelector('input').checked;
      if (state0.manual && !manualInput.value) manualInput.value = compose();
      refresh();
    });

    const save = h('button.btn.btn-primary', { type: 'button' }, '적용');
    const close = openModal({
      title: `${col.name} 의 타입`,
      width: 560,
      body: [
        domains.length
          ? field('도메인', domainSelect, '도메인을 고르면 그 정의(타입·NULL 여부·기본값)를 따릅니다.')
          : h('p.field-help', {}, '도메인을 만들어 두면 여기서 골라 재사용할 수 있습니다 (상단 "도메인").'),
        field('타입', typePick.node),
        paramWrap,
        unsignedBox,
        arrayBox,
        autoWrap,
        defaultWrap,
        manualToggle,
        manualWrap,
        noteLine,
        h('div.field', {}, h('span.field-label', {}, '결과'), preview),
      ],
      footer: (dismiss) => [
        h('button.btn', { type: 'button', onclick: dismiss }, '취소'),
        save,
      ],
    });
    refresh();

    save.addEventListener('click', () => {
      const name = typeof currentName === 'function' ? currentName() : currentName;
      if (domainSelect.value) {
        this.send('column.update', {
          table: ref.serverKey, name, domain: domainSelect.value,
        });
        close();
        return;
      }
      const text = compose();
      if (!text) {
        toast('타입을 고르거나 직접 입력하세요', 'error');
        return;
      }
      // 도메인을 쓰던 컬럼에서 타입을 직접 정하면 연결을 끊는다(서버도 같은 규칙이다).
      //
      // identity를 늘 함께 보내는 이유: 자동 증가 칸이 사라지는 경우(정수가 아닌 타입을
      // 골랐다)에도 값을 정리해야 한다. 그러지 않으면 varchar인데 자동 증가가 켜진
      // 컬럼이 남고, ERD에서는 멀쩡해 보이다가 마이그레이션에서 거부된다.
      const wantsAuto = autoWrap.style.display !== 'none'
        && autoBox.querySelector('input').checked;
      this.send('column.update', {
        table: ref.serverKey, name, type: text, domain: '', identity: wantsAuto,
        // 기본값도 함께 보낸다. 비우면 서버가 "기본값 없음"으로 정리한다.
        default: defaultPick.value.trim(),
      });
      close();
    });
  }

  openCheckDialog(ref) {
    const tbl = ref.table();
    if (!tbl) return;
    const nameInput = input({ value: `ck_${tbl.name}_`, autofocus: true });
    const exprInput = input({ placeholder: '예: amount >= 0' });
    openModal({
      title: '체크 제약 추가',
      width: 480,
      body: () => [
        h('label.field', {}, h('span.field-label', {}, '이름'), nameInput),
        h('label.field', {}, h('span.field-label', {}, '조건식'), exprInput,
          h('span.field-help', {}, '대상 DB의 SQL 식으로 그대로 사용됩니다')),
      ],
      footer: (close) => [
        h('button.btn', { type: 'button', onclick: close }, '취소'),
        h('button.btn.btn-primary', {
          type: 'button',
          onclick: () => {
            this.send('check.add', {
              table: ref.serverKey, name: nameInput.value.trim(), expression: exprInput.value,
            });
            close();
          },
        }, '추가'),
      ],
    });
  }

  async showDiff() {
    const body = h('div', {}, spinner('대상 DB와 비교하는 중…'));
    openModal({ title: '초안 ↔ 대상 DB 비교', width: 780, body: () => body });
    try {
      const res = await api.post(`/erd/documents/${encodeURIComponent(this.docID)}/diff`);
      mount(body, diffView(res));
    } catch (err) {
      mount(body, errorPanel(err));
    }
  }

  // createMigration은 이 초안을 대상 DB에 적용하는 계획을 만든다.
  //
  // 만들기만 하고 실행하지는 않는다. 실행은 리뷰·승인을 거친 뒤 마이그레이션
  // 화면에서 이뤄진다 — 초안 화면에서 곧바로 실행할 수 있으면 그 절차가 무의미해진다.
  //
  // 만들자마자 실행될 SQL을 보여준다. 계획을 만드는 사람은 대개 그것을 붙여 두거나
  // 리뷰어에게 보내야 하는데, 그러려면 마이그레이션 화면까지 따라가 다시 찾아야 했다.
  createMigration() {
    const titleInput = input({ value: this.doc.name });
    const box = h('div');
    let migration = null;
    const picker = basePicker({ connectionId: this.connection?.id });

    openModal({
      title: '마이그레이션 만들기',
      width: 720,
      body: () => [
        h('p.modal-message', {},
          '고른 기준과 이 초안의 차이로 마이그레이션 계획을 만듭니다. ' +
          '만든 뒤 리뷰와 승인을 거쳐야 실행할 수 있습니다.'),
        h('label.field', {}, h('span.field-label', {}, '제목'), titleInput),
        picker.node,
        box,
      ],
      footer: (close) => {
        const openBtn = h('a.btn', { hidden: true }, '마이그레이션 화면으로');
        const makeBtn = h('button.btn.btn-primary', { type: 'button' }, '만들기');
        makeBtn.addEventListener('click', async () => {
          makeBtn.disabled = true;
          mount(box, spinner('기준과 비교하는 중…'));
          try {
            const res = await api.post('/migrations/', {
              docId: this.docID, title: titleInput.value, base: picker.value,
            });
            migration = res.migration;
            toast('마이그레이션 계획을 만들었습니다', 'success');
            mount(box, migrationCreatedView(migration));
            // 만들고 나면 같은 버튼을 다시 누를 일이 없다. 두 번 누르면
            // 같은 내용의 계획이 두 개 생긴다.
            makeBtn.hidden = true;
            titleInput.disabled = true;
            openBtn.hidden = false;
            openBtn.href = `/migrations/${encodeURIComponent(migration.id)}`;
          } catch (err) {
            makeBtn.disabled = false;
            mount(box, h('div.notice.notice-danger', {}, icon('alert'),
              h('div', {}, err.message ?? '만들지 못했습니다',
                err.detail ? h('p.muted', {}, err.detail) : null)));
          }
        });
        return [
          h('button.btn', { type: 'button', onclick: close }, '닫기'),
          openBtn,
          makeBtn,
        ];
      },
    });
  }

  // ---------- SQL 주고받기 ----------

  // importSQL은 SQL 스크립트를 읽어 초안에 얹는다.
  //
  // 두 단계다: 먼저 서버가 파싱해 "무엇이 바뀌는지"를 돌려주고(dryRun), 사용자가
  // 그것을 본 뒤에 적용한다. 불러오기는 테이블을 지울 수도 있는 동작이므로
  // 누르는 것과 결과 사이에 확인이 한 번 있어야 한다.
  importSQL() {
    const editor = codeEditor({
      language: 'sql', rows: 12, lineNumbers: true,
      placeholder: 'CREATE TABLE …  /  ALTER TABLE …  /  DROP TABLE …',
    });
    const preview = h('div.erd-import-preview');
    const fileInput = h('input', {
      type: 'file', accept: '.sql,.txt,text/plain', class: 'erd-import-file',
    });
    let plan = null;

    fileInput.addEventListener('change', async () => {
      const file = fileInput.files?.[0];
      if (!file) return;
      try {
        editor.value = await file.text();
        plan = null;
        mount(preview);
      } catch (err) {
        toast(`파일을 읽지 못했습니다: ${err.message}`, 'error');
      }
    });

    const request = async (dryRun) => api.post(
      `/erd/documents/${encodeURIComponent(this.docID)}/import`,
      { sql: editor.value, label: fileInput.files?.[0]?.name ?? '', dryRun },
    );

    openModal({
      title: 'SQL 불러오기',
      width: 760,
      body: () => [
        h('p.modal-message', {},
          '테이블 정의를 읽어 이 초안에 더합니다. 이름이 같은 테이블은 불러온 쪽으로 ' +
          '덮어쓰고, DROP TABLE 이 있으면 초안에서도 지웁니다. 초안에만 있는 테이블은 ' +
          '그대로 남습니다.'),
        h('label.field', {},
          h('span.field-label', {}, 'SQL 파일'), fileInput,
          h('span.field-help', {}, '파일을 고르면 아래 편집기에 채워집니다. 직접 붙여 넣어도 됩니다')),
        editor.el,
        preview,
      ],
      footer: (close) => {
        const applyBtn = h('button.btn.btn-primary', { type: 'button', disabled: true }, '적용');
        const checkBtn = h('button.btn', { type: 'button' }, icon('activity'), '무엇이 바뀌는지 보기');

        checkBtn.addEventListener('click', async () => {
          if (!editor.value.trim()) {
            toast('불러올 SQL을 입력하세요', 'error');
            return;
          }
          checkBtn.disabled = true;
          mount(preview, spinner('SQL을 읽는 중…'));
          try {
            plan = await request(true);
            mount(preview, importPreviewView(plan));
            applyBtn.disabled = false;
          } catch (err) {
            plan = null;
            applyBtn.disabled = true;
            mount(preview, h('div.notice.notice-danger', {}, icon('alert'),
              h('div', {}, err.message ?? '읽지 못했습니다',
                err.detail ? h('p.muted', {}, err.detail) : null)));
          } finally {
            checkBtn.disabled = false;
          }
        });

        applyBtn.addEventListener('click', async () => {
          const dropped = plan?.summary?.dropped ?? [];
          if (dropped.length > 0) {
            const ok = await confirmDialog({
              title: '테이블 삭제를 포함합니다',
              message: `${dropped.join(', ')} 이(가) 초안에서 지워집니다. `
                + '이 테이블을 참조하던 외래키도 함께 사라집니다.',
              confirmLabel: '적용',
              danger: true,
            });
            if (!ok) return;
          }
          applyBtn.disabled = true;
          try {
            const res = await request(false);
            // 서버가 적용 후 문서를 함께 돌려준다. 소켓 브로드캐스트를 기다리지
            // 않고 바로 반영해 두면, 소켓이 끊긴 상태에서도 결과가 보인다.
            if (res.document) {
              this.doc = res.document;
              this.pruneSelection();
              this.fitView();
              this.renderCanvas();
              this.renderPanel();
            }
            close();
            toast(`불러왔습니다 — 추가 ${res.summary.added.length}개, `
              + `덮어쓰기 ${res.summary.updated.length}개`, 'success');
          } catch (err) {
            applyBtn.disabled = false;
            toast(err.message ?? '적용하지 못했습니다', 'error', 6000);
          }
        });

        return [
          h('button.btn', { type: 'button', onclick: close }, '취소'),
          checkBtn,
          applyBtn,
        ];
      },
    });
  }

  // exportSQL은 초안 전체를 만드는 CREATE 스크립트를 보여준다.
  //
  // 대상 DB와의 차이가 아니라 "처음부터 만드는" 스크립트다. 대상이 없는 초안에서
  // 유일하게 얻을 수 있는 결과물이고, 대상이 있는 초안에서도 새 환경을 세울 때
  // 필요한 것은 대개 이쪽이다.
  async exportSQL() {
    const box = h('div', {}, spinner('SQL을 만드는 중…'));
    const dialects = (state.meta?.dbKinds ?? []).filter((k) => k.capabilities?.migrate);
    const dialectSelect = select(
      dialects.map((k) => ({ value: k.kind, label: k.label })),
      { value: this.doc.dialect },
    );

    const load = async () => {
      mount(box, spinner('SQL을 만드는 중…'));
      try {
        const res = await api.get(
          `/erd/documents/${encodeURIComponent(this.docID)}/ddl?dialect=`
          + encodeURIComponent(dialectSelect.value)
          + '&base=' + encodeURIComponent(picker.value));
        mount(box, exportView(res, this.doc.name));
      } catch (err) {
        mount(box, errorPanel(err));
      }
    };
    dialectSelect.addEventListener('change', load);
    // 기준을 바꾸면 그 자리에서 다시 만든다. 고르고 나서 또 눌러야 하면,
    // 무엇을 보고 있는지가 한 박자씩 어긋난다.
    const picker = basePicker({ connectionId: this.connection?.id, includeEmpty: true, onChange: load });

    openModal({
      title: 'SQL 내보내기',
      width: 780,
      body: () => [
        h('div.filter-bar', {},
          h('label.field.field-inline', {},
            h('span.field-label', {}, '대상 DB'), dialectSelect),
          h('span.muted.small', {}, '다른 종류를 고르면 타입을 변환해 만듭니다')),
        picker.node,
        box,
      ],
    });
    await load();
  }

  async showHistory() {
    const body = h('div', {}, spinner('편집 이력을 불러오는 중…'));
    openModal({ title: '편집 이력', width: 680, body: () => body });
    try {
      const res = await api.get(`/erd/documents/${encodeURIComponent(this.docID)}/ops`);
      const ops = (res.ops ?? []).slice().reverse();
      mount(body, ops.length === 0
        ? h('p.muted', {}, '기록된 편집이 없습니다 (스냅샷으로 압축되었을 수 있습니다)')
        : h('table.table.erd-history', {},
          h('thead', {}, h('tr', {},
            h('th', {}, '#'), h('th', {}, '작업'), h('th', {}, '사람'), h('th', {}, '시각'))),
          h('tbody', {}, ops.map((op) => h('tr', {},
            h('td', {}, String(op.seq)),
            h('td', {}, h('code', {}, op.kind), ' ', h('span.muted', {}, opSummary(op))),
            h('td', {}, op.actorName || '—'),
            h('td.nowrap', {}, relativeTime(op.at)),
          )))));
    } catch (err) {
      mount(body, errorPanel(err));
    }
  }

  findTable(key) {
    return (this.doc.schema?.tables ?? []).find((t) => tableKey(t) === key) ?? null;
  }
}

// aiMessageNode는 저장된 AI 대화 한 줄을 그린다.
//
// 어시스턴트 답변만 마크다운으로 그린다 — 사람이 친 `*`는 강조가 아니라 별표다.
function aiMessageNode(m, onShare) {
  if (m.role === 'user') {
    return [h('div.erd-chat-msg.is-me', {}, h('p.erd-chat-body', {}, m.text ?? ''))];
  }
  if (m.role !== 'assistant') return [];
  const out = [];
  if ((m.text ?? '').trim()) {
    out.push(h('div.erd-chat-msg.is-ai', {},
      h('div.erd-chat-md', {}, renderMarkdown(m.text)),
      h('button.btn.btn-ghost.btn-small.erd-share', {
        type: 'button', title: '이 답변을 전체 대화로 옮깁니다',
        onclick: () => onShare(m.text),
      }, icon('share'), '전체 대화로 공유')));
  }
// 툴 호출은 접어 둔다. 무엇이 바뀌었는지는 캔버스에 이미 보이므로, 여기서는
// "무엇을 했는지" 한 줄이면 충분하다.
  for (const call of m.toolCalls ?? []) {
    out.push(h('div.erd-ai-tool', {}, icon('workflow'), h('code', {}, call.name)));
  }
  return out;
}

// erdStreamNode는 응답 중인 말풍선이다.
//
// 토큰이 도착할 때마다 마크다운을 다시 그리되 프레임당 한 번으로 묶는다 —
// 토큰은 프레임보다 훨씬 빠르게 온다.
function erdStreamNode() {
  const bubble = h('div.erd-chat-md', {}, h('span.ai-cursor', {}, '▌'));
  const tools = h('div.erd-ai-tools');
  const el = h('div.erd-chat-msg.is-ai', {}, bubble, tools);
  const seen = new Map();
  let text = '';
  let frame = 0;
  const paint = () => {
    frame = 0;
    mount(bubble, renderMarkdown(text), h('span.ai-cursor', {}, '▌'));
  };
  return {
    el,
    appendText(chunk) {
      text += chunk;
      if (!frame) frame = requestAnimationFrame(paint);
    },
    addTool(call) {
      const row = h('div.erd-ai-tool', {}, icon('workflow'), h('code', {}, call.name), h('span.muted', {}, '실행 중…'));
      seen.set(call.id, row);
      tools.appendChild(row);
    },
    setToolResult(res) {
      const row = seen.get(res.callId);
      if (!row) return;
      const tail = row.querySelector('span.muted');
      if (tail) mount(tail, res.error ? `실패: ${truncate(res.error, 80)}` : '완료');
      if (res.error) row.classList.add('is-error');
    },
    addNotice(msg) {
      tools.appendChild(h('div.erd-ai-tool.is-notice', {}, msg));
    },
    setError(msg) {
      if (frame) cancelAnimationFrame(frame);
      frame = 0;
      mount(bubble, renderMarkdown(text));
      el.appendChild(h('p.erd-ai-error', {}, msg));
    },
  };
}

// ---------- 경량 op 적용 ----------

// applyLightOp은 서버가 문서를 함께 보내지 않는 op만 반영한다.
// 좌표와 메모는 대입이 곧 적용이므로 서버 로직과 어긋날 여지가 없다.
export function applyLightOp(doc, op) {
  const p = op.payload ?? {};
  switch (op.kind) {
    case 'table.move': {
      // table.move는 좌표만 옮기는 op가 아니다. 접기·색·아이콘·폭이 같은 Box에
      // 담기므로 함께 온다(서버의 applyTableMove와 같은 목록이다).
      // 하나라도 빠뜨리면 그 값만 화면에 반영되지 않고, 새로고침해야 나타난다 —
      // 아이콘을 골라도 아무 일이 없던 이유가 이것이었다.
      const prev = doc.layout[p.key] ?? {};
      const next = { ...prev, x: p.x, y: p.y };
      for (const k of ['collapsed', 'color', 'icon', 'width']) {
        if (p[k] !== undefined) next[k] = p[k];
      }
      // 컬럼 아이콘은 **보낸 것만** 덮어쓴다(서버의 applyTableMove와 같다).
      // 통째로 갈아 끼우면 방금 다른 사람이 고른 아이콘이 내 화면에서만 사라진다.
      if (p.columnIcons) {
        const icons = { ...(prev.columnIcons ?? {}) };
        for (const [name, ic] of Object.entries(p.columnIcons)) {
          if (ic) icons[name.toLowerCase()] = ic;
          else delete icons[name.toLowerCase()];
        }
        next.columnIcons = icons;
      }
      doc.layout[p.key] = next;
      break;
    }
    case 'note.add':
      if (!(doc.notes ?? []).some((n) => n.id === p.id)) {
        doc.notes = [...(doc.notes ?? []), {
          id: p.id, text: p.text ?? '', x: p.x ?? 0, y: p.y ?? 0, color: p.color ?? '',
        }];
      }
      break;
    case 'note.update': {
      const note = (doc.notes ?? []).find((n) => n.id === p.id);
      if (note) {
        if (p.text !== undefined) note.text = p.text;
        if (p.x !== undefined) note.x = p.x;
        if (p.y !== undefined) note.y = p.y;
        if (p.color !== undefined) note.color = p.color;
      }
      break;
    }
    case 'note.delete':
      doc.notes = (doc.notes ?? []).filter((n) => n.id !== p.id);
      break;
    case 'group.add':
      if (!(doc.groups ?? []).some((g) => g.id === p.id)) {
        doc.groups = [...(doc.groups ?? []), {
          id: p.id, label: p.label ?? '', x: p.x ?? 0, y: p.y ?? 0,
          w: p.w ?? 320, h: p.h ?? 240, color: p.color ?? '',
        }];
      }
      break;
    case 'group.update': {
      const group = (doc.groups ?? []).find((g) => g.id === p.id);
      if (group) {
        for (const k of ['label', 'x', 'y', 'w', 'h', 'color']) {
          if (p[k] !== undefined) group[k] = p[k];
        }
      }
      break;
    }
    case 'group.delete':
      doc.groups = (doc.groups ?? []).filter((g) => g.id !== p.id);
      break;
    default:
      break;
  }
}

// ---------- 표시 헬퍼 ----------

function diffView(res) {
  const changes = res.diff?.changes ?? [];
  const plan = res.plan ?? {};
  if (changes.length === 0) {
    return h('div', {},
      h('p.notice.notice-success', {}, icon('check'), '초안과 대상 DB의 구조가 같습니다.'),
    );
  }
  return h('div', {},
    h('p.erd-diff-summary', {},
      `변경 ${changes.length}건`,
      res.diff?.destructiveCount ? badge(`파괴적 ${res.diff.destructiveCount}건`, 'danger') : null,
    ),
    (plan.warnings ?? []).length
      ? h('div.notice.notice-warn', {}, icon('alert'),
        h('ul.note-list', {}, plan.warnings.map((w) => h('li', {}, w))))
      : null,
    h('table.table', {},
      h('thead', {}, h('tr', {}, h('th', {}, '종류'), h('th', {}, '내용'))),
      h('tbody', {}, changes.map((ch) => h('tr', { class: ch.destructive ? 'is-destructive' : '' },
        h('td.nowrap', {}, ch.destructive ? badge(ch.kind, 'danger') : badge(ch.kind, 'neutral')),
        h('td', {}, ch.summary),
      ))),
    ),
    plan.up?.length
      ? h('details.erd-diff-sql', {},
        h('summary', {}, `적용 SQL ${plan.up.length}문장`),
        h('div.panel-head', {},
          h('span.muted', {}, '실행 전 검토가 필요합니다'),
          h('button.btn.btn-small', {
            type: 'button', onclick: () => copyToClipboard(plan.up.map((s) => s.sql).join(';\n\n') + ';'),
          }, icon('copy'), '복사')),
        codeBlock(plan.up.map((s) => `${s.sql};`).join('\n\n'), 'sql', { className: 'sql-block' }))
      : null,
  );
}

// migrationCreatedView는 방금 만든 계획의 SQL을 보여준다.
//
// 실행 버튼은 없다. 이 화면에서 실행할 수 있으면 리뷰·승인 절차를 우회하는
// 길이 하나 더 생기고, 그것이 이 앱이 막으려는 바로 그 상황이다.
function migrationCreatedView(mig) {
  const sql = mig.upSql ?? (mig.plan?.up ?? []).map((s) => `${s.sql};`).join('\n\n');
  return h('div.erd-mig-created', {},
    h('p.notice.notice-success', {}, icon('check'),
      `계획을 만들었습니다 — 문장 ${mig.plan?.up?.length ?? 0}개`
      + (mig.destructiveCount ? `, 파괴적 변경 ${mig.destructiveCount}건` : '')),
    (mig.plan?.warnings ?? []).length
      ? h('div.notice.notice-warn', {}, icon('alert'),
        h('ul.note-list', {}, mig.plan.warnings.map((w) => h('li', {}, w))))
      : null,
    h('div.panel-head', {},
      h('span.muted', {}, '실행될 SQL (승인 후 마이그레이션 화면에서 실행합니다)'),
      h('button.btn.btn-small', {
        type: 'button', onclick: () => copyToClipboard(sql),
      }, icon('copy'), '복사')),
    codeBlock(sql, 'sql', { className: 'sql-block' }),
  );
}

// exportView는 내보낼 스크립트와 그것을 가져가는 두 가지 방법을 보여준다.
// basePicker는 "무엇으로부터의 변경인가"를 고르는 칸이다.
//
// 초안은 대개 지금 DB에서 출발하지만, "v3에서 이 설계로 가는 SQL"이 필요한 때가
// 있다 — 다른 환경이 아직 v3에 있거나, 지난 버전과의 차이를 보고 싶을 때다.
// 기준이 지금 DB로 고정되어 있으면 그 SQL을 얻을 길이 없어 사람이 손으로 만든다.
//
// 목록은 나중에 채운다. 버전 이력을 못 불러와도 "지금 DB"는 고를 수 있어야 하기
// 때문이다 — 고르개 하나 때문에 대화상자 전체가 열리지 않는 것이 더 나쁘다.
function basePicker({ connectionId, includeEmpty = false, onChange }) {
  const base = [];
  if (includeEmpty) {
    base.push({ value: '', label: '처음부터 (빈 데이터베이스)' });
  }
  if (connectionId) {
    base.push({ value: 'live', label: '지금 DB' });
  }
  const sel = select(base, { value: includeEmpty ? '' : 'live' });
  const note = h('span.field-help');
  const fire = () => onChange?.(sel.value);
  sel.addEventListener('change', () => { syncNote(); fire(); });

  function syncNote() {
    // 지금 DB가 아닌 기준을 고르면 실행이 막힐 수 있다는 것을 미리 말한다.
    // 사전 검사가 어차피 막지만, 만들고 나서 듣는 것과 고르기 전에 아는 것은 다르다.
    if (sel.value === '' || sel.value === 'live') {
      note.textContent = sel.value === ''
        ? '초안 전체를 처음부터 만드는 스크립트입니다.'
        : '지금 대상 DB의 구조와 비교합니다.';
      return;
    }
    note.textContent = '고른 버전에서 이 초안으로 가는 SQL을 만듭니다. '
      + '지금 DB가 그 버전과 다르면 실행은 사전 검사에서 막힙니다 — SQL을 뽑는 데는 쓸 수 있습니다.';
  }
  syncNote();

  const node = h('div.field', {},
    h('span.field-label', {}, '기준'),
    sel,
    note);

  (async () => {
    if (!connectionId) return;
    let versions = [];
    try {
      const res = await api.get(`/connections/${encodeURIComponent(connectionId)}/versions?limit=50`);
      versions = res.versions ?? [];
    } catch {
      // 버전을 못 읽어도 지금 DB 기준은 그대로 쓸 수 있다.
      return;
    }
    if (versions.length === 0) return;
    const [newest, ...older] = versions;
    const label = (v) => `v${v.versionNo} · ${versionSourceLabel(v.source)} · ${formatDate(v.createdAt)}`;
    // 최신 버전은 이름을 따로 준다. "최신"을 고른다는 것과 "지금 v12"를 고른다는
    // 것은 사람에게 다른 뜻이다.
    sel.appendChild(h('option', { value: 'latest' }, `최신 버전 · ${label(newest)}`));
    for (const v of older) {
      sel.appendChild(h('option', { value: String(v.id) }, label(v)));
    }
  })();

  return { node, get value() { return sel.value; } };
}

function exportView(res, docName) {
  const sql = res.upSql ?? '';
  if (!sql.trim()) {
    // 기준과 같으면 "만들 것이 없다"이고, 처음부터라면 "초안이 비었다"이다.
    // 같은 빈 화면이지만 사람이 할 일은 정반대다.
    return emptyStateNotice(res.fromEmpty
      ? '내보낼 것이 없습니다. 초안에 테이블을 먼저 만드세요.'
      : `${res.base ?? '기준'} 과 초안의 구조가 같아 만들 SQL이 없습니다.`);
  }
  const stats = res.stats ?? {};
  return h('div', {},
    h('p.erd-diff-summary', {},
      `테이블 ${stats.tables ?? 0}개 · 컬럼 ${stats.columns ?? 0}개 · 문장 ${res.plan?.up?.length ?? 0}개`),
    (res.plan?.warnings ?? []).length
      ? h('div.notice.notice-warn', {}, icon('alert'),
        h('ul.note-list', {}, res.plan.warnings.map((w) => h('li', {}, w))))
      : null,
    h('div.panel-head', {},
      h('span.muted', {}, res.fromEmpty
        ? '이 스크립트는 빈 데이터베이스를 기준으로 만들어졌습니다'
        : `${res.base ?? '기준'} 에서 이 초안으로 가는 변경 ${res.changes ?? 0}건입니다`),
      h('div.erd-export-actions', {},
        h('button.btn.btn-small', {
          type: 'button', onclick: () => copyToClipboard(sql),
        }, icon('copy'), '복사'),
        h('button.btn.btn-small', {
          type: 'button', onclick: () => downloadText(`${safeFileName(docName)}.sql`, sql),
        }, icon('save'), '파일로 저장'))),
    codeBlock(sql, 'sql', { className: 'sql-block' }),
  );
}

// importPreviewView는 불러오기가 초안에 무엇을 할지 보여준다.
//
// 삭제를 맨 위에 둔다. 추가와 덮어쓰기는 되돌릴 수 있지만(다시 그리면 된다)
// 삭제는 사람이 만든 것을 없애는 유일한 항목이다.
function importPreviewView(plan) {
  const s = plan.summary ?? {};
  const rows = [
    s.dropped?.length ? ['삭제', s.dropped, 'danger'] : null,
    s.added?.length ? ['추가', s.added, 'success'] : null,
    s.updated?.length ? ['덮어쓰기', s.updated, 'accent'] : null,
  ].filter(Boolean);

  if (rows.length === 0) {
    return h('p.notice.notice-warn', {}, icon('alert'),
      `문장 ${plan.statements}개를 읽었지만 초안에 반영할 테이블이 없습니다.`);
  }

  return h('div.erd-import-plan', {},
    h('p.erd-diff-summary', {}, `문장 ${plan.statements}개를 읽었습니다`),
    h('div.erd-import-rows', {}, rows.map(([label, names, kind]) =>
      h('div.erd-import-row', {},
        badge(`${label} ${names.length}`, kind),
        h('span.erd-import-names', {}, names.join(', '))))),
    s.droppedRefs?.length
      ? h('div.notice.notice-warn', {}, icon('alert'),
        h('div', {}, '함께 사라지는 외래키:',
          h('ul.note-list', {}, s.droppedRefs.map((r) => h('li', {}, r)))))
      : null,
    s.missingRefs?.length
      ? h('div.notice.notice-info', {}, icon('alert'),
        h('div', {}, '참조 대상이 초안에 없는 외래키입니다. 관계선은 대상 테이블을 '
          + '불러온 뒤에 그려집니다:',
          h('ul.note-list', {}, s.missingRefs.map((r) => h('li', {}, r)))))
      : null,
    (plan.notes ?? []).length
      ? h('details.erd-import-notes', {},
        h('summary', {}, `읽지 못한 문장 ${plan.notes.length}건`),
        h('ul.note-list', {}, plan.notes.map((n) => h('li', {}, n))))
      : null,
  );
}

function emptyStateNotice(message) {
  return h('p.notice.notice-info', {}, icon('alert'), message);
}

// downloadText는 만든 문자열을 파일로 내려받게 한다.
// 서버를 거치지 않는다 — 내용이 이미 브라우저에 있고, 왕복하면 같은 것을 두 번 만든다.
function downloadText(filename, text) {
  const url = URL.createObjectURL(new Blob([text], { type: 'text/plain;charset=utf-8' }));
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  // 즉시 해제하면 브라우저가 저장을 시작하기 전에 사라질 수 있다.
  setTimeout(() => URL.revokeObjectURL(url), 10000);
}

function safeFileName(name) {
  const cleaned = String(name ?? 'schema').replace(/[\\/:*?"<>|]/g, '_').trim();
  return cleaned || 'schema';
}

function opSummary(op) {
  const p = op.payload ?? {};
  return [p.key, p.table, p.name, p.newName].filter(Boolean).join(' → ');
}

// fkActionBadges는 기본값이 아닌 참조 동작만 배지로 만든다.
//
// 기준은 생성되는 DDL과 같다(schema/ddl.go): NO ACTION 이면 SQL에 적지 않는다.
// 화면과 SQL이 다른 것을 말하면 어느 쪽을 믿어야 하는지 알 수 없다.
function fkActionBadges(fk) {
  const out = [];
  const add = (label, value) => {
    const v = (value ?? '').trim().toUpperCase();
    if (!v || v === 'NO ACTION') return;
    out.push(badge(`${label} ${v}`, 'neutral'));
  };
  add('ON DELETE', fk.onDelete);
  add('ON UPDATE', fk.onUpdate);
  return out;
}

function uniqueColumnSets(tbl) {
  const out = [];
  if (tbl.primaryKey?.columns?.length) out.push(tbl.primaryKey.columns);
  for (const idx of tbl.indexes ?? []) {
    if (!idx.unique) continue;
    const cols = (idx.columns ?? []).map((c) => c.column).filter(Boolean);
    if (cols.length) out.push(cols);
  }
  return out;
}

// commitOn은 입력 확정(Enter 또는 포커스 아웃) 시에만 콜백을 호출한다.
// 키를 누를 때마다 op를 보내면 op-log가 타이핑 기록으로 가득 차고,
// 다른 참여자의 화면이 매 글자마다 다시 그려진다.
//
// Enter에서 blur()만 호출하고 확정을 blur 핸들러에 맡기지 않는 이유: 포커스가 없는
// 엘리먼트에 blur()를 불러도 이벤트가 발생하지 않고, 엘리먼트가 DOM에서 제거될 때도
// blur는 발생하지 않는다. 그러면 사용자가 입력한 값이 아무 표시 없이 사라진다.
// 그래서 Enter에서 직접 확정하고, 마지막 확정값을 기억해 중복 전송을 막는다.
// identityFits는 고른 타입에 자동 증가를 붙일 수 있는지 본다.
//
// 타입 이름을 화면에서 짐작하지 않고 카탈로그의 표시(identity)를 쓴다 — 어디까지
// 되는지는 DB마다 다르고, 그 규칙은 서버 한 곳에만 있어야 한다.
//
// 자릿수만 여기서 한 번 더 본다: MS-SQL·Oracle은 소수 자릿수가 0인 DECIMAL/NUMBER에만
// 자동 증가를 붙일 수 있는데, 그것은 타입이 아니라 **인자**에 달린 조건이라 카탈로그의
// 표시로는 나타낼 수 없다.
function identityFits(def, arg) {
  if (!def?.identity) return false;
  if (def.param !== 'precision') return true;
  const scale = String(arg ?? '').split(',')[1];
  return !(Number(scale) > 0);
}

// singleColumnIndex는 그 컬럼 **하나만** 걸린 인덱스를 찾는다.
//
// 컬럼이 하나인지까지 보는 이유: (email, tenant_id) 복합 인덱스는 email 컬럼에
// 대한 인덱스가 아니다. 유일성도 조합에 대한 것이지 컬럼에 대한 것이 아니다.
function singleColumnIndex(table, name, unique) {
  const want = (name ?? '').toLowerCase();
  return (table?.indexes ?? []).find((ix) => Boolean(ix.unique) === unique
    && (ix.columns ?? []).length === 1
    && ((ix.columns[0].column ?? '').toLowerCase() === want)) ?? null;
}

// 마우스 도구는 브라우저에 기억한다. 서버에 둘 값이 아니다 — 문서의 성질이 아니라
// 그 사람의 손버릇이고, 같은 문서를 보는 두 사람이 다른 도구를 쓸 수 있어야 한다.
const TOOL_KEY = 'dbstudio.erd.tool';

function readTool() {
  try {
    return localStorage.getItem(TOOL_KEY) === 'pan' ? 'pan' : 'select';
  } catch {
    // 저장소를 못 쓰는 브라우저(사생활 보호 모드 등)에서도 편집은 되어야 한다.
    return 'select';
  }
}

function writeTool(tool) {
  try {
    localStorage.setItem(TOOL_KEY, tool);
  } catch {
    // 기억하지 못할 뿐이다.
  }
}

// isTyping은 지금 이 영역 안에서 글자를 입력하고 있는지 본다.
//
// 버튼·체크박스에 포커스가 있는 것은 입력이 아니다. 그 구분이 없으면
// "버튼을 눌렀는데 화면이 그대로"가 된다.
// defaultsFor는 기본값 제안을 고른다. 어울리는 것을 먼저, 나머지도 모두.
//
// 걸러 내지 않고 나누기만 하는 이유: 기본값은 타입만으로 정해지지 않는다. 문자
// 컬럼에 시각을 문자열로 넣기도 하고(SQLite가 그렇다), 숫자 컬럼에 시퀀스를 물리기도
// 한다. 어울리는 것만 남기면 그런 자리에서 이 칸은 아무 도움이 못 된다.
function defaultsFor(cat, def) {
  const all = cat?.defaults ?? [];
  const row = (d, group) => ({ value: d.expr, label: d.expr, hint: d.label, group });
  if (!def) return all.map((d) => row(d, ''));

  const fits = (d) => !d.for?.length || d.for.includes(def.category);
  return [
    ...all.filter(fits).map((d) => row(d, '이 타입에 어울림')),
    ...all.filter((d) => !fits(d)).map((d) => row(d, '그 밖의 기본값')),
  ];
}

function isTyping(root) {
  const el = document.activeElement;
  if (!el || !root.contains(el)) return false;
  if (el.isContentEditable) return true;
  if (el.tagName === 'TEXTAREA') return true;
  if (el.tagName !== 'INPUT') return false;
  return !['checkbox', 'radio', 'button', 'submit', 'reset', 'file', 'range', 'color']
    .includes(el.type);
}

// commitOn은 입력을 **확정될 때** 서버로 보낸다(Enter 또는 포커스 이동).
//
// 글자마다 op를 보내지 않는 이유: 이름을 고치는 도중의 `use`, `user`, `users`가
// 모두 편집 이력에 남고 다른 참여자에게 브로드캐스트된다.
//
// preview를 함께 받는 이유는 그 반대편의 문제다. 확정할 때까지 캔버스가 옛 이름을
// 보여주면 "지금 어느 카드를 고치고 있는지"가 눈에서 사라진다. 그래서 **화면만**
// 글자마다 따라가게 하고(preview), 서버로 가는 것은 확정 때 한 번이다.
// required는 "빈 값으로 둘 수 없는 칸"이다(이름 칸).
//
// 빈 이름은 서버가 거부하므로 아무것도 보내지 않는데, 칸을 비운 채 다른 곳을 누르면
// 화면에는 이름 없는 칸이 남고 서버는 옛 이름을 들고 있는 어긋난 상태가 된다.
// 그래서 **칸을 떠날 때** 지금 이름으로 되돌린다. 타자 도중(디바운스)에는 되돌리지
// 않는다 — 지우고 새로 치는 중에 글자가 되살아나면 커서가 튀고 입력을 방해한다.
function commitOn(el, fn, preview = null, { debounce = 700, required = false } = {}) {
  let committed = el.value;
  let timer = null;
  const commit = () => {
    if (timer) {
      clearTimeout(timer);
      timer = null;
    }
    if (el.value === committed) return;
    committed = el.value;
    fn();
  };
  const restore = () => {
    el.value = committed;
    preview?.(committed);
  };
  el.addEventListener('input', () => {
    preview?.(el.value);
    // 타자를 멈추면 곧 보낸다. Enter나 포커스 이동만 기다리면 "고쳤는데
    // 반영이 안 된다"로 보이고, 실제로 그 상태에서 다른 칸을 건드리면
    // 서버는 아직 옛 이름을 알고 있어 오류가 난다.
    if (!debounce) return;
    if (timer) clearTimeout(timer);
    timer = setTimeout(commit, debounce);
  });
  el.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      commit();
      el.blur();
      return;
    }
    if (e.key === 'Escape') {
      // 되돌리고 확정하지 않는다.
      restore();
      el.blur();
    }
  });
  el.addEventListener('blur', () => {
    if (required && el.value.trim() === '') {
      restore();
      return;
    }
    commit();
  });
}





