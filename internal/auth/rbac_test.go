package auth

import (
	"slices"
	"testing"

	"dbstudio/internal/model"
)

// 두 축(등급과 데이터 능력)이 서로 독립인지 확인한다.
//
// 이 성질이 깨지면 요구사항이 무너진다: "마이그레이션은 할 수 있지만 고객 데이터는
// 못 본다"와 그 반대가 모두 표현 가능해야 한다.
func TestResolveKeepsLevelAndCapsIndependent(t *testing.T) {
	cases := []struct {
		name      string
		policy    *model.AccessPolicy
		conn      string
		server    string
		allowed   bool
		wantLevel model.Level
		wantCaps  []model.Capability
	}{
		{
			name: "높은 등급이라도 능력을 주지 않으면 데이터는 못 본다",
			policy: &model.AccessPolicy{
				Mode: model.AccessAll, DefaultLevel: model.LevelMigrate,
				DefaultCaps: []model.Capability{},
			},
			conn: "c1", allowed: true, wantLevel: model.LevelMigrate,
			wantCaps: []model.Capability{},
		},
		{
			name: "등급이 none이어도 데이터 능력만 줄 수 있다",
			policy: &model.AccessPolicy{
				Mode: model.AccessAll, DefaultLevel: model.LevelNone,
				DefaultCaps: []model.Capability{model.CapDataRead},
			},
			conn: "c1", allowed: true, wantLevel: model.LevelNone,
			wantCaps: []model.Capability{model.CapDataRead},
		},
		{
			name: "등급도 능력도 없으면 접근 불가",
			policy: &model.AccessPolicy{
				Mode: model.AccessAll, DefaultLevel: model.LevelNone,
				DefaultCaps: []model.Capability{},
			},
			conn: "c1", allowed: false,
		},
		{
			name: "커넥션별 능력 오버라이드가 기본값을 이긴다",
			policy: &model.AccessPolicy{
				Mode: model.AccessAll, DefaultLevel: model.LevelMonitor,
				DefaultCaps: []model.Capability{model.CapDataRead},
				CapOverrides: map[string][]model.Capability{
					"prod": {},
				},
			},
			conn: "prod", allowed: true, wantLevel: model.LevelMonitor,
			wantCaps: []model.Capability{},
		},
		{
			name: "범위 밖이면 능력도 적용되지 않는다",
			policy: &model.AccessPolicy{
				Mode: model.AccessAllowlist, Items: []string{"dev"},
				DefaultLevel: model.LevelMigrate,
				DefaultCaps:  []model.Capability{model.CapDataRead, model.CapDataWrite},
			},
			conn: "prod", allowed: false,
		},
		{
			name: "denylist에 없으면 기본값이 적용된다",
			policy: &model.AccessPolicy{
				Mode: model.AccessDenylist, Items: []string{"prod"},
				DefaultLevel: model.LevelERD,
				DefaultCaps:  []model.Capability{model.CapSQLRun},
			},
			conn: "dev", allowed: true, wantLevel: model.LevelERD,
			wantCaps: []model.Capability{model.CapSQLRun},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.policy.Capabilities == nil {
				tc.policy.Capabilities = map[string]model.Level{}
			}
			if tc.policy.CapOverrides == nil {
				tc.policy.CapOverrides = map[string][]model.Capability{}
			}
			// 프로젝트 관문은 이 표의 관심사가 아니다. 모든 대상을 한 프로젝트에
			// 두고 거기에 참여시켜 두어, 등급·능력 규칙만 남긴다.
			tc.policy.Projects = []string{"p1"}
			d := resolveWithPolicy(tc.policy,
				model.Scope{ProjectID: "p1", ConnectionID: tc.conn, ServerID: tc.server})
			if d.Allowed != tc.allowed {
				t.Fatalf("Allowed = %v, want %v (%s)", d.Allowed, tc.allowed, d.Reason)
			}
			if !tc.allowed {
				return
			}
			if d.Level != tc.wantLevel {
				t.Errorf("Level = %s, want %s", d.Level, tc.wantLevel)
			}
			if len(d.Caps) != len(tc.wantCaps) {
				t.Fatalf("Caps = %v, want %v", d.Caps, tc.wantCaps)
			}
			for _, c := range tc.wantCaps {
				if !slices.Contains(d.Caps, c) {
					t.Errorf("%s 가 빠졌다: %v", c, d.Caps)
				}
			}
		})
	}
}

func TestDecisionHasCap(t *testing.T) {
	d := Decision{Allowed: true, Caps: []model.Capability{model.CapDataRead}}
	if !d.HasCap(model.CapDataRead) {
		t.Error("가진 능력을 못 찾는다")
	}
	if d.HasCap(model.CapDataWrite) {
		t.Error("없는 능력을 가졌다고 한다")
	}
	// 접근 자체가 막힌 판정은 능력이 목록에 남아 있어도 거짓이어야 한다.
	blocked := Decision{Allowed: false, Caps: []model.Capability{model.CapDataRead}}
	if blocked.HasCap(model.CapDataRead) {
		t.Error("접근이 거부된 판정에서 능력이 참이면 안 된다")
	}
}

// 서버 단위 부여와 DB 단위 예외가 함께 성립해야 한다.
//
// 이 규칙이 뒤집히면(넓은 쪽이 이기면) "이 서버 전체에 모니터링, 단 billing만 제외"를
// 표현할 방법이 없어지고, 사람들은 결국 예외를 포기하고 전체를 열게 된다.
func TestServerScopeIsOverriddenByConnection(t *testing.T) {
	base := func() *model.AccessPolicy {
		return &model.AccessPolicy{
			Mode:               model.AccessAllowlist,
			DefaultLevel:       model.LevelNone,
			DefaultCaps:        []model.Capability{},
			Items:              []string{},
			Capabilities:       map[string]model.Level{},
			CapOverrides:       map[string][]model.Capability{},
			ServerItems:        []string{"srv1"},
			ServerCapabilities: map[string]model.Level{"srv1": model.LevelMonitor},
			ServerCapOverrides: map[string][]model.Capability{"srv1": {model.CapDataRead}},
			Projects:           []string{"p1"},
		}
	}
	appdb := model.Scope{ProjectID: "p1", ConnectionID: "appdb", ServerID: "srv1"}

	t.Run("서버 항목만으로 허용 목록을 통과한다", func(t *testing.T) {
		d := resolveWithPolicy(base(), appdb)
		if !d.Allowed || d.Level != model.LevelMonitor {
			t.Fatalf("서버 부여가 적용되지 않았다: %+v", d)
		}
		if !slices.Contains(d.Caps, model.CapDataRead) {
			t.Errorf("서버 능력이 적용되지 않았다: %v", d.Caps)
		}
	})

	t.Run("DB 오버라이드가 서버를 이긴다", func(t *testing.T) {
		p := base()
		p.Capabilities["appdb"] = model.LevelMigrate
		p.CapOverrides["appdb"] = []model.Capability{model.CapDataWrite}
		d := resolveWithPolicy(p, appdb)
		if d.Level != model.LevelMigrate {
			t.Errorf("Level = %s, want migrate", d.Level)
		}
		if slices.Contains(d.Caps, model.CapDataRead) || !slices.Contains(d.Caps, model.CapDataWrite) {
			t.Errorf("능력이 서버 값에 머물렀다: %v", d.Caps)
		}
	})

	t.Run("DB 오버라이드로 예외를 뺄 수 있다", func(t *testing.T) {
		p := base()
		p.Capabilities["billing"] = model.LevelNone
		d := resolveWithPolicy(p, model.Scope{ProjectID: "p1", ConnectionID: "billing", ServerID: "srv1"})
		if d.Allowed {
			t.Errorf("예외가 적용되지 않았다: %+v", d)
		}
	})

	t.Run("다른 서버의 DB는 여전히 범위 밖", func(t *testing.T) {
		d := resolveWithPolicy(base(), model.Scope{ProjectID: "p1", ConnectionID: "other", ServerID: "srv2"})
		if d.Allowed {
			t.Errorf("서버 부여가 다른 서버로 샜다: %+v", d)
		}
	})

	t.Run("denylist에서는 서버가 걸리면 아래 DB가 전부 막힌다", func(t *testing.T) {
		p := base()
		p.Mode = model.AccessDenylist
		p.DefaultLevel = model.LevelMigrate
		d := resolveWithPolicy(p, appdb)
		if d.Allowed {
			t.Errorf("차단 목록의 서버가 통과했다: %+v", d)
		}
		// 그 서버에 속하지 않은 DB는 영향받지 않는다.
		if other := resolveWithPolicy(p, model.Scope{ProjectID: "p1", ConnectionID: "x", ServerID: "srv2"}); !other.Allowed {
			t.Errorf("무관한 DB까지 막혔다: %+v", other)
		}
	})

	t.Run("서버가 없는 대상은 커넥션 규칙만 본다", func(t *testing.T) {
		p := base()
		p.Items = []string{"loose"}
		p.Capabilities["loose"] = model.LevelERD
		d := resolveWithPolicy(p, model.Scope{ProjectID: "p1", ConnectionID: "loose"})
		if !d.Allowed || d.Level != model.LevelERD {
			t.Fatalf("서버 없는 대상 판정이 틀렸다: %+v", d)
		}
	})
}

// 프로젝트는 등급보다 앞선 관문이다.
//
// 참여하지 않은 프로젝트의 DB는 등급이 무엇으로 적혀 있든 보이지 않아야 한다.
// 반대로 두면(등급을 먼저 보면) 옛 권한 설정이 남아 있는 사람에게 새 프로젝트의
// DB가 그대로 열린다 — 프로젝트를 나눈 이유가 사라진다.
func TestProjectGatesEverything(t *testing.T) {
	wide := func() *model.AccessPolicy {
		return &model.AccessPolicy{
			Mode:         model.AccessAll,
			DefaultLevel: model.LevelMigrate,
			DefaultCaps:  model.AllCapabilities(),
			Capabilities: map[string]model.Level{},
			CapOverrides: map[string][]model.Capability{},
			Projects:     []string{"mine"},
		}
	}

	t.Run("참여한 프로젝트는 지금까지처럼 판정한다", func(t *testing.T) {
		d := resolveWithPolicy(wide(), model.Scope{ProjectID: "mine", ConnectionID: "c1"})
		if !d.Allowed || d.Level != model.LevelMigrate {
			t.Fatalf("참여한 프로젝트가 막혔다: %+v", d)
		}
	})

	t.Run("참여하지 않은 프로젝트는 전체 허용이어도 막힌다", func(t *testing.T) {
		d := resolveWithPolicy(wide(), model.Scope{ProjectID: "theirs", ConnectionID: "c1"})
		if d.Allowed {
			t.Fatalf("남의 프로젝트가 열렸다: %+v", d)
		}
	})

	t.Run("DB별 등급을 따로 준 것도 프로젝트 밖이면 막힌다", func(t *testing.T) {
		p := wide()
		p.Capabilities["c9"] = model.LevelMigrate
		p.CapOverrides["c9"] = model.AllCapabilities()
		d := resolveWithPolicy(p, model.Scope{ProjectID: "theirs", ConnectionID: "c9"})
		if d.Allowed {
			t.Fatalf("옛 등급 설정이 관문을 넘었다: %+v", d)
		}
	})

	t.Run("프로젝트를 알 수 없는 대상은 막는다", func(t *testing.T) {
		d := resolveWithPolicy(wide(), model.Scope{ConnectionID: "c1"})
		if d.Allowed {
			t.Fatalf("소속을 모르는 DB가 열렸다: %+v", d)
		}
	})
}
