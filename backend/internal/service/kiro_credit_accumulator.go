package service

import (
	"context"
	"log/slog"
	"time"
)

// kiroDefaultCreditLimit is the baseline credit quota shown in the UI for
// every Kiro account. Kiro's free tier gives ~1000 credits/month; paid
// tiers give more but can also overshoot. The UI shows "used / limit" as
// a progress bar; exceeding the limit is allowed (overage).
const kiroDefaultCreditLimit = 1000.0

// accumulateKiroCreditUsed adds the given credit amount to the account's
// extra.kiro_credit_used field. Uses UpdateExtra (jsonb merge) which is
// not perfectly atomic under concurrent writes, but Kiro requests are
// serialised per-account via sticky sessions + concurrency control so
// the race window is negligible in practice.
func (s *GatewayService) accumulateKiroCreditUsed(ctx context.Context, accountID int64, credit float64) {
	if credit <= 0 {
		return
	}
	// Read current value, add, write back. Not perfectly atomic but good
	// enough given the serialisation guarantees on the Kiro path.
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// Fetch current account to read existing credit_used.
		account, err := s.accountRepo.GetByID(bgCtx, accountID)
		if err != nil {
			slog.Warn("kiro credit accumulate: fetch account failed",
				"account_id", accountID, "error", err)
			return
		}

		var current float64
		if account.Extra != nil {
			if v, ok := account.Extra["kiro_credit_used"].(float64); ok {
				current = v
			}
		}

		newTotal := current + credit
		if err := s.accountRepo.UpdateExtra(bgCtx, accountID, map[string]any{
			"kiro_credit_used": newTotal,
		}); err != nil {
			slog.Warn("kiro credit accumulate: update failed",
				"account_id", accountID, "error", err)
		}
	}()
}

// GetKiroCreditUsed returns the accumulated credit usage for a Kiro
// account from its extra field. Returns 0 if not set.
func GetKiroCreditUsed(account *Account) float64 {
	if account == nil || account.Extra == nil {
		return 0
	}
	if v, ok := account.Extra["kiro_credit_used"].(float64); ok {
		return v
	}
	return 0
}

// GetKiroCreditLimit returns the credit limit for a Kiro account.
// Currently a fixed constant; could be made per-account in the future
// via account.Extra["kiro_credit_limit"].
func GetKiroCreditLimit(account *Account) float64 {
	if account != nil && account.Extra != nil {
		if v, ok := account.Extra["kiro_credit_limit"].(float64); ok && v > 0 {
			return v
		}
	}
	return kiroDefaultCreditLimit
}
