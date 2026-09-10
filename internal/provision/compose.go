package provision

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// compose.yml 만들기.
//
// ── 왜 필요한가 ─────────────────────────────────────────────────────
// 화면에서 정한 것을 **가져갈 수 있어야** 한다. 이 앱 없이도 같은 DB 를 띄울 수
// 있어야 하고, 그 파일을 저장소에 넣어 검토할 수 있어야 한다. 그렇지 않으면
// 여기서 만든 DB 는 이 앱 안에서만 존재하는 것이 되고, 그것은 사람을 묶어 두는
// 종류의 편리함이다.
//
// ── 왜 손으로 적는가 ────────────────────────────────────────────────
// YAML 라이브러리를 들이지 않는다. 이 저장소의 방식이기도 하지만(SigV4 도 TOTP 도
// 손으로 썼다), 여기서 내보내는 모양은 우리가 정한 고정된 형태 하나뿐이라
// 일반 YAML 직렬화기가 필요하지 않다. 대신 문자열은 **언제나 따옴표로 감싼다** —
// 무엇을 감싸야 하는지 판단하려 들면 그 판단이 언젠가 틀린다(`on`, `1.10`,
// `*ref`, 콜론이 든 값…).
//
// ── 비밀은 넣지 않는다 ──────────────────────────────────────────────
// 비밀번호 자리에는 `${VAR}` 만 적는다. 이 파일은 저장소에 들어가라고 만드는
// 것이고, 그 자리에 평문을 적으면 그것이 새는 가장 흔한 길이 된다.

// ComposeItem은 파일에 넣을 서비스 하나다.
type ComposeItem struct {
	// Service는 compose 안에서의 이름이다(컨테이너 이름의 접두사를 뗀 것).
	Service string
	Plan    *Plan
	// HostPort는 실제로 잡힌 포트다. 0 이면 계획의 값을 쓴다.
	HostPort int
}

// ComposeFile은 만들어진 파일 한 벌이다.
type ComposeFile struct {
	// YAML은 compose.yml 의 내용이다.
	YAML string `json:"yaml"`
	// EnvExample은 함께 둘 .env 의 뼈대다(값은 비어 있다).
	EnvExample string `json:"envExample"`
	// Files는 함께 저장해야 하는 설정 파일이다(상대 경로 → 내용).
	//
	// ClickHouse 처럼 설정 파일로만 받는 DB 가 있다. compose 는 파일 내용을 안에
	// 담을 수 없으므로 옆에 두고 마운트한다.
	Files map[string]string `json:"files,omitempty"`
	// Notes는 이 파일을 쓰기 전에 알아야 하는 것이다.
	Notes []string `json:"notes,omitempty"`
}

// Format은 이 파일을 무엇이 읽을지다.
//
// 갈라 두되 **한 렌더러가 둘을 만든다.** 두 벌로 적으면 한쪽만 고치게 되고,
// 그러면 같은 DB 를 말하는 두 파일이 서로 다른 것을 말하게 된다.
type Format string

const (
	// ForCompose는 docker compose 용이다.
	ForCompose Format = "compose"
	// ForStack은 docker stack deploy 용이다(스웜).
	ForStack Format = "stack"
)

// Compose는 docker compose 용 파일을 만든다.
func Compose(items []ComposeItem) *ComposeFile { return render(items, ForCompose) }

// Stack은 docker stack deploy 용 파일을 만든다.
//
// ── 무엇이 다른가 (살아 있는 스웜에 돌려 보고 하나씩 찾은 것들) ─────
//   - 최상위 `name:` 을 거부한다("Additional property name is not allowed").
//     스택 이름은 명령줄이 정한다.
//   - `container_name:` 과 `restart:` 를 무시한다. 다시 시작 정책은
//     deploy.restart_policy 로 적어야 한다.
//   - 네트워크는 오버레이여야 하고 이름을 고정할 수 없다. 고정하면 이미 있는
//     브리지 네트워크와 부딪혀 "already exists" 로 멈춘다.
//   - **`.env` 를 읽지 않는다.** 이것이 가장 위험하다 — 비밀번호 자리가 빈
//     값이 되고 DB 는 오류 없이 **비밀번호 없이** 뜬다. 실제로 그렇게 떴고
//     아무 경고도 없었다(붙어 보니 비밀번호 없이 들어갔다). 그래서 스택
//     파일에서는 `${VAR:?…}` 로 적어 **값이 없으면 배포가 멈추게** 한다.
func Stack(items []ComposeItem) *ComposeFile { return render(items, ForStack) }

func render(items []ComposeItem, format Format) *ComposeFile {
	stack := format == ForStack
	out := &ComposeFile{Files: map[string]string{}}
	var b strings.Builder

	if stack {
		b.WriteString("# DB Studio 가 만든 스웜 스택 파일입니다.\n")
		b.WriteString("#\n")
		b.WriteString("#   docker stack deploy -c stack.yaml dbstudio\n")
		b.WriteString("#\n")
		b.WriteString("# **비밀번호는 환경변수로 넣어야 합니다.** docker stack deploy 는 .env 를\n")
		b.WriteString("# 읽지 않습니다. 넣지 않으면 배포가 멈추도록 적어 두었습니다 — 그 장치가\n")
		b.WriteString("# 없으면 DB 가 비밀번호 없이 조용히 뜹니다.\n")
		b.WriteString("#\n")
		b.WriteString("#   $env:이름_PASSWORD=\"…\"   # PowerShell\n")
		b.WriteString("#   export 이름_PASSWORD=…      # bash\n")
		b.WriteString("#\n")
		b.WriteString("# 볼륨은 그 서비스가 뜬 노드에만 있습니다. 노드가 여럿이면 다른 노드로\n")
		b.WriteString("# 옮겨 뜰 때 데이터가 따라가지 않습니다.\n")
	} else {
		b.WriteString("# DB Studio 가 만든 compose 파일입니다.\n")
		b.WriteString("#\n")
		b.WriteString("# 비밀번호는 들어 있지 않습니다. 옆의 .env 에 채워 넣으세요.\n")
		b.WriteString("# 볼륨과 네트워크에 name 을 적어 둔 이유: 그렇게 하지 않으면 compose 가\n")
		b.WriteString("# 프로젝트 이름을 앞에 붙여 새 볼륨을 만듭니다 — 같은 기계에서 돌릴 때\n")
		b.WriteString("# 쓰던 데이터가 아니라 빈 데이터로 뜨게 됩니다.\n")
		b.WriteString("#\n")
		b.WriteString("# \"network " + NetworkName + " was found but has incorrect label\" 로 멈춘다면,\n")
		b.WriteString("# 그 네트워크가 compose 라벨 없이 먼저 만들어진 것입니다(옛 판의 DB Studio).\n")
		b.WriteString("# 붙어 있는 컨테이너를 멈춘 뒤 `docker network rm " + NetworkName + "` 하면\n")
		b.WriteString("# compose 가 다시 만듭니다.\n")
	}
	if stack {
		b.WriteString("\nservices:\n")
	} else {
		fmt.Fprintf(&b, "\nname: %s\n\nservices:\n", yamlStr(ProjectName))
	}

	var envVars []string
	volumes := map[string]bool{}

	sort.Slice(items, func(i, j int) bool { return items[i].Service < items[j].Service })
	for _, it := range items {
		p := it.Plan
		if p == nil {
			continue
		}
		svc := it.Service
		if svc == "" {
			svc = strings.TrimPrefix(p.Container, "dbstudio-")
		}
		fmt.Fprintf(&b, "  %s:\n", svc)
		fmt.Fprintf(&b, "    image: %s\n", yamlStr(p.Image))
		// 컨테이너 이름과 restart 는 스웜이 무시한다("Ignoring unsupported
		// options: restart"). 적어 두면 배포할 때마다 경고가 나오고, 경고가
		// 흔해지면 정작 봐야 하는 경고를 지나치게 된다.
		if !stack {
			fmt.Fprintf(&b, "    container_name: %s\n", yamlStr(p.Container))
			if p.Restart != "" {
				fmt.Fprintf(&b, "    restart: %s\n", yamlStr(p.Restart))
			}
		}

		// 비밀은 **값으로** 가린다. 이름으로 가리면 길이 하나 더 있는 것을
		// 놓친다 — Redis 의 비밀번호는 환경변수가 아니라 실행 인자로 간다.
		mask := newMasker(svc, p, stack)
		envVars = append(envVars, mask.vars()...)

		if len(p.Env) > 0 {
			b.WriteString("    environment:\n")
			for _, k := range sortedKeys(p.Env) {
				fmt.Fprintf(&b, "      %s: %s\n", k, yamlStr(mask.apply(p.Env[k])))
			}
		}

		if len(p.Args) > 0 {
			b.WriteString("    command:\n")
			for _, a := range p.Args {
				fmt.Fprintf(&b, "      - %s\n", yamlStr(mask.apply(a)))
			}
		}

		port := it.HostPort
		if port == 0 {
			port = p.HostPort
		}
		b.WriteString("    ports:\n")
		if port > 0 {
			fmt.Fprintf(&b, "      - %s\n", yamlStr(fmt.Sprintf("%d:%d", port, p.Port)))
		} else {
			// 포트를 정하지 않았으면 도커가 고르게 둔다. 여기서 아무 번호나
			// 적으면 그 번호가 이미 쓰이고 있는 기계에서 뜨지 않는다.
			fmt.Fprintf(&b, "      - %s\n", yamlStr(strconv.Itoa(p.Port)))
		}

		mounts := []string{}
		if p.Volume != "" && p.Recipe.DataPath != "" {
			mounts = append(mounts, p.Volume+":"+p.Recipe.DataPath)
			volumes[p.Volume] = true
		}
		for _, path := range sortedKeys(p.Files) {
			// 파일은 옆에 두고 **파일 하나만** 마운트한다. 디렉터리째 덮으면
			// 이미지가 넣어 둔 것이 사라진다(ClickHouse 의 listen_host 설정과
			// 엔트리포인트가 만드는 비밀번호 파일이 그렇게 사라진다).
			rel := "./" + svc + path
			// 설정 파일에도 비밀이 들어갈 수 있다(클러스터의 remote_servers 는
			// 노드끼리 붙을 계정을 담는다). 환경변수와 인자만 가리고 파일을
			// 그대로 내보내면, 저장소에 들어가라고 만든 파일에 평문이 실린다.
			out.Files[strings.TrimPrefix(rel, "./")] = mask.applyInText(p.Files[path])
			mounts = append(mounts, rel+":"+path+":ro")
		}
		if len(mounts) > 0 {
			b.WriteString("    volumes:\n")
			for _, m := range mounts {
				fmt.Fprintf(&b, "      - %s\n", yamlStr(m))
			}
		}

		if len(p.Recipe.Health) > 0 {
			b.WriteString("    healthcheck:\n      test:\n")
			for _, part := range p.Recipe.Health {
				// $ 를 두 번 적는다. compose 는 파일을 읽으며 $VAR 를 자기가
				// 채워 넣는데, 이 문자열의 $ 는 **컨테이너 안의 셸**이 채워야
				// 하는 것이다. 그대로 두면 compose 가 빈 값으로 바꿔 버리고,
				// 헬스체크는 계정 없이 돌다 영영 실패한다.
				fmt.Fprintf(&b, "        - %s\n", yamlStr(escapeDollar(part)))
			}
			b.WriteString("      interval: 10s\n      timeout: 5s\n")
			b.WriteString("      retries: 12\n      start_period: 30s\n")
		}

		hasLimits := p.MemoryMB > 0 || nanoCPUs(p.CPUs) > 0
		if hasLimits || stack {
			// deploy 아래에 적는 이유: compose 와 swarm 이 같은 자리를 읽는다.
			// mem_limit 은 compose 전용이고 스펙에서 물러난 자리다.
			b.WriteString("    deploy:\n")
			if stack {
				fmt.Fprintf(&b, "      restart_policy:\n        condition: %s\n",
					yamlStr(swarmRestart(p.Restart)))
			}
			if hasLimits {
				b.WriteString("      resources:\n        limits:\n")
				if p.MemoryMB > 0 {
					fmt.Fprintf(&b, "          memory: %s\n", yamlStr(fmt.Sprintf("%dM", p.MemoryMB)))
				}
				if p.CPUs != "" && nanoCPUs(p.CPUs) > 0 {
					fmt.Fprintf(&b, "          cpus: %s\n", yamlStr(p.CPUs))
				}
			}
		}

		if p.Recipe.ID == "clickhouse" {
			b.WriteString("    ulimits:\n      nofile:\n        soft: 262144\n        hard: 262144\n")
			out.Notes = append(out.Notes,
				"ClickHouse 는 파일 핸들 상한을 올려 줘야 뜨는 호스트가 있어 ulimits 를 넣었습니다.")
		}

		fmt.Fprintf(&b, "    networks:\n      - %s\n", yamlStr(NetworkName))
	}

	if len(volumes) > 0 {
		b.WriteString("\nvolumes:\n")
		for _, v := range sortedKeys(boolKeys(volumes)) {
			fmt.Fprintf(&b, "  %s:\n    name: %s\n", v, yamlStr(v))
		}
	}
	if stack {
		// 오버레이여야 하고, 이름을 고정하지 않는다. 고정하면 이미 있는 브리지
		// 네트워크와 부딪혀 "network dbstudio-net already exists" 로 멈춘다 —
		// 스택은 자기 이름을 앞에 붙여 따로 만든다.
		fmt.Fprintf(&b, "\nnetworks:\n  %s:\n    driver: overlay\n", NetworkName)
	} else {
		fmt.Fprintf(&b, "\nnetworks:\n  %s:\n    name: %s\n", NetworkName, yamlStr(NetworkName))
	}

	out.YAML = b.String()
	if len(envVars) > 0 {
		sort.Strings(envVars)
		var e strings.Builder
		e.WriteString("# compose.yml 이 읽는 값입니다. 이 파일은 저장소에 넣지 마세요.\n")
		for _, v := range envVars {
			fmt.Fprintf(&e, "%s=\n", v)
		}
		out.EnvExample = e.String()
		if stack {
			// 스택은 이 파일을 읽지 않는다. 그래도 함께 주는 이유는 이름 목록이
			// 필요해서다 — 무엇을 환경변수로 넣어야 하는지가 여기 적혀 있다.
			out.Notes = append(out.Notes,
				"docker stack deploy 는 .env 를 읽지 않습니다. 아래 이름들을 환경변수로 "+
					"넣고 배포하세요 — 넣지 않으면 배포가 멈춥니다(그렇게 적어 두었습니다).")
		} else {
			out.Notes = append(out.Notes,
				"비밀번호는 파일에 넣지 않았습니다. .env 에 채워 넣어야 뜹니다.")
		}
	}
	if len(out.Files) > 0 {
		what := "compose.yml"
		if stack {
			what = "stack.yaml"
		}
		out.Notes = append(out.Notes,
			"설정 파일을 "+what+" 옆에 같은 경로로 두어야 합니다. 없으면 도커가 그 자리에 "+
				"빈 디렉터리를 만들고, DB 는 설정 없이 뜹니다.")
	}
	if stack {
		out.Notes = append(out.Notes,
			"스웜에는 unless-stopped 가 없어 restart_policy 를 가장 가까운 것으로 적었습니다. "+
				"사람이 멈춘 것도 다시 띄운다는 점이 다릅니다.")
		if len(out.Files) > 0 {
			out.Notes = append(out.Notes,
				"설정 파일은 **그 서비스가 뜨는 노드**에 있어야 합니다. 노드가 여럿이면 "+
					"모든 노드에 같은 경로로 두거나, 그 노드에만 뜨도록 묶어야 합니다.")
		}
	}
	return out
}

// swarmRestart는 도커의 다시 시작 정책을 스웜의 것으로 옮긴다.
//
// 스웜에는 unless-stopped 가 없다. 가장 가까운 것이 any 인데, 그 둘은 다르다 —
// any 는 사람이 멈춘 것도 다시 띄운다. 조용히 바꾸지 않고 안내에 적어 둔다.
func swarmRestart(restart string) string {
	switch restart {
	case "no", "":
		return "none"
	case "on-failure":
		return "on-failure"
	default: // always, unless-stopped
		return "any"
	}
}

// masker는 비밀 값을 `${VAR}` 자리로 바꾼다.
//
// ── 왜 값으로 찾는가 ────────────────────────────────────────────────
// 비밀이 컨테이너에 닿는 길이 하나가 아니다. 환경변수로 가는 것도 있고
// (POSTGRES_PASSWORD), 실행 인자로 가는 것도 있다(Redis 의 --requirepass).
// 환경변수 이름만 가리면 인자로 간 값이 그대로 파일에 실린다 — 검사가 실제로
// 그것을 잡았다. 길이 늘어날 때마다 가리는 곳을 늘리는 대신, 값 자체를 찾는다.
//
// ── 어떻게 찾는가 ───────────────────────────────────────────────────
// 통째로 같을 때와 `--이름=값` 의 오른쪽일 때만 바꾼다. 아무 데나 부분 문자열로
// 바꾸면 짧은 비밀번호가 다른 값 안에서 걸린다("5432" 같은 것을 쓰는 사람이 있고,
// 그것을 포트 번호 안에서 바꾸면 파일이 망가진다).
type masker struct {
	// byValue는 값 → 자리 이름이다.
	byValue map[string]string
	names   []string
	// required면 값이 없을 때 배포가 멈추는 모양으로 적는다.
	required bool
}

// required 면 값이 없을 때 배포가 멈추도록 적는다(`${VAR:?…}`).
//
// 스택에서만 켠다. docker stack deploy 는 .env 를 읽지 않아서, 이 장치가 없으면
// 비밀번호 자리가 빈 값이 되고 DB 가 **비밀번호 없이** 조용히 뜬다. 실제로
// 그렇게 떴다. compose 쪽은 .env 를 읽으므로 그냥 `${VAR}` 로 둔다 — 거기서
// 같은 장치를 켜면 .env 를 채운 사람에게도 오류가 나기 쉽다(빈 줄 하나로 멈춘다).
func newMasker(service string, p *Plan, required bool) *masker {
	m := &masker{byValue: map[string]string{}, required: required}
	for _, key := range sortedKeys(p.secretValues) {
		v := p.secretValues[key]
		if v == "" {
			continue
		}
		name := envVarName(service, key)
		if _, seen := m.byValue[v]; seen {
			continue // 같은 값이 두 필드에 있으면 자리도 하나면 된다
		}
		m.byValue[v] = name
		m.names = append(m.names, name)
	}
	return m
}

// placeholder는 값 자리에 적을 문자열이다.
func (m *masker) placeholder(name string) string {
	if m.required {
		return "${" + name + ":?" + name + " 를 환경변수로 넣어야 합니다}"
	}
	return "${" + name + "}"
}

func (m *masker) vars() []string { return m.names }

// apply는 값 하나를 파일에 적을 모양으로 바꾼다.
func (m *masker) apply(v string) string {
	if name, ok := m.byValue[v]; ok {
		return m.placeholder(name)
	}
	// `--max_connections=100` 처럼 이름과 값이 붙어 오는 인자.
	if i := strings.Index(v, "="); i > 0 {
		if name, ok := m.byValue[v[i+1:]]; ok {
			return escapeDollar(v[:i+1]) + m.placeholder(name)
		}
	}
	return escapeDollar(v)
}

// applyInText는 글 안에 박힌 비밀 값을 자리로 바꾼다.
//
// apply 와 달리 **부분 문자열**로 찾는다. 설정 파일 안의 비밀번호는 태그
// 사이에 끼어 있어서 값 전체가 같을 수 없다. 짧은 비밀번호가 다른 낱말 안에서
// 걸릴 수 있지만, 설정 파일에서는 그 위험보다 평문이 새는 쪽이 훨씬 나쁘다.
func (m *masker) applyInText(text string) string {
	for _, v := range sortedKeys(m.byValue) {
		if v == "" {
			continue
		}
		text = strings.ReplaceAll(text, v, m.placeholder(m.byValue[v]))
	}
	return text
}

// envVarName은 비밀 자리에 쓸 변수 이름이다.
//
// 서비스 이름을 앞에 붙이는 이유: 한 파일에 DB 가 여럿이면 POSTGRES_PASSWORD 가
// 둘이 되고, 그러면 한 값이 두 DB 의 비밀번호가 된다.
func envVarName(service, key string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(service + "_" + key) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// escapeDollar는 compose 가 자기 것으로 읽지 않게 $ 를 두 번 적는다.
func escapeDollar(s string) string { return strings.ReplaceAll(s, "$", "$$") }

// yamlStr는 문자열 하나를 큰따옴표로 감싼다.
//
// 언제나 감싼다. 무엇을 감싸야 하는지 고르려 들면 그 판단이 언젠가 틀린다 —
// YAML 에서 `on`·`no`·`1.10`·`*x` 는 모두 문자열이 아닌 것으로 읽힌다.
func yamlStr(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString("\\\"")
		case '\\':
			b.WriteString("\\\\")
		case '\n':
			b.WriteString("\\n")
		case '\t':
			b.WriteString("\\t")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func boolKeys(m map[string]bool) map[string]string {
	out := make(map[string]string, len(m))
	for k := range m {
		out[k] = k
	}
	return out
}

// configHash는 compose 가 "이 컨테이너가 지금 설정과 같은가"를 재는 값이다.
//
// ── 왜 이것을 붙이는가 ──────────────────────────────────────────────
// 살아 있는 데몬에 재 보고 알았다. compose 는 project·service 라벨만으로는
// 컨테이너를 자기 것으로 보지 않는다 — **config-hash 가 있어야** `docker compose
// -p dbstudio ps` 에 나타난다. 라벨 조합을 하나씩 지워 가며 확인했다.
//
// 값 자체는 우리가 정한다(compose 의 계산식과 같을 필요는 없다). 같은 설정이면
// 같은 값이 나오는 것이 중요하다 — 그래야 다시 만들었을 때 compose 가 "바뀌었다"고
// 보지 않는다.
func (p *Plan) configHash() string {
	var b strings.Builder
	b.WriteString(p.Image)
	b.WriteString("\n" + p.Container)
	b.WriteString("\n" + p.Volume)
	b.WriteString("\n" + strconv.Itoa(p.Port) + ":" + strconv.Itoa(p.HostPort))
	b.WriteString("\n" + p.Restart)
	b.WriteString("\n" + strconv.Itoa(p.MemoryMB) + "/" + p.CPUs)
	for _, k := range sortedKeys(p.Env) {
		b.WriteString("\n" + k + "=" + p.Env[k])
	}
	for _, a := range p.Args {
		b.WriteString("\n arg " + a)
	}
	for _, k := range sortedKeys(p.Files) {
		b.WriteString("\n file " + k + "=" + p.Files[k])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
