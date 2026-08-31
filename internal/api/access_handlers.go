package api

import (
	"errors"
	"slices"
	"strings"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// handleGetAccess는 한 사용자의 접근 정책과, 전체 커넥션에 대한 실효 권한을 함께 반환한다.
// 관리자가 "이 사용자가 실제로 무엇을 할 수 있는가"를 한 화면에서 확인할 수 있게 하기 위함이다.
func (s *Server) handleGetAccess(c *fiber.Ctx) error {
	id := c.Params("id")
	target, err := s.st.GetUser(c.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "사용자를 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}

	policy, err := s.st.GetAccessPolicy(c.Context(), id)
	if err != nil {
		return err
	}
	conns, err := s.st.ListConnections(c.Context())
	if err != nil {
		return err
	}
	effective, err := s.authz.EffectiveAccessList(c.Context(), target, conns)
	if err != nil {
		return err
	}

	// 이 화면은 슈퍼 어드민 전용이라 전부 본다(nil = 제한 없음).
	servers, err := s.st.ListServers(c.Context(), nil)
	if err != nil {
		return err
	}
	// 프로젝트 전체를 함께 준다. 이 화면을 여는 사람은 슈퍼 어드민이고, 참여를
	// 정하려면 있는 프로젝트를 다 볼 수 있어야 한다.
	projects, err := s.st.ListProjects(c.Context(), "")
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{
		"user":        target,
		"policy":      policy,
		"connections": conns,
		"servers":     servers,
		"projects":    projects,
		"effective":   effective,
	})
}

type putAccessRequest struct {
	Mode         model.AccessMode       `json:"mode"`
	DefaultLevel model.Level            `json:"defaultLevel"`
	Items        []string               `json:"items"`
	Capabilities map[string]model.Level `json:"capabilities"`

	DefaultCaps  []model.Capability            `json:"defaultCaps"`
	CapOverrides map[string][]model.Capability `json:"capOverrides"`

	// 서버 단위 일괄 부여. 커넥션 지정이 있으면 그쪽이 이긴다.
	ServerItems        []string                      `json:"serverItems"`
	ServerCapabilities map[string]model.Level        `json:"serverCapabilities"`
	ServerCapOverrides map[string][]model.Capability `json:"serverCapOverrides"`

	// Projects는 참여 프로젝트다. 등급보다 앞선 관문이라 이 화면에서 함께 정한다 —
	// 등급을 아무리 줘도 참여하지 않았으면 아무것도 보이지 않고, 그 사실이 권한
	// 화면 어디에도 적혀 있지 않으면 원인을 짚을 수 없다.
	//
	// 포인터인 이유는 Perms와 같다: nil이면 손대지 않는다. 이 필드를 모르는 옛
	// 호출이 참여를 통째로 지우면 그 사람은 앱에서 아무것도 못 보게 된다.
	Projects *[]string `json:"projects"`

	// Perms는 전역 권한(매크로·셸·외부 호출)이다. 커넥션에 매이지 않지만
	// "권한을 바꾸려면 한 화면만 보면 된다"가 되도록 여기서 함께 받는다.
	//
	// 포인터인 이유: nil이면 손대지 않는다. 이 필드를 모르는 호출이 기존 전역
	// 권한을 조용히 지우면, 권한 회수가 아니라 사고가 된다.
	Perms *[]model.Perm `json:"perms"`
}

// validateScopeRefs는 정책에 남은 ID가 실재하는지 확인한다.
//
// 지워진 대상의 ID가 정책에 남으면 화면에는 보이지 않으면서 저장될 때마다 따라다니고,
// 같은 ID가 재사용되는 날 조용히 되살아난다.
func validateScopeRefs[T any](known map[string]bool, items []string, over map[string]T, label string) (string, string) {
	for _, id := range items {
		if !known[id] {
			return "존재하지 않는 " + label + "이 포함되어 있습니다", id
		}
	}
	for id := range over {
		if !known[id] {
			return "존재하지 않는 " + label + "이 포함되어 있습니다", id
		}
	}
	return "", ""
}

// normalizeCaps는 능력 집합을 검증하고 정리한다.
//
// data.write만 있고 data.read가 없는 조합은 거부한다. 어떤 행을 고칠지 고르려면
// 그 행이 보여야 하므로 화면에서 실행 불가능한 상태이고, "수정은 되는데 조회는
// 안 된다"는 설정은 언제나 실수다. 서버가 조용히 read를 끼워 넣는 방법도 있지만
// 그러면 저장한 것과 다른 값이 남아 관리자가 무엇을 준 것인지 알 수 없게 된다.
func normalizeCaps(caps []model.Capability) ([]model.Capability, string) {
	out := []model.Capability{}
	for _, c := range caps {
		if !c.Valid() {
			return nil, "알 수 없는 능력입니다: " + string(c)
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	if slices.Contains(out, model.CapDataWrite) && !slices.Contains(out, model.CapDataRead) {
		return nil, "데이터 수정 권한에는 데이터 조회 권한이 함께 필요합니다"
	}
	return out, ""
}

func (s *Server) handlePutAccess(c *fiber.Ctx) error {
	id := c.Params("id")
	target, err := s.st.GetUser(c.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "사용자를 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}

	var req putAccessRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	if !req.Mode.Valid() {
		return fail(c, fiber.StatusBadRequest, "invalid_mode", "알 수 없는 접근 모드입니다")
	}
	if !req.DefaultLevel.Valid() {
		return fail(c, fiber.StatusBadRequest, "invalid_level", "알 수 없는 기본 등급입니다")
	}
	for connID, level := range req.Capabilities {
		if !level.Valid() {
			return failDetail(c, fiber.StatusBadRequest, "invalid_level",
				"알 수 없는 등급입니다", connID)
		}
	}
	// 저장을 시작하기 전에 검증한다. 정책만 저장된 뒤 전역 권한에서 실패하면
	// 화면이 보여준 것과 저장된 것이 갈라진다.
	if req.Perms != nil {
		for _, p := range *req.Perms {
			if !p.Valid() {
				return failDetail(c, fiber.StatusBadRequest, "invalid_perm", "알 수 없는 권한입니다", string(p))
			}
		}
	}

	// 존재하지 않는 커넥션 ID가 정책에 남지 않도록 검증한다.
	conns, err := s.st.ListConnections(c.Context())
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(conns))
	for _, conn := range conns {
		known[conn.ID] = true
	}
	items := make([]string, 0, len(req.Items))
	for _, connID := range req.Items {
		if !known[connID] {
			return failDetail(c, fiber.StatusBadRequest, "unknown_connection",
				"존재하지 않는 커넥션이 포함되어 있습니다", connID)
		}
		items = append(items, connID)
	}
	caps := make(map[string]model.Level, len(req.Capabilities))
	for connID, level := range req.Capabilities {
		if !known[connID] {
			return failDetail(c, fiber.StatusBadRequest, "unknown_connection",
				"존재하지 않는 커넥션이 포함되어 있습니다", connID)
		}
		caps[connID] = level
	}

	defaultCaps, msg := normalizeCaps(req.DefaultCaps)
	if msg != "" {
		return fail(c, fiber.StatusBadRequest, "invalid_caps", msg)
	}
	capOverrides := make(map[string][]model.Capability, len(req.CapOverrides))
	for connID, list := range req.CapOverrides {
		if !known[connID] {
			return failDetail(c, fiber.StatusBadRequest, "unknown_connection",
				"존재하지 않는 커넥션이 포함되어 있습니다", connID)
		}
		normalized, msg := normalizeCaps(list)
		if msg != "" {
			return failDetail(c, fiber.StatusBadRequest, "invalid_caps", msg, connID)
		}
		capOverrides[connID] = normalized
	}

	// ---- 서버 단위 ----
	servers, err := s.st.ListServers(c.Context(), nil)
	if err != nil {
		return err
	}
	knownServers := make(map[string]bool, len(servers))
	for _, srv := range servers {
		knownServers[srv.ID] = true
	}
	for srvID, level := range req.ServerCapabilities {
		if !level.Valid() {
			return failDetail(c, fiber.StatusBadRequest, "invalid_level", "알 수 없는 등급입니다", srvID)
		}
	}
	if msg, detail := validateScopeRefs(knownServers, req.ServerItems, req.ServerCapabilities, "서버"); msg != "" {
		return failDetail(c, fiber.StatusBadRequest, "unknown_server", msg, detail)
	}
	if msg, detail := validateScopeRefs(knownServers, nil, req.ServerCapOverrides, "서버"); msg != "" {
		return failDetail(c, fiber.StatusBadRequest, "unknown_server", msg, detail)
	}
	serverCapOverrides := make(map[string][]model.Capability, len(req.ServerCapOverrides))
	for srvID, list := range req.ServerCapOverrides {
		normalized, msg := normalizeCaps(list)
		if msg != "" {
			return failDetail(c, fiber.StatusBadRequest, "invalid_caps", msg, srvID)
		}
		serverCapOverrides[srvID] = normalized
	}
	serverItems := req.ServerItems
	if serverItems == nil {
		serverItems = []string{}
	}
	serverCaps := req.ServerCapabilities
	if serverCaps == nil {
		serverCaps = map[string]model.Level{}
	}

	// 참여 프로젝트. 실재하는 것만 남긴다.
	projects, err := s.st.ListProjects(c.Context(), "")
	if err != nil {
		return err
	}
	knownProjects := make(map[string]bool, len(projects))
	for _, p := range projects {
		knownProjects[p.ID] = true
	}
	joined, err := s.st.ProjectIDsForUser(c.Context(), id)
	if err != nil {
		return err
	}
	if req.Projects != nil {
		joined = []string{}
		for _, pid := range *req.Projects {
			if !knownProjects[pid] {
				return failDetail(c, fiber.StatusBadRequest, "unknown_project",
					"존재하지 않는 프로젝트가 포함되어 있습니다", pid)
			}
			joined = append(joined, pid)
		}
	}

	policy := &model.AccessPolicy{
		UserID:             id,
		Projects:           joined,
		Mode:               req.Mode,
		DefaultLevel:       req.DefaultLevel,
		Items:              items,
		Capabilities:       caps,
		DefaultCaps:        defaultCaps,
		CapOverrides:       capOverrides,
		ServerItems:        serverItems,
		ServerCapabilities: serverCaps,
		ServerCapOverrides: serverCapOverrides,
	}
	if err := s.st.SetAccessPolicy(c.Context(), policy); err != nil {
		return err
	}
	if req.Perms != nil {
		if _, err := s.st.UpdateUser(c.Context(), id, store.UpdateUserParams{Perms: req.Perms}); err != nil {
			return err
		}
	}
	// 권한 변경은 즉시 효력을 가져야 하므로 대상 사용자의 세션을 무효화한다.
	if err := s.st.DeleteUserSessions(c.Context(), id); err != nil {
		return err
	}

	detail := map[string]any{
		"username":     target.Username,
		"mode":         req.Mode,
		"defaultLevel": req.DefaultLevel,
		"itemCount":    len(items),
		"overrides":    len(caps),
		// 데이터 능력은 감사 로그에 값 그대로 남긴다. "누가 언제 데이터 수정
		// 권한을 줬는가"는 사고 조사에서 가장 먼저 확인하는 항목이다.
		"defaultCaps":  model.CapsToString(defaultCaps),
		"capOverrides": len(capOverrides),
		// 서버 단위 부여는 한 번에 여러 DB를 여는 설정이므로 따로 센다.
		"serverItems":     len(serverItems),
		"serverOverrides": len(serverCaps) + len(serverCapOverrides),
		// 참여 프로젝트는 개수만으로 부족하다. 어느 팀의 자원에 손이 닿게 되었는지가
		// 사고 조사에서 먼저 필요한 값이다.
		"projects": strings.Join(joined, ","),
	}
	// 셸 실행·외부 호출은 값 그대로 남긴다. 데이터 능력과 같은 이유다.
	if req.Perms != nil {
		detail["perms"] = model.PermsToString(*req.Perms)
	}
	s.audit(c, store.AuditParams{
		Action: store.ActionAccessUpdated, TargetType: "user", TargetID: id, Detail: detail,
	})

	saved, err := s.st.GetAccessPolicy(c.Context(), id)
	if err != nil {
		return err
	}
	effective, err := s.authz.EffectiveAccessList(c.Context(), target, conns)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"policy": saved, "effective": effective})
}
