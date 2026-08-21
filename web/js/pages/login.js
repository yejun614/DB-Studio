// 로그인 화면과 비밀번호 변경 강제 화면.
import { api } from '../core/api.js';
import { loadSession, loadMeta, state } from '../core/store.js';
import { h, mount, field, input, toast, toastError, icon } from '../core/ui.js';
import { navigate } from '../core/router.js';
import * as theme from '../core/theme.js';
import { codeInput } from './totp.js';

// 아이디를 기억해 두는 자리.
//
// 비밀번호는 절대 담지 않는다. 브라우저의 비밀번호 관리자는 그 일을 안전하게 하도록
// 만들어진 물건이고, 앱이 흉내 내면 저장소를 읽을 수 있는 누구에게나 자격증명을
// 넘겨주는 셈이 된다. 여기 담기는 것은 "누구로 로그인하는가" 하나다.
//
// 서버가 아니라 localStorage인 이유: 로그인 전에는 서버에 물어볼 수 있는 것이 없고,
// 이것은 이 브라우저에서의 편의일 뿐이다(공용 PC에서는 체크를 끄면 지워진다).
const REMEMBER_KEY = 'dbstudio.login.username';

function rememberedUsername() {
  try {
    return localStorage.getItem(REMEMBER_KEY) ?? '';
  } catch {
    // 사생활 보호 모드 등에서 저장소를 못 읽는 것은 오류가 아니라 "기억이 없다"이다.
    return '';
  }
}

function saveUsername(name) {
  try {
    if (name) localStorage.setItem(REMEMBER_KEY, name);
    else localStorage.removeItem(REMEMBER_KEY);
  } catch { /* 저장 못 해도 로그인 자체는 된다 */ }
}

export function renderLogin(outlet, { onSuccess }) {
  // placeholder에 실제 존재하는 계정명을 두지 않는다.
  // 인증 전 화면이므로 누구나 보며, 유효한 아이디를 알려주면 대입 공격의 출발점이 된다.
  // 무엇을 넣는 칸인지는 위의 라벨("아이디")이 이미 말해 준다.
  const username = input({ name: 'username', autocomplete: 'username', required: true });
  const password = input({ type: 'password', name: 'password', autocomplete: 'current-password', required: true });
  const remembered = rememberedUsername();
  username.value = remembered;
  const rememberBox = h('input', { type: 'checkbox', checked: Boolean(remembered) });
  const submitBtn = h('button.btn.btn-primary.btn-block', { type: 'submit' }, '로그인');

  const form = h('form.auth-form', {
    onsubmit: async (e) => {
      e.preventDefault();
      submitBtn.disabled = true;
      submitBtn.textContent = '확인 중…';
      try {
        const res = await api.post('/auth/login', {
          username: username.value.trim(),
          password: password.value,
        }, { skipAuthRedirect: true });
        // 2단계 인증을 켠 계정이면 아직 로그인된 것이 아니다.
        // 서버는 세션 대신 챌린지 쿠키를 내려보냈고, 사용자 정보도 주지 않았다.
        if (res?.twoFactor?.required) {
          // 2단계가 남았어도 아이디는 맞았다는 뜻이다. 여기서 기억해 두면
          // 코드 입력에 실패해 돌아왔을 때 아이디를 다시 치지 않아도 된다.
          saveUsername(rememberBox.checked ? username.value.trim() : '');
          renderTOTPChallenge(outlet, { onSuccess, username: username.value.trim() });
          return;
        }
        // 저장은 **로그인에 성공한 뒤**에 한다. 오타를 기억해 두면 다음에도
        // 그 오타로 시작하게 된다.
        saveUsername(rememberBox.checked ? username.value.trim() : '');
        await loadSession();
        await loadMeta(true);
        onSuccess?.();
      } catch (err) {
        // 401은 자격증명 오류이므로 필드를 유지하고 비밀번호만 비운다.
        password.value = '';
        password.focus();
        toastError(err);
      } finally {
        submitBtn.disabled = false;
        submitBtn.textContent = '로그인';
      }
    },
  },
    field('아이디', username),
    field('비밀번호', password),
    h('label.checkbox.login-remember', {}, rememberBox, h('span', {}, '아이디 저장')),
    submitBtn,
  );

  mount(outlet,
    h('div.auth-shell', {},
      themeCorner(),
      h('div.auth-card', {},
        h('div.auth-brand', {}, icon('database', 28), h('h1', {}, 'DB Studio')),
        h('p.auth-sub', {}, '데이터베이스 관리 콘솔'),
        form,
      ),
    ),
  );
  // 아이디를 기억하고 있으면 비밀번호 칸부터 시작한다. 이미 채워진 칸에
  // 커서를 두면 사용자가 한 번 더 탭을 눌러야 한다.
  if (remembered) password.focus();
  else username.focus();
}

// renderTOTPChallenge는 로그인 2단계 화면이다.
//
// 아이디·비밀번호 화면을 그대로 두고 위에 칸 하나를 얹지 않는 이유: 이 단계에서
// 비밀번호 칸이 남아 있으면 브라우저 비밀번호 관리자가 다시 채워 넣고, 사용자는
// 무엇을 입력해야 하는지 헷갈린다. 지금 필요한 것은 코드 하나뿐이다.
function renderTOTPChallenge(outlet, { onSuccess, username }) {
  // 복구 코드 모드에서는 숫자 자판과 자동완성 힌트가 방해가 된다.
  let recovery = false;
  const code = codeInput({ required: true });
  const submitBtn = h('button.btn.btn-primary.btn-block', { type: 'submit' }, '확인');
  const notice = h('div');
  const label = h('span.field-label', {}, '인증 코드');
  const help = h('span.field-help', {}, '인증 앱에 표시된 6자리 숫자를 입력하세요');

  const toggle = h('button.link-btn', {
    type: 'button',
    onclick: () => {
      recovery = !recovery;
      code.value = '';
      code.type = 'text';
      code.setAttribute('inputmode', recovery ? 'text' : 'numeric');
      code.setAttribute('autocomplete', recovery ? 'off' : 'one-time-code');
      code.placeholder = recovery ? 'xxxx-xxxx-xxxx-xxxx' : '000000';
      label.textContent = recovery ? '복구 코드' : '인증 코드';
      help.textContent = recovery
        ? '등록할 때 저장해 둔 일회용 코드를 입력하세요. 한 번 쓰면 사라집니다'
        : '인증 앱에 표시된 6자리 숫자를 입력하세요';
      toggle.textContent = recovery ? '인증 앱 코드 사용' : '인증 앱을 쓸 수 없나요? 복구 코드 사용';
      code.focus();
    },
  }, '인증 앱을 쓸 수 없나요? 복구 코드 사용');

  const form = h('form.auth-form', {
    onsubmit: async (e) => {
      e.preventDefault();
      submitBtn.disabled = true;
      submitBtn.textContent = '확인 중…';
      try {
        await api.post('/auth/login/totp', { code: code.value.trim() }, { skipAuthRedirect: true });
        await loadSession();
        await loadMeta(true);
        onSuccess?.();
      } catch (err) {
        code.value = '';
        code.focus();
        // 챌린지가 끝났으면 코드를 아무리 잘 넣어도 통하지 않는다.
        // 그 상태로 붙잡아 두면 사용자는 계속 틀린 코드를 넣는다고 생각한다.
        if (err.code === 'challenge_expired') {
          toast(err.message, 'error');
          renderLogin(outlet, { onSuccess });
          return;
        }
        mount(notice, h('p.notice', {
          class: err.code === 'totp_resynced' ? 'notice-warn' : 'notice-danger',
        }, icon('alert'), err.message));
      } finally {
        submitBtn.disabled = false;
        submitBtn.textContent = '확인';
      }
    },
  },
    h('label.field', {}, label, code, help),
    notice,
    submitBtn,
    toggle,
    h('button.link-btn', {
      type: 'button',
      onclick: () => renderLogin(outlet, { onSuccess }),
    }, '다른 계정으로 로그인'),
  );

  mount(outlet,
    h('div.auth-shell', {},
      themeCorner(),
      h('div.auth-card', {},
        h('div.auth-brand', {}, icon('shield', 26), h('h1', {}, '2단계 인증')),
        h('p.auth-sub', {}, `${username} 계정의 인증 코드를 입력하세요.`),
        form,
      ),
    ),
  );
  code.focus();
}

// themeCorner는 로그인 화면 우상단의 테마 토글이다. 셸의 것과 같은 상태를 공유한다.
function themeCorner() {
  const btn = h('button.icon-btn.auth-theme', { type: 'button' });
  const render = () => {
    const mode = theme.currentMode();
    btn.title = `${theme.modeLabel(mode)} (눌러서 변경)`;
    btn.setAttribute('aria-label', theme.modeLabel(mode));
    mount(btn, icon(theme.modeIcon(mode), 16));
  };
  btn.addEventListener('click', () => {
    theme.applyMode(theme.nextMode(theme.currentMode()));
    render();
  });
  render();
  return btn;
}

// renderPasswordChange는 must_change_password 상태에서 다른 화면 진입을 차단하는 전용 화면이다.
export function renderPasswordChange(outlet, { onSuccess }) {
  const current = input({ type: 'password', autocomplete: 'current-password', required: true });
  const next = input({ type: 'password', autocomplete: 'new-password', required: true });
  const confirm = input({ type: 'password', autocomplete: 'new-password', required: true });
  const submitBtn = h('button.btn.btn-primary.btn-block', { type: 'submit' }, '비밀번호 변경');

  const form = h('form.auth-form', {
    onsubmit: async (e) => {
      e.preventDefault();
      if (next.value !== confirm.value) {
        toast('새 비밀번호가 일치하지 않습니다', 'error');
        confirm.focus();
        return;
      }
      submitBtn.disabled = true;
      try {
        await api.post('/auth/password', {
          currentPassword: current.value,
          newPassword: next.value,
        });
        toast('비밀번호를 변경했습니다', 'success');
        await loadSession();
        await loadMeta(true);
        onSuccess?.();
      } catch (err) {
        toastError(err);
      } finally {
        submitBtn.disabled = false;
      }
    },
  },
    field('현재 비밀번호', current),
    field('새 비밀번호', next, '10자 이상, 영문자와 숫자/기호를 함께 포함'),
    field('새 비밀번호 확인', confirm),
    submitBtn,
  );

  mount(outlet,
    h('div.auth-shell', {},
      h('div.auth-card', {},
        h('div.auth-brand', {}, icon('lock', 26), h('h1', {}, '비밀번호 변경 필요')),
        h('p.auth-sub', {}, `${state.user?.username ?? ''} 계정의 비밀번호를 변경해야 계속할 수 있습니다.`),
        form,
      ),
    ),
  );
  current.focus();
}

// renderChangePasswordPage는 로그인 후 사용자가 자발적으로 비밀번호를 바꾸는 화면이다.
export function renderChangePasswordPage(outlet) {
  renderPasswordChange(outlet, { onSuccess: () => navigate('/') });
}
