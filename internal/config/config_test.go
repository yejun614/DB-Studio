package config

import (
	"path/filepath"
	"testing"
)

// 플래그가 아닌 나머지 인자는 Args 로 와야 한다.
//
// 하위 명령이 이것으로 대상을 받는다(`reset-password ... superadmin`).
// 여기서 버려지면 그 명령은 늘 기본값으로만 돌고, 사람이 준 아이디는 조용히
// 무시된다 — 다른 계정을 고쳤다고 믿게 되는 것이 최악이다.
func TestLoadKeepsPositionalArgs(t *testing.T) {
	dir := t.TempDir()

	cfg, err := Load([]string{"-data", dir, "-monitor=false", "superadmin"})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if len(cfg.Args) != 1 || cfg.Args[0] != "superadmin" {
		t.Errorf("Args = %v", cfg.Args)
	}
	if cfg.DataDir != mustAbs(t, dir) {
		t.Errorf("데이터 디렉터리 = %s", cfg.DataDir)
	}

	// 위치 인자가 없으면 비어야 한다(nil 이든 빈 슬라이스든).
	cfg, err = Load([]string{"-data", dir})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if len(cfg.Args) != 0 {
		t.Errorf("빈 경우 Args = %v", cfg.Args)
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return abs
}
