package api

import (
	"strings"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 자기 프로필 수정.
//
// 사용자 관리 API(`PATCH /users/:id`)와 일부러 분리했다. 그쪽은 슈퍼 어드민만 쓸 수
// 있고 역할·상태까지 바꾼다. 여기서 바꿀 수 있는 것은 **표시에 관한 것뿐**이다:
// 표시 이름, 이메일, 아이콘. 권한에 영향을 주는 필드는 요청에 들어와도 무시되는
// 것이 아니라 애초에 구조체에 없다 — 있으면 언젠가 연결된다.
type updateProfileRequest struct {
	DisplayName *string `json:"displayName"`
	Email       *string `json:"email"`
	Avatar      *string `json:"avatar"`
}

const (
	maxDisplayNameLen = 60
	maxEmailLen       = 254 // RFC 5321의 경로 길이 상한
)

// profileParams는 요청을 검증해 저장 파라미터로 바꾼다.
// code가 빈 문자열이 아니면 거부이며, 그때 message를 그대로 사용자에게 보여준다.
// 핸들러에서 떼어낸 이유는 검증 규칙을 서버 없이 시험할 수 있어야 하기 때문이다.
func profileParams(req updateProfileRequest) (p store.UpdateUserParams, code, message string) {
	if req.DisplayName != nil {
		name := strings.TrimSpace(*req.DisplayName)
		if name == "" {
			return p, "invalid_display_name", "이름을 입력하세요"
		}
		if len([]rune(name)) > maxDisplayNameLen {
			return p, "invalid_display_name", "이름은 60자 이내로 입력하세요"
		}
		p.DisplayName = &name
	}

	if req.Email != nil {
		email := strings.TrimSpace(*req.Email)
		// 빈 값은 허용한다(이메일은 이 앱에서 표시용이다). 형식 검사는 최소한만 한다 —
		// 정규식으로 유효한 주소를 걸러내려는 시도는 늘 멀쩡한 주소를 거부하는 쪽으로 끝난다.
		if email != "" {
			if len(email) > maxEmailLen || strings.ContainsAny(email, " \t\r\n") ||
				strings.Count(email, "@") != 1 || strings.HasPrefix(email, "@") ||
				strings.HasSuffix(email, "@") {
				return p, "invalid_email", "이메일 형식이 올바르지 않습니다"
			}
		}
		p.Email = &email
	}

	if req.Avatar != nil {
		avatar := strings.TrimSpace(*req.Avatar)
		if !model.ValidAvatar(avatar) {
			return p, "invalid_avatar", "알 수 없는 아이콘입니다"
		}
		p.Avatar = &avatar
	}

	if p.DisplayName == nil && p.Email == nil && p.Avatar == nil {
		return p, "no_changes", "변경할 내용이 없습니다"
	}
	return p, "", ""
}

func (s *Server) handleUpdateProfile(c *fiber.Ctx) error {
	u := currentUser(c)

	var req updateProfileRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}

	params, code, message := profileParams(req)
	if code != "" {
		return fail(c, fiber.StatusBadRequest, code, message)
	}

	updated, err := s.st.UpdateUser(c.Context(), u.ID, params)
	if err != nil {
		return err
	}

	// 감사 로그에는 무엇을 바꿨는지만 남긴다. 프로필은 민감 정보가 아니지만
	// "누가 언제 표시 이름을 바꿨는지"는 다른 사람 눈에 이름이 달라 보이는 이유가 된다.
	detail := map[string]any{"fields": changedProfileFields(params)}
	s.audit(c, store.AuditParams{
		Action:     store.ActionProfileUpdated,
		TargetType: "user",
		TargetID:   u.ID,
		Detail:     detail,
	})

	return c.JSON(fiber.Map{"ok": true, "user": updated})
}

func changedProfileFields(p store.UpdateUserParams) []string {
	out := []string{}
	if p.DisplayName != nil {
		out = append(out, "displayName")
	}
	if p.Email != nil {
		out = append(out, "email")
	}
	if p.Avatar != nil {
		out = append(out, "avatar")
	}
	return out
}
