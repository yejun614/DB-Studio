package provision

import (
	"context"
	"sort"
	"strings"
	"time"

	"dbstudio/internal/docker"
	"dbstudio/internal/store"
)

// 만든 뒤에 설정 고치기.
//
// ── 왜 "무엇이 필요한가"를 먼저 말하는가 ────────────────────────────
// 고칠 수 있는 값이라고 다 같은 값이 아니다. 메모리 상한은 도커가 컨테이너를
// 그대로 두고 바꿔 주지만, 실행 인자는 만들 때 정해지므로 다시 만들어야 하고,
// 계정과 비밀번호는 **다시 만들어도 안 바뀌는** DB 가 있다(첫 실행에서만 쓴다).
//
// 그 차이를 말하지 않고 "저장했습니다"라고만 하면, 사람은 바뀐 줄 알고 새
// 비밀번호로 접속을 시도하다 막힌다. 무엇이 실제로 일어났는지가 이 기능의
// 절반이다 — 어느 것이 어느 쪽인지는 살아 있는 컨테이너로 재서 카탈로그에
// 적어 두었다(Field.Apply).

// Change는 값 하나가 바뀐 것이다.
type Change struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// From·To는 화면에 보일 값이다. 비밀은 가린다.
	From  string `json:"from"`
	To    string `json:"to"`
	Apply Apply  `json:"apply"`
}

// Diff는 두 값 묶음의 차이를 낸다.
//
// 레시피에 없는 열쇠는 무시한다. 화면이 보내 온 것 중 우리가 모르는 것은
// 적용할 길도 없고, 저장해 두면 다음에 계획을 세울 때 "모르는 칸"이 늘어난다.
func Diff(r *Recipe, before, after map[string]string) []Change {
	var out []Change
	for i := range r.Fields {
		f := &r.Fields[i]
		old, wasSet := before[f.Key]
		now, isSet := after[f.Key]
		old, now = strings.TrimSpace(old), strings.TrimSpace(now)

		// 안 준 칸은 "그대로 두라"는 뜻이다. 빈 값으로 지우려면 화면이
		// 빈 문자열을 명시적으로 보내야 한다.
		if !isSet {
			continue
		}
		// 적어 두지 않은 칸의 지금 값은 **기본값**이다.
		//
		// 이것을 빈 값으로 보면, 화면이 칸을 채워 보낼 때마다 "안 정한 것 →
		// 기본값"이 변경으로 잡힌다. 메모리 상한 하나를 고쳤을 뿐인데 손대지
		// 않은 칸 둘이 함께 바뀐 것이 되고, 그중 하나가 다시 만들기를 요구하면
		// 컨테이너가 괜히 다시 만들어진다 — 실제로 그렇게 됐다.
		if !wasSet {
			old = f.Default
		}
		if old == now {
			continue
		}
		out = append(out, Change{
			Key: f.Key, Label: f.Label,
			From: showValue(f, old), To: showValue(f, now),
			Apply: ApplyOf(f),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// showValue는 화면에 보일 값이다. 비밀은 가린다.
func showValue(f *Field, v string) string {
	if v == "" {
		return ""
	}
	if f.Secret {
		return "(가림)"
	}
	return v
}

// Needed는 이 변경들을 적용하려면 무엇이 필요한지다(가장 무거운 것).
//
// init-only 는 무게에 넣지 않는다. 그것은 "무엇이 필요한가"가 아니라 "해도
// 소용없다"이고, 그것만 바뀌었다면 컨테이너를 건드릴 이유가 없다.
func Needed(changes []Change) Apply {
	weight := map[Apply]int{ApplyInitOnly: 0, ApplyLive: 1, ApplyRestart: 2, ApplyRecreate: 3}
	worst := ApplyInitOnly
	for _, c := range changes {
		if weight[c.Apply] > weight[worst] {
			worst = c.Apply
		}
	}
	return worst
}

// InitOnly는 고쳐도 지금 DB 에는 적용되지 않는 것들이다.
func InitOnly(changes []Change) []Change {
	var out []Change
	for _, c := range changes {
		if c.Apply == ApplyInitOnly {
			out = append(out, c)
		}
	}
	return out
}

// Update는 바뀐 설정을 실제로 적용한다.
//
// 무엇을 할지는 Needed 가 정한다. 셋은 겹친다 — 다시 만들면 재시작과 자원
// 바꾸기가 그 안에 들어 있으므로 따로 하지 않는다.
func (r *Runner) Update(ctx context.Context, in *store.DBInstance, plan *Plan, need Apply) error {
	if in.ContainerID == "" {
		// 컨테이너가 없으면 값만 저장된 것이다. 다음에 만들 때 쓰인다.
		return nil
	}
	switch need {
	case ApplyLive:
		return r.updateLive(ctx, in, plan)
	case ApplyRestart:
		return r.updateRestart(ctx, in, plan)
	case ApplyRecreate:
		return r.recreate(ctx, in, plan)
	default:
		return nil // init-only 만 바뀌었다. 컨테이너를 건드릴 이유가 없다.
	}
}

// updateLive는 컨테이너를 그대로 두고 자원만 바꾼다.
func (r *Runner) updateLive(ctx context.Context, in *store.DBInstance, plan *Plan) error {
	res := docker.UpdateResources{Restart: &docker.RestartPolicy{Name: plan.Restart}}
	if plan.MemoryMB > 0 {
		res.Memory = int64(plan.MemoryMB) << 20
	}
	if n := nanoCPUs(plan.CPUs); n > 0 {
		res.NanoCPUs = n
	}
	warnings, err := r.docker.UpdateContainer(ctx, in.ContainerID, res)
	if err != nil {
		return err
	}
	if len(warnings) > 0 {
		// 경고는 실패가 아니다. 다만 삼키지도 않는다 — 무엇이 안 걸렸는지
		// 사람이 알아야 "왜 상한이 안 먹지"를 여기서 끝낼 수 있다.
		r.progress(in.ID, "바꿨습니다 (도커의 경고: "+strings.Join(warnings, "; ")+")")
	}
	return nil
}

// updateRestart는 설정 파일을 다시 넣고 재시작한다.
func (r *Runner) updateRestart(ctx context.Context, in *store.DBInstance, plan *Plan) error {
	// 자원도 함께 바뀌었을 수 있다. 재시작은 그것을 되돌리지 않으므로 먼저 건다.
	if err := r.updateLive(ctx, in, plan); err != nil {
		return err
	}
	for path, body := range plan.Files {
		if err := r.docker.PutFile(ctx, in.ContainerID, path, body); err != nil {
			return err
		}
	}
	if err := r.docker.RestartContainer(ctx, in.ContainerID, 20*time.Second); err != nil {
		return err
	}
	return r.waitReady(ctx, in.ID, in.ContainerID, plan)
}

// recreate는 컨테이너를 지우고 같은 볼륨으로 다시 만든다.
//
// ── 볼륨은 절대 지우지 않는다 ───────────────────────────────────────
// 설정을 고치려고 데이터를 잃는 일이 있어서는 안 된다. 여기서 지우는 것은
// 컨테이너뿐이고, 새 컨테이너는 같은 이름의 볼륨을 다시 붙인다.
func (r *Runner) recreate(ctx context.Context, in *store.DBInstance, plan *Plan) error {
	r.progress(in.ID, "컨테이너를 다시 만듭니다")
	if err := r.docker.RemoveContainer(ctx, in.ContainerID, true); err != nil &&
		!docker.IsNotFound(err) {
		return err
	}
	// 지운 것을 먼저 적는다. 여기서 앱이 죽으면 없는 컨테이너를 가리키는 줄이
	// 남고, 그 줄로는 시작도 지우기도 되지 않는다.
	empty := ""
	if err := r.st.UpdateDBInstance(ctx, in.ID,
		store.UpdateDBInstanceParams{ContainerID: &empty}); err != nil {
		return err
	}

	if plan.Volume != "" {
		if err := r.docker.CreateVolume(ctx, plan.Volume, plan.Labels(in.ID)); err != nil {
			return err
		}
	}
	res, err := r.docker.CreateContainer(ctx, plan.Container, plan.CreateRequest(in.ID))
	if err != nil {
		return err
	}
	if err := r.setContainer(in.ID, res.ID); err != nil {
		return err
	}
	// 설정 파일은 시작하기 **전에** 넣는다(만들 때와 같은 이유).
	for path, body := range plan.Files {
		if err := r.docker.PutFile(ctx, res.ID, path, body); err != nil {
			return err
		}
	}
	if err := r.docker.StartContainer(ctx, res.ID); err != nil {
		return err
	}
	r.progress(in.ID, "뜨기를 기다립니다")
	return r.waitReady(ctx, in.ID, res.ID, plan)
}

// DescribeNeed는 무엇이 필요한지를 사람의 말로 적는다.
func DescribeNeed(need Apply) string {
	switch need {
	case ApplyLive:
		return "컨테이너를 그대로 두고 바꿉니다"
	case ApplyRestart:
		return "설정 파일을 다시 넣고 재시작합니다"
	case ApplyRecreate:
		return "컨테이너를 다시 만듭니다 (데이터는 남습니다)"
	default:
		return "컨테이너는 건드리지 않습니다"
	}
}
