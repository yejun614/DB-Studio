package api

import (
	"fmt"
	"strings"

	"dbstudio/internal/erd"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
)

// 아직 없는 데이터베이스를 대상으로 삼은 초안의 스크립트 앞머리.

// prependTargetDatabase는 계획 맨 앞에 CREATE DATABASE 를 붙인다.
//
// 설계할 때 만들지 않고 여기서 만드는 이유: 초안은 지우고 다시 그리는 물건이라,
// 그릴 때마다 서버에 빈 데이터베이스가 쌓이면 아무도 그것을 치우지 않는다. 그래서
// "만들 계획"만 문서에 적어 두고, 실제로 만드는 것은 이 스크립트가 실행되는 순간이다.
//
// 되돌리기(down)에는 DROP DATABASE 를 넣지 않는다. 마이그레이션을 되돌리는 것과
// 데이터베이스를 통째로 지우는 것은 사람이 기대하는 크기가 다르다 — 되돌릴 수 없는
// 항목으로 적어 두고 사람이 판단하게 한다.
func prependTargetDatabase(plan *schema.Plan, doc *erd.Document) {
	if plan == nil || doc == nil || doc.TargetDB == nil {
		return
	}
	create := doc.CreateDatabaseSQL()
	if create == "" {
		return
	}
	name := doc.TargetDB.Name
	head := []schema.Statement{{
		SQL: create, Kind: schema.CreateTable, Object: name,
		Note: fmt.Sprintf("%s 데이터베이스를 만듭니다 (설계 단계에서는 만들지 않았습니다)", name),
	}}
	if use := doc.UseDatabaseSQL(); use != "" {
		head = append(head, schema.Statement{
			SQL: use, Kind: schema.CreateTable, Object: name,
			Note: "이후 문장은 이 데이터베이스 안에서 실행됩니다",
		})
	} else {
		// 한 세션 안에서 옮겨 갈 수 없는 DB 다. 이 사실을 적지 않으면 나머지
		// 문장이 접속해 있던 데이터베이스에 표를 만들어 놓고 성공했다고 보고한다.
		plan.Warnings = append(plan.Warnings, fmt.Sprintf(
			"%s 에서는 한 접속 안에서 데이터베이스를 옮겨 갈 수 없습니다. "+
				"첫 문장(CREATE DATABASE %s)을 먼저 실행하고, %s 에 새로 접속해서 나머지를 실행하세요",
			plan.Dialect, name, name))
	}
	plan.Up = append(head, plan.Up...)
	plan.Irreversible = append(plan.Irreversible,
		fmt.Sprintf("%s 데이터베이스 만들기 — 되돌리기에 DROP DATABASE 를 넣지 않았습니다. "+
			"지우려면 직접 실행하세요", name))
}

// targetDatabaseWarning은 마이그레이션 대상이 초안이 말하는 데이터베이스와 다를 때의 경고다.
//
// 커넥션은 이미 있는 데이터베이스에 붙는다. 초안이 "새 DB 를 만들겠다"고 적어 두었는데
// 커넥션이 다른 데이터베이스를 가리키면, 계획은 아무 불평 없이 **그 다른 곳에** 표를
// 만든다. 실행이 끝난 뒤에야 "왜 저기 생겼지"가 되는 종류의 어긋남이다.
func targetDatabaseWarning(doc *erd.Document, conn *model.Connection) string {
	if doc == nil || doc.TargetDB == nil || conn == nil {
		return ""
	}
	want := strings.TrimSpace(doc.TargetDB.Name)
	got := strings.TrimSpace(conn.DatabaseName)
	if want == "" || got == "" || strings.EqualFold(want, got) {
		return ""
	}
	return fmt.Sprintf(
		"이 초안은 %s 데이터베이스를 새로 만들 계획인데, 커넥션 %s 는 %s 를 가리킵니다. "+
			"이대로 실행하면 표가 %s 에 만들어집니다", want, conn.Name, got, got)
}
