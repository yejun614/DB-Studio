// Package bootstrap은 최초 실행 시 슈퍼 어드민 계정을 생성한다.
package bootstrap

import (
	"context"
	"errors"
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

// ResetPassword는 한 계정의 비밀번호를 새 랜덤 값으로 바꾼다.
//
// ── 무엇을 위한 것인가 ──────────────────────────────────────────────
// 부트스트랩 비밀번호는 딱 한 번 터미널에 찍히고 어디에도 남지 않는다
// (로그 수집기에 비밀번호가 들어가지 않게 일부러 그렇게 했다). 그 터미널을
// 닫았고 다른 관리자도 없으면 들어갈 길이 사라진다 — 해시는 argon2id 라
// 되돌릴 수 없다. 그때 쓰는 문이 이것이다.
//
// ── 왜 이것이 권한 우회가 아닌가 ────────────────────────────────────
// 이 함수를 부르려면 **데이터 디렉터리와 master.key 를 읽을 수 있어야** 한다.
// 그 둘을 가진 사람은 이미 저장된 모든 DB 자격증명을 풀 수 있으므로, 여기서
// 역할을 따져 막아도 얻는 것이 없다. 그래서 막는 대신 **남긴다** — 감사 기록에
// 누가(cli) 무엇을 했는지가 들어가고, 그 줄은 화면에서 보인다.
//
// 두 가지를 함께 한다.
//   - 변경 강제를 켠다. 새 비밀번호는 터미널에 찍히고 그 자리는 스크롤백과
//     세션 기록에 남는다. 첫 로그인에서 바꾸게 해야 그 값이 오래 살지 않는다.
//   - 그 사용자의 세션을 지운다. 남겨 두면 예전 비밀번호로 받은 쿠키가 그대로
//     살아 있어서, 비밀번호를 바꾼 의미가 없다(앱의 비밀번호 변경도 같이 한다).
func ResetPassword(ctx context.Context, st *store.Store, username string) (*Result, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		username = DefaultSuperadminUsername
	}
	u, err := st.GetUserByUsername(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		// 있는 아이디를 함께 알려 준다. 오타 하나로 실패했을 때 그것을 모르면
		// 사람은 데이터 디렉터리를 의심하고 엉뚱한 곳을 뒤진다.
		return nil, fmt.Errorf("%s 라는 계정이 없습니다%s", username, knownUsers(ctx, st))
	}
	if err != nil {
		return nil, fmt.Errorf("find user: %w", err)
	}

	password, err := crypto.GeneratePassword(24)
	if err != nil {
		return nil, fmt.Errorf("generate password: %w", err)
	}
	hash, err := crypto.HashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}
	if err := st.SetPassword(ctx, u.ID, hash, true); err != nil {
		return nil, fmt.Errorf("set password: %w", err)
	}
	if err := st.DeleteUserSessions(ctx, u.ID); err != nil {
		return nil, fmt.Errorf("delete sessions: %w", err)
	}

	// 감사 기록을 남기지 못해도 비밀번호는 이미 바뀌었다. 그 사실을 오류로
	// 올려 실패처럼 보이게 하면 사람은 다시 돌리고, 그러면 방금 받은
	// 비밀번호가 또 바뀐다.
	if err := st.Audit(ctx, store.AuditParams{
		ActorName:  "cli",
		Action:     store.ActionUserPasswordSet,
		TargetType: "user",
		TargetID:   u.ID,
		Detail: map[string]any{
			"username": u.Username, "role": u.Role,
			"via": "reset-password 명령", "sessionsRevoked": true,
		},
	}); err != nil {
		return &Result{Created: true, Username: u.Username, Password: password},
			fmt.Errorf("%w (비밀번호는 바뀌었습니다)", err)
	}
	return &Result{Created: true, Username: u.Username, Password: password}, nil
}

// knownUsers는 오류 메시지에 붙일 "있는 아이디" 목록이다.
func knownUsers(ctx context.Context, st *store.Store) string {
	users, err := st.ListUsers(ctx)
	if err != nil || len(users) == 0 {
		return ""
	}
	names := make([]string, 0, len(users))
	for _, u := range users {
		names = append(names, u.Username)
		if len(names) == 10 {
			names = append(names, "…")
			break
		}
	}
	return ". 있는 아이디: " + strings.Join(names, ", ")
}

// PrintCredentials는 생성된 자격증명을 터미널에 강조 출력한다.
// slog가 아닌 stdout에 직접 쓰는 이유는 구조화 로그 수집기에 비밀번호가
// 파싱되어 저장되는 것을 피하고, 사람이 바로 읽게 하려는 것이다.
func PrintCredentials(r *Result) {
	if r == nil || !r.Created {
		return
	}
	printBox(" 슈퍼 어드민 계정이 생성되었습니다 ", r)
}

// PrintResetCredentials는 다시 정한 비밀번호를 같은 모양으로 출력한다.
//
// 같은 박스를 쓰는 이유: 사람이 이 값을 읽어 옮기는 자리는 하나뿐이다.
// 모양이 다르면 어느 것이 아이디이고 어느 것이 비밀번호인지 다시 찾게 된다.
func PrintResetCredentials(r *Result) {
	if r == nil || !r.Created {
		return
	}
	printBox(" 비밀번호를 다시 정했습니다 ", r)
}

func printBox(title string, r *Result) {
	const width = 72
	line := strings.Repeat("═", width)

	fmt.Fprintf(os.Stdout, "\n╔%s╗\n", line)
	center(title, width)
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
