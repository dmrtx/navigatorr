package action

import "reflect"

func objectMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// FrozenHardware reads the media engine choice frozen into the size policy
// that holder carries under "mode_policy" (a batch's state, or one item's
// goal-change row): search rules present mean on with that margin, absent
// mean off. ok is false when holder has no size policy, so a quality, compact
// or HEVC attempt reports no choice and so does an operation not frozen yet.
func FrozenHardware(holder any) (allow bool, margin any, ok bool) {
	size := objectMap(objectMap(objectMap(holder)["mode_policy"])["size"])
	if size == nil {
		return false, nil, false
	}
	rules := objectMap(size["hardware_search"])
	if rules == nil {
		return false, nil, true
	}
	return true, rules["margin_percent"], true
}

// BatchItemFrozenHardware is the choice one batch item runs under: its own
// frozen policy when a goal change gave it one, else the batch's. state is the
// batch's decoded state.
func BatchItemFrozenHardware(state map[string]any, itemKey string) (allow bool, margin any, ok bool) {
	holder := any(state)
	if row := objectMap(objectMap(state["batch_item_settings"])[itemKey]); objectMap(row["mode_policy"]) != nil {
		holder = row
	}
	return FrozenHardware(holder)
}

// HardwareChoice summarises the frozen media engine choices of several files:
// the shared choice when they agree, and Mixed when they do not.
type HardwareChoice struct {
	Known  bool
	Allow  bool
	Margin any
	Mixed  bool
}

// Add records one file's frozen choice.
func (c *HardwareChoice) Add(allow bool, margin any) {
	switch {
	case !c.Known:
		c.Known, c.Allow, c.Margin = true, allow, margin
	case c.Allow != allow || !reflect.DeepEqual(c.Margin, margin):
		c.Mixed = true
	}
}
