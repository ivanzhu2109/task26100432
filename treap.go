package econtrace

import (
	"math/rand"
	"time"
)

// eventKey 年度内排序键：先按发生时间，同一时间按 id 的 UTF-8 字节序升序。
type eventKey struct {
	at time.Time
	id string
}

func lessKey(a, b eventKey) bool {
	if !a.at.Equal(b.at) {
		return a.at.Before(b.at)
	}
	return a.id < b.id
}

// node 是按 eventKey 排序的 treap 节点（键定序、随机优先级堆）。
// 子树聚合对中序序列（即排序后的流水序列）维护金额/笔数的
// 合计与前缀和最大值，用于 O(logN) 定位“首次越线”的流水。
type node struct {
	key  eventKey
	ev   Event
	prio uint64

	left, right *node

	sumAmt     int64 // 子树金额合计
	maxPrefAmt int64 // 子树中序序列的金额前缀和最大值（含空前缀 0）
	sumCnt     int64 // 子树销售笔数合计
	maxPrefCnt int64 // 子树中序序列的笔数前缀和最大值（含空前缀 0）
}

func cntDelta(ev Event) int64 {
	if ev.Type == Sale {
		return 1
	}
	return 0
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func sumAmtOf(n *node) int64 {
	if n == nil {
		return 0
	}
	return n.sumAmt
}

func maxPrefAmtOf(n *node) int64 {
	if n == nil {
		return 0
	}
	return n.maxPrefAmt
}

func sumCntOf(n *node) int64 {
	if n == nil {
		return 0
	}
	return n.sumCnt
}

func maxPrefCntOf(n *node) int64 {
	if n == nil {
		return 0
	}
	return n.maxPrefCnt
}

// pull 由左右子树与自身重新计算聚合。
// 中序序列 = left ++ [self] ++ right，前缀和最大值据此合并。
func (n *node) pull() {
	ls, rs := sumAmtOf(n.left), sumAmtOf(n.right)
	lc, rc := sumCntOf(n.left), sumCntOf(n.right)
	selfCnt := cntDelta(n.ev)
	n.sumAmt = ls + n.ev.AmountCents + rs
	n.maxPrefAmt = max64(maxPrefAmtOf(n.left),
		max64(ls+n.ev.AmountCents, ls+n.ev.AmountCents+maxPrefAmtOf(n.right)))
	n.sumCnt = lc + selfCnt + rc
	n.maxPrefCnt = max64(maxPrefCntOf(n.left),
		max64(lc+selfCnt, lc+selfCnt+maxPrefCntOf(n.right)))
}

func rotateRight(n *node) *node {
	l := n.left
	n.left = l.right
	l.right = n
	n.pull()
	l.pull()
	return l
}

func rotateLeft(n *node) *node {
	r := n.right
	n.right = r.left
	r.left = n
	n.pull()
	r.pull()
	return r
}

// treap 是单个（辖区, 年度）的有序流水集合。
type treap struct {
	root *node
	rng  *rand.Rand
}

func newTreap(rng *rand.Rand) *treap {
	return &treap{rng: rng}
}

func (t *treap) insert(ev Event) {
	key := eventKey{at: ev.At, id: ev.ID}
	t.root = insertNode(t.root, key, ev, t.rng.Uint64())
}

func insertNode(n *node, key eventKey, ev Event, prio uint64) *node {
	if n == nil {
		nd := &node{key: key, ev: ev, prio: prio}
		nd.pull()
		return nd
	}
	if lessKey(key, n.key) {
		n.left = insertNode(n.left, key, ev, prio)
		if n.left.prio < n.prio {
			n = rotateRight(n)
		}
	} else {
		n.right = insertNode(n.right, key, ev, prio)
		if n.right.prio < n.prio {
			n = rotateLeft(n)
		}
	}
	n.pull()
	return n
}

func (t *treap) erase(key eventKey) {
	t.root = eraseNode(t.root, key)
}

func eraseNode(n *node, key eventKey) *node {
	if n == nil {
		return nil
	}
	switch {
	case lessKey(key, n.key):
		n.left = eraseNode(n.left, key)
	case lessKey(n.key, key):
		n.right = eraseNode(n.right, key)
	default:
		return mergeNodes(n.left, n.right)
	}
	n.pull()
	return n
}

// mergeNodes 合并两棵键域不相交（left 全部小于 right）的 treap。
func mergeNodes(l, r *node) *node {
	switch {
	case l == nil:
		return r
	case r == nil:
		return l
	case l.prio < r.prio:
		l.right = mergeNodes(l.right, r)
		l.pull()
		return l
	default:
		r.left = mergeNodes(l, r.left)
		r.pull()
		return r
	}
}

// splitAt 将集合拆为发生时间 <= at 与 > at 两部分；调用方负责随后 merge 还原。
func (t *treap) splitAt(at time.Time) (le, gt *treap) {
	l, r := splitNodeAt(t.root, at)
	return &treap{root: l, rng: t.rng}, &treap{root: r, rng: t.rng}
}

func splitNodeAt(n *node, at time.Time) (le, gt *node) {
	if n == nil {
		return nil, nil
	}
	if !n.key.at.After(at) {
		// n 及其左子树全部 <= at，递归拆右子树。
		l, r := splitNodeAt(n.right, at)
		n.right = l
		n.pull()
		return n, r
	}
	l, r := splitNodeAt(n.left, at)
	n.left = r
	n.pull()
	return l, n
}

// merge 把 other 合并回本集合，要求本集合所有键小于 other 所有键。
func (t *treap) merge(other *treap) {
	t.root = mergeNodes(t.root, other.root)
	other.root = nil
}

// firstReach 返回中序序列中首个使前缀和 >= threshold 的节点；
// useCount 为 true 时按笔数口径，否则按金额口径。threshold 必须为正。
// base 为序列之前的累计值（本实现中调用处恒为 0）。
func firstReach(n *node, threshold, base int64, useCount bool) *node {
	for n != nil {
		var lsum, lmax, self int64
		if useCount {
			lsum, lmax, self = sumCntOf(n.left), maxPrefCntOf(n.left), cntDelta(n.ev)
		} else {
			lsum, lmax, self = sumAmtOf(n.left), maxPrefAmtOf(n.left), n.ev.AmountCents
		}
		if base+lmax >= threshold {
			n = n.left
			continue
		}
		if base+lsum+self >= threshold {
			return n
		}
		base += lsum + self
		n = n.right
	}
	return nil
}
