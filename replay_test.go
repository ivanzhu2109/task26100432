package nexus

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"
)

// replayStatus 是逐笔重放参考实现：对当前有效账本按
// (时间, id 字节序) 排序后逐笔扫描到 at，O(N)。
type replayLedger struct {
	thresholds map[string]Thresholds
	events     map[string]Event
}

func newReplayLedger(th map[string]Thresholds) *replayLedger {
	return &replayLedger{thresholds: th, events: make(map[string]Event)}
}

func (r *replayLedger) Upsert(e Event)   { r.events[e.ID] = e }
func (r *replayLedger) Remove(id string) { delete(r.events, id) }

func (r *replayLedger) Status(jurisdiction string, at time.Time) Status {
	th, ok := r.thresholds[jurisdiction]
	if !ok {
		return Status{}
	}
	at = at.UTC()
	year := at.Year()
	var evs []Event
	for _, e := range r.events {
		if e.Jurisdiction == jurisdiction && e.At.UTC().Year() == year && !e.At.UTC().After(at) {
			evs = append(evs, e)
		}
	}
	sort.Slice(evs, func(i, j int) bool {
		a, b := evs[i].At.UTC(), evs[j].At.UTC()
		if !a.Equal(b) {
			return a.Before(b)
		}
		return evs[i].ID < evs[j].ID
	})

	warnAmt, estAmt := warnTarget(th.AmountCents), th.AmountCents
	warnCnt, estCnt := warnTarget(th.Count), th.Count

	var st Status
	for _, e := range evs {
		st.NetSalesCents += e.AmountCents
		if e.Kind == Sale {
			st.SaleCount++
		}
		lvl := LevelNone
		if st.NetSalesCents >= estAmt || st.SaleCount >= estCnt {
			lvl = LevelEstablished
		} else if st.NetSalesCents >= warnAmt || st.SaleCount >= warnCnt {
			lvl = LevelWarning
		}
		if lvl >= LevelWarning && !st.FirstWarning.Reached {
			st.FirstWarning = Trigger{EventID: e.ID, At: e.At.UTC(), Reached: true}
		}
		if lvl >= LevelEstablished && !st.FirstEstablished.Reached {
			st.FirstEstablished = Trigger{EventID: e.ID, At: e.At.UTC(), Reached: true}
		}
		if lvl > st.Level {
			st.Level = lvl
		}
	}
	return st
}

func sameStatus(t *testing.T, got, want Status, ctx string) {
	t.Helper()
	if got.NetSalesCents != want.NetSalesCents ||
		got.SaleCount != want.SaleCount ||
		got.Level != want.Level {
		t.Fatalf("%s: got {net:%d cnt:%d lvl:%v}, want {net:%d cnt:%d lvl:%v}",
			ctx, got.NetSalesCents, got.SaleCount, got.Level,
			want.NetSalesCents, want.SaleCount, want.Level)
	}
	pairs := []struct {
		name string
		got  Trigger
		want Trigger
	}{
		{"warning", got.FirstWarning, want.FirstWarning},
		{"established", got.FirstEstablished, want.FirstEstablished},
	}
	for _, p := range pairs {
		if p.got.Reached != p.want.Reached || p.got.EventID != p.want.EventID || !p.got.At.Equal(p.want.At) {
			t.Fatalf("%s: first-%s = %+v, want %+v", ctx, p.name, p.got, p.want)
		}
	}
}

// 固定随机种子：混合补录、替换（含跨辖区 / 跨年移动）、删除与历史查询，
// 与参考实现逐项对比。
func TestRandomizedDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(20251004))
	thresholds := map[string]Thresholds{
		"CA": {AmountCents: 500, Count: 6},
		"NY": {AmountCents: 900, Count: 4},
		"TX": {AmountCents: 300, Count: 10},
	}
	juris := []string{"CA", "NY", "TX"}
	tr := New(thresholds)
	ref := newReplayLedger(thresholds)

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	span := int64(3 * 365 * 24 * time.Hour / time.Microsecond) // 2024-2026，微秒
	randTime := func() time.Time { return base.Add(time.Duration(rng.Int63n(span)) * time.Microsecond).UTC() }

	var ids []string
	nextID := 0
	newEvent := func() Event {
		id := fmt.Sprintf("ev-%d", nextID)
		nextID++
		ids = append(ids, id)
		kind := Sale
		amt := int64(rng.Intn(400) + 1)
		if rng.Intn(4) == 0 {
			kind = Refund
			amt = -amt
		}
		return Event{ID: id, Jurisdiction: juris[rng.Intn(len(juris))],
			At: randTime(), Kind: kind, AmountCents: amt}
	}

	for step := 0; step < 30000; step++ {
		switch op := rng.Intn(100); {
		case op < 45 || len(ids) == 0: // 新增
			e := newEvent()
			tr.Upsert(e)
			ref.Upsert(e)
		case op < 65: // 替换：可改时间 / 辖区 / 类型 / 金额（含跨年移动）
			id := ids[rng.Intn(len(ids))]
			e := newEvent()
			e.ID = id
			tr.Upsert(e)
			ref.Upsert(e)
		case op < 80: // 删除
			i := rng.Intn(len(ids))
			tr.Remove(ids[i])
			ref.Remove(ids[i])
			ids = append(ids[:i], ids[i+1:]...)
		default: // 历史查询（随机时刻，不限当前）
			j := juris[rng.Intn(len(juris))]
			at := randTime()
			sameStatus(t, tr.Status(j, at), ref.Status(j, at),
				fmt.Sprintf("step %d status(%s, %v)", step, j, at))
		}
	}
	// 收尾：对所有辖区在年末做一次全量对比。
	for _, j := range juris {
		for _, y := range []int{2024, 2025, 2026} {
			at := time.Date(y, 12, 31, 23, 59, 59, 999999000, time.UTC)
			sameStatus(t, tr.Status(j, at), ref.Status(j, at),
				fmt.Sprintf("final status(%s, %v)", j, at))
		}
	}
	t.Logf("final live events: %d", len(ids))
}
