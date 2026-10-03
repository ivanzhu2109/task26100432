package econtrace

import (
	"errors"
	"fmt"
	"math/rand"
	"time"
)

// Thresholds 单个辖区的年度门槛，均为正整数，运行期间不变。
type Thresholds struct {
	AmountCents int64 // 销售额门槛（整数分）
	Sales       int64 // 交易笔数门槛
}

// yearKey 索引单个（辖区, UTC 自然年）的有序流水集合。
type yearKey struct {
	jurisdiction string
	year         int
}

// Tracker 经济关联门槛追溯组件。非并发安全，调用方需串行使用。
type Tracker struct {
	thresholds map[string]Thresholds
	events     map[string]Event   // id -> 当前有效流水
	years      map[yearKey]*treap // (辖区, 年度) -> 有序流水集合
	rng        *rand.Rand
}

// New 以各辖区门槛初始化组件。门槛必须为正整数。
func New(thresholds map[string]Thresholds) (*Tracker, error) {
	th := make(map[string]Thresholds, len(thresholds))
	for j, v := range thresholds {
		if v.AmountCents <= 0 || v.Sales <= 0 {
			return nil, fmt.Errorf("econtrace: jurisdiction %q: thresholds must be positive", j)
		}
		th[j] = v
	}
	return &Tracker{
		thresholds: th,
		events:     make(map[string]Event),
		years:      make(map[yearKey]*treap),
		rng:        rand.New(rand.NewSource(time.Now().UnixNano())),
	}, nil
}

// 预警门槛 = ceil(门槛 * 4 / 5)，整数计算，不溢出、不使用浮点。
func warnThreshold(v int64) int64 { return v - v/5 }

// validate 校验流水字段并归一化（UTC、微秒精度）。
func (t *Tracker) validate(ev Event) (Event, error) {
	if ev.ID == "" {
		return ev, errors.New("econtrace: empty event id")
	}
	if _, ok := t.thresholds[ev.Jurisdiction]; !ok {
		return ev, fmt.Errorf("econtrace: unknown jurisdiction %q", ev.Jurisdiction)
	}
	switch ev.Type {
	case Sale:
		if ev.AmountCents <= 0 {
			return ev, fmt.Errorf("econtrace: sale %q must have positive amount", ev.ID)
		}
	case Refund:
		if ev.AmountCents >= 0 {
			return ev, fmt.Errorf("econtrace: refund %q must have negative amount", ev.ID)
		}
	default:
		return ev, fmt.Errorf("econtrace: event %q has unknown type", ev.ID)
	}
	ev.At = ev.At.UTC().Truncate(time.Microsecond)
	return ev, nil
}

// Upsert 新增流水，或以相同 id 完整替换旧流水。
// 替换可改变时间、辖区、类型和金额（含跨年移动），旧贡献立即撤销。
func (t *Tracker) Upsert(ev Event) error {
	ev, err := t.validate(ev)
	if err != nil {
		return err
	}
	if old, ok := t.events[ev.ID]; ok {
		t.eraseFromYear(old)
	}
	t.events[ev.ID] = ev
	yk := yearKey{ev.Jurisdiction, ev.At.Year()}
	tr, ok := t.years[yk]
	if !ok {
		tr = newTreap(t.rng)
		t.years[yk] = tr
	}
	tr.insert(ev)
	return nil
}

// Remove 删除指定 id 的流水；id 不存在时返回 false。
func (t *Tracker) Remove(id string) bool {
	ev, ok := t.events[id]
	if !ok {
		return false
	}
	t.eraseFromYear(ev)
	delete(t.events, id)
	return true
}

func (t *Tracker) eraseFromYear(ev Event) {
	yk := yearKey{ev.Jurisdiction, ev.At.Year()}
	if tr, ok := t.years[yk]; ok {
		tr.erase(eventKey{at: ev.At, id: ev.ID})
	}
}

// earlier 返回排序键更靠前的节点（nil 视为无穷靠后）。
func earlier(a, b *node) *node {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case lessKey(a.key, b.key):
		return a
	default:
		return b
	}
}

func triggerOf(n *node) *Trigger {
	if n == nil {
		return nil
	}
	return &Trigger{EventID: n.ev.ID, At: n.ev.At}
}

// Status 查询辖区在 at 时刻（含 at）的年度状态。
// 结果与对当前有效账本按 (时间, id) 逐笔重放到 at 完全一致。
func (t *Tracker) Status(jurisdiction string, at time.Time) Status {
	th, ok := t.thresholds[jurisdiction]
	if !ok {
		return Status{}
	}
	at = at.UTC().Truncate(time.Microsecond)
	yk := yearKey{jurisdiction, at.Year()}
	tr, ok := t.years[yk]
	if !ok || tr.root == nil {
		return Status{}
	}

	// 拆出发生时间 <= at 的前缀，计算后立即合并还原，均摊 O(logN)。
	le, gt := tr.splitAt(at)
	defer func() { le.merge(gt); tr.root = le.root }()

	root := le.root
	st := Status{
		AmountCents: sumAmtOf(root),
		Sales:       sumCntOf(root),
	}

	warnAmt, warnCnt := warnThreshold(th.AmountCents), warnThreshold(th.Sales)
	warn := earlier(
		firstReach(root, warnAmt, 0, false),
		firstReach(root, warnCnt, 0, true),
	)
	est := earlier(
		firstReach(root, th.AmountCents, 0, false),
		firstReach(root, th.Sales, 0, true),
	)

	st.Warning = triggerOf(warn)
	st.Established = triggerOf(est)
	switch {
	case est != nil:
		st.Level = LevelEstablished
	case warn != nil:
		st.Level = LevelWarning
	default:
		st.Level = LevelNormal
	}
	return st
}
