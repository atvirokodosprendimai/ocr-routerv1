package identity

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
)

// MinBufferLimit is the smallest in-flight cap a customer may be given.
//
// ⚠ ZERO IS REFUSED, and it is the one validation here that is not obvious.
// Admission compares a customer's in-flight jobs against this limit, so a limit
// of 0 rejects every upload they ever make — reported to them as `buffer full`,
// which reads exactly like ordinary back-pressure from a busy system. An
// operator who set it to 0 meaning "stop this customer" would have stopped them
// in a way neither side can diagnose. `active` is what stops a customer, and the
// refusal message says so.
const MinBufferLimit = 1

// UpdateSettings changes a customer's operator-tunable settings.
//
// The admin check comes FIRST, before validation: an unauthorised caller must
// not be able to learn which values are valid by watching which errors come
// back.
func (s *Service) UpdateSettings(ctx context.Context, actor core.Principal, userID string,
	bufferLimit, priority, jobTTLSecs int,
) error {
	if !actor.IsAdmin() {
		return core.ErrForbidden
	}

	if bufferLimit < MinBufferLimit {
		return fmt.Errorf("%w: buffer limit must be at least %d; use the active toggle to stop a customer",
			core.ErrInvalidParam, MinBufferLimit)
	}
	if jobTTLSecs < 0 {
		// Zero is VALID and means no deadline — a real and common choice, so this
		// is `< 0` and never `<= 0`.
		return fmt.Errorf("%w: job TTL cannot be negative; zero means no deadline",
			core.ErrInvalidParam)
	}
	// priority is deliberately unconstrained, negatives included. Higher wins,
	// and ADR-0001 made it an integer rather than a named tier so that adding a
	// tier is an UPDATE rather than a deploy.

	return s.repo.SetUserSettings(ctx, userID, bufferLimit, priority, jobTTLSecs)
}

// AdjustCredits moves a customer's balance by a signed delta and records why.
//
// ⚠ THERE IS NO METHOD THAT SETS A BALANCE, HERE OR ANYWHERE. credit_entries is
// the append-only audit of every movement, and Repo.AddCredits writes the
// balance and the entry in one transaction. A direct write would move the
// balance with no entry, and the ledger would stop being an audit of every
// movement with nothing failing to say so — while ADR-0002's
// ocrr_credits_debited_total continues to promise that the two can be
// reconciled.
//
// The reason is REQUIRED. This is the only field an administrator can change
// that directly moves money, and an adjustment with no reason is
// indistinguishable from a mistake six months later.
func (s *Service) AdjustCredits(ctx context.Context, actor core.Principal, userID string,
	delta int, reason string, now time.Time,
) error {
	if !actor.IsAdmin() {
		return core.ErrForbidden
	}

	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("%w: a reason is required for a credit adjustment", core.ErrInvalidParam)
	}
	if delta == 0 {
		// An entry recording that nothing happened is noise in the one log that
		// has to stay readable.
		return fmt.Errorf("%w: a credit adjustment cannot be zero", core.ErrInvalidParam)
	}

	// The user has to exist: AddCredits' UPDATE would otherwise match no row and
	// still write a ledger entry, leaving an orphan that reconciles against
	// nothing.
	if _, err := s.repo.UserByID(ctx, userID); err != nil {
		return err
	}

	// ⚠ No floor on how far negative this may take the balance. ADR-0001 already
	// allows a balance to go negative by design, and an admin correcting a debtor
	// downward is legitimate. Deliberate, not an oversight.
	return s.repo.AddCredits(ctx, userID, delta, "admin: "+reason, now)
}
