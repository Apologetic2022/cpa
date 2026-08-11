package relay

import "fmt"

// RejectError is a documented 4xx returned to tenants whose traffic the constrained
// coding-agent surface cannot faithfully serve (docs 6.6.2/6.6.3). The message always
// points at the official API: for that traffic this architecture offers zero safety
// gain over an ordinary proxy, so there is no reason to carry its risk.
type RejectError struct {
	Status int
	Reason string
}

func (e *RejectError) Error() string { return e.Reason }

// StatusCode implements the executor StatusError contract.
func (e *RejectError) StatusCode() int { return e.Status }

func reject(status int, format string, args ...any) *RejectError {
	return &RejectError{Status: status, Reason: fmt.Sprintf(format, args...)}
}

// QuotaError is a 429 with a Retry-After hint, returned when the bound account is at
// its fixed budget or its bounded FIFO is full (docs 6.8: never spill to another account).
type QuotaError struct {
	Reason     string
	RetryAfter int // seconds
}

func (e *QuotaError) Error() string { return e.Reason }

// StatusCode implements the executor StatusError contract.
func (e *QuotaError) StatusCode() int { return 429 }

// BanError rejects dispatch when the bound account is quarantined/banned or the ban
// store cannot be read (fail-closed, docs F20).
type BanError struct {
	Reason string
	Status int
}

func (e *BanError) Error() string { return e.Reason }

// StatusCode implements the executor StatusError contract.
func (e *BanError) StatusCode() int {
	if e.Status != 0 {
		return e.Status
	}
	return 503
}
