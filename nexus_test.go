package nexus

import (
	"testing"
	"time"
)

func ts(day int, micro int64) time.Time {
	return time.Date(2025, 3, day, 0, 0, 0, int(micro)*1000, time.UTC)
}

func sale(id string, at time.Time, amt int64) Event {
	return Event{ID: id, Jurisdiction: "CA", At: at, Kind: Sale, AmountCents: amt}
}

func refund(id string, at time.Time, amt int64) Event {
	return Event{ID: id, Jurisdiction: "CA", At: at, Kind: Refund, AmountCents: -amt}
}

// 金额刚好 80% / 100%，随后退款回落：级别与首次触发点必须保持。
func TestBoundaryThenRefundKeepsLevel(t *testing.T) {
	tr := New(map[string]Thresholds{"CA": {AmountCents: 1000, Count: 1 << 40}})

	tr.Upsert(sale("a", ts(1, 0), 799))
	if st := tr.Status("CA", ts(1, 0)); st.Level != LevelNone {
		t.Fatalf("799: got %v, want none", st.Level)
	}
	tr.Upsert(sale("b", ts(2, 0), 1)) // 恰好 800 = 80%
	st := tr.Status("CA", ts(2, 0))
	if st.Level != LevelWarning {
		t.Fatalf("800: got %v, want warning", st.Level)
	}
	if !st.FirstWarning.Reached || st.FirstWarning.EventID != "b" || !st.FirstWarning.At.Equal(ts(2, 0)) {
		t.Fatalf("first warning = %+v, want b@day2", st.FirstWarning)
	}
	if st.FirstEstablished.Reached {
		t.Fatalf("established should not be reached, got %+v", st.FirstEstablished)
	}

	tr.Upsert(sale("c", ts(3, 0), 200)) // 恰好 1000 = 100%
	st = tr.Status("CA", ts(3, 0))
	if st.Level != LevelEstablished {
		t.Fatalf("1000: got %v, want established", st.Level)
	}
	if !st.FirstEstablished.Reached || st.FirstEstablished.EventID != "c" {
		t.Fatalf("first established = %+v, want c@day3", st.FirstEstablished)
	}
	if st.FirstWarning.EventID != "b" {
		t.Fatalf("first warning should stay b, got %+v", st.FirstWarning)
	}

	// 退款使余额回落到门槛之下：级别与首次触发点不得降低 / 消失。
	tr.Upsert(refund("r1", ts(4, 0), 950)) // 余额 50
	st = tr.Status("CA", ts(4, 0))
	if st.NetSalesCents != 50 {
		t.Fatalf("net = %d, want 50", st.NetSalesCents)
	}
	if st.Level != LevelEstablished {
		t.Fatalf("after refund: got %v, want established", st.Level)
	}
	if st.FirstWarning.EventID != "b" || st.FirstEstablished.EventID != "c" {
		t.Fatalf("triggers moved: %+v %+v", st.FirstWarning, st.FirstEstablished)
	}
}

// 删除关键销售后，旧的越线结论必须撤回（触发点后移或消失）。
func TestDeleteRetractsCrossing(t *testing.T) {
	tr := New(map[string]Thresholds{"CA": {AmountCents: 1000, Count: 1 << 40}})
	tr.Upsert(sale("a", ts(1, 0), 600))
	tr.Upsert(sale("b", ts(2, 0), 400)) // 触发 established
	tr.Upsert(refund("r", ts(3, 0), 900))
	tr.Upsert(sale("c", ts(4, 0), 1300)) // 余额 1400，但首次越线仍是 b

	st := tr.Status("CA", ts(4, 0))
	if st.FirstEstablished.EventID != "b" || st.FirstWarning.EventID != "b" {
		t.Fatalf("triggers = %+v %+v, want both b", st.FirstWarning, st.FirstEstablished)
	}

	tr.Remove("b") // 删除关键销售：a(600) r(-900) c(1300) → 首次越线后移到 c
	st = tr.Status("CA", ts(4, 0))
	if st.Level != LevelEstablished {
		t.Fatalf("level = %v, want established", st.Level)
	}
	if st.FirstWarning.EventID != "c" || st.FirstEstablished.EventID != "c" {
		t.Fatalf("triggers = %+v %+v, want both c@day4", st.FirstWarning, st.FirstEstablished)
	}
	if !st.FirstWarning.At.Equal(ts(4, 0)) {
		t.Fatalf("warning time = %v, want day4", st.FirstWarning.At)
	}

	tr.Remove("c") // 再删：越线彻底消失
	st = tr.Status("CA", ts(4, 0))
	if st.Level != LevelNone || st.FirstWarning.Reached || st.FirstEstablished.Reached {
		t.Fatalf("after removing c: %+v", st)
	}
}

// 同一微秒内按 id 的 UTF-8 字节序裁决，与写入顺序无关。
func TestSameMicrosecondIDOrder(t *testing.T) {
	tr := New(map[string]Thresholds{"CA": {AmountCents: 1000, Count: 1 << 40}})
	at := ts(1, 42)
	// 逆序写入：id 字节序仍应决定先后。
	tr.Upsert(sale("z", at, 500))
	tr.Upsert(sale("a", at, 500))
	st := tr.Status("CA", at)
	if st.FirstEstablished.EventID != "z" {
		t.Fatalf("first = %+v, want z (a→500, z→1000)", st.FirstEstablished)
	}
	// 查询 at 之前一微秒：两条都不计入。
	if st := tr.Status("CA", ts(1, 41)); st.Level != LevelNone || st.NetSalesCents != 0 {
		t.Fatalf("before instant: %+v", st)
	}
	// 把 z 改成退款：a→500, z→0，越线消失。
	tr.Upsert(Event{ID: "z", Jurisdiction: "CA", At: at, Kind: Refund, AmountCents: -500})
	if st := tr.Status("CA", at); st.Level != LevelNone || st.NetSalesCents != 0 {
		t.Fatalf("after replace: %+v", st)
	}
}

// 一次销售同时首次触发预警与建立关联。
func TestSingleSaleTriggersBoth(t *testing.T) {
	tr := New(map[string]Thresholds{"CA": {AmountCents: 1000, Count: 1 << 40}})
	tr.Upsert(sale("big", ts(1, 0), 1000))
	st := tr.Status("CA", ts(1, 0))
	if st.Level != LevelEstablished {
		t.Fatalf("level = %v", st.Level)
	}
	if st.FirstWarning.EventID != "big" || st.FirstEstablished.EventID != "big" {
		t.Fatalf("triggers = %+v %+v", st.FirstWarning, st.FirstEstablished)
	}
}

// 笔数门槛：80% 向上取整，退款不计笔数。
func TestCountThreshold(t *testing.T) {
	tr := New(map[string]Thresholds{"CA": {AmountCents: 1 << 60, Count: 5}})
	// 笔数预警线 = ceil(5*0.8) = 4。
	for i, id := range []string{"a", "b", "c"} {
		tr.Upsert(sale(id, ts(1, int64(i)), 1))
	}
	if st := tr.Status("CA", ts(1, 10)); st.Level != LevelNone {
		t.Fatalf("3 sales: %v", st.Level)
	}
	tr.Upsert(sale("d", ts(1, 3), 1)) // 第 4 笔 → 预警
	st := tr.Status("CA", ts(1, 10))
	if st.Level != LevelWarning || st.FirstWarning.EventID != "d" || st.SaleCount != 4 {
		t.Fatalf("4 sales: %+v", st)
	}
	tr.Upsert(refund("r", ts(1, 4), 1)) // 退款不计笔数
	tr.Upsert(sale("e", ts(1, 5), 1))   // 第 5 笔 → 建立关联
	st = tr.Status("CA", ts(1, 10))
	if st.Level != LevelEstablished || st.FirstEstablished.EventID != "e" || st.SaleCount != 5 {
		t.Fatalf("5 sales: %+v", st)
	}
}

// 新年不继承上年级别；跨年移动只保留一份贡献。
func TestYearBoundaryAndCrossYearMove(t *testing.T) {
	tr := New(map[string]Thresholds{"CA": {AmountCents: 1000, Count: 1 << 40}})
	y2024 := time.Date(2024, 12, 31, 23, 59, 59, 0, time.UTC)
	y2025 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	tr.Upsert(sale("a", y2024, 2000))
	if st := tr.Status("CA", y2024); st.Level != LevelEstablished {
		t.Fatalf("2024: %v", st.Level)
	}
	// 2025 年不继承。
	if st := tr.Status("CA", y2025); st.Level != LevelNone || st.NetSalesCents != 0 {
		t.Fatalf("2025 should be clean: %+v", st)
	}
	// 跨年移动：a 从 2024 移到 2025。
	tr.Upsert(sale("a", y2025, 2000))
	if st := tr.Status("CA", y2024); st.Level != LevelNone || st.NetSalesCents != 0 {
		t.Fatalf("2024 after move: %+v", st)
	}
	st := tr.Status("CA", y2025)
	if st.Level != LevelEstablished || st.NetSalesCents != 2000 || st.FirstEstablished.EventID != "a" {
		t.Fatalf("2025 after move: %+v", st)
	}
}

// 辖区相互独立；未知辖区返回零状态。
func TestJurisdictionIsolation(t *testing.T) {
	tr := New(map[string]Thresholds{
		"CA": {AmountCents: 1000, Count: 1 << 40},
		"NY": {AmountCents: 1 << 60, Count: 2},
	})
	tr.Upsert(sale("a", ts(1, 0), 1000))
	tr.Upsert(Event{ID: "b", Jurisdiction: "NY", At: ts(1, 0), Kind: Sale, AmountCents: 1})
	if st := tr.Status("NY", ts(1, 0)); st.Level != LevelNone || st.SaleCount != 1 {
		t.Fatalf("NY: %+v", st)
	}
	if st := tr.Status("ZZ", ts(1, 0)); st != (Status{}) {
		t.Fatalf("unknown jurisdiction: %+v", st)
	}
}

// 同一时刻多条流水、一天内多次越线与回落，逐笔判断。
func TestMultipleCrossingsWithinADay(t *testing.T) {
	tr := New(map[string]Thresholds{"CA": {AmountCents: 100, Count: 1 << 40}})
	d := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	us := func(n int64) time.Time { return d.Add(time.Duration(n) * time.Microsecond) }
	tr.Upsert(sale("s1", us(1), 80))    // 预警
	tr.Upsert(sale("s2", us(2), 20))    // 建立关联
	tr.Upsert(refund("r1", us(3), 100)) // 回落到 0
	tr.Upsert(sale("s3", us(4), 80))    // 再次越预警线（非首次）
	st := tr.Status("CA", us(4))
	if st.Level != LevelEstablished {
		t.Fatalf("level = %v", st.Level)
	}
	if st.FirstWarning.EventID != "s1" || st.FirstEstablished.EventID != "s2" {
		t.Fatalf("triggers = %+v %+v", st.FirstWarning, st.FirstEstablished)
	}
	if st.NetSalesCents != 80 {
		t.Fatalf("net = %d, want 80", st.NetSalesCents)
	}
	// 中间时刻：us(1) 只见到 s1。
	st = tr.Status("CA", us(1))
	if st.Level != LevelWarning || st.FirstEstablished.Reached {
		t.Fatalf("at us(1): %+v", st)
	}
}
