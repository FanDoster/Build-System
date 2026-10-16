package models

// AgentNotificationPreferences controls Telegram only, by actual agent name.
// Missing storage means all enabled, not Go's zero value.
type AgentNotificationPreferences struct {
	Enabled      bool `json:"enabled"`
	Availability bool `json:"availability"`
	Health       bool `json:"health"`
	Builds       bool `json:"builds"`
}

func DefaultAgentNotificationPreferences() AgentNotificationPreferences {
	return AgentNotificationPreferences{Enabled: true, Availability: true, Health: true, Builds: true}
}
