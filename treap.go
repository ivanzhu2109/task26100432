package nexus

import (
	"math/rand/v2"
	"time"
)

// yearKey 定位一个（辖区, UTC 自然年）的有序事件集合。
type yearKey struct {
	jurisdiction string
	year         int
}

// node 是按 (发生时间, id 的 UTF-8 字节序) 排序的笛卡尔树（treap）节点。
// 子树维护前缀和所需的聚合信息，支持 O(log N) 期望的插入、删除、
// 按时间分裂，以及“首个使累计值达到门槛的事件”查询。
type node struct {
	at  time.Time
	id  string
	amt int64 // 金额（分），销售为正、退款为负
	cnt int64 // 销售计 1，退款计 0
	pri uint64

	left, right *node

	sum     int64 // 子树金额合计
	count   int64 // 子树笔数合计
	maxPref int64 // 子树内金额前缀和的最大值（空前缀不计）
	maxPCnt int64 // 子树内笔数前缀和的最大值（空前缀不计）
}

// less 按 (时间, id 字节序) 比较两个键。
func less(atA time.Time, idA string, atB time.Time, idB string) bool {
	if !atA.Equal(atB) {
		return atA.Before(atB)
	}
	return idA < idB
}

// pull 由子节点与自身值重算聚合信息。
func pull(n *node) {
	n.sum = n.amt
	n.count = n.cnt
	n.maxPref = n.amt
	n.maxPCnt = n.cnt
	if l := n.left; l != nil {
		n.sum = l.sum + n.amt
		n.count = l.count + n.cnt
		n.maxPref = max(l.maxPref, l.sum+n.amt)
		n.maxPCnt = max(l.maxPCnt, l.count+n.cnt)
	}
	if r := n.right; r != nil {
		n.maxPref = max(n.maxPref, n.sum+r.maxPref)
		n.maxPCnt = max(n.maxPCnt, n.count+r.maxPCnt)
		n.sum += r.sum
		n.count += r.count
	}
}

// split 把树按键 (at, id) 分裂：l 含所有键 < (at, id) 的节点，r 含其余。
func split(root *node, at time.Time, id string) (l, r *node) {
	if root == nil {
		return nil, nil
	}
	if less(root.at, root.id, at, id) {
		ll, rr := split(root.right, at, id)
		root.right = ll
		pull(root)
		return root, rr
	}
	ll, rr := split(root.left, at, id)
	root.left = rr
	pull(root)
	return ll, root
}

// splitTime 把树按时间分裂：l 含所有 at <= t 的节点（含该时刻全部 id），
// r 含其余。
func splitTime(root *node, t time.Time) (l, r *node) {
	if root == nil {
		return nil, nil
	}
	if !root.at.After(t) {
		ll, rr := splitTime(root.right, t)
		root.right = ll
		pull(root)
		return root, rr
	}
	ll, rr := splitTime(root.left, t)
	root.left = rr
	pull(root)
	return ll, root
}

// merge 合并两棵树，要求 l 中所有键都小于 r 中所有键。
func merge(l, r *node) *node {
	if l == nil {
		return r
	}
	if r == nil {
		return l
	}
	if l.pri > r.pri {
		l.right = merge(l.right, r)
		pull(l)
		return l
	}
	r.left = merge(l, r.left)
	pull(r)
	return r
}

// insert 插入新节点（键必须不存在）。
func insert(root, n *node) *node {
	l, r := split(root, n.at, n.id)
	return merge(merge(l, n), r)
}

// remove 删除键 (at, id) 对应的节点；不存在时原样返回。
func remove(root *node, at time.Time, id string) *node {
	if root == nil {
		return nil
	}
	if less(at, id, root.at, root.id) {
		root.left = remove(root.left, at, id)
		pull(root)
		return root
	}
	if less(root.at, root.id, at, id) {
		root.right = remove(root.right, at, id)
		pull(root)
		return root
	}
	return merge(root.left, root.right)
}

// firstReachAmount 返回中序首个使“base + 金额前缀和 >= target”的节点；
// 不存在时返回 nil。调用方应保证 base 之前的前缀均未达标。
func firstReachAmount(n *node, base, target int64) *node {
	for n != nil {
		var lsum, lmax int64
		if n.left != nil {
			lsum = n.left.sum
			lmax = n.left.maxPref
			if base+lmax >= target {
				n = n.left
				continue
			}
		}
		if base+lsum+n.amt >= target {
			return n
		}
		base += lsum + n.amt
		n = n.right
	}
	return nil
}

// firstReachCount 返回中序首个使“base + 笔数前缀和 >= target”的节点；
// 不存在时返回 nil。
func firstReachCount(n *node, base, target int64) *node {
	for n != nil {
		var lsum, lmax int64
		if n.left != nil {
			lsum = n.left.count
			lmax = n.left.maxPCnt
			if base+lmax >= target {
				n = n.left
				continue
			}
		}
		if base+lsum+n.cnt >= target {
			return n
		}
		base += lsum + n.cnt
		n = n.right
	}
	return nil
}

// newNode 构造叶节点。
func newNode(e Event) *node {
	n := &node{at: e.At, id: e.ID, amt: e.AmountCents, pri: rand.Uint64()}
	if e.Kind == Sale {
		n.cnt = 1
	}
	pull(n)
	return n
}
