// 테마 전환. shadcn/ui 프로젝트의 관례(next-themes)를 프레임워크 없이 옮긴 것이다.
//
// 상태는 세 가지다: system(기본) / light / dark.
// system을 없애고 두 가지로만 두면 "OS 설정을 따르던 상태"로 되돌아갈 방법이 사라진다.
//
// 실제 적용은 <html>의 data-theme 속성 하나로 끝난다. CSS가 그 속성과
// prefers-color-scheme을 함께 보고 팔레트를 고르므로 여기서는 값만 관리한다.

const KEY = 'dbstudio-theme';
const MODES = ['system', 'light', 'dark'];

const LABELS = {
  system: '테마: 시스템 설정',
  light: '테마: 라이트',
  dark: '테마: 다크',
};

const ICONS = { system: 'monitor', light: 'sun', dark: 'moon' };

export function currentMode() {
  try {
    const saved = localStorage.getItem(KEY);
    if (MODES.includes(saved)) return saved;
  } catch {
    // 사생활 보호 모드에서는 localStorage 접근이 예외를 던진다. 기본값으로 진행한다.
  }
  return 'system';
}

export function applyMode(mode) {
  const root = document.documentElement;
  if (mode === 'system') root.removeAttribute('data-theme');
  else root.setAttribute('data-theme', mode);
  try {
    if (mode === 'system') localStorage.removeItem(KEY);
    else localStorage.setItem(KEY, mode);
  } catch {
    // 저장에 실패해도 이번 세션에는 적용된다.
  }
}

export function nextMode(mode) {
  return MODES[(MODES.indexOf(mode) + 1) % MODES.length];
}

export function modeLabel(mode) {
  return LABELS[mode] ?? LABELS.system;
}

export function modeIcon(mode) {
  return ICONS[mode] ?? ICONS.system;
}

// init은 앱 시작 시 저장된 값을 다시 적용한다.
// index.html의 인라인 스크립트가 이미 적용했더라도, 그 스크립트가 실패한 경우의 보루다.
export function init() {
  applyMode(currentMode());
}
