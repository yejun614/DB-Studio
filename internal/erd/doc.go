// Package erd은 ERD 초안 문서와 그 문서에 적용되는 편집 연산(op)을 정의한다.
//
// 동기화 방식은 서버 권위(server-authoritative) op-log다. 클라이언트는 op를 보내고,
// 서버가 검증 후 순번(seq)을 부여해 모든 구독자에게 브로드캐스트한다. 같은 문서의
// op는 서버에서 완전히 직렬화되므로 순서가 하나뿐이고, 그 순서대로 적용하면
// 모든 참여자가 같은 상태에 도달한다. CRDT는 이 도메인(도형·필드 편집)에 과하다.
//
// 충돌 해결은 필드 단위 LWW(last-write-wins)다. 그래서 갱신 op는 전체 교체가 아니라
// **패치**여야 한다 — 없는 필드는 "변경하지 않음"을 뜻한다. 두 사람이 같은 컬럼의
// 서로 다른 속성(한 명은 타입, 한 명은 주석)을 동시에 고쳤을 때 전체 교체 방식이면
// 나중 op가 앞의 편집을 되돌려버린다.
package erd

import (
	"sort"
	"strings"

	"dbstudio/internal/schema"
)

// Document는 ERD 초안 문서 하나다.
//
// 구조 정보는 schema.Schema를 그대로 쓴다. 그래서 ERD로 그린 것과 실제 DB에서 읽은
// 것을 같은 diff 함수로 비교할 수 있고(P7 마이그레이션의 전제), 별도 변환 계층이 없다.
type Document struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// ProjectID는 이 문서가 속한 프로젝트다.
	//
	// 대상 커넥션이 있으면 그 커넥션의 프로젝트와 같다(저장 계층이 커넥션 쪽을
	// 참으로 삼아 맞춘다). 독립 초안에는 커넥션이 없으므로 이 값이 유일한 근거다 —
	// 설계는 DB를 만들기 전에 시작되는 일이 더 많아서, 독립 초안이야말로
	// 프로젝트가 필요한 쪽이다.
	ProjectID string `json:"projectId"`
	// ConnectionID는 이 초안이 향하는 대상 커넥션이다. dialect 결정과 권한 판정에 쓴다.
	// 비어 있을 수 없다 — 어떤 DB를 위한 설계인지 모르면 타입도 마이그레이션도 정할 수 없다.
	ConnectionID string `json:"connectionId"`
	Dialect      string `json:"dialect"`
	Status       string `json:"status"`
	// Kind는 이 문서가 무엇인가다: "draft"는 만들고 싶은 것을 그리는 초안,
	// "structure"는 실제 DB의 지금 모습을 함께 보는 구조 문서다. 구조 문서에서는
	// 스키마 레이어가 읽기 전용이라 서버가 op 종류를 제한한다.
	Kind string `json:"kind,omitempty"`

	Schema *schema.Schema `json:"schema"`

	// Layout은 테이블 키 → 캔버스 위치다. 스키마 IR에 좌표를 넣지 않는 이유가 중요하다:
	// IR은 구조를 표현하고 그 지문(Fingerprint)이 드리프트 감지와 버전 비교의 기준이다.
	// 좌표가 IR에 들어가면 테이블을 옮기기만 해도 "스키마가 바뀌었다"가 되어버린다.
	Layout map[string]*Box `json:"layout"`
	Notes  []*Note         `json:"notes"`
	Groups []*Group        `json:"groups,omitempty"`

	// Domains는 이 설계에서 재사용하는 타입 정의다.
	//
	// IR(schema.Schema)이 아니라 문서에 두는 이유: 도메인은 **설계의 어휘**이지
	// 대부분의 DB에 실제로 만들어지는 물건이 아니다(PostgreSQL의 CREATE DOMAIN을 빼면
	// 대응하는 개념이 없다). IR에 넣으면 지문이 바뀌어, 도메인을 정리했을 뿐인데
	// 대상 DB가 "구조가 달라졌다"고 보고된다. 컬럼에는 도메인의 **결과**(구체 타입)가
	// 들어가므로 마이그레이션은 도메인을 몰라도 된다.
	Domains []*Domain `json:"domains,omitempty"`

	// TableDefaults는 이 초안에서 **새로 만드는 표**가 물려받을 저장 설정이다
	// (MySQL 의 엔진·문자셋, PostgreSQL 의 테이블스페이스 등).
	//
	// 표마다 따로 적지 않고 여기 두는 이유: 한 초안 안에서 표마다 엔진이 다른 일은
	// 거의 없다. 그런데 표를 만들 때마다 같은 값을 다시 고르게 하면 반드시 몇 개는
	// 빠지고, 빠진 표는 서버 기본값으로 만들어져 나중에야 드러난다.
	//
	// **물려받기는 만들 때 한 번뿐이다.** 이 값을 고쳐도 이미 있는 표는 그대로다 —
	// 표에 일부러 다르게 적어 둔 값을 조용히 지우지 않기 위해서다. 모두에 적용하는
	// 것은 문서 설정에서 따로 고른다.
	TableDefaults map[string]string `json:"tableDefaults,omitempty"`

	// TargetDB는 아직 만들지 않은 데이터베이스를 대상으로 삼을 때의 계획이다.
	TargetDB *TargetDB `json:"targetDb,omitempty"`

	// Seq는 이 문서에 적용된 마지막 op의 순번이다. 클라이언트는 이 값을 기준으로
	// 자기가 어디까지 봤는지 판단하고, 재접속 시 이후 op만 받는다.
	Seq int64 `json:"seq"`
}

// 문서 상태. 리뷰/승인 워크플로(P7)가 이 값을 확장한다.
const (
	StatusDraft    = "draft"
	StatusInReview = "in_review"
	StatusApplied  = "applied"
	StatusArchived = "archived"
)

// Box는 캔버스에서 테이블 카드가 놓인 위치와 크기다.
type Box struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	// W는 카드의 가로 폭이다. 0이면 화면의 기본값(260)을 쓴다.
	//
	// 이름이 긴 표가 하나 있다고 모든 카드를 넓힐 이유가 없어 카드마다 따로 둔다.
	// 높이는 두지 않는다 — 그것은 컬럼 수로 정해지는 값이라 사람이 정할 것이 아니다.
	W float64 `json:"w,omitempty"`
	// Collapsed면 컬럼 목록을 접어 제목만 보여준다. 테이블이 많은 ERD에서 필요하다.
	Collapsed bool `json:"collapsed,omitempty"`
	// Color는 사용자가 지정한 강조색(그룹 구분용)이다. 비어 있으면 기본 색.
	Color string `json:"color,omitempty"`
	// Icon은 카드 제목 왼쪽에 붙는 표식이다(users, key, activity …).
	//
	// 스키마가 아니라 레이아웃에 두는 이유: 이것은 "이 테이블이 무엇인가"에 대한
	// 사람의 메모이지 DB에 만들어질 무엇이 아니다. 구조 지문에 들어가면 아이콘만
	// 바꿔도 드리프트로 잡힌다.
	Icon string `json:"icon,omitempty"`
	// ColumnIcons는 컬럼 이름(소문자) → 아이콘이다. 비어 있는 컬럼은 화면이 타입과
	// 키 여부를 보고 알아서 고른다.
	//
	// 여기(레이아웃)에 두는 이유는 Icon과 같다: 이것은 "이 컬럼이 무엇인가"에 대한
	// 사람의 메모이지 DB에 만들어질 무엇이 아니다. 구조 지문에 들어가면 아이콘만
	// 바꿔도 드리프트로 잡힌다.
	ColumnIcons map[string]string `json:"columnIcons,omitempty"`
	// Logical은 이 테이블의 논리명이다("회원", "주문 상세").
	//
	// 물리명(schema.Table.Name)은 DB에 실제로 만들어지는 이름이고, 논리명은 그것이
	// 무엇을 뜻하는지를 사람 말로 적은 것이다. 설계 회의에서는 논리명으로 이야기하고
	// 코드에서는 물리명으로 쓴다 — 둘 다 있어야 그 사이를 오갈 수 있다.
	//
	// 레이아웃에 두는 이유는 Icon과 같다. 논리명은 DB에 만들어지는 무엇이 아니므로
	// 구조 지문에 들어가서는 안 된다 — 들어가면 이름을 한국어로 적는 순간 대상 DB와
	// 다르다고(드리프트) 잡힌다. 주석(Comment)과도 다르다: 주석은 DB에 COMMENT로
	// 실려 가는 설명이고, 논리명은 그 이름 자체다.
	Logical string `json:"logical,omitempty"`
	// ColumnLogical은 컬럼 이름(소문자) → 논리명이다.
	ColumnLogical map[string]string `json:"columnLogical,omitempty"`
}

// Group은 캔버스에 놓는 반투명 사각형이다. 테이블 몇 개를 묶어 보이게 한다.
//
// 메모와 마찬가지로 구조가 아니므로 IR이 아니라 문서에 담는다. 어떤 테이블이
// 이 그룹에 속하는지 데이터로 관리하지 않는 이유: 테이블을 옮길 때마다 소속이
// 바뀌면 오히려 성가시고, 겹쳐 놓는 것만으로 묶여 보이면 충분하다.
type Group struct {
	ID    string  `json:"id"`
	Label string  `json:"label"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	W     float64 `json:"w"`
	H     float64 `json:"h"`
	Color string  `json:"color,omitempty"`
}

// Domain은 재사용하는 타입 정의다("이메일은 VARCHAR(320), 금액은 DECIMAL(13,2)").
//
// 왜 필요한가: 같은 뜻의 컬럼이 표마다 다른 타입으로 만들어지는 것은 설계 단계에서
// 가장 흔한 어긋남이다. 이름을 붙여 한 번 정해 두면 그 뒤로는 이름으로 고르고,
// 정의를 고치면 그 이름을 쓰는 컬럼이 함께 바뀐다.
//
// 컬럼에는 도메인 이름과 **그 결과인 구체 타입이 함께** 남는다. 이름만 남기면
// 도메인을 지우는 순간 컬럼이 타입을 잃고, DDL을 만들 때 그 컬럼만 빈칸이 된다.
type Domain struct {
	Name string `json:"name"`
	// Type은 대상 DB의 타입 문자열이다(VARCHAR(320) 같은).
	Type string `json:"type"`
	// Nullable은 이 도메인을 쓰는 컬럼의 NULL 허용 여부다.
	// nil이면 도메인이 관여하지 않는다 — 컬럼마다 다르게 두고 싶은 경우가 많다.
	Nullable *bool `json:"nullable,omitempty"`
	// Default는 기본값 식이다. 비어 있으면 도메인이 관여하지 않는다.
	Default string `json:"default,omitempty"`
	Comment string `json:"comment,omitempty"`
}

// Key는 대소문자를 무시한 식별 키다. 타입 이름과 같은 규칙을 쓴다.
func (d *Domain) Key() string { return strings.ToLower(strings.TrimSpace(d.Name)) }

// Note는 캔버스에 붙이는 메모다. 스키마 구조가 아니므로 IR이 아니라 문서에 담는다.
type Note struct {
	ID    string  `json:"id"`
	Text  string  `json:"text"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Color string  `json:"color,omitempty"`
	// W·H는 메모 상자의 크기다. 0이면 화면이 기본값으로 그린다.
	//
	// 크기를 사람이 정하게 두는 이유는 그룹과 같다: 글의 양에 맞춰 자동으로
	// 늘리면 메모 하나가 캔버스를 가로지르는 일이 생기고, 그것을 막으려면
	// "얼마나 길어야 줄을 바꾸는가"를 또 정해야 한다.
	W float64 `json:"w,omitempty"`
	H float64 `json:"h,omitempty"`
}

// NewDocument는 빈 초안을 만든다.
func NewDocument(id, name, connectionID, dialect string) *Document {
	return &Document{
		ID: id, Name: name, ConnectionID: connectionID, Dialect: dialect,
		Status: StatusDraft,
		Schema: &schema.Schema{
			Dialect: dialect, Shape: schema.ShapeRelational,
			Name: name, Tables: []*schema.Table{}, Views: []*schema.View{},
		},
		Layout: map[string]*Box{},
		Notes:  []*Note{},
	}
}

// FromSchema는 실제 DB에서 읽은 스키마를 초안의 출발점으로 삼는다.
// 좌표는 관계를 따라 자동 배치한다(AutoLayout) — 빈 캔버스에 테이블이 겹쳐 쌓여
// 있으면 쓸 수 없고, 격자로 나열하면 관계선이 도면을 가로지른다.
func FromSchema(id, name, connectionID string, sc *schema.Schema) *Document {
	doc := NewDocument(id, name, connectionID, sc.Dialect)
	doc.Schema = sc
	doc.Schema.Sort()
	doc.Layout = AutoLayout(sc)
	return doc
}

// 자동 배치 격자. 카드 폭/높이는 프론트엔드와 맞춰야 겹치지 않는다.
const (
	layoutColumns = 4
	// 한 열에 쌓는 카드 수의 한계. 넘으면 옆 칸을 하나 더 쓴다 — 세로로 길어진
	// 도면은 확대해서 보든 줄여서 보든 한 화면에 들어오지 않는다.
	layoutRowsMax = 6
	layoutStepX   = 320.0
	layoutStepY   = 260.0
	layoutOriginX = 80.0
	layoutOriginY = 80.0

	// 카드 폭의 한계. 화면(erdcanvas.js)도 같은 값으로 막지만, 여기서 한 번 더
	// 자른다 — 화면을 거치지 않는 길(AI 툴·API·다른 클라이언트)로 들어온 값이
	// 문서에 남으면 그 카드는 아무도 읽을 수 없는 폭이 된다.
	cardMinW = 180.0
	cardMaxW = 720.0

	// 카드 치수. web/js/core/erdcanvas.js 의 상수와 같아야 한다.
	//
	// 두 벌로 두는 것이 마음에 걸리지만, 서버가 좌표를 정하려면 카드가 얼마나
	// 높은지 알아야 하고 그 값은 그리는 쪽에 있다. 어긋나면 초기 배치에서 카드가
	// 겹치므로, 한쪽을 고칠 때 다른 쪽도 고쳐야 한다는 것을 여기 적어 둔다.
	cardHeadH = 30.0
	cardRowH  = 20.0
	cardPadH  = 8.0
	// 카드 사이에 남기는 세로 여백.
	cardGapY = 40.0
)

// clampCardWidth는 카드 폭을 읽을 수 있는 범위로 자른다.
//
// 0은 "정하지 않음"이라 그대로 둔다 — 화면이 기본값을 쓴다.
func clampCardWidth(w float64) float64 {
	if w <= 0 {
		return 0
	}
	if w < cardMinW {
		return cardMinW
	}
	if w > cardMaxW {
		return cardMaxW
	}
	return w
}

// CardHeight는 컬럼 n개짜리 카드의 높이다.
//
// 컬럼을 전부 그리기로 하면서(접지 않는다) 카드 높이가 표마다 크게 달라졌다.
// 고정 격자에 놓으면 컬럼이 열두 개만 넘어도 아래 줄 카드를 덮는다.
func CardHeight(columns int) float64 {
	if columns < 0 {
		columns = 0
	}
	return cardHeadH + float64(columns)*cardRowH + cardPadH
}

// SlotAt은 n번째 격자점의 좌표다.
//
// 밖으로 노출하는 이유: 구조 화면도 좌표가 없는 테이블을 같은 격자에 놓아야 한다.
// 두 화면의 초기 배치가 다르면 같은 DB가 화면마다 다른 모양으로 보인다.
func SlotAt(n int) (float64, float64) {
	return layoutOriginX + float64(n%layoutColumns)*layoutStepX,
		layoutOriginY + float64(n/layoutColumns)*layoutStepY
}

// AutoLayout은 테이블을 격자에 배치한다.
//
// 참조 관계를 고려한 배치(force-directed 등)를 서버에서 하지 않는 이유: 좌표는
// 사용자가 옮기면 그 값이 정답이 되고, 서버가 다시 계산할 근거가 없다. 초기 배치는
// "겹치지 않고 예측 가능하게" 만으로 충분하며, 정렬은 사용자가 캔버스에서 한다.
//
// 세로는 격자가 아니라 **쌓기**다. 컬럼을 전부 그리므로 카드 높이가 표마다 다르고,
// 고정 간격으로 놓으면 컬럼 많은 표가 아래 카드를 덮는다. 가로는 그대로 격자다 —
// 폭은 모든 카드가 같아서 어긋날 일이 없고, 열이 가지런해야 눈이 따라간다.
func AutoLayout(sc *schema.Schema) map[string]*Box {
	out := make(map[string]*Box, len(sc.Tables))
	if sc == nil {
		return out
	}
	if len(sc.Tables) == 0 {
		placeViews(sc, out, 0)
		return out
	}

	byKey := make(map[string]*schema.Table, len(sc.Tables))
	for _, t := range sc.Tables {
		byKey[t.Key()] = t
	}
	// refs는 이 표가 **가리키는** 표들이다(외래키 방향). 같은 표를 두 번 가리켜도
	// 한 번만 센다 — 열을 정하는 데 필요한 것은 "누구를 가리키는가"뿐이다.
	refs := make(map[string][]string, len(sc.Tables))
	for _, t := range sc.Tables {
		seen := map[string]bool{}
		for _, fk := range t.ForeignKeys {
			key := refKeyOf(t, fk)
			if key == "" || key == t.Key() || seen[key] {
				continue // 자기 참조는 열을 밀어낼 이유가 없다.
			}
			if _, ok := byKey[key]; !ok {
				continue // 문서 밖의 표를 가리키는 외래키(불러오기 중간 상태).
			}
			seen[key] = true
			refs[t.Key()] = append(refs[t.Key()], key)
		}
	}

	depth := layoutDepths(sc, refs)
	// 열(depth) 안에서의 순서는 **가리키는 표들의 자리**를 따른다. 부모가 위쪽에
	// 있는 자식이 위쪽에 서면 선이 덜 엉킨다. 부모가 없으면 이름 순이다(같은
	// 스키마를 두 번 불러오면 같은 그림이 나와야 한다 — sc.Tables 는 이미 정렬돼 있다).
	byDepth := map[int][]*schema.Table{}
	maxDepth := 0
	for _, t := range sc.Tables {
		d := depth[t.Key()]
		byDepth[d] = append(byDepth[d], t)
		if d > maxDepth {
			maxDepth = d
		}
	}

	// 열마다 쓸 가로 칸 수를 먼저 정한다.
	//
	// 한 열에 스무 개가 쌓이면 세로로 5000px 이 되어 화면에 담기지 않는다. 관계가
	// 없는 스키마에서는 모든 표가 0열에 몰리므로 특히 그렇다. 그래서 한 열이
	// layoutRowsMax 를 넘으면 옆으로 한 칸 더 쓰고, 다음 열들을 그만큼 밀어 둔다 —
	// 밀지 않으면 다음 열이 이 열의 두 번째 칸과 겹친다.
	xSlot := make([]int, maxDepth+1)
	cursor := 0
	for d := 0; d <= maxDepth; d++ {
		xSlot[d] = cursor
		sub := (len(byDepth[d]) + layoutRowsMax - 1) / layoutRowsMax
		if sub < 1 {
			sub = 1
		}
		cursor += sub
	}

	// 열마다 위에서 아래로 쌓는다. 카드 높이가 컬럼 수마다 다르므로 실제 높이로 센다.
	rowOf := make(map[string]int, len(sc.Tables))
	for d := 0; d <= maxDepth; d++ {
		list := byDepth[d]
		if d > 0 {
			sortTablesByParentRow(list, refs, rowOf)
		}
		// 나눠 쓰는 칸마다 다음 카드가 놓일 y 를 따로 들고 간다.
		ys := map[int]float64{}
		for i, t := range list {
			sub := i / layoutRowsMax
			if _, ok := ys[sub]; !ok {
				ys[sub] = layoutOriginY
			}
			out[t.Key()] = &Box{
				X: layoutOriginX + float64(xSlot[d]+sub)*layoutStepX,
				Y: ys[sub],
			}
			rowOf[t.Key()] = i
			step := CardHeight(len(t.Columns)) + cardGapY
			if step < layoutStepY {
				step = layoutStepY
			}
			ys[sub] += step
		}
	}
	placeViews(sc, out, cursor)
	return out
}

// placeViews는 뷰 카드를 표 오른쪽의 새 열에 세운다.
//
// 표와 섞지 않는 이유: 뷰는 표를 읽는 것이라 도면에서 늘 "끝"이다. 표들 사이에
// 끼워 넣으면 관계선이 그 위를 지나가고, 무엇이 실체이고 무엇이 그것을 읽는
// 것인지가 그림에서 사라진다. 열 하나에 여섯 개씩 쌓는 것은 표와 같은 규칙이다.
func placeViews(sc *schema.Schema, out map[string]*Box, col int) {
	for i, v := range sc.Views {
		sub := i / layoutRowsMax
		row := i % layoutRowsMax
		out[v.Key()] = &Box{
			X: layoutOriginX + float64(col+sub)*layoutStepX,
			Y: layoutOriginY + float64(row)*layoutStepY,
		}
	}
}

// refKeyOf는 외래키가 가리키는 표의 키다(스키마가 비어 있으면 이 표의 스키마).
func refKeyOf(t *schema.Table, fk *schema.ForeignKey) string {
	if fk == nil || fk.RefTable == "" {
		return ""
	}
	ns := fk.RefNamespace
	if ns == "" {
		ns = t.Namespace
	}
	if ns == "" {
		return strings.ToLower(fk.RefTable)
	}
	return strings.ToLower(ns + "." + fk.RefTable)
}

// layoutDepths는 표마다 열 번호를 정한다.
//
// 규칙: **아무것도 가리키지 않는 표가 0열**이고, 어떤 표의 열은 그것이 가리키는
// 표들보다 한 칸 오른쪽이다. 그러면 왼쪽에서 오른쪽으로 "참조되는 쪽 → 참조하는 쪽"
// 순서가 되어, 회원 → 주문 → 주문상세 처럼 읽는 순서 그대로 늘어선다.
//
// 순환이 있으면(A→B→A) 그 관계는 열을 정할 수 없다. 그때는 더 밀지 않고 멈춘다 —
// 끝나지 않는 계산보다 조금 어긋난 배치가 낫다.
func layoutDepths(sc *schema.Schema, refs map[string][]string) map[string]int {
	depth := make(map[string]int, len(sc.Tables))
	// 표 수만큼 훑으면 순환이 없는 그래프에서는 반드시 안정된다(가장 긴 사슬의
	// 길이가 표 수를 넘지 못한다). 순환이 있으면 그 횟수에서 멈춘다.
	for round := 0; round < len(sc.Tables)+1; round++ {
		changed := false
		for _, t := range sc.Tables {
			want := 0
			for _, parent := range refs[t.Key()] {
				if depth[parent]+1 > want {
					want = depth[parent] + 1
				}
			}
			if want > depth[t.Key()] {
				depth[t.Key()] = want
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return depth
}

// sortTablesByParentRow는 한 열의 표들을 부모의 자리 순서로 세운다.
//
// 안정 정렬이어야 한다. 부모의 자리가 같은 표들(또는 부모가 없는 표들)은 들어온
// 순서를 지켜야, 같은 스키마에서 같은 그림이 나온다.
func sortTablesByParentRow(list []*schema.Table, refs map[string][]string, rowOf map[string]int) {
	score := make(map[string]float64, len(list))
	for _, t := range list {
		sum := 0.0
		n := 0
		for _, parent := range refs[t.Key()] {
			if row, ok := rowOf[parent]; ok {
				sum += float64(row)
				n++
			}
		}
		if n == 0 {
			// 부모가 아직 안 놓였으면 맨 아래로 보낸다. 위쪽은 선이 이어지는
			// 표들의 자리다.
			score[t.Key()] = 1e9
			continue
		}
		score[t.Key()] = sum / float64(n)
	}
	sort.SliceStable(list, func(i, j int) bool {
		return score[list[i].Key()] < score[list[j].Key()]
	})
}

// nextFreeSlot은 새 테이블을 놓을 빈 자리를 찾는다.
// 이미 쓰인 격자점을 피해 배치하므로 여러 사람이 동시에 테이블을 추가해도 겹치지 않는다.
func (d *Document) nextFreeSlot() (float64, float64) {
	used := make(map[[2]float64]bool, len(d.Layout))
	for _, b := range d.Layout {
		used[[2]float64{b.X, b.Y}] = true
	}
	for i := 0; i < 1024; i++ {
		x := layoutOriginX + float64(i%layoutColumns)*layoutStepX
		y := layoutOriginY + float64(i/layoutColumns)*layoutStepY
		if !used[[2]float64{x, y}] {
			return x, y
		}
	}
	return layoutOriginX, layoutOriginY
}

// Note는 ID로 메모를 찾는다.
func (d *Document) Note(id string) *Note {
	for _, n := range d.Notes {
		if n.ID == id {
			return n
		}
	}
	return nil
}

// Clone은 문서를 깊게 복사한다. 검증 실패 시 부분 적용된 상태가 남지 않게 하려면
// op를 사본에 적용해보고 성공했을 때만 교체해야 한다.
func (d *Document) Clone() *Document {
	out := *d
	out.Schema = cloneSchema(d.Schema)
	out.Layout = make(map[string]*Box, len(d.Layout))
	for k, v := range d.Layout {
		b := *v
		out.Layout[k] = &b
	}
	out.Notes = make([]*Note, 0, len(d.Notes))
	for _, n := range d.Notes {
		c := *n
		out.Notes = append(out.Notes, &c)
	}
	// 그룹도 깊은 복사여야 한다. 슬라이스 헤더만 복사하면 사본을 고칠 때 원본의
	// Group까지 바뀌어, "사본에 먼저 적용하고 실패하면 버린다"는 규칙이 깨진다.
	out.Groups = make([]*Group, 0, len(d.Groups))
	for _, g := range d.Groups {
		c := *g
		out.Groups = append(out.Groups, &c)
	}
	// 도메인도 같은 이유로 깊은 복사다. 얕게 두면 사본에서 정의를 고칠 때 원본까지
	// 바뀌어, 되돌리기가 "바뀐 것이 없다"고 판단한다(diff는 두 문서를 비교한다).
	// 문서 수준 설정도 깊은 복사다. 맵과 포인터를 얕게 두면 사본에 적용한 op 가
	// 실패했을 때 원본의 설정만 바뀐 채로 남는다.
	out.TableDefaults = cloneKV(d.TableDefaults)
	out.TargetDB = d.TargetDB.clone()
	out.Domains = make([]*Domain, 0, len(d.Domains))
	for _, dom := range d.Domains {
		c := *dom
		if dom.Nullable != nil {
			v := *dom.Nullable
			c.Nullable = &v
		}
		out.Domains = append(out.Domains, &c)
	}
	return &out
}

// cloneSchema·cloneTable은 schema 패키지의 깊은 복사를 부른다.
//
// 여기 두었던 구현을 옮긴 이유: 복사 규칙은 그 구조체를 아는 쪽에 있어야 한다.
// 필드가 하나 늘 때 이쪽을 함께 고치는 것을 잊으면, 사본을 고쳤는데 원본이 함께
// 바뀌는 조용한 버그가 된다.
func cloneSchema(sc *schema.Schema) *schema.Schema { return sc.Clone() }

func cloneTable(t *schema.Table) *schema.Table { return t.Clone() }

// findTable은 키(namespace.name, 소문자)로 테이블을 찾는다.
func (d *Document) findTable(key string) *schema.Table {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, t := range d.Schema.Tables {
		if t.Key() == key {
			return t
		}
	}
	return nil
}
