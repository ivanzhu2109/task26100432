package econtrace

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"
)

// replayStatus 小规模逐笔重放参考实现：对当前有效账本按
// (时间, id 字节序) 排序后逐笔重放到 at，独立推导状态。
func replayStatus(events map[string]Event, th Thresholds, jurisdiction string, at time.Time) Status {
	at = at.UTC().Truncate(time.Microsecond)
	var seq []Event
	for _, ev := range events {
		if ev.Jurisdiction == jurisdiction && ev.At.Year() == at.Year() && !ev.At.After(at) {
			seq = append(seq, ev)
		}
	}
	sort.Slice(seq, func(i, j int) bool {
		a, b := seq[i], seq[j]
		if !a.At.Equal(b.At) {
			return a.At.Before(b.At)
		}
		return a.ID < b.ID
	})
	warnAmt := th.AmountCents - th.AmountCents/5
	warnCnt := th.Sales - th.Sales/5
	st := Status{}
	for _, ev := range seq {
		st.AmountCents += ev.AmountCents
		if ev.Type == Sale {
			st.Sales++
		}
		if st.Warning == nil && (st.AmountCents >= warnAmt || st.Sales >= warnCnt) {
			st.Warning = &Trigger{EventID: ev.ID, At: ev.At}
		}
		if st.Established == nil && (st.AmountCents >= th.AmountCents || st.Sales >= th.Sales) {
			st.Established = &Trigger{EventID: ev.ID, At: ev.At}
		}
	}
	switch {
	case st.Established != nil:
		st.Level = LevelEstablished
	case st.Warning != nil:
		st.Level = LevelWarning
	}
	return st
}

func equalTrigger(a, b *Trigger) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.EventID == b.EventID && a.At.Equal(b.At)
}

// 固定随机种子混合补录、替换（含跨辖区、跨年移动）、删除与历史查询，
// 逐项与重放参考实现比对。
func TestRandomizedAgainstReplay(t *testing.T) {
	ths := map[string]Thresholds{
		"J1": {AmountCents: 500, Sales: 8},
		"J2": {AmountCents: 2000, Sales: 5},
		"J3": {AmountCents: 300, Sales: 1000},
	}
	jurs := []string{"J1", "J2", "J3"}
	tr, err := New(ths)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(20261003))
	base := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	randTime := func() time.Time {
		// 跨 3 个年度，集中在少数微秒上以制造同刻冲突。
		return base.Add(time.Duration(rng.Intn(3*365*24*3600)) * time.Second).
			Add(time.Duration(rng.Intn(40)) * time.Microsecond)
	}
	ref := make(map[string]Event)
	var liveIDs []string
	nextID := 0

	for op := 0; op < 6000; op++ {
		switch rng.Intn(10) {
		case 0, 1, 2, 3, 4: // upsert 新增或替换
			var id string
			if len(liveIDs) > 0 && rng.Intn(3) == 0 {
				id = liveIDs[rng.Intn(len(liveIDs))] // 替换既有 id，可能跨年/跨辖区
			} else {
				id = fmt.Sprintf("ev-%d", nextID)
				nextID++
			}
			typ := Sale
			amt := int64(rng.Intn(700) + 1)
			if rng.Intn(4) == 0 {
				typ = Refund
				amt = -int64(rng.Intn(700) + 1)
			}
			ev := Event{ID: id, Jurisdiction: jurs[rng.Intn(len(jurs))], At: randTime(), Type: typ, AmountCents: amt}
			if _, existed := ref[id]; !existed {
				liveIDs = append(liveIDs, id)
			}
			if err := tr.Upsert(ev); err != nil {
				t.Fatalf("op %d upsert: %v", op, err)
			}
			ref[id] = ev
		case 5, 6: // remove
			if len(liveIDs) == 0 {
				continue
			}
			i := rng.Intn(len(liveIDs))
			id := liveIDs[i]
			if tr.Remove(id) != true {
				t.Fatalf("op %d remove %s failed", op, id)
			}
			delete(ref, id)
			liveIDs = append(liveIDs[:i], liveIDs[i+1:]...)
		default: // status 历史查询
			jur := jurs[rng.Intn(len(jurs))]
			at := randTime()
			got := tr.Status(jur, at)
			want := replayStatus(ref, ths[jur], jur, at)
			if got.AmountCents != want.AmountCents || got.Sales != want.Sales ||
				got.Level != want.Level ||
				!equalTrigger(got.Warning, want.Warning) ||
				!equalTrigger(got.Established, want.Established) {
				t.Fatalf("op %d status %s@%v:\n got  %+v\n want %+v", op, jur, at, got, want)
			}
		}
	}
	// 收尾：对所有辖区、若干随机时刻全量比对一次。
	for i := 0; i < 200; i++ {
		jur := jurs[rng.Intn(len(jurs))]
		at := randTime()
		got := tr.Status(jur, at)
		want := replayStatus(ref, ths[jur], jur, at)
		if got.AmountCents != want.AmountCents || got.Sales != want.Sales ||
			got.Level != want.Level ||
			!equalTrigger(got.Warning, want.Warning) ||
			!equalTrigger(got.Established, want.Established) {
			t.Fatalf("final status %s@%v:\n got  %+v\n want %+v", jur, at, got, want)
		}
	}
}
