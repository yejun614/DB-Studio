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
// 좌표는 격자로 자동 배치한다 — 빈 캔버스에 테이블이 겹쳐 쌓여 있으면 쓸 수 없다.
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
	layoutStepX   = 320.0
	layoutStepY   = 260.0
	layoutOriginX = 80.0
	layoutOriginY = 80.0
)

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
func AutoLayout(sc *schema.Schema) map[string]*Box {
	out := make(map[string]*Box, len(sc.Tables))
	for i, t := range sc.Tables {
		out[t.Key()] = &Box{
			X: layoutOriginX + float64(i%layoutColumns)*layoutStepX,
			Y: layoutOriginY + float64(i/layoutColumns)*layoutStepY,
		}
	}
	return out
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
