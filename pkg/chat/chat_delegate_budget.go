package chat

const (
	DefaultWakeReserve      = 5
	DefaultWakeReserveRatio = 0.10
)

func (h *ChatHarness) CurrentTurn() int {
	if h == nil {
		return 0
	}
	return h.currentTurn
}

func (h *ChatHarness) SubAgentBudgetCap() int {
	if h == nil {
		return 0
	}
	maxTurns := h.MaxTurns
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}
	turnsUsed := h.CurrentTurn()
	if turnsUsed < 0 {
		turnsUsed = 0
	}
	if turnsUsed > maxTurns {
		turnsUsed = maxTurns
	}
	reserve := h.WakeReserve
	if reserve <= 0 {
		reserve = DefaultWakeReserve
	}
	ratio := h.WakeReserveRatio
	if ratio <= 0 {
		ratio = DefaultWakeReserveRatio
	}
	ratioReserve := int(float64(maxTurns) * ratio)
	if ratioReserve > reserve {
		reserve = ratioReserve
	}
	cap := maxTurns - turnsUsed - reserve
	if cap < 1 {
		return 1
	}
	return cap
}
