// Package econtrace 提供经济关联门槛追溯组件：
// 税务人员补录、更正、删除销售流水后，可查询任一历史时刻的年度累计状态。
package econtrace

import "time"

// EventType 流水类型。
type EventType int

const (
	// Sale 销售：金额为正（整数分），计 1 笔。
	Sale EventType = iota
	// Refund 退款：金额为负（整数分），计 0 笔。
	Refund
)

// Event 一条流水。ID 全局唯一；At 为精确到微秒的 UTC 发生时间。
type Event struct {
	ID           string
	Jurisdiction string
	At           time.Time
	Type         EventType
	AmountCents  int64
}

// Level 关联级别。
type Level int

const (
	// LevelNormal 正常：预警与关联门槛均未达到。
	LevelNormal Level = iota
	// LevelWarning 预警：金额或笔数达到各自门槛的 80%。
	LevelWarning
	// LevelEstablished 已建立关联：任一指标达到门槛的 100%。
	LevelEstablished
)

// Trigger 某级别在本年度的首次触发点。
type Trigger struct {
	EventID string
	At      time.Time
}

// Status 某辖区在 at 时刻（含 at）的年度状态。
type Status struct {
	AmountCents int64    // 年初至 at 的累计净销售额（整数分，可为负）
	Sales       int64    // 年初至 at 的销售笔数
	Level       Level    // 年初至 at 曾达到的最高级别
	Warning     *Trigger // 本年度首次达到预警门槛的流水；未达到为 nil
	Established *Trigger // 本年度首次达到关联门槛的流水；未达到为 nil
}
