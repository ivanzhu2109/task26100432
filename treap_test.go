package econtrace

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"
)

func collect(n *node, out *[]Event) {
	if n == nil {
		return
	}
	collect(n.left, out)
	*out = append(*out, n.ev)
	collect(n.right, out)
}

func checkAggs(t *testing.T, n *node) {
	t.Helper()
	if n == nil {
		return
	}
	checkAggs(t, n.left)
	checkAggs(t, n.right)
	var seq []Event
	collect(n, &seq)
	var sumA, maxA, sumC, maxC, curA, curC int64
	for _, ev := range seq {
		curA += ev.AmountCents
		curC += cntDelta(ev)
		sumA += ev.AmountCents
		sumC += cntDelta(ev)
		maxA = max64(maxA, curA)
		maxC = max64(maxC, curC)
	}
	if n.sumAmt != sumA || n.maxPrefAmt != maxA || n.sumCnt != sumC || n.maxPrefCnt != maxC {
		t.Fatalf("agg mismatch at %v: got (%d,%d,%d,%d) want (%d,%d,%d,%d)",
			n.key, n.sumAmt, n.maxPrefAmt, n.sumCnt, n.maxPrefCnt, sumA, maxA, sumC, maxC)
	}
}

func TestTreapInsertEraseSplitMerge(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	tr := newTreap(rng)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var ref []Event
	// 插入 2000 条随机流水（含同一时间不同 id）。
	for i := 0; i < 2000; i++ {
		ev := Event{
			ID:          fmt.Sprintf("id-%04d", rng.Intn(1500)),
			At:          base.Add(time.Duration(rng.Intn(500)) * time.Microsecond),
			Type:        Sale,
			AmountCents: int64(rng.Intn(2001) - 1000),
		}
		if rng.Intn(3) == 0 {
			ev.Type = Refund
			ev.AmountCents = -int64(rng.Intn(1000) + 1)
		}
		dup := false
		for _, e := range ref {
			if e.ID == ev.ID {
				dup = true
			}
		}
		if dup {
			continue
		}
		tr.insert(ev)
		ref = append(ref, ev)
		checkAggs(t, tr.root)
	}
	sort.Slice(ref, func(i, j int) bool {
		return lessKey(eventKey{ref[i].At, ref[i].ID}, eventKey{ref[j].At, ref[j].ID})
	})
	var got []Event
	collect(tr.root, &got)
	if len(got) != len(ref) {
		t.Fatalf("size %d != %d", len(got), len(ref))
	}
	for i := range ref {
		if got[i].ID != ref[i].ID {
			t.Fatalf("order mismatch at %d", i)
		}
	}
	// split / merge 还原。
	at := base.Add(250 * time.Microsecond)
	le, gt := tr.splitAt(at)
	var leSeq, gtSeq []Event
	collect(le.root, &leSeq)
	collect(gt.root, &gtSeq)
	for _, ev := range leSeq {
		if ev.At.After(at) {
			t.Fatalf("le contains %v after at", ev)
		}
	}
	for _, ev := range gtSeq {
		if !ev.At.After(at) {
			t.Fatalf("gt contains %v not after at", ev)
		}
	}
	if len(leSeq)+len(gtSeq) != len(ref) {
		t.Fatalf("split lost events")
	}
	checkAggs(t, le.root)
	checkAggs(t, gt.root)
	le.merge(gt)
	var merged []Event
	collect(le.root, &merged)
	if len(merged) != len(ref) {
		t.Fatalf("merge lost events")
	}
	for i := range ref {
		if merged[i].ID != ref[i].ID {
			t.Fatalf("merge order mismatch at %d", i)
		}
	}
	// 删除一半。
	for i := 0; i < len(ref); i += 2 {
		le.erase(eventKey{ref[i].At, ref[i].ID})
		checkAggs(t, le.root)
	}
	var remain []Event
	collect(le.root, &remain)
	if len(remain) != len(ref)/2 {
		t.Fatalf("remain %d", len(remain))
	}
}

func TestFirstReach(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	tr := newTreap(rng)
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	amts := []int64{100, -50, 80, -200, 300, -40, 90}
	var pref []int64
	cur := int64(0)
	for i, a := range amts {
		tr.insert(Event{
			ID:          fmt.Sprintf("e%d", i),
			At:          base.Add(time.Duration(i) * time.Microsecond),
			Type:        Sale,
			AmountCents: a,
		})
		cur += a
		pref = append(pref, cur)
	}
	for _, th := range []int64{1, 50, 100, 130, 280, 320, 1000} {
		want := -1
		for i, p := range pref {
			if p >= th {
				want = i
				break
			}
		}
		got := firstReach(tr.root, th, 0, false)
		if want < 0 {
			if got != nil {
				t.Fatalf("th=%d: want nil got %v", th, got.key)
			}
			continue
		}
		if got == nil || got.key.id != fmt.Sprintf("e%d", want) {
			t.Fatalf("th=%d: want e%d got %+v", th, want, got)
		}
	}
}
