// fetch 래퍼. 모든 API 호출이 이 모듈을 거친다.
// 상태 변경 요청에 X-Requested-With 헤더를 붙여 서버의 CSRF 검사를 통과한다.

export class ApiError extends Error {
  constructor(status, code, message, detail, payload) {
    super(message || 'API 오류');
    this.status = status;
    this.code = code;
    this.detail = detail;
    // payload는 서버가 보낸 오류 본문 전체다.
    //
    // 이것을 버리면 안 되는 이유: 일부 실패는 message 한 줄로 설명할 수 없다.
    // 마이그레이션 차단은 사유 목록(blockers)을, Git 푸시 실패는 실패 단계(stage)를
    // 함께 보낸다. 서버가 애써 만든 그 정보를 클라이언트가 조용히 버리면
    // 사용자는 "왜 안 되는지"를 알 수 없다.
    this.payload = payload ?? {};
  }
}

// 401이 발생하면 앱 전체가 로그인 화면으로 전환되어야 한다.
// 라우터가 직접 fetch를 감싸지 않도록 콜백으로 통지한다.
let onUnauthorized = null;
export function setUnauthorizedHandler(fn) {
  onUnauthorized = fn;
}

let onPasswordChangeRequired = null;
export function setPasswordChangeHandler(fn) {
  onPasswordChangeRequired = fn;
}

// 2단계 인증이 의무화되었는데 아직 등록하지 않은 상태.
// 비밀번호 강제 변경과 같은 구조다 — 어느 API를 부르든 서버가 같은 코드로 막으므로,
// 화면은 그 신호를 받아 등록 화면으로 전환한다.
let onTOTPSetupRequired = null;
export function setTOTPSetupHandler(fn) {
  onTOTPSetupRequired = fn;
}

async function request(method, path, body, options = {}) {
  const headers = { Accept: 'application/json' };
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
  }
  if (method !== 'GET' && method !== 'HEAD') {
    headers['X-Requested-With'] = 'dbstudio';
  }

  let res;
  try {
    res = await fetch(`/api/v1${path}`, {
      method,
      headers,
      credentials: 'same-origin',
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch (err) {
    throw new ApiError(0, 'network', '서버에 연결할 수 없습니다');
  }

  if (res.status === 204) return null;

  let payload = null;
  const text = await res.text();
  if (text) {
    try {
      payload = JSON.parse(text);
    } catch {
      payload = { message: text };
    }
  }

  if (!res.ok) {
    const code = payload?.error ?? 'error';
    const err = new ApiError(res.status, code, payload?.message, payload?.detail, payload);
    if (res.status === 401 && !options.skipAuthRedirect && onUnauthorized) {
      onUnauthorized(err);
    }
    if (code === 'password_change_required' && onPasswordChangeRequired) {
      onPasswordChangeRequired(err);
    }
    if (code === 'totp_setup_required' && onTOTPSetupRequired) {
      onTOTPSetupRequired(err);
    }
    throw err;
  }
  return payload;
}

export const api = {
  get: (path, options) => request('GET', path, undefined, options),
  post: (path, body, options) => request('POST', path, body ?? {}, options),
  put: (path, body, options) => request('PUT', path, body ?? {}, options),
  patch: (path, body, options) => request('PATCH', path, body ?? {}, options),
  del: (path, options) => request('DELETE', path, undefined, options),
};
