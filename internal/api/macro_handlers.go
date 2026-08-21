package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/valyala/fasthttp"

	"dbstudio/internal/applog"
	"dbstudio/internal/macro"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 매크로 API.
//
// **매크로는 만든 사람의 것이다.** 기본은 비공개이고, 공유는 세 가지 방법으로만 넓어진다:
// 공개로 전환(조회+실행 / 수정 허용 중 선택), 협업자 지정, 그리고 슈퍼어드민.
// 규칙 자체는 model.ResolveMacroAccess 한 곳에 있고, 여기서는 그 결과를 HTTP 응답으로
// 옮긴다(requireMacro).
//
// 이 파일에서 지키는 두 가지:
//
//  1. **볼 수 없는 것은 없는 것이다.** 권한 없는 매크로에 대한 응답은 403이 아니라
//     404다. 403은 "그런 이름의 매크로가 있다"는 사실을 알려주고, 비공개 매크로에서는
//     그 사실 자체가 새어 나가면 안 되는 정보다.
//  2. **접근 권한과 실행 권한은 다른 축이다.** 매크로를 볼 수 있다고 그 안의 노드가
//     도는 것은 아니다. 실행은 언제나 실행자의 커넥션 권한으로 노드마다 판정된다
//     (macroRunPermission). 두 판정을 모두 통과해야 실행된다.

// viewer는 현재 로그인 사용자의 뷰어를 만든다.
func viewer(c *fiber.Ctx) store.MacroViewer {
	return store.MacroViewer{User: currentUser(c)}
}

// requireMacro는 매크로를 읽고 need 이상의 권한이 있는지 확인한다.
//
// 매크로를 만지는 핸들러는 예외 없이 이 함수로 시작한다. 핸들러마다 조건을 직접 쓰면
// 새 핸들러가 추가될 때 확인을 잊는 것이 시간문제다.
func (s *Server) requireMacro(c *fiber.Ctx, id string, need model.MacroAccess) (*store.Macro, error) {
	m, err := s.st.GetMacro(c.Context(), id, viewer(c))
	if errors.Is(err, store.ErrNotFound) {
		return nil, fiber.NewError(fiber.StatusNotFound, "매크로를 찾을 수 없습니다")
	}
	if err != nil {
		return nil, err
	}
	// 볼 수조차 없으면 존재를 숨긴다.
	if !m.Access.CanView() {
		return nil, fiber.NewError(fiber.StatusNotFound, "매크로를 찾을 수 없습니다")
	}
	if m.Access < need {
		return nil, fiber.NewError(fiber.StatusForbidden, macroDenyReason(need))
	}
	return m, nil
}

// macroDenyReason은 무엇이 모자란지 사람 말로 알려준다.
// "권한이 없습니다"만 보여주면 사용자는 누구에게 무엇을 요청해야 할지 알 수 없다.
func macroDenyReason(need model.MacroAccess) string {
	switch need {
	case model.MacroAccessOwn:
		return "매크로를 삭제할 수 있는 사람은 만든 사람뿐입니다"
	case model.MacroAccessManage:
		return "공유 설정과 자동 실행은 만든 사람과 협업자만 바꿀 수 있습니다"
	case model.MacroAccessEdit:
		return "이 매크로는 조회와 실행만 허용되어 있습니다"
	}
	return "이 매크로에 접근할 권한이 없습니다"
}

func (s *Server) handleMacroMeta(c *fiber.Ctx) error {
	u := currentUser(c)
	return c.JSON(fiber.Map{
		"specs":        macro.Specs(),
		"shellEnabled": s.cfg.AllowShell,
		"canRunShell":  s.cfg.AllowShell && u.HasPerm(model.PermScriptRun),
	})
}

func (s *Server) handleListMacros(c *fiber.Ctx) error {
	macros, err := s.st.ListMacros(c.Context(), viewer(c))
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"items": macros})
}

type macroRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Graph       string `json:"graph"`
	Note        string `json:"note"`
}

func (s *Server) handleCreateMacro(c *fiber.Ctx) error {
	u := currentUser(c)
	var req macroRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return fail(c, fiber.StatusBadRequest, "invalid_name", "이름을 입력하세요")
	}

	// 빈 매크로도 시작 노드는 갖고 태어난다. 빈 캔버스에서 "무엇부터 놓아야 하는가"를
	// 고민하게 만들 이유가 없고, 시작 노드는 어차피 반드시 하나 있어야 한다.
	graph := req.Graph
	if strings.TrimSpace(graph) == "" {
		graph = defaultGraph()
	}
	parsed, err := macro.ParseGraph(graph)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid_graph", err.Error())
	}
	if issues := parsed.Validate(macro.KnownTypes()); macro.HasFatal(issues) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid_graph", "message": "매크로에 오류가 있습니다", "issues": issues,
		})
	}

	m, err := s.st.CreateMacro(c.Context(), req.Name, req.Description, graph, u, displayName(u))
	if errors.Is(err, store.ErrDuplicateName) {
		return fail(c, fiber.StatusConflict, "duplicate", "같은 이름의 매크로가 이미 있습니다")
	}
	if err != nil {
		return err
	}

	s.audit(c, store.AuditParams{
		Action: "macro.created", TargetType: "macro", TargetID: m.ID,
		Detail: map[string]any{"name": m.Name, "visibility": m.Visibility},
	})
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"macro": m})
}

// handleGetMacro는 편집 화면이 필요한 것을 한 번에 준다.
// 매크로·그래프·노드 정의·팔레트·실행 가능 여부를 따로 받으면 화면이 다섯 번
// 왕복하고, 그 사이에 서로 어긋난 상태를 그리게 된다.
func (s *Server) handleGetMacro(c *fiber.Ctx) error {
	m, err := s.requireMacro(c, c.Params("id"), model.MacroAccessView)
	if err != nil {
		return err
	}

	version := m.CurrentVersion
	if v := c.Query("version"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fail(c, fiber.StatusBadRequest, "bad_request", "버전 번호가 올바르지 않습니다")
		}
		version = n
	}
	ver, err := s.st.GetMacroVersion(c.Context(), m.ID, version)
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "해당 버전을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}

	versions, err := s.st.ListMacroVersions(c.Context(), m.ID)
	if err != nil {
		return err
	}

	graph, err := macro.ParseGraph(ver.Graph)
	if err != nil {
		return failDetail(c, fiber.StatusInternalServerError, "invalid_graph",
			"저장된 그래프를 읽지 못했습니다", err.Error())
	}
	issues := graph.Validate(macro.KnownTypes())

	defs, err := s.macroPalette(c, m, graph)
	if err != nil {
		return err
	}
	collaborators, err := s.st.ListMacroCollaborators(c.Context(), m.ID)
	if err != nil {
		return err
	}

	permission, err := s.macroRunPermission(c, graph, defs)
	if err != nil {
		return err
	}

	return c.JSON(fiber.Map{
		"macro":         m,
		"version":       ver,
		"graph":         graph,
		"versions":      versions,
		"nodeDefs":      defs,
		"collaborators": collaborators,
		"issues":        issues,
		"permission":    permission,
	})
}

// macroPalette는 편집 화면이 쓸 노드 정의 목록을 만든다.
//
// 볼 수 있는 노드에 더해, **그래프가 이미 참조하는 노드는 볼 수 없어도 껍데기를
// 함께 넣는다.** 없으면 캔버스에 이름 없는 상자가 뜨고 "알 수 없는 노드" 오류가
// 나는데, 정작 그 매크로는 잘 돌기 때문이다(실행은 공개 설정을 보지 않는다).
// 껍데기에는 스크립트가 빠져 있다(store.scanNodeDef) — 이름과 포트는 그래프를
// 그리는 데 필요하고, 본문은 그렇지 않다.
func (s *Server) macroPalette(c *fiber.Ctx, m *store.Macro, g *macro.Graph) ([]*store.NodeDef, error) {
	defs, err := s.st.ListVisibleNodeDefs(c.Context(), m.ID, viewer(c))
	if err != nil {
		return nil, err
	}
	have := make(map[string]bool, len(defs))
	for _, d := range defs {
		have[d.ID] = true
	}
	for _, n := range g.Nodes {
		if n.Type != macro.TypeCustom || n.NodeRef == "" || have[n.NodeRef] {
			continue
		}
		d, err := s.st.GetNodeDef(c.Context(), n.NodeRef, viewer(c))
		if errors.Is(err, store.ErrNotFound) {
			continue // 지워진 노드다. 그래프 검증이 "알 수 없는 노드"로 잡는다.
		}
		if err != nil {
			return nil, err
		}
		have[d.ID] = true
		defs = append(defs, d)
	}
	return defs, nil
}

// macroRunPermission은 이 사용자가 이 그래프를 실행할 수 있는지 판정한다.
//
// 실행 버튼을 누르기 전에 알려주는 것이 요구사항이다("실행 권한이 부족한 경우
// 경고를 띄우고 실행 버튼을 비활성화"). 이 판정은 편의이고, 실행 시점에 엔진이
// 노드마다 다시 확인한다 — 화면을 조작해 이 값을 바꿔도 실행은 막힌다.
func (s *Server) macroRunPermission(c *fiber.Ctx, g *macro.Graph, defs []*store.NodeDef) (fiber.Map, error) {
	u := currentUser(c)

	// 판정은 엔진이 한다. 화면·MCP·AI 툴·자동 실행이 모두 같은 함수를 쓰므로
	// 어느 하나가 느슨해질 수 없다(macro.Engine.Blockers 참조).
	blockers, err := s.macros.Blockers(c.Context(), u, g)
	if err != nil {
		return nil, err
	}

	specs := map[string]macro.NodeSpec{}
	for _, spec := range macro.Specs() {
		specs[spec.Type] = spec
	}
	usesShell, usesCustom := false, false
	for _, n := range g.Nodes {
		if n.Disabled {
			continue
		}
		if n.Type == macro.TypeCustom {
			// 사용자 노드는 스크립트 안에서 무엇을 부르는지 미리 알 수 없다.
			// 실행 시점에 호스트 함수가 막지만, 그 사실을 화면에도 알려 준다.
			usesCustom = true
		}
		if spec, ok := specs[n.Type]; ok && spec.NeedsShell {
			usesShell = true
		}
	}

	items := make([]fiber.Map, 0, len(blockers))
	for _, b := range blockers {
		items = append(items, fiber.Map{"nodeId": b.NodeID, "node": b.Node, "reason": b.Reason})
	}
	return fiber.Map{
		"canRun":     len(blockers) == 0,
		"blockers":   items,
		"usesShell":  usesShell,
		"usesCustom": usesCustom,
	}, nil
}

func (s *Server) handleUpdateMacro(c *fiber.Ctx) error {
	u := currentUser(c)
	var req macroRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return fail(c, fiber.StatusBadRequest, "invalid_name", "이름을 입력하세요")
	}

	if _, err := s.requireMacro(c, c.Params("id"), model.MacroAccessEdit); err != nil {
		return err
	}

	err := s.st.UpdateMacroMeta(c.Context(), c.Params("id"), req.Name, req.Description, displayName(u))
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "매크로를 찾을 수 없습니다")
	}
	if errors.Is(err, store.ErrDuplicateName) {
		return fail(c, fiber.StatusConflict, "duplicate", "같은 이름의 매크로가 이미 있습니다")
	}
	if err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "macro.updated", TargetType: "macro", TargetID: c.Params("id"),
		Detail: map[string]any{"name": req.Name},
	})
	m, err := s.st.GetMacro(c.Context(), c.Params("id"), viewer(c))
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"macro": m})
}

// handleDeleteMacro는 매크로를 지운다. 만든 사람(과 슈퍼어드민)만 할 수 있다.
//
// 협업자에게도 열지 않은 이유: 삭제는 이 기능에서 유일하게 되돌릴 수 없는 동작이다.
// 버전이 전부 함께 사라지므로, 잘못된 수정과 달리 이력으로 복구할 수 없다.
func (s *Server) handleDeleteMacro(c *fiber.Ctx) error {
	m, err := s.requireMacro(c, c.Params("id"), model.MacroAccessOwn)
	if err != nil {
		return err
	}
	if err := s.st.DeleteMacro(c.Context(), m.ID); err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "macro.deleted", TargetType: "macro", TargetID: m.ID,
		Detail: map[string]any{"name": m.Name, "versions": m.VersionCount},
	})
	return c.JSON(fiber.Map{"ok": true})
}

// handleSaveMacroVersion은 편집 결과를 새 버전으로 저장한다.
//
// 덮어쓰기가 아니라 언제나 새 버전인 이유는 요구사항이자 안전장치다. 여러 사람이
// 같은 매크로를 고치므로(실시간 협업은 하지 않는다), 마지막에 저장한 사람이 앞사람의
// 작업을 지우는 일이 반드시 생긴다. 버전이 쌓이면 그것은 사고가 아니라 되돌릴 수 있는
// 이력이 된다.
func (s *Server) handleSaveMacroVersion(c *fiber.Ctx) error {
	u := currentUser(c)
	m, err := s.requireMacro(c, c.Params("id"), model.MacroAccessEdit)
	if err != nil {
		return err
	}

	var req macroRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	graph, err := macro.ParseGraph(req.Graph)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid_graph", err.Error())
	}
	issues := graph.Validate(macro.KnownTypes())
	if macro.HasFatal(issues) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid_graph", "message": "매크로에 오류가 있어 저장할 수 없습니다",
			"issues": issues,
		})
	}

	// 정규화한 JSON을 저장한다. 화면이 보낸 문자열을 그대로 두면 키 순서나 공백
	// 차이만으로도 버전이 달라 보이고, 버전 비교가 무의미해진다.
	normalized, err := graph.JSON()
	if err != nil {
		return err
	}
	version, err := s.st.CreateMacroVersion(c.Context(), m.ID, normalized,
		strings.TrimSpace(req.Note), u.ID, displayName(u))
	if err != nil {
		return err
	}

	s.audit(c, store.AuditParams{
		Action: "macro.version.saved", TargetType: "macro", TargetID: m.ID,
		Detail: map[string]any{
			"name": m.Name, "version": version, "nodes": len(graph.Nodes), "note": req.Note,
		},
	})
	return c.JSON(fiber.Map{"version": version, "issues": issues})
}

func (s *Server) handleListMacroVersions(c *fiber.Ctx) error {
	if _, err := s.requireMacro(c, c.Params("id"), model.MacroAccessView); err != nil {
		return err
	}
	versions, err := s.st.ListMacroVersions(c.Context(), c.Params("id"))
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"items": versions})
}

func (s *Server) handleGetMacroVersion(c *fiber.Ctx) error {
	if _, err := s.requireMacro(c, c.Params("id"), model.MacroAccessView); err != nil {
		return err
	}
	version, err := strconv.Atoi(c.Params("version"))
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "버전 번호가 올바르지 않습니다")
	}
	v, err := s.st.GetMacroVersion(c.Context(), c.Params("id"), version)
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "버전을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"version": v})
}

// handleRestoreMacroVersion은 옛 버전의 그래프로 되돌린다.
//
// current_version을 옛 번호로 바꾸는 대신 **새 버전을 만든다.** 포인터만 옮기면
// 그 뒤에 만들어진 버전들이 미래에 남아 이력이 시간순으로 읽히지 않고,
// "지금 무엇이 최신인가"가 번호로 판단되지 않는다.
func (s *Server) handleRestoreMacroVersion(c *fiber.Ctx) error {
	u := currentUser(c)
	version, err := strconv.Atoi(c.Params("version"))
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "버전 번호가 올바르지 않습니다")
	}
	// 되돌리기는 새 버전을 만드는 일이므로 수정 권한이 필요하다.
	m, err := s.requireMacro(c, c.Params("id"), model.MacroAccessEdit)
	if err != nil {
		return err
	}
	old, err := s.st.GetMacroVersion(c.Context(), m.ID, version)
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "버전을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	if old.Version == m.CurrentVersion {
		return fail(c, fiber.StatusBadRequest, "same_version", "이미 현재 버전입니다")
	}

	next, err := s.st.CreateMacroVersion(c.Context(), m.ID, old.Graph,
		"v"+strconv.Itoa(old.Version)+" 으로 되돌림", u.ID, displayName(u))
	if err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "macro.version.restored", TargetType: "macro", TargetID: m.ID,
		Detail: map[string]any{"name": m.Name, "from": old.Version, "version": next},
	})
	return c.JSON(fiber.Map{"version": next})
}

// ---------- 실행 ----------

type runMacroRequest struct {
	Version int            `json:"version"`
	Params  map[string]any `json:"params"`
}

func (s *Server) handleRunMacro(c *fiber.Ctx) error {
	u := currentUser(c)
	// 실행은 조회 권한만 있으면 된다. 공개 매크로를 "조회+실행"으로 열어 두는 것이
	// 공유의 기본 형태이고, 실행 자체의 위험은 노드 단위 판정이 막는다.
	m, err := s.requireMacro(c, c.Params("id"), model.MacroAccessView)
	if err != nil {
		return err
	}

	var req runMacroRequest
	_ = c.BodyParser(&req)

	// 실행 전에 권한을 한 번 더 판정한다. 화면이 막았더라도 API는 직접 호출될 수 있고,
	// 무엇보다 화면을 연 뒤에 권한이 회수되었을 수 있다.
	ver, err := s.st.GetMacroVersion(c.Context(), m.ID, req.Version)
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "실행할 버전을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	graph, err := macro.ParseGraph(ver.Graph)
	if err != nil {
		return failDetail(c, fiber.StatusBadRequest, "invalid_graph",
			"저장된 그래프를 읽지 못했습니다", err.Error())
	}
	defs, err := s.st.ListNodeDefs(c.Context(), m.ID)
	if err != nil {
		return err
	}
	permission, err := s.macroRunPermission(c, graph, defs)
	if err != nil {
		return err
	}
	if canRun, _ := permission["canRun"].(bool); !canRun {
		s.audit(c, store.AuditParams{
			Action: "macro.run.denied", TargetType: "macro", TargetID: m.ID,
			Result: "denied",
			Detail: map[string]any{"name": m.Name, "blockers": permission["blockers"]},
		})
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "forbidden", "message": "이 매크로를 실행할 권한이 없습니다",
			"blockers": permission["blockers"],
		})
	}

	runID, err := s.macros.Start(c.Context(), macro.RunRequest{
		MacroID: m.ID, Version: ver.Version, Actor: u, ActorIP: clientIP(c),
		Params: req.Params, Trigger: "manual",
	})
	if err != nil {
		return failDetail(c, fiber.StatusBadRequest, "run_failed", "매크로를 시작하지 못했습니다", err.Error())
	}

	s.audit(c, store.AuditParams{
		Action: "macro.run.started", TargetType: "macro", TargetID: m.ID,
		Detail: map[string]any{
			"name": m.Name, "version": ver.Version, "runId": runID, "params": req.Params,
		},
	})
	return c.JSON(fiber.Map{"runId": runID})
}

func (s *Server) handleListMacroRuns(c *fiber.Ctx) error {
	limit, _ := strconv.Atoi(c.Query("limit"))
	runs, err := s.st.ListMacroRuns(c.Context(), c.Query("macro"), c.Query("status"), limit, viewer(c))
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"items": runs})
}

// requireRun은 실행 기록 하나를 읽고 볼 자격이 있는지 확인한다.
//
// 두 갈래로 통과한다: 내가 돌린 실행이거나, 그 매크로를 볼 수 있거나.
// 앞의 것이 필요한 이유는 매크로가 나중에 비공개가 되거나 삭제될 수 있어서다 —
// 내가 무엇을 실행했는지는 남이 설정을 바꿨다고 사라지면 안 된다.
func (s *Server) requireRun(c *fiber.Ctx, runID string) (*store.MacroRun, error) {
	run, err := s.st.GetMacroRun(c.Context(), runID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fiber.NewError(fiber.StatusNotFound, "실행 기록을 찾을 수 없습니다")
	}
	if err != nil {
		return nil, err
	}
	u := currentUser(c)
	if run.ActorID != "" && run.ActorID == u.ID {
		return run, nil
	}
	if u.Role == model.RoleSuperadmin {
		return run, nil
	}
	// 매크로가 지워졌으면(macro_id NULL) 판정할 대상이 없다. 실행자 본인과
	// 슈퍼어드민만 남는다 — 위에서 이미 통과했으므로 여기서는 막는다.
	if run.MacroID == "" {
		return nil, fiber.NewError(fiber.StatusNotFound, "실행 기록을 찾을 수 없습니다")
	}
	if _, err := s.requireMacro(c, run.MacroID, model.MacroAccessView); err != nil {
		return nil, fiber.NewError(fiber.StatusNotFound, "실행 기록을 찾을 수 없습니다")
	}
	return run, nil
}

func (s *Server) handleGetMacroRun(c *fiber.Ctx) error {
	run, err := s.requireRun(c, c.Params("runId"))
	if err != nil {
		return err
	}
	after, _ := strconv.Atoi(c.Query("after"))
	logs, err := s.st.ListRunLogs(c.Context(), run.ID, after)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"run": run, "logs": logs, "live": s.macros.IsRunning(run.ID)})
}

func (s *Server) handleCancelMacroRun(c *fiber.Ctx) error {
	u := currentUser(c)
	run, err := s.requireRun(c, c.Params("runId"))
	if err != nil {
		return err
	}
	if err := s.macros.Cancel(run.ID, u); err != nil {
		return fail(c, fiber.StatusBadRequest, "not_running", err.Error())
	}
	s.audit(c, store.AuditParams{
		Action: "macro.run.canceled", TargetType: "macro", TargetID: run.MacroID,
		Detail: map[string]any{"name": run.MacroName, "runId": run.ID},
	})
	return c.JSON(fiber.Map{"ok": true})
}

// handleStreamMacroRun은 실행 로그를 SSE로 흘려보낸다.
//
// 폴링 대신 스트림을 쓰는 이유: 매크로는 몇 초에서 몇 분까지 돌고, 그동안 사용자는
// 화면을 보고 있다. 1초 폴링은 대부분 빈 응답이고 로그가 몰릴 때는 늦는다.
func (s *Server) handleStreamMacroRun(c *fiber.Ctx) error {
	run, err := s.requireRun(c, c.Params("runId"))
	if err != nil {
		return err
	}
	after, _ := strconv.Atoi(c.Query("after"))

	// 구독을 **먼저** 건다. 지난 로그를 읽은 뒤에 구독하면 그 사이에 생긴 줄이
	// 사라진다. 먼저 구독하고 나중에 중복을 seq로 거르는 편이 안전하다.
	events, unsubscribe := s.macros.Subscribe(run.ID)

	backlog, err := s.st.ListRunLogs(c.Context(), run.ID, after)
	if err != nil {
		unsubscribe()
		return err
	}
	finished := run.Status != "running"

	c.Set(fiber.HeaderContentType, "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	srv := s
	runID := run.ID
	c.Context().SetBodyStreamWriter(fasthttp.StreamWriter(func(w *bufio.Writer) {
		// 스트림 라이터는 핸들러가 반환한 뒤 실행되므로 Fiber의 recover가 감싸지 않는다.
		defer applog.Recover("macro.run.stream")
		defer unsubscribe()

		out := &sseWriter{w: w}
		lastSeq := after
		for _, entry := range backlog {
			if entry.Seq <= lastSeq {
				continue
			}
			lastSeq = entry.Seq
			if out.send("log", entry) != nil {
				return
			}
		}
		if finished {
			final, err := srv.st.GetMacroRun(context.Background(), runID)
			if err == nil {
				_ = out.send("done", final)
			}
			return
		}

		// 하트비트: 프록시와 브라우저가 조용한 연결을 끊는 것을 막는다.
		// 매크로는 한동안 로그 없이 도는 구간이 흔하다(긴 쿼리 하나).
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case entry, ok := <-events:
				if !ok {
					return
				}
				if entry.Seq <= lastSeq {
					continue
				}
				lastSeq = entry.Seq
				if out.send("log", entry) != nil {
					return
				}
			case <-ticker.C:
				if out.send("ping", fiber.Map{"seq": lastSeq}) != nil {
					return
				}
			}
			if !srv.macros.IsRunning(runID) {
				// 실행이 끝났다. 마지막 로그가 아직 채널에 남아 있을 수 있으므로
				// 잠깐 더 비운 뒤 최종 상태를 보낸다.
				drainDeadline := time.After(500 * time.Millisecond)
			drain:
				for {
					select {
					case entry, ok := <-events:
						if !ok {
							break drain
						}
						if entry.Seq > lastSeq {
							lastSeq = entry.Seq
							if out.send("log", entry) != nil {
								return
							}
						}
					case <-drainDeadline:
						break drain
					}
				}
				final, err := srv.st.GetMacroRun(context.Background(), runID)
				if err == nil {
					_ = out.send("done", final)
				}
				return
			}
		}
	}))
	return nil
}

// ---------- 사용자 노드 정의 ----------

type nodeDefRequest struct {
	Name        string `json:"name"`
	Scope       string `json:"scope"`
	MacroID     string `json:"macroId"`
	Description string `json:"description"`
	Fields      any    `json:"fields"`
	Ports       any    `json:"ports"`
	Script      string `json:"script"`
	Note        string `json:"note"`
}

// requireNodeDef는 노드 정의를 읽고 need 이상의 권한이 있는지 확인한다.
//
// 매크로 전용 노드는 소속 매크로의 판정을 물려받는다 — 그 계산은 저장 계층이
// 이미 끝냈으므로(store.scanNodeDef) 여기서는 결과만 본다.
func (s *Server) requireNodeDef(c *fiber.Ctx, id string, need model.MacroAccess) (*store.NodeDef, error) {
	def, err := s.st.GetNodeDef(c.Context(), id, viewer(c))
	if errors.Is(err, store.ErrNotFound) {
		return nil, fiber.NewError(fiber.StatusNotFound, "노드를 찾을 수 없습니다")
	}
	if err != nil {
		return nil, err
	}
	if !def.Access.CanView() {
		return nil, fiber.NewError(fiber.StatusNotFound, "노드를 찾을 수 없습니다")
	}
	if def.Access < need {
		return nil, fiber.NewError(fiber.StatusForbidden, "이 노드를 수정할 권한이 없습니다")
	}
	return def, nil
}

func (s *Server) handleListNodeDefs(c *fiber.Ctx) error {
	// 매크로를 지정했다면 그 매크로를 볼 수 있어야 한다.
	// 그러지 않으면 이 경로가 비공개 매크로의 전용 노드를 읽는 뒷문이 된다.
	if macroID := strings.TrimSpace(c.Query("macro")); macroID != "" {
		if _, err := s.requireMacro(c, macroID, model.MacroAccessView); err != nil {
			return err
		}
	}
	defs, err := s.st.ListVisibleNodeDefs(c.Context(), c.Query("macro"), viewer(c))
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"items": defs})
}

func nodeDefParams(req nodeDefRequest, u *model.User) (store.SaveNodeDefParams, string) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return store.SaveNodeDefParams{}, "노드 이름을 입력하세요"
	}
	scope := req.Scope
	if scope != "global" && scope != "macro" {
		return store.SaveNodeDefParams{}, "노드 범위는 global 또는 macro 여야 합니다"
	}
	if scope == "macro" && strings.TrimSpace(req.MacroID) == "" {
		return store.SaveNodeDefParams{}, "매크로 전용 노드는 대상 매크로가 필요합니다"
	}
	if strings.TrimSpace(req.Script) == "" {
		return store.SaveNodeDefParams{}, "스크립트를 입력하세요"
	}

	fields := "[]"
	if req.Fields != nil {
		if b, err := json.Marshal(req.Fields); err == nil {
			fields = string(b)
		}
	}
	ports := "[]"
	if req.Ports != nil {
		if b, err := json.Marshal(req.Ports); err == nil {
			ports = string(b)
		}
	}
	return store.SaveNodeDefParams{
		Name: name, Scope: scope, MacroID: req.MacroID, Description: req.Description,
		Fields: fields, Ports: ports, Script: req.Script, Note: strings.TrimSpace(req.Note),
		AuthorID: u.ID, AuthorName: displayName(u),
		Viewer: store.MacroViewer{User: u},
	}, ""
}

func (s *Server) handleCreateNodeDef(c *fiber.Ctx) error {
	u := currentUser(c)
	var req nodeDefRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	params, msg := nodeDefParams(req, u)
	if msg != "" {
		return fail(c, fiber.StatusBadRequest, "invalid_node", msg)
	}
	// 매크로 전용 노드는 그 매크로를 고칠 수 있는 사람만 만든다.
	// 전역 노드는 매크로 권한이 있으면 누구나 만들 수 있고, 만든 사람이 주인이 된다.
	if params.Scope == "macro" {
		if _, err := s.requireMacro(c, params.MacroID, model.MacroAccessEdit); err != nil {
			return err
		}
	}
	def, err := s.st.CreateNodeDef(c.Context(), params)
	if err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "macro.node.created", TargetType: "macro_node", TargetID: def.ID,
		Detail: map[string]any{"name": def.Name, "scope": def.Scope, "visibility": def.Visibility},
	})
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"nodeDef": def})
}

func (s *Server) handleUpdateNodeDef(c *fiber.Ctx) error {
	u := currentUser(c)
	existing, err := s.requireNodeDef(c, c.Params("defId"), model.MacroAccessEdit)
	if err != nil {
		return err
	}
	var req nodeDefRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	// 범위와 소속 매크로는 요청이 정하지 못한다. 전역 노드를 남의 매크로 전용으로
	// 옮기거나 그 반대를 허용하면 소유권이 요청 본문 한 줄로 바뀐다.
	req.Scope, req.MacroID = existing.Scope, existing.MacroID
	params, msg := nodeDefParams(req, u)
	if msg != "" {
		return fail(c, fiber.StatusBadRequest, "invalid_node", msg)
	}
	def, err := s.st.UpdateNodeDef(c.Context(), existing.ID, params)
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "노드를 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "macro.node.updated", TargetType: "macro_node", TargetID: def.ID,
		Detail: map[string]any{"name": def.Name, "version": def.CurrentVersion},
	})
	return c.JSON(fiber.Map{"nodeDef": def})
}

func (s *Server) handleListNodeDefVersions(c *fiber.Ctx) error {
	// 버전 목록에는 스크립트 전문이 들어 있다. 노드를 볼 수 있는 사람만 읽는다.
	if _, err := s.requireNodeDef(c, c.Params("defId"), model.MacroAccessView); err != nil {
		return err
	}
	versions, err := s.st.ListNodeDefVersions(c.Context(), c.Params("defId"))
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"items": versions})
}

// handleDeleteNodeDef는 노드 정의를 지운다.
// 범위별 삭제 조건은 저장 계층이 CanDelete로 계산해 둔다(store.scanNodeDef).
func (s *Server) handleDeleteNodeDef(c *fiber.Ctx) error {
	def, err := s.requireNodeDef(c, c.Params("defId"), model.MacroAccessView)
	if err != nil {
		return err
	}
	if !def.CanDelete {
		return fail(c, fiber.StatusForbidden, "forbidden", "이 노드를 삭제할 권한이 없습니다")
	}
	if err := s.st.DeleteNodeDef(c.Context(), def.ID); err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "macro.node.deleted", TargetType: "macro_node", TargetID: def.ID,
		Detail: map[string]any{"name": def.Name},
	})
	return c.JSON(fiber.Map{"ok": true})
}

// defaultGraph는 새 매크로의 초기 내용이다.
func defaultGraph() string {
	return `{"nodes":[{"id":"start","type":"start","label":"시작","x":80,"y":80,"params":{}}],` +
		`"edges":[],"params":[]}`
}
