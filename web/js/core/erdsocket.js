// ERD 실시간 편집 클라이언트.
//
// 동기화 규약(서버와 짝을 이룬다):
//   - 편집은 낙관적으로 즉시 화면에 반영한다. 그러지 않으면 왕복 지연 때문에
//     드래그와 타이핑이 끊긴다.
//   - 같은 op를 서버에 보내고, 서버는 seq를 붙여 모든 참여자에게 되돌려준다.
//     자기 op도 되돌아오므로 정합 경로가 하나뿐이다.
//   - 서버가 거부하면 최신 상태 전체가 함께 오므로 그것으로 교체한다.
//     낙관적 적용을 되돌리는 역연산을 만들지 않는 이유: 역연산은 op마다 따로
//     필요하고 틀리기 쉬운데, 문서는 작아서 통째로 받는 비용이 더 싸다.
//   - 연결이 끊기면 확인받지 못한 op를 큐에 남기고, 재접속 후 다시 보낸다.
//     서버가 op.id로 중복을 걸러내므로 두 번 적용되지 않는다.

const RECONNECT_BASE_MS = 500;
const RECONNECT_MAX_MS = 10000;

let opCounter = 0;

// newOpID는 op 식별자를 만든다. 브로드캐스트된 op가 자기 것인지 알아보는 데 쓰고,
// 재전송 시 서버가 중복을 걸러내는 열쇠이기도 하다.
function newOpID() {
  opCounter += 1;
  const rand = Math.random().toString(36).slice(2, 10);
  return `${rand}-${Date.now().toString(36)}-${opCounter}`;
}

// MAX_OPS_PER_MESSAGE는 한 메시지에 담는 op 수다. 서버의 같은 한도와 맞춰 둔다
// (internal/erdhub/hub.go의 maxOpsPerMessage).
const MAX_OPS_PER_MESSAGE = 64;

export class ErdSession {
  constructor(docID, handlers = {}) {
    this.docID = docID;
    this.handlers = handlers;
    this.ws = null;
    this.closed = false;
    // suspended는 "일시적으로 끊었다"는 표시다. closed와 구분해야 한다:
    // closed는 영구 종료(화면을 떠남)이고 suspended는 나중에 다시 붙을 상태다.
    this.suspended = false;
    this.attempt = 0;

    // 서버가 확인한 마지막 seq. 재접속 시 이 값 이후만 받으면 된다.
    this.seq = 0;
    // 아직 확인받지 못한 op. 재접속 후 다시 보낸다.
    this.pending = [];
    this.you = null;
    this.participants = [];
    this.connected = false;
  }

  connect() {
    if (this.closed || this.suspended) return;
    const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const url = `${scheme}//${location.host}/api/v1/erd/documents/${encodeURIComponent(this.docID)}/socket`;

    let ws;
    try {
      ws = new WebSocket(url);
    } catch (err) {
      this.#scheduleReconnect();
      return;
    }
    this.ws = ws;

    ws.onopen = () => {
      this.attempt = 0;
      this.connected = true;
      this.handlers.onStatus?.('connected');
      // 확인받지 못한 op를 먼저 보낸다. 서버가 중복을 걸러내므로 안전하다.
      if (this.pending.length) this.#flush();
    };

    ws.onmessage = (event) => {
      let msg;
      try {
        msg = JSON.parse(event.data);
      } catch {
        return;
      }
      this.#dispatch(msg);
    };

    ws.onclose = () => {
      this.connected = false;
      this.ws = null;
      if (this.closed || this.suspended) return;
      this.handlers.onStatus?.('disconnected');
      this.#scheduleReconnect();
    };

    ws.onerror = () => {
      // onclose가 이어서 호출되므로 여기서는 상태만 알린다.
      this.handlers.onStatus?.('error');
    };
  }

  #scheduleReconnect() {
    this.attempt += 1;
    // 지수 백오프. 서버가 재시작 중일 때 초당 수십 번 재연결하면 부팅을 방해한다.
    const delay = Math.min(RECONNECT_BASE_MS * 2 ** (this.attempt - 1), RECONNECT_MAX_MS);
    this.handlers.onStatus?.('reconnecting', delay);
    setTimeout(() => this.connect(), delay);
  }

  #dispatch(msg) {
    switch (msg.type) {
      case 'init':
        this.you = msg.you;
        this.seq = msg.seq ?? 0;
        this.participants = msg.participants ?? [];
        this.handlers.onInit?.(msg);
        break;
      case 'state':
        this.seq = msg.seq ?? 0;
        // 상태를 통째로 받았으므로 낙관적 적용은 모두 무효다.
        this.pending = [];
        this.handlers.onState?.(msg);
        break;
      case 'ops':
        // 구조를 바꾼 op에는 서버가 적용 결과(문서 전체)를 함께 보낸다.
        // 클라이언트가 op 적용 로직을 다시 구현하지 않기 위한 규약이다.
        if (msg.document) {
          this.seq = msg.seq ?? this.seq;
          this.handlers.onState?.({ document: msg.document, seq: this.seq });
        }
        for (const op of msg.ops ?? []) {
          if (op.seq > this.seq) this.seq = op.seq;
          // 자기 op가 확인된 것이면 대기 큐에서 제거한다.
          const idx = this.pending.findIndex((p) => p.id === op.id);
          if (idx >= 0) this.pending.splice(idx, 1);
          this.handlers.onOp?.(op, idx >= 0, Boolean(msg.document));
        }
        break;
      case 'reject':
        this.pending = this.pending.filter((p) => p.id !== msg.opId);
        this.seq = msg.seq ?? this.seq;
        this.handlers.onReject?.(msg);
        break;
      case 'presence':
        this.participants = msg.participants ?? [];
        this.handlers.onPresence?.(this.participants);
        break;
      case 'cursor':
        this.handlers.onCursor?.(msg);
        break;
      case 'chat':
        this.handlers.onChat?.(msg.message);
        break;
      case 'meta':
        this.handlers.onMeta?.(msg);
        break;
      // 되돌리기 스택은 서버가 사람마다 들고 있다. 버튼을 켤지 끌지는
      // 이 알림으로만 안다 — 화면이 따로 세면 서버와 어긋난다.
      case 'undo_state':
        this.handlers.onUndoState?.(Boolean(msg.canUndo), Boolean(msg.canRedo));
        break;
      case 'closed':
        this.closed = true;
        this.handlers.onClosed?.(msg.reason);
        break;
      case 'error':
        this.handlers.onError?.(msg.message);
        break;
      case 'pong':
        break;
      default:
        break;
    }
  }

  // send는 op를 보내고 op 객체를 반환한다(낙관적 적용용).
  //
  // batch는 '한 동작에서 나온 편집'의 이름이다. 여러 개를 함께 옮기면 대상마다 op가
  // 하나씩 생기는데, 같은 batch로 묶어 보내면 서버의 되돌리기가 그 전체를 한 번에
  // 되돌린다. 비워 두면 지금까지처럼 op 하나가 곧 한 번의 되돌리기다.
  send(kind, payload, batch = '') {
    const op = { id: newOpID(), kind, payload };
    if (batch) op.batch = batch;
    this.pending.push(op);
    this.#flush();
    return op;
  }

  #flush() {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return;
    if (!this.pending.length) return;
    // 한 메시지에 담을 수 있는 op 수에 서버 한도가 있다. 넘긴 메시지는 통째로
    // 거부되므로, 카드 백 장을 함께 옮기면 **아무것도** 옮겨지지 않는다.
    //
    // 나눠 보내도 안전한 이유: 서버는 op ID로 재전송을 걸러 낸다. 같은 op가 두 번
    // 담겨도 두 번 적용되지 않는다.
    for (let i = 0; i < this.pending.length; i += MAX_OPS_PER_MESSAGE) {
      this.#raw({
        type: 'ops',
        ops: this.pending.slice(i, i + MAX_OPS_PER_MESSAGE),
        baseSeq: this.seq,
      });
    }
  }

  presence(update) {
    this.#raw({ type: 'presence', ...update });
  }

  chat(body, targetKey = '') {
    this.#raw({ type: 'chat', body, targetKey });
  }

  // resync는 놓친 편집을 받아 온다.
  //
  // full=true면 순번을 대지 않아 **문서 전체**를 받는다. 편집이 거부된 뒤가 그런
  // 경우다: 서버의 순번은 오르지 않았으므로 "이미 최신"으로 판정되는데, 정작 화면에는
  // 거부된 미리보기가 남아 있다. 그때 필요한 것은 놓친 op가 아니라 진짜 문서다.
  resync(full = false) {
    this.#raw({ type: 'resync', sinceSeq: full ? 0 : this.seq });
  }

  // undo/redo는 서버에 있는 내 편집 스택을 한 칸 되돌린다.
  // 클라이언트가 역연산을 만들지 않는 이유는 op 적용과 같다 — 규칙이 두 벌이면
  // 언젠가 어긋나고, 그때 사라지는 것은 남의 편집이다.
  undo() {
    this.#raw({ type: 'undo' });
  }

  redo() {
    this.#raw({ type: 'redo' });
  }

  #raw(msg) {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return false;
    try {
      this.ws.send(JSON.stringify(msg));
      return true;
    } catch {
      return false;
    }
  }

  // suspend는 연결을 잠시 끊는다 (재연결 예약 없이).
  //
  // 브라우저가 페이지를 뒤로/앞으로 캐시(BFCache)에 넣을 때 필요하다. 캐시된 페이지의
  // WebSocket은 열린 채로 남을 수 있는데, 서버 입장에서는 아직 접속 중인 참여자로
  // 보이므로 프레즌스 목록에 유령이 남는다. 핑/퐁 타임아웃이 결국 걷어내지만
  // 그때까지 다른 사람들은 "누가 편집 중인가"를 잘못 본다.
  suspend() {
    if (this.closed) return;
    this.suspended = true;
    this.#drop();
  }

  // resume은 suspend된 연결을 되살린다 (BFCache에서 페이지가 복원될 때).
  resume() {
    if (this.closed || !this.suspended) return;
    this.suspended = false;
    this.attempt = 0;
    // 상태가 어긋났을 수 있으므로 재접속 후 init으로 최신 문서를 다시 받는다.
    this.connect();
  }

  close() {
    this.closed = true;
    this.#drop();
  }

  #drop() {
    if (!this.ws) return;
    try {
      this.ws.close();
    } catch {
      // 이미 닫힌 소켓이면 무시한다.
    }
    this.ws = null;
    this.connected = false;
  }
}

// throttle은 커서 이동처럼 빈도가 높은 이벤트를 제한한다.
// 마우스 이동마다 메시지를 보내면 초당 수백 건이 되어 다른 참여자의 수신 버퍼를 넘긴다.
export function throttle(fn, ms) {
  let last = 0;
  let timer = null;
  let lastArgs = null;
  return (...args) => {
    lastArgs = args;
    const now = Date.now();
    const wait = ms - (now - last);
    if (wait <= 0) {
      last = now;
      fn(...lastArgs);
      return;
    }
    if (timer) return;
    timer = setTimeout(() => {
      timer = null;
      last = Date.now();
      fn(...lastArgs);
    }, wait);
  };
}
