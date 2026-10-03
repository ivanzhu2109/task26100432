package econtrace

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// 规模测试：30 万条流水集中在一天，随后 1 万次“早期更正 + 全天分布的
// 历史查询”交替执行。笔数门槛不可达，金额首次越线点随更正前后移动。
// 固定种子，可重复运行。运行：go test -run TestScale -v
func TestScale(t *testing.T) {
	const (
		n          = 300_000
		iterations = 10_000
		unit       = 100        // 每笔销售 100 分
		stepMicros = 288_000    // 相邻流水间隔（一天内均匀分布）
		boost      = 5_000_000  // 更正给首笔增加的金额
		amountTh   = 20_000_000 // 关联门槛：正常在第 199999 条越线
	)

	tr, err := New(map[string]Thresholds{
		"BIG": {AmountCents: amountTh, Sales: 1 << 60}, // 笔数门槛不可达
	})
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	idOf := func(i int) string { return fmt.Sprintf("e%06d", i) }
	atOf := func(i int) time.Time { return day.Add(time.Duration(i) * stepMicros * time.Microsecond) }

	start := time.Now()
	for i := 0; i < n; i++ {
		if err := tr.Upsert(sale(idOf(i), "BIG", atOf(i), unit)); err != nil {
			t.Fatal(err)
		}
	}
	buildDur := time.Since(start)
	t.Logf("insert %d events: %v (%.0f ops/s)", n, buildDur, float64(n)/buildDur.Seconds())

	// 预警门槛 = 16,000,000（= amountTh - amountTh/5）。
	// 首笔的三种状态对应不同的首次越线位置（闭式解，用于逐项断言）：
	//   正常 100 分：预警 idx 159999，关联 idx 199999
	//   加码 +5,000,000：预警 idx 109999，关联 idx 149999
	//   删除：预警 idx 160000，关联 idx 200000
	type phase struct{ warnIdx, estIdx int }
	phases := map[string]phase{
		"normal":  {warnIdx: 159999, estIdx: 199999},
		"boosted": {warnIdx: 109999, estIdx: 149999},
		"removed": {warnIdx: 160000, estIdx: 200000},
	}
	cycle := []string{"boosted", "normal", "boosted", "removed", "normal"}

	rng := rand.New(rand.NewSource(9001))
	endOfDay := day.Add(24*time.Hour - time.Microsecond)
	moved := map[string]int{}

	start = time.Now()
	for it := 0; it < iterations; it++ {
		name := cycle[it%len(cycle)]
		switch name {
		case "boosted":
			if err := tr.Upsert(sale(idOf(0), "BIG", atOf(0), unit+boost)); err != nil {
				t.Fatal(err)
			}
		case "normal":
			if err := tr.Upsert(sale(idOf(0), "BIG", atOf(0), unit)); err != nil {
				t.Fatal(err)
			}
		case "removed":
			if !tr.Remove(idOf(0)) {
				t.Fatal("remove e000000")
			}
		}
		ph := phases[name]

		// 分布于全天的历史查询（一半概率查日末）。
		at := endOfDay
		if rng.Intn(2) == 0 {
			at = day.Add(time.Duration(rng.Int63n(24*3600*1e6)) * time.Microsecond)
		}
		st := tr.Status("BIG", at)

		check := func(got *Trigger, idx int) {
			t.Helper()
			wantAt := atOf(idx)
			if wantAt.After(at) {
				if got != nil {
					t.Fatalf("it %d (%s)@%v: want nil trigger, got %+v", it, name, at, got)
				}
				return
			}
			if got == nil || got.EventID != idOf(idx) || !got.At.Equal(wantAt) {
				t.Fatalf("it %d (%s)@%v: want trigger %s@%v, got %+v", it, name, at, idOf(idx), wantAt, got)
			}
		}
		check(st.Warning, ph.warnIdx)
		check(st.Established, ph.estIdx)
		if st.Established != nil && st.Level != LevelEstablished {
			t.Fatalf("it %d: level %v with established trigger", it, st.Level)
		}
		if st.Established == nil && st.Warning != nil && st.Level != LevelWarning {
			t.Fatalf("it %d: level %v with warning trigger", it, st.Level)
		}
		moved[name]++
	}
	loopDur := time.Since(start)
	t.Logf("%d correction+query iterations: %v (%.0f ops/s)", iterations, loopDur, float64(iterations)/loopDur.Seconds())
	t.Logf("crossing moved across phases: %v", moved)

	// 日末终态校验（cycle 末态为 normal）。
	st := tr.Status("BIG", endOfDay)
	if st.AmountCents != n*unit || st.Sales != n {
		t.Fatalf("final cum: got %d/%d", st.AmountCents, st.Sales)
	}
	if st.Established == nil || st.Established.EventID != idOf(199999) {
		t.Fatalf("final established: got %+v", st.Established)
	}
}
