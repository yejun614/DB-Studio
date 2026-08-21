// Package bootstrap은 최초 실행 시 슈퍼 어드민 계정을 생성한다.
package bootstrap

import (
	"context"
	"fmt"
	"os"
	"strings"

	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// DefaultSuperadminUsername은 부트스트랩으로 만드는 계정의 아이디다.
const DefaultSuperadminUsername = "superadmin"

// Result는 부트스트랩 결과다. Created가 false면 아무것도 하지 않았다는 뜻이다.
type Result struct {
	Created  bool
	Username string
	Password string
}

// EnsureSuperadmin은 사용자가 한 명도 없을 때만 슈퍼 어드민을 생성한다.
// 비밀번호는 랜덤 생성되며 이 함수의 반환값으로 단 한 번 노출된다.
func EnsureSuperadmin(ctx context.Context, st *store.Store) (*Result, error) {
	n, err := st.CountUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("count users: %w", err)
	}
	if n > 0 {
		return &Result{Created: false}, nil
	}

	password, err := crypto.GeneratePassword(24)
	if err != nil {
		return nil, fmt.Errorf("generate password: %w", err)
	}
	hash, err := crypto.HashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	u, err := st.CreateUser(ctx, store.CreateUserParams{
		Username:    DefaultSuperadminUsername,
		DisplayName: "슈퍼 어드민",
		Role:        model.RoleSuperadmin,
		// 최초 계정도 첫 로그인 시 비밀번호 변경을 강제한다.
		// 터미널 출력이 로그 파일이나 스크롤백에 남을 수 있기 때문이다.
		MustChangePassword: true,
		PasswordHash:       hash,
	})
	if err != nil {
		return nil, fmt.Errorf("create superadmin: %w", err)
	}

	if err := st.Audit(ctx, store.AuditParams{
		ActorName:  "system",
		Action:     store.ActionBootstrap,
		TargetType: "user",
		TargetID:   u.ID,
		Detail:     map[string]any{"username": u.Username, "reason": "no users existed"},
	}); err != nil {
		return nil, fmt.Errorf("audit bootstrap: %w", err)
	}

	return &Result{Created: true, Username: u.Username, Password: password}, nil
}

// PrintCredentials는 생성된 자격증명을 터미널에 강조 출력한다.
// slog가 아닌 stdout에 직접 쓰는 이유는 구조화 로그 수집기에 비밀번호가
// 파싱되어 저장되는 것을 피하고, 사람이 바로 읽게 하려는 것이다.
func PrintCredentials(r *Result) {
	if r == nil || !r.Created {
		return
	}
	const width = 72
	line := strings.Repeat("═", width)

	fmt.Fprintf(os.Stdout, "\n╔%s╗\n", line)
	center(" 슈퍼 어드민 계정이 생성되었습니다 ", width)
	fmt.Fprintf(os.Stdout, "╠%s╣\n", line)
	field("아이디", r.Username, width)
	field("비밀번호", r.Password, width)
	fmt.Fprintf(os.Stdout, "╠%s╣\n", line)
	center(" 이 비밀번호는 다시 표시되지 않습니다. 지금 안전한 곳에 보관하세요. ", width)
	center(" 첫 로그인 시 비밀번호 변경이 요구됩니다. ", width)
	fmt.Fprintf(os.Stdout, "╚%s╝\n\n", line)
}

func field(label, value string, width int) {
	// fmt의 %-10s는 룬 수로 폭을 맞추므로 한글(전각) 라벨이 어긋난다.
	// 표시 폭 기준으로 직접 채워 정렬을 맞춘다.
	const labelWidth = 12
	padded := label
	if w := runeWidth(label); w < labelWidth {
		padded += strings.Repeat(" ", labelWidth-w)
	}
	fmt.Fprintf(os.Stdout, "║%s║\n", pad("  "+padded+": "+value, width))
}

func center(text string, width int) {
	w := runeWidth(text)
	if w >= width {
		fmt.Fprintf(os.Stdout, "║%s║\n", text)
		return
	}
	left := (width - w) / 2
	right := width - w - left
	fmt.Fprintf(os.Stdout, "║%s%s%s║\n",
		strings.Repeat(" ", left), text, strings.Repeat(" ", right))
}

func pad(text string, width int) string {
	w := runeWidth(text)
	if w >= width {
		return text
	}
	return text + strings.Repeat(" ", width-w)
}

// runeWidth는 한글(전각) 문자를 2칸으로 계산해 박스 정렬을 맞춘다.
func runeWidth(s string) int {
	w := 0
	for _, r := range s {
		if isWide(r) {
			w += 2
		} else {
			w++
		}
	}
	return w
}

func isWide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, // 한글 자모
		r >= 0x2E80 && r <= 0xA4CF, // CJK
		r >= 0xAC00 && r <= 0xD7A3, // 한글 음절
		r >= 0xF900 && r <= 0xFAFF,
		r >= 0xFF00 && r <= 0xFF60,
		r >= 0xFFE0 && r <= 0xFFE6:
		return true
	}
	return false
}
