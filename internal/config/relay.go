package config

// RelayConfig configures the Mode-B relay control plane (CPA fork per relay docs ch.6).
// In this fork the Claude provider never talks to Anthropic directly: requests are
// dispatched to per-account real Claude Code agents over local sockets, the gateway
// itself never opens a socket to api.anthropic.com, and the credential refresh path
// is deleted (the CC agent owns the OAuth token lifecycle, F3).
type RelayConfig struct {
	// Agents maps account IDs (auth IDs) to per-account agent endpoints.
	// Endpoint forms:
	//   unix:///run/relay/agent-<id>.sock   (Linux; also AF_UNIX on Windows 10+)
	//   tcp://127.0.0.1:7301                (dev only; loopback is enforced)
	Agents map[string]string `yaml:"agents" json:"agents"`

	// Store configures the persistent affinity + ban/quarantine authority (docs F8/F20).
	Store RelayStoreConfig `yaml:"store" json:"store"`

	// Admission controls the entry gate (docs 6.6.3). Requests a real coding agent
	// cannot faithfully serve (custom system, custom tool schemas, assistant prefill,
	// json-mode/stop sequences, non-coding shapes) are rejected with a documented 4xx.
	Admission RelayAdmissionConfig `yaml:"admission" json:"admission"`

	// Limits configures the per-account S2 rate limiting (docs 6.8).
	Limits RelayLimitsConfig `yaml:"limits" json:"limits"`

	// TurnTimeoutSeconds is the hard per-turn wall-clock cap (F11). Default 900.
	TurnTimeoutSeconds int `yaml:"turn-timeout-seconds" json:"turn-timeout-seconds"`

	// TurnMaxOutputTokens caps a single turn's output tokens (F11). 0 = no cap.
	TurnMaxOutputTokens int `yaml:"turn-max-output-tokens" json:"turn-max-output-tokens"`

	// QueueDepth bounds the per-account FIFO waiting for the in-flight turn.
	// Overflowing requests get 429 + Retry-After; they never spill to another account.
	// Default 8.
	QueueDepth int `yaml:"queue-depth" json:"queue-depth"`
}

// RelayStoreConfig selects the affinity + ban/quarantine authority (docs F8/F20:
// persistent, replicable, linearizable reads, fail-closed when the authority is
// unreachable or stale).
type RelayStoreConfig struct {
	// Driver: "rqlite" (default; replicated Raft authority per docs) or "file"
	// (single-node JSON, dev/tests only — not fleet-safe).
	Driver string `yaml:"driver" json:"driver"`

	// Path is the JSON file location for the "file" driver.
	// Default: <auth-dir>/relay-store.json
	Path string `yaml:"path" json:"path"`

	// RqliteURL is the rqlited HTTP endpoint for the "rqlite" driver.
	// Default: http://127.0.0.1:4001
	RqliteURL string `yaml:"rqlite-url" json:"rqlite-url"`
}

// RelayAdmissionConfig tunes the admission gate.
type RelayAdmissionConfig struct {
	// CodingGate: "strict" rejects traffic that does not look like coding (default);
	// "off" disables the heuristic classifier (structural checks always apply).
	CodingGate string `yaml:"coding-gate" json:"coding-gate"`

	// AllowTools permits tenant-declared tool schemas whose names are an exact subset
	// of the default Claude Code toolset (docs 6.6.3: only NON-default schemas are
	// rejected). Custom schemas are always rejected. Default true per docs.
	AllowTools *bool `yaml:"allow-tools" json:"allow-tools"`
}

// AllowDefaultToolset reports whether exact-name default CC toolset schemas are
// tolerated (docs 6.6.3 default: yes; custom schemas are never tolerated).
func (c RelayAdmissionConfig) AllowDefaultToolset() bool {
	if c.AllowTools == nil {
		return true
	}
	return *c.AllowTools
}

// RelayLimitsConfig configures fixed per-account token budgets (docs 6.8/C7).
// Budgets are fixed per account, seeded from the account ID, sized at a randomized
// 25-35% of the plan ceiling; realtime ratelimit headers are never fed back into pacing.
type RelayLimitsConfig struct {
	// PlanCap5h is the assumed per-account 5-hour plan ceiling in tokens (Sonnet bucket).
	// The enforced budget is a per-account seeded 25-35% of this. Default 220000 (Pro).
	PlanCap5h int64 `yaml:"plan-cap-5h" json:"plan-cap-5h"`

	// PlanCap7d is the assumed per-account 7-day plan ceiling in tokens. Default 0
	// disables the 7d window.
	PlanCap7d int64 `yaml:"plan-cap-7d" json:"plan-cap-7d"`

	// OpusPlanCap5h is the Opus bucket's 5h ceiling (separate accounting). Default 0
	// routes Opus usage into the Sonnet bucket.
	OpusPlanCap5h int64 `yaml:"opus-plan-cap-5h" json:"opus-plan-cap-5h"`

	// QuietHours enables the diurnal silence mask derived from each account's timezone
	// (docs 6.8.6): no dispatch during local night hours. Default true when relay enabled.
	QuietHours *bool `yaml:"quiet-hours" json:"quiet-hours"`

	// AccountTimezone maps account IDs to IANA timezones for the quiet mask and pacing.
	// Accounts without an entry use DefaultTimezone.
	AccountTimezone map[string]string `yaml:"account-timezone" json:"account-timezone"`

	// DefaultTimezone is used when an account has no explicit timezone. Default "UTC".
	DefaultTimezone string `yaml:"default-timezone" json:"default-timezone"`
}

// QuietHoursEnabled reports whether the diurnal quiet mask is active (default true).
func (c RelayLimitsConfig) QuietHoursEnabled() bool {
	if c.QuietHours == nil {
		return true
	}
	return *c.QuietHours
}
