package api

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 여러 사람을 같은 권한으로 한 번에 등록한다.
//
// 팀이 들어오는 일은 한 명씩 오지 않는다. 다섯 명을 넣으려면 지금까지는 창을 다섯 번
// 열고, 그다음 권한 화면을 다섯 번 더 열어 같은 값을 다섯 번 적어야 했다. 같은 값을
// 손으로 다섯 번 적는 일에서는 반드시 한 번이 어긋나고, 어긋난 그 한 명은 "왜 나만
// 안 보이지"로 며칠 뒤에 발견된다.
//
// **권한을 이 창에서 새로 정하지 않는다.** 역할만 여기서 받고, 나머지는 "이 사람과
// 같게"로 받는다. 접근 범위·등급·데이터 능력·서버별 예외를 이 창에 다시 그리면 권한을
// 정하는 곳이 두 곳이 되고, 두 곳이 어긋나면 어느 쪽이 참인지 화면이 답하지 못한다.
// 참여 프로젝트만 예외로 함께 받는다 — 그것이 나머지 모두보다 앞선 관문이라, 비워
// 두면 계정을 만들어 놓고 아무것도 못 보는 상태가 되기 때문이다.

const maxBulkUsers = 200

type bulkUsersRequest struct {
	// Text는 한 줄에 한 사람이다: 아이디, 이름(선택), 이메일(선택).
	Text string `json:"text"`
	// Role은 모두에게 같이 적용된다. 역할은 계정의 신분이라 부여 권한과 따로 받는다.
	Role model.Role `json:"role"`
	// CopyFrom이 있으면 그 사용자의 접근 정책과 전역 권한을 그대로 복사한다.
	CopyFrom string `json:"copyFrom"`
	// Projects는 참여 프로젝트다. CopyFrom이 있어도 이 값이 이긴다 —
	// 화면이 보여 준 체크박스가 곧 저장되는 것이어야 한다.
	Projects []string `json:"projects"`
}

// bulkUserLine은 한 줄에서 읽어 낸 한 사람이다.
type bulkUserLine struct {
	Username    string
	DisplayName string
	Email       string
}

func (s *Server) handleBulkCreateUsers(c *fiber.Ctx) error {
	var req bulkUsersRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	if req.Role == "" {
		req.Role = model.RoleMember
	}
	if !req.Role.Valid() {
		return fail(c, fiber.StatusBadRequest, "invalid_role", "알 수 없는 역할입니다")
	}

	lines, invalid := parseBulkUsers(req.Text)
	if len(lines) == 0 && len(invalid) == 0 {
		return fail(c, fiber.StatusBadRequest, "no_users", "추가할 사용자를 한 줄에 하나씩 적으세요")
	}
	if len(lines) > maxBulkUsers {
		return fail(c, fiber.StatusBadRequest, "too_many",
			"한 번에 200명까지 넣을 수 있습니다")
	}

	// 권한 원본을 먼저 읽는다. 계정을 만든 뒤에 실패하면 권한 없는 계정만 남는다.
	policy, perms, err := s.bulkPolicy(c, &req)
	if err != nil {
		return err
	}

	actor := currentUser(c)
	created := []fiber.Map{}
	skipped := []string{}
	for _, line := range lines {
		password, gerr := crypto.GeneratePassword(20)
		if gerr != nil {
			return gerr
		}
		hash, herr := crypto.HashPassword(password)
		if herr != nil {
			return herr
		}
		u, cerr := s.st.CreateUser(c.Context(), store.CreateUserParams{
			Username: line.Username, Email: line.Email, DisplayName: line.DisplayName,
			Role: req.Role, PasswordHash: hash,
			// 관리자가 만든 계정은 언제나 첫 로그인에 비밀번호를 바꾼다.
			MustChangePassword: true,
			CreatedBy:          actor.ID,
		})
		if errors.Is(cerr, store.ErrDuplicateName) {
			// 멈추지 않는다. 목록의 절반이 이미 있는 것이 보통이고, 거기서 끊으면
			// 사람이 그 줄을 지우고 다시 붙여넣는 일을 반복하게 된다.
			skipped = append(skipped, line.Username)
			continue
		}
		if cerr != nil {
			return cerr
		}

		// 권한을 입힌다. 여기서 실패하면 계정은 남으므로 그 사실을 감사 로그가 알 수
		// 있도록 오류를 그대로 올린다 — 조용히 넘기면 권한 없는 계정이 생긴다.
		applied := *policy
		applied.UserID = u.ID
		if aerr := s.st.SetAccessPolicy(c.Context(), &applied); aerr != nil {
			return aerr
		}
		if perms != nil {
			if _, uerr := s.st.UpdateUser(c.Context(), u.ID, store.UpdateUserParams{Perms: perms}); uerr != nil {
				return uerr
			}
		}

		s.audit(c, store.AuditParams{
			Action: store.ActionUserCreated, TargetType: "user", TargetID: u.ID,
			Detail: map[string]any{
				"username": u.Username, "role": u.Role, "bulk": true,
				"copyFrom": req.CopyFrom, "projects": strings.Join(applied.Projects, ","),
			},
		})
		created = append(created, fiber.Map{
			// 임시 비밀번호는 이 응답에서 단 한 번만 나간다.
			"user": u, "temporaryPassword": password,
		})
	}

	s.audit(c, store.AuditParams{
		Action: "user.bulk", TargetType: "user", TargetID: "",
		Detail: map[string]any{
			"created": len(created), "skipped": len(skipped), "invalid": len(invalid),
			"role": req.Role, "copyFrom": req.CopyFrom,
		},
	})
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"created": created, "skipped": skipped, "invalid": invalid,
	})
}

// bulkPolicy는 모두에게 입힐 권한을 한 번 만든다.
//
// 원본이 있으면 그 사람의 것을 그대로 복사한다("김대리와 같은 권한으로"가 실제로
// 부탁받는 형태다). 없으면 새 계정의 기본값이고, 그때도 참여 프로젝트는 화면에서
// 고른 것을 넣는다.
func (s *Server) bulkPolicy(c *fiber.Ctx, req *bulkUsersRequest) (*model.AccessPolicy, *[]model.Perm, error) {
	var policy *model.AccessPolicy
	var perms *[]model.Perm

	if id := strings.TrimSpace(req.CopyFrom); id != "" {
		src, err := s.st.GetUser(c.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, fiber.NewError(fiber.StatusBadRequest, "권한 원본 사용자를 찾을 수 없습니다")
		}
		if err != nil {
			return nil, nil, err
		}
		policy, err = s.st.GetAccessPolicy(c.Context(), src.ID)
		if err != nil {
			return nil, nil, err
		}
		// 슈퍼 어드민의 정책은 비어 있다(역할로 통과하므로 적어 둘 것이 없다).
		// 그것을 복사해 놓고 "같은 권한"이라고 하면 아무 권한도 없는 계정이 생긴다.
		if src.Role == model.RoleSuperadmin {
			return nil, nil, fiber.NewError(fiber.StatusBadRequest,
				"슈퍼 어드민은 권한 원본이 될 수 없습니다. 그 권한은 역할에서 나오므로 복사할 것이 없습니다")
		}
		list := append([]model.Perm{}, src.Perms...)
		perms = &list
	} else {
		policy = &model.AccessPolicy{
			Mode:               model.AccessAllowlist,
			DefaultLevel:       model.LevelMonitor,
			Items:              []string{},
			Capabilities:       map[string]model.Level{},
			DefaultCaps:        []model.Capability{},
			CapOverrides:       map[string][]model.Capability{},
			ServerItems:        []string{},
			ServerCapabilities: map[string]model.Level{},
			ServerCapOverrides: map[string][]model.Capability{},
		}
	}

	// 참여 프로젝트는 화면이 보여 준 것이 곧 저장되는 것이어야 한다.
	// 실재하는 것만 남긴다 — 지워진 프로젝트의 아이디가 정책에 남으면 화면에는
	// 보이지 않으면서 저장될 때마다 따라다닌다.
	projects, err := s.st.ListProjects(c.Context(), "")
	if err != nil {
		return nil, nil, err
	}
	known := make(map[string]bool, len(projects))
	for _, p := range projects {
		known[p.ID] = true
	}
	joined := []string{}
	for _, id := range req.Projects {
		if !known[id] {
			return nil, nil, fiber.NewError(fiber.StatusBadRequest,
				"존재하지 않는 프로젝트가 포함되어 있습니다")
		}
		joined = append(joined, id)
	}
	policy.Projects = joined
	return policy, perms, nil
}

// parseBulkUsers는 붙여넣은 목록을 사람 단위로 읽는다.
//
// 쉼표·탭 어느 쪽이든 받는다. 엑셀에서 복사하면 탭이고 손으로 적으면 쉼표다 —
// 둘 중 하나만 받으면 그 사실을 사람이 알아내야 한다.
func parseBulkUsers(text string) ([]bulkUserLine, []string) {
	out := []bulkUserLine{}
	invalid := []string{}
	seen := map[string]bool{}

	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sep := ","
		if strings.Contains(line, "\t") {
			sep = "\t"
		}
		parts := strings.SplitN(line, sep, 3)
		one := bulkUserLine{Username: strings.TrimSpace(parts[0])}
		if len(parts) > 1 {
			one.DisplayName = strings.TrimSpace(parts[1])
		}
		if len(parts) > 2 {
			one.Email = strings.TrimSpace(parts[2])
		}
		if !usernamePattern.MatchString(one.Username) {
			invalid = append(invalid, line)
			continue
		}
		// 같은 줄이 두 번 있으면 두 번째는 어차피 중복으로 걸린다. 그때
		// "이미 있는 아이디"로 보고되면 방금 만든 것인지 원래 있던 것인지
		// 구분할 수 없으므로, 여기서 미리 접는다.
		key := strings.ToLower(one.Username)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, one)
	}
	return out, invalid
}
