package nexus

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"
)

// 规模测试（可重复，固定种子）：
//   - 30 万条流水集中在同一辖区同一天；
//   - 笔数门槛设为不可达，金额首次越线点完全由金额索引决定；
//   - 随后 1 万次“早期更正 + 分布于全天的历史查询”交替执行；
//   - 结束时与逐笔重放参考实现做全量对比。
//
// 运行：go test -run TestScale -v .
func TestScale(t *testing.T) {
	const (
		nEvents   = 300_000
		nRounds   = 10_000
		saleCents = 10_000
	)
	day := time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)
	dayLen := 24 * time.Hour
	// 86400e6 微秒 / 300000 = 288 微秒间隔；直接 i*dayLen 会溢出 int64。
	eventAt := func(i int) time.Time {
		return day.Add(time.Duration(i) * (dayLen / nEvents))
	}
	thresholds := map[string]Thresholds{
		"BIG": {AmountCents: nEvents * saleCents, Count: math.MaxInt64},
	}
	tr := New(thresholds)

	start := time.Now()
	for i := 0; i < nEvents; i++ {
		tr.Upsert(Event{ID: fmt.Sprintf("ev-%08d", i), Jurisdiction: "BIG",
			At: eventAt(i), Kind: Sale, AmountCents: saleCents})
	}
	setupDur := time.Since(start)

	// 基线：首次建立关联应出现在最后一条流水。
	st := tr.Status("BIG", day.Add(dayLen-time.Microsecond))
	if st.Level != LevelEstablished || st.FirstEstablished.EventID != fmt.Sprintf("ev-%08d", nEvents-1) {
		t.Fatalf("baseline: %+v", st)
	}

	rng := rand.New(rand.NewSource(26100432))
	start = time.Now()
	for r := 0; r < nRounds; r++ {
		// 早期更正：修改前 1000 条中某条的金额，使金额首次越线点前后移动。
		i := rng.Intn(1000)
		id := fmt.Sprintf("ev-%08d", i)
		delta := int64(rng.Intn(41)-20) * 1_000 // ±2 万分的扰动
		tr.Upsert(Event{ID: id, Jurisdiction: "BIG", At: eventAt(i), Kind: Sale,
			AmountCents: saleCents + delta})
		// 分布于全天的历史查询。
		q := day.Add(time.Duration(rng.Int63n(int64(dayLen/time.Microsecond))) * time.Microsecond)
		got := tr.Status("BIG", q)
		if got.SaleCount < 0 || got.SaleCount > nEvents {
			t.Fatalf("round %d: impossible count %d", r, got.SaleCount)
		}
	}
	loopDur := time.Since(start)

	// 与逐笔重放参考实现全量对比（年末时刻，覆盖全天）。
	ref := newReplayLedger(thresholds)
	for id, e := range tr.events {
		_ = id
		ref.Upsert(e)
	}
	end := day.Add(dayLen - time.Microsecond)
	sameStatus(t, tr.Status("BIG", end), ref.Status("BIG", end), "final day-end")
	mid := day.Add(12 * time.Hour)
	sameStatus(t, tr.Status("BIG", mid), ref.Status("BIG", mid), "final midday")

	t.Logf("setup: %d upserts in %v (%.0f ops/s)", nEvents, setupDur,
		float64(nEvents)/setupDur.Seconds())
	t.Logf("loop: %d rounds (correction+query) in %v (%.0f rounds/s, avg %v/round)",
		nRounds, loopDur, float64(nRounds)/loopDur.Seconds(), loopDur/nRounds)
}
