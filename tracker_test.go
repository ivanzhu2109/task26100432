package econtrace

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)

func sale(id, jur string, at time.Time, cents int64) Event {
	return Event{ID: id, Jurisdiction: jur, At: at, Type: Sale, AmountCents: cents}
}

func refund(id, jur string, at time.Time, cents int64) Event {
	return Event{ID: id, Jurisdiction: jur, At: at, Type: Refund, AmountCents: cents}
}

func mustUpsert(t *testing.T, tr *Tracker, ev Event) {
	t.Helper()
	if err := tr.Upsert(ev); err != nil {
		t.Fatalf("upsert %+v: %v", ev, err)
	}
}

func checkTrigger(t *testing.T, what string, got *Trigger, wantID string, wantAt time.Time) {
	t.Helper()
	if wantID == "" {
		if got != nil {
			t.Fatalf("%s: want nil, got %+v", what, got)
		}
		return
	}
	if got == nil {
		t.Fatalf("%s: want %s@%v, got nil", what, wantID, wantAt)
	}
	if got.EventID != wantID || !got.At.Equal(wantAt) {
		t.Fatalf("%s: want %s@%v, got %+v", what, wantID, wantAt, got)
	}
}

// 金额刚好达到 80%/100% 后退款回落：级别不回降；
// 同一微秒内按 id 字节序裁决；删除关键销售后旧越线撤回；补录使触发点前移。
func TestBoundaryRefundAndRetraction(t *testing.T) {
	tr, err := New(map[string]Thresholds{
		"J1": {AmountCents: 1000, Sales: 10000}, // 预警 800，关联 1000，笔数不可达
	})
	if err != nil {
		t.Fatal(err)
	}
	t1, t2, t3 := t0.Add(time.Microsecond), t0.Add(2*time.Microsecond), t0.Add(3*time.Microsecond)

	// 故意逆序写入，证明写入顺序不参与裁决。
	mustUpsert(t, tr, sale("s2", "J1", t0, 300))
	mustUpsert(t, tr, sale("s1", "J1", t0, 500))

	// 同一微秒内 s1 先于 s2：s1 后累计 500，s2 后累计 800，恰好 80%。
	st := tr.Status("J1", t0)
	if st.AmountCents != 800 || st.Sales != 2 {
		t.Fatalf("cum: got %d/%d", st.AmountCents, st.Sales)
	}
	if st.Level != LevelWarning {
		t.Fatalf("level: got %v want warning", st.Level)
	}
	checkTrigger(t, "warning", st.Warning, "s2", t0)
	checkTrigger(t, "established", st.Established, "", time.Time{})

	// 只到 s1 的时刻（同微秒之前不存在，用更早时刻验证 500 < 800 不预警）。
	st = tr.Status("J1", t0.Add(-time.Microsecond))
	if st.Level != LevelNormal || st.Warning != nil {
		t.Fatalf("early: got %+v", st)
	}

	// 恰好 100% 建立关联。
	mustUpsert(t, tr, sale("s3", "J1", t1, 200))
	st = tr.Status("J1", t1)
	if st.Level != LevelEstablished {
		t.Fatalf("level: got %v want established", st.Level)
	}
	checkTrigger(t, "warning", st.Warning, "s2", t0)
	checkTrigger(t, "established", st.Established, "s3", t1)

	// 退款回落至 50：级别与首次触发点不变。
	mustUpsert(t, tr, refund("r1", "J1", t2, -950))
	st = tr.Status("J1", t2)
	if st.AmountCents != 50 {
		t.Fatalf("cum after refund: got %d", st.AmountCents)
	}
	if st.Level != LevelEstablished {
		t.Fatalf("level after refund: got %v want established", st.Level)
	}
	checkTrigger(t, "warning", st.Warning, "s2", t0)
	checkTrigger(t, "established", st.Established, "s3", t1)

	// 删除关键销售 s2：重放序列 s1(500) s3(700) r1(-250)，越线撤回。
	if !tr.Remove("s2") {
		t.Fatal("remove s2")
	}
	st = tr.Status("J1", t3)
	if st.Level != LevelNormal || st.Warning != nil || st.Established != nil {
		t.Fatalf("after remove: got %+v", st)
	}
	if st.AmountCents != -250 {
		t.Fatalf("cum after remove: got %d", st.AmountCents)
	}

	// 补录 s4=+350 于 t0（与 s1 同微秒，id 字节序 s1<s4）：
	// 预警触发点由消失的 s2 变为 s4，关联仍在 s3。
	mustUpsert(t, tr, sale("s4", "J1", t0, 350))
	st = tr.Status("J1", t3)
	checkTrigger(t, "warning", st.Warning, "s4", t0)
	checkTrigger(t, "established", st.Established, "s3", t1)

	// 更正 s4 为 +500：关联触发点前移到 s4（s1+s4=1000）。
	mustUpsert(t, tr, sale("s4", "J1", t0, 500))
	st = tr.Status("J1", t3)
	checkTrigger(t, "warning", st.Warning, "s4", t0)
	checkTrigger(t, "established", st.Established, "s4", t0)
	if st.Level != LevelEstablished {
		t.Fatalf("final level: got %v", st.Level)
	}
}

// 同一微秒内多笔流水按 id 字节序决定首次触发点，与写入顺序无关。
func TestSameMicrosecondIDOrder(t *testing.T) {
	tr, err := New(map[string]Thresholds{
		"J1": {AmountCents: 1 << 60, Sales: 10}, // 预警 8 笔，关联 10 笔
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{"h", "a", "g", "b", "f", "c", "e", "d"} // 写入顺序打乱
	for i, id := range ids {
		mustUpsert(t, tr, sale(id, "J1", t0, 1))
		st := tr.Status("J1", t0)
		wantSales := int64(i + 1)
		if st.Sales != wantSales {
			t.Fatalf("sales: got %d want %d", st.Sales, wantSales)
		}
	}
	st := tr.Status("J1", t0)
	// 字节序 a,b,c,d,e,f,g,h：第 8 笔是 "h"。
	checkTrigger(t, "warning", st.Warning, "h", t0)
	checkTrigger(t, "established", st.Established, "", time.Time{})
	if st.Level != LevelWarning {
		t.Fatalf("level: got %v", st.Level)
	}
	// 再补两笔（同一微秒），第 10 笔触发关联。
	mustUpsert(t, tr, sale("j", "J1", t0, 1))
	mustUpsert(t, tr, sale("i", "J1", t0, 1))
	st = tr.Status("J1", t0)
	checkTrigger(t, "established", st.Established, "j", t0)
}

// 年度独立：跨年不继承级别；退款归属发生年度；跨年移动改写两个年度。
func TestYearIndependenceAndCrossYearMove(t *testing.T) {
	tr, err := New(map[string]Thresholds{
		"J1": {AmountCents: 1000, Sales: 10000},
	})
	if err != nil {
		t.Fatal(err)
	}
	y25 := time.Date(2025, 12, 31, 23, 0, 0, 0, time.UTC)
	y26 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	mustUpsert(t, tr, sale("a", "J1", y25, 1000))
	st := tr.Status("J1", y25)
	if st.Level != LevelEstablished {
		t.Fatalf("2025: got %v", st.Level)
	}
	// 新年不继承。
	st = tr.Status("J1", y26)
	if st.Level != LevelNormal || st.AmountCents != 0 || st.Warning != nil {
		t.Fatalf("2026: got %+v", st)
	}
	// 跨年移动：a 移到 2026，2025 的关联被撤回，2026 建立。
	mustUpsert(t, tr, sale("a", "J1", y26, 1000))
	st = tr.Status("J1", y25)
	if st.Level != LevelNormal || st.Established != nil {
		t.Fatalf("2025 after move: got %+v", st)
	}
	st = tr.Status("J1", y26)
	checkTrigger(t, "established", st.Established, "a", y26)
	// 退款归属发生年度：2026 的退款不影响 2025。
	mustUpsert(t, tr, refund("r", "J1", y26, -100))
	st = tr.Status("J1", y25)
	if st.AmountCents != 0 {
		t.Fatalf("2025 cum: got %d", st.AmountCents)
	}
}

// 同一笔销售可同时首次触发预警与关联（此前无任何越线）。
func TestSimultaneousTrigger(t *testing.T) {
	tr, err := New(map[string]Thresholds{
		"J1": {AmountCents: 1000, Sales: 10000},
	})
	if err != nil {
		t.Fatal(err)
	}
	mustUpsert(t, tr, sale("big", "J1", t0, 1000))
	st := tr.Status("J1", t0)
	if st.Level != LevelEstablished {
		t.Fatalf("level: got %v", st.Level)
	}
	checkTrigger(t, "warning", st.Warning, "big", t0)
	checkTrigger(t, "established", st.Established, "big", t0)
}

// 辖区之间互不影响；未知辖区返回零值。
func TestJurisdictionIsolation(t *testing.T) {
	tr, err := New(map[string]Thresholds{
		"J1": {AmountCents: 100, Sales: 2},
		"J2": {AmountCents: 10000, Sales: 200},
	})
	if err != nil {
		t.Fatal(err)
	}
	mustUpsert(t, tr, sale("a", "J1", t0, 100))
	mustUpsert(t, tr, sale("b", "J1", t0.Add(time.Microsecond), 1))
	st1 := tr.Status("J1", t0.Add(time.Microsecond))
	if st1.Level != LevelEstablished {
		t.Fatalf("J1: got %v", st1.Level)
	}
	st2 := tr.Status("J2", t0.Add(time.Microsecond))
	if st2.Level != LevelNormal || st2.AmountCents != 0 {
		t.Fatalf("J2: got %+v", st2)
	}
	stX := tr.Status("JX", t0)
	if stX.Level != LevelNormal || stX.Warning != nil {
		t.Fatalf("JX: got %+v", stX)
	}
}
