// 앱 전역 상태. 이벤트 기반 최소 구현.
import { api } from './api.js';

const listeners = new Set();

export const state = {
  ready: false,
  user: null,
  permissions: null,
  session: null,
  meta: null, // dbKinds, roles, levels, accessModes, environments
};

export function subscribe(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

function notify() {
  for (const fn of listeners) fn(state);
}

export function setState(patch) {
  Object.assign(state, patch);
  notify();
}

// loadSession은 현재 로그인 상태를 확인한다.
// 미로그인(401)은 정상 흐름이므로 예외로 전파하지 않는다.
export async function loadSession() {
  try {
    const res = await api.get('/auth/me', { skipAuthRedirect: true });
    setState({ user: res.user, permissions: res.permissions, session: res.session });
    return res.user;
  } catch (err) {
    if (err.status === 401) {
      setState({ user: null, permissions: null, session: null });
      return null;
    }
    // 비밀번호 변경 강제 상태에서는 /auth/me가 허용되므로 여기까지 오지 않는다.
    throw err;
  }
}

export async function loadMeta(force = false) {
  if (state.meta && !force) return state.meta;
  const meta = await api.get('/meta');
  setState({ meta });
  return meta;
}

export function clearSession() {
  setState({ user: null, permissions: null, session: null });
}

// 라벨 조회 헬퍼. meta가 로드되지 않았어도 안전하게 동작한다.
export function kindLabel(kind) {
  const found = state.meta?.dbKinds?.find((k) => k.kind === kind);
  return found?.label ?? kind;
}

export function kindInfo(kind) {
  return state.meta?.dbKinds?.find((k) => k.kind === kind) ?? null;
}

export function levelLabel(level) {
  const found = state.meta?.levels?.find((l) => l.value === level);
  return found?.label ?? level;
}
