package opendesign

import "time"

const (
	billingAPIURL        = "https://amr-api.open-design.ai"
	quotaRefreshInterval = time.Minute
	quotaRefreshBudget   = 30 * time.Second
	maxQuotaBody         = 1 << 20
)

// Quota data is advisory and independent of CPA scheduling. All amounts retain
// the official decimal strings; a nil data pointer means unknown, never zero.
type quotaResponse struct {
	CredentialID      string                      `json:"credential_id"`
	BillingConfigured bool                        `json:"billing_configured"`
	WorkspaceID       string                      `json:"workspace_id"`
	State             string                      `json:"state"`
	FetchedAt         *time.Time                  `json:"fetched_at"`
	NextRefreshAt     *time.Time                  `json:"next_refresh_at"`
	Wallet            quotaPart[quotaWalletData]  `json:"wallet"`
	Funding           quotaPart[quotaFundingData] `json:"funding"`
}

type quotaPart[T any] struct {
	State     string      `json:"state"`
	FetchedAt *time.Time  `json:"fetched_at"`
	Data      *T          `json:"data"`
	Error     *quotaError `json:"error"`
}

type quotaError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

type quotaWalletData struct {
	BalanceUSD string  `json:"balanceUsd"`
	UpdatedAt  *string `json:"updatedAt"`
}

type quotaFundingData struct {
	BalanceUSD   string          `json:"balanceUsd"`
	Funding      string          `json:"funding"`
	ModelCovered *bool           `json:"modelCovered"`
	FundingCheck *string         `json:"fundingCheck,omitempty"`
	CodingPlan   quotaCodingPlan `json:"codingPlan"`

	// Validated official values; never included in management JSON or logs.
	generatedAt       time.Time
	workspaceMemberID string
}

type quotaCodingPlan struct {
	Tier    *string       `json:"tier"`
	Windows []quotaWindow `json:"windows"`
}

type quotaWindow struct {
	PolicyID         string        `json:"policyId"`
	DurationSeconds  int64         `json:"durationSeconds"`
	ResetMode        string        `json:"resetMode"`
	UsedCredits      string        `json:"usedCredits"`
	LimitCredits     string        `json:"limitCredits"`
	RemainingCredits string        `json:"remainingCredits"`
	WindowStart      *string       `json:"windowStart"`
	ResetsAt         *string       `json:"resetsAt"`
	Forecast         quotaForecast `json:"forecast"`
}

type quotaForecast struct {
	State            string     `json:"state"`
	SecondsToExhaust *int64     `json:"seconds_to_exhaust,omitempty"`
	ExhaustsAt       *time.Time `json:"exhausts_at,omitempty"`
	Basis            string     `json:"basis,omitempty"`
}

// quotaEntry and all its bookkeeping are guarded by Service.mu. Published data
// pointers are immutable, so copying a view out of the lock is safe.
type quotaEntry struct {
	epoch       uint64
	view        quotaResponse
	lastAttempt time.Time
	inflight    chan struct{}
	invalidated chan struct{}
	samples     map[quotaWindowIdentity]quotaWindowSample
}
