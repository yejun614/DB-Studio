// Package auth는 인증(세션)과 인가(RBAC) 판정을 담당한다.
package auth

import (
	"context"
	"fmt"
	"slices"

	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// Authorizer는 커넥션 접근 권한 판정을 한 곳에 모은다.
// HTTP 핸들러, WebSocket 허브, AI 툴 실행기가 모두 이 타입을 통해서만 권한을 판단해야 한다.
type Authorizer struct {
	st *store.Store
}

func NewAuthorizer(st *store.Store) *Authorizer { return &Authorizer{st: st} }

// Decision은 판정 결과와 그 근거를 담는다. 근거는 감사 로그와 UI 설명에 쓰인다.
type Decision struct {
	Allowed bool
	Level   model.Level
	// Caps는 이 커넥션에 대해 가진 데이터 능력이다. Level과 독립적인 축이므로
	// 등급이 높다고 자동으로 채워지지 않는다.
	Caps   []model.Capability
	Reason string
}

// HasCap은 판정 결과가 특정 능력을 포함하는지 반환한다.
func (d Decision) HasCap(c model.Capability) bool {
	return d.Allowed && slices.Contains(d.Caps, c)
}

// Resolve는 사용자가 특정 커넥션에 대해 가진 실효 등급을 계산한다.
//
// 판정 순서:
//  1. 비활성 사용자 → 거부
//  2. superadmin → 무조건 migrate
//  3. 프로젝트 참여 확인 → 참여하지 않았으면 거부
//  4. 접근 범위(mode) 확인 → 범위 밖이면 거부
//  5. 커넥션별 오버라이드가 있으면 그 등급, 없으면 default_level
//  6. 등급이 none이면 거부
func (a *Authorizer) Resolve(ctx context.Context, u *model.User, connectionID string) (Decision, error) {
	if d, done := shortCircuit(u); done {
		return d, nil
	}
	// 커넥션을 읽어야 소속 서버와 프로젝트를 알 수 있다.
	//
	// 예전에는 조회에 실패해도 서버 없이 판정을 이어 갔다 — 서버 설정은 커넥션
	// 설정보다 약해서 실패가 권한을 넓히지 않았기 때문이다. 프로젝트는 반대다.
	// 어느 프로젝트인지 모르면 참여 여부를 확인할 방법이 없고, 모른 채로 통과시키면
	// 관문이 조용히 열린다. 그래서 이제는 막는다.
	conn, err := a.st.GetConnection(ctx, connectionID)
	if err != nil {
		return Decision{Reason: "대상 DB를 확인할 수 없음"}, nil
	}
	return a.resolveScope(ctx, u, conn.Scope())
}

// ResolveScope는 커넥션을 이미 읽어 둔 호출부용이다. Resolve의 재조회를 피한다.
func (a *Authorizer) ResolveScope(ctx context.Context, u *model.User, scope model.Scope) (Decision, error) {
	if d, done := shortCircuit(u); done {
		return d, nil
	}
	return a.resolveScope(ctx, u, scope)
}

func (a *Authorizer) resolveScope(ctx context.Context, u *model.User, scope model.Scope) (Decision, error) {
	policy, err := a.st.GetAccessPolicy(ctx, u.ID)
	if err != nil {
		return Decision{}, fmt.Errorf("resolve access policy: %w", err)
	}
	return resolveWithPolicy(policy, scope), nil
}

// shortCircuit은 정책을 읽기 전에 결론이 나는 경우를 처리한다.
func shortCircuit(u *model.User) (Decision, bool) {
	if u == nil {
		return Decision{Reason: "인증되지 않은 요청"}, true
	}
	if model.UserStatus(u.Status) == model.UserDisabled {
		return Decision{Reason: "비활성화된 계정"}, true
	}
	if u.Role == model.RoleSuperadmin {
		return Decision{
			Allowed: true, Level: model.LevelMigrate,
			Caps: model.AllCapabilities(), Reason: "슈퍼 어드민",
		}, true
	}
	return Decision{}, false
}

// resolveWithPolicy는 순수 함수다. 정책과 대상만으로 결과가 결정되므로 테스트가 쉽다.
//
// 서버와 커넥션 두 층이 있고 **좁은 쪽이 이긴다**. "이 서버 전체에 모니터링, 단
// billing DB만 접근 불가"가 표현되어야 하기 때문이다. 반대로 두면 예외를 적을 수 없다.
func resolveWithPolicy(p *model.AccessPolicy, scope model.Scope) Decision {
	// 프로젝트가 첫 관문이다.
	//
	// 등급·능력보다 앞에 두는 이유: 프로젝트는 "무엇을 할 수 있는가"가 아니라
	// "무엇이 내 일인가"다. 참여하지 않은 프로젝트의 DB는 등급이 무엇으로 적혀
	// 있든 보이지 않아야 한다. 반대로 두면 옛 등급 설정이 남아 있는 사람에게 새
	// 프로젝트의 DB가 그대로 열린다.
	//
	// 프로젝트를 알 수 없는 대상(ProjectID가 빈 값)도 막는다. 참여 여부를 확인할
	// 수 없다는 뜻이고, 확인할 수 없는 것을 통과시키면 관문이 아니다.
	if scope.ProjectID == "" || !slices.Contains(p.Projects, scope.ProjectID) {
		return Decision{Reason: "참여하지 않은 프로젝트"}
	}

	inList := slices.Contains(p.Items, scope.ConnectionID)
	// 서버가 목록에 있으면 그 아래 DB도 목록에 있는 것으로 본다.
	// 이것이 일괄 부여의 실체다 — DB를 추가해도 권한을 다시 챙길 필요가 없다.
	serverListed := scope.ServerID != "" && slices.Contains(p.ServerItems, scope.ServerID)

	switch p.Mode {
	case model.AccessAll:
		// 전체 허용
	case model.AccessAllowlist:
		if !inList && !serverListed {
			return Decision{Reason: "허용 목록에 없는 DB"}
		}
	case model.AccessDenylist:
		// 어느 한쪽이라도 차단 목록에 있으면 막는다. 차단은 넓게 걸리는 편이 안전하다.
		if inList {
			return Decision{Reason: "차단 목록에 포함된 DB"}
		}
		if serverListed {
			return Decision{Reason: "차단 목록에 포함된 서버"}
		}
	default:
		return Decision{Reason: "알 수 없는 접근 모드"}
	}

	level, reason := p.DefaultLevel, "기본 등급"
	if override, ok := p.ServerCapabilities[scope.ServerID]; ok && scope.ServerID != "" {
		level, reason = override, "서버 등급 설정"
	}
	connLevel, hasConnLevel := p.Capabilities[scope.ConnectionID]
	if hasConnLevel {
		level, reason = connLevel, "DB별 등급 설정"
	}

	caps := p.DefaultCaps
	if override, ok := p.ServerCapOverrides[scope.ServerID]; ok && scope.ServerID != "" {
		caps = override
	}
	connCaps, hasConnCaps := p.CapOverrides[scope.ConnectionID]
	if hasConnCaps {
		caps = connCaps
	}

	// DB 하나만 "없음"으로 내린 것은 **예외를 빼겠다**는 뜻이다.
	// 그런데 서버에서 물려받은 데이터 능력이 남아 있으면 그 DB는 여전히 열려 있고,
	// 화면에는 "없음"으로 보인다 — 권한 화면이 거짓말을 하게 된다.
	// 그래서 이 경우 물려받은 능력까지 함께 지운다.
	// 다만 그 DB에 능력을 따로 지정했다면 그것은 의도한 것이므로 남긴다.
	if hasConnLevel && connLevel == model.LevelNone && !hasConnCaps {
		caps = nil
	}

	if !level.Valid() || level == model.LevelNone {
		// 등급이 none이어도 데이터 능력이 있으면 접근은 허용해야 한다.
		// "스키마는 못 보지만 데이터는 조회한다"가 이상해 보일 수 있으나,
		// 두 축을 독립으로 설계한 이상 한쪽이 0이라고 다른 쪽을 막을 근거가 없다.
		// 막으면 데이터 능력만 부여하는 설정이 조용히 무력화된다.
		if len(caps) == 0 {
			return Decision{Reason: "부여된 등급 없음(none)"}
		}
		return Decision{Allowed: true, Level: model.LevelNone, Caps: caps, Reason: "데이터 능력만 부여됨"}
	}
	return Decision{Allowed: true, Level: level, Caps: caps, Reason: reason}
}

// Can은 사용자가 커넥션에 대해 need 등급 이상의 권한을 가지는지 확인한다.
// 앱 전체에서 권한이 필요한 지점은 반드시 이 함수를 통과해야 한다.
func (a *Authorizer) Can(ctx context.Context, u *model.User, connectionID string, need model.Level) (Decision, error) {
	d, err := a.Resolve(ctx, u, connectionID)
	if err != nil {
		return Decision{}, err
	}
	if !d.Allowed {
		return d, nil
	}
	if !d.Level.Includes(need) {
		return Decision{
			Allowed: false,
			Level:   d.Level,
			Caps:    d.Caps,
			Reason:  fmt.Sprintf("%s 권한 필요 (현재 %s)", need, d.Level),
		}, nil
	}
	return d, nil
}

// CanCap은 사용자가 커넥션에 대해 특정 데이터 능력을 가지는지 확인한다.
//
// Can과 나란히 두는 이유: 두 축 어느 쪽이든 권한 판정은 이 파일을 통과해야 한다.
// 핸들러가 정책을 직접 읽어 판단하기 시작하면 판정 규칙이 흩어지고, 그러면
// 한 곳을 고쳐도 다른 곳이 옛 규칙으로 남는다.
func (a *Authorizer) CanCap(ctx context.Context, u *model.User, connectionID string, need model.Capability) (Decision, error) {
	d, err := a.Resolve(ctx, u, connectionID)
	if err != nil {
		return Decision{}, err
	}
	if !d.Allowed {
		return d, nil
	}
	if !slices.Contains(d.Caps, need) {
		return Decision{
			Allowed: false,
			Level:   d.Level,
			Caps:    d.Caps,
			Reason:  fmt.Sprintf("%s 권한이 없습니다", need.Label()),
		}, nil
	}
	return d, nil
}

// EffectiveAccessList는 주어진 커넥션 목록에 대한 판정 결과를 한 번에 계산한다.
// 정책을 한 번만 읽으므로 목록 화면에서 N+1 조회를 피할 수 있다.
func (a *Authorizer) EffectiveAccessList(ctx context.Context, u *model.User, conns []*model.Connection) ([]model.EffectiveAccess, error) {
	out := make([]model.EffectiveAccess, 0, len(conns))
	if u == nil {
		return out, nil
	}
	if u.Role == model.RoleSuperadmin {
		for _, c := range conns {
			out = append(out, model.EffectiveAccess{
				ConnectionID: c.ID, Accessible: true,
				Level: model.LevelMigrate, Caps: model.AllCapabilities(), Reason: "슈퍼 어드민",
			})
		}
		return out, nil
	}
	policy, err := a.st.GetAccessPolicy(ctx, u.ID)
	if err != nil {
		return nil, fmt.Errorf("resolve access policy: %w", err)
	}
	for _, c := range conns {
		d := resolveWithPolicy(policy, c.Scope())
		out = append(out, model.EffectiveAccess{
			ConnectionID: c.ID, Accessible: d.Allowed, Level: d.Level,
			Caps: d.Caps, Reason: d.Reason,
		})
	}
	return out, nil
}

// FilterByCap은 사용자가 특정 데이터 능력을 가진 커넥션만 남긴다.
// 데이터 화면과 SQL 콘솔의 커넥션 선택 목록이 이것을 쓴다 — 고를 수는 있는데
// 누르면 403이 나는 항목을 보여주면 권한 설정이 잘못된 것처럼 보인다.
func (a *Authorizer) FilterByCap(ctx context.Context, u *model.User, conns []*model.Connection, need model.Capability) ([]*model.Connection, map[string][]model.Capability, error) {
	acc, err := a.EffectiveAccessList(ctx, u, conns)
	if err != nil {
		return nil, nil, err
	}
	caps := make(map[string][]model.Capability, len(acc))
	for _, e := range acc {
		if e.Accessible && slices.Contains(e.Caps, need) {
			caps[e.ConnectionID] = e.Caps
		}
	}
	out := make([]*model.Connection, 0, len(conns))
	for _, c := range conns {
		if _, ok := caps[c.ID]; ok {
			out = append(out, c)
		}
	}
	return out, caps, nil
}

// FilterAccessible는 사용자가 need 등급 이상으로 접근 가능한 커넥션만 남긴다.
func (a *Authorizer) FilterAccessible(ctx context.Context, u *model.User, conns []*model.Connection, need model.Level) ([]*model.Connection, map[string]model.Level, error) {
	acc, err := a.EffectiveAccessList(ctx, u, conns)
	if err != nil {
		return nil, nil, err
	}
	levels := make(map[string]model.Level, len(acc))
	for _, e := range acc {
		if e.Accessible && e.Level.Includes(need) {
			levels[e.ConnectionID] = e.Level
		}
	}
	out := make([]*model.Connection, 0, len(conns))
	for _, c := range conns {
		if _, ok := levels[c.ID]; ok {
			out = append(out, c)
		}
	}
	return out, levels, nil
}
