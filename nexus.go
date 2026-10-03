// Package nexus 提供经济关联（economic nexus）门槛追溯组件。
//
// 税务人员补录、更正或删除销售流水后，可查询任一辖区在任一历史时刻
// 所在自然年（UTC）的累计净销售额、销售笔数、关联级别，
// 以及当年首次达到预警 / 建立关联门槛的流水与时间。
//
// 所有操作为进程内串行调用，无持久化。更新、删除、查询的摊还期望
// 时间均为 O(log N)，常驻内存 O(N)。
package nexus

import "time"

// Kind 流水类型。
type Kind int

const (
	// Sale 销售：金额为正（整数分），计 1 笔。
	Sale Kind = iota
	// Refund 退款：金额为负（整数分），计 0 笔。
	Refund
)

// Event 一条销售 / 退款流水。ID 全局唯一。
type Event struct {
	ID           string    // 全局唯一 id
	Jurisdiction string    // 辖区
	At           time.Time // UTC 发生时间，精确到微秒
	Kind         Kind      // 类型
	AmountCents  int64     // 金额（整数分）：销售为正，退款为负
}

// Level 关联级别。
type Level int

const (
	// LevelNone 正常：未达到预警门槛。
	LevelNone Level = iota
	// LevelWarning 预警：金额或笔数达到各自门槛的 80%。
	LevelWarning
	// LevelEstablished 已建立关联：任一指标达到 100%。
	LevelEstablished
)

func (l Level) String() string {
	switch l {
	case LevelNone:
		return "none"
	case LevelWarning:
		return "warning"
	case LevelEstablished:
		return "established"
	}
	return "unknown"
}

// Trigger 首次达到某门槛的流水与时间；未达时 Reached 为 false。
type Trigger struct {
	EventID string
	At      time.Time
	Reached bool
}

// Status 某辖区在 at 所在年度、从年初到 at（含 at 时刻全部流水）的状态。
type Status struct {
	NetSalesCents    int64   // 累计净销售额（分），允许为负
	SaleCount        int64   // 销售笔数（退款不计）
	Level            Level   // 当年曾达到的最高级别（按当前有效账本）
	FirstWarning     Trigger // 当年首次达到预警门槛的流水与时间
	FirstEstablished Trigger // 当年首次达到建立关联门槛的流水与时间
}

// Thresholds 单一辖区的门槛，均为正整数，运行期间不变。
type Thresholds struct {
	AmountCents int64 // 销售额门槛（分）
	Count       int64 // 交易笔数门槛
}

// Tracker 经济关联门槛追溯组件。非并发安全，调用方需串行使用。
type Tracker struct {
	thresholds map[string]Thresholds
	events     map[string]Event // id -> 当前有效流水
	years      map[yearKey]*node
}

// New 以各辖区门槛初始化组件。门槛均为正整数，运行期间不变。
func New(thresholds map[string]Thresholds) *Tracker {
	cp := make(map[string]Thresholds, len(thresholds))
	for j, th := range thresholds {
		cp[j] = th
	}
	return &Tracker{
		thresholds: cp,
		events:     make(map[string]Event),
		years:      make(map[yearKey]*node),
	}
}

// Upsert 新增流水，或以相同 id 完整替换旧流水（可改变时间、辖区、
// 类型、金额，包括跨年移动）；不会同时保留新旧贡献。
func (t *Tracker) Upsert(e Event) {
	e.At = e.At.UTC()
	if old, ok := t.events[e.ID]; ok {
		t.removeNode(old)
	}
	t.events[e.ID] = e
	k := yearKey{e.Jurisdiction, e.At.Year()}
	t.years[k] = insert(t.years[k], newNode(e))
}

// Remove 删除指定 id 的流水，使其不再参与任何历史查询。
// id 不存在时为空操作。
func (t *Tracker) Remove(id string) {
	old, ok := t.events[id]
	if !ok {
		return
	}
	t.removeNode(old)
	delete(t.events, id)
}

func (t *Tracker) removeNode(e Event) {
	k := yearKey{e.Jurisdiction, e.At.Year()}
	t.years[k] = remove(t.years[k], e.At, e.ID)
}

// warnTarget 返回门槛的 80% 向上取整（整数运算，不用浮点）：
// 最小的 v 使得 v >= threshold * 4 / 5。
func warnTarget(threshold int64) int64 {
	return (threshold*4 + 4) / 5
}

// Status 查询 jurisdiction 在 at 所在 UTC 自然年、从年初到 at 的状态，
// 包含 at 时刻的全部流水。
func (t *Tracker) Status(jurisdiction string, at time.Time) Status {
	at = at.UTC()
	th, ok := t.thresholds[jurisdiction]
	if !ok {
		return Status{}
	}
	k := yearKey{jurisdiction, at.Year()}
	root := t.years[k]
	if root == nil {
		return Status{}
	}

	// 分裂出“年初到 at（含 at 时刻全部流水）”的前缀，计算后合并回去。
	l, r := splitTime(root, at)
	defer func() { t.years[k] = merge(l, r) }()
	if l == nil {
		return Status{}
	}

	st := Status{NetSalesCents: l.sum, SaleCount: l.count}

	warnAmt, estAmt := warnTarget(th.AmountCents), th.AmountCents
	warnCnt, estCnt := warnTarget(th.Count), th.Count

	st.FirstWarning = firstTrigger(l, warnAmt, warnCnt)
	st.FirstEstablished = firstTrigger(l, estAmt, estCnt)

	switch {
	case st.FirstEstablished.Reached:
		st.Level = LevelEstablished
	case st.FirstWarning.Reached:
		st.Level = LevelWarning
	}
	return st
}

// firstTrigger 在树 l（某年度的一段前缀）上找当年首次同时满足
// “金额前缀 >= targetAmt 或 笔数前缀 >= targetCnt”的事件。
func firstTrigger(l *node, targetAmt, targetCnt int64) Trigger {
	byAmt := firstReachAmount(l, 0, targetAmt)
	byCnt := firstReachCount(l, 0, targetCnt)
	switch {
	case byAmt == nil && byCnt == nil:
		return Trigger{}
	case byAmt == nil:
		return Trigger{EventID: byCnt.id, At: byCnt.at, Reached: true}
	case byCnt == nil:
		return Trigger{EventID: byAmt.id, At: byAmt.at, Reached: true}
	case less(byCnt.at, byCnt.id, byAmt.at, byAmt.id):
		return Trigger{EventID: byCnt.id, At: byCnt.at, Reached: true}
	default:
		return Trigger{EventID: byAmt.id, At: byAmt.at, Reached: true}
	}
}
