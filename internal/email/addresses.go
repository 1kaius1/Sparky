// SPDX-License-Identifier: AGPL-3.0-or-later

package email

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/mailer"
	"github.com/1kaius1/Sparky/internal/rbac"
)

const (
	// confirmationTTL is how long a confirmation link works.
	confirmationTTL = 24 * time.Hour

	// confirmationCooldown is the shortest time between two requests for one
	// person, so the form cannot be used to flood an address with mail.
	confirmationCooldown = time.Minute
)

var (
	// ErrDirectoryManaged is returned when someone tries to edit the address of
	// an AD-backed account: it comes from the directory at every login.
	ErrDirectoryManaged = errors.New("this account's address comes from the directory")

	// ErrNoPublicURL is returned when a confirmation link was needed but
	// SPARKY_PUBLIC_URL is not set, so there is nowhere for the link to point.
	ErrNoPublicURL = errors.New("SPARKY_PUBLIC_URL is not set, so a confirmation link cannot be sent")

	// ErrTooSoon is returned for a second request inside the cooldown.
	ErrTooSoon = errors.New("a confirmation email was sent a moment ago; wait a minute before asking again")

	// ErrUnchanged is returned when the address asked for is already the one
	// in use.
	ErrUnchanged = errors.New("that is already the address in use")

	// ErrConfirmationNotFound is returned for a token that matches nothing, was
	// replaced by a newer request, or belongs to someone else. The three are
	// deliberately indistinguishable.
	ErrConfirmationNotFound = errors.New("this confirmation link is not valid")

	// ErrConfirmationExpired is returned for a token past its 24 hours.
	ErrConfirmationExpired = errors.New("this confirmation link has expired; ask for a new one")
)

// userEmailStore is the subset of *db.UserEmailRepository this package needs.
type userEmailStore interface {
	Get(ctx context.Context, userID string) (*db.UserEmail, error)
	ListAll(ctx context.Context) (map[string]*db.UserEmail, error)
	SetDirectory(ctx context.Context, userID string, email *string) (bool, error)
	SetLocal(ctx context.Context, userID string, email *string, source db.EmailSource) (bool, error)
	PutConfirmation(ctx context.Context, c db.EmailConfirmation) error
	PendingConfirmation(ctx context.Context, userID string) (*db.EmailConfirmation, error)
	ConfirmationByTokenHash(ctx context.Context, hash string) (*db.EmailConfirmation, error)
	DeleteConfirmation(ctx context.Context, userID string) (bool, error)

	SetPreferences(ctx context.Context, userID string, delivery db.EmailDelivery, minSeverity db.AlertSeverity) error
	RecipientsForAlert(ctx context.Context, alertID string, severity db.AlertSeverity) ([]db.EmailRecipient, error)
	AddDigestItem(ctx context.Context, userID, alertID string, severity db.AlertSeverity, title string) error
	UsersWithDigestItems(ctx context.Context, mode db.EmailDelivery) ([]string, error)
	PendingDigest(ctx context.Context, userID string) ([]db.DigestItem, error)
	DeleteDigestItems(ctx context.Context, userID string, alertIDs []string) error
	DeleteDigestItemsOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
	DeleteExpiredConfirmations(ctx context.Context, now time.Time) (int64, error)
}

// RecordDirectoryEmail stores the address AD gave for an AD-backed account at
// sign-in, or clears it when AD gave none or something that is not an email
// address. It never fails a sign-in (it returns nothing; a problem is logged),
// and is not audited, like the display name refresh that happens at the same
// moment: it is the directory's data arriving, not a person acting.
func (s *Service) RecordDirectoryEmail(ctx context.Context, userID, raw string) {
	var addr *string
	if raw != "" {
		if a, err := mailer.ParseAddress(raw); err == nil {
			addr = &a
		} else {
			s.logger.Printf("email: the directory's mail attribute for user %s is not an email address, ignoring it", userID)
		}
	}
	if _, err := s.users.SetDirectory(ctx, userID, addr); err != nil {
		s.logger.Printf("email: record the directory address for user %s: %v", userID, err)
	}
}

// AddressView is one person's address as their Account page shows it.
type AddressView struct {
	Email  string
	Source db.EmailSource

	// Directory is true for an AD-backed account: the address is read from the
	// directory and cannot be edited here.
	Directory bool

	// Pending is a change waiting for its owner to confirm, nil if none.
	Pending *PendingView

	// EmailOn and CanConfirm say whether a change can be asked for right now:
	// email must be turned on, and SPARKY_PUBLIC_URL set so the link has
	// somewhere to point.
	EmailOn    bool
	CanConfirm bool

	// Delivery and MinSeverity are how this person wants alert email: off,
	// immediately or in a digest, and the lowest severity they want.
	Delivery    db.EmailDelivery
	MinSeverity db.AlertSeverity

	// DailyDigestTime and Zone say when a daily digest goes out, for the
	// Account page to tell the person.
	DailyDigestTime string
	Zone            string
}

// PendingView is a change of address waiting to be confirmed.
type PendingView struct {
	Email     string
	ExpiresAt time.Time
	ByAdmin   bool
}

// MyAddress returns one person's address for their Account page.
func (s *Service) MyAddress(ctx context.Context, userID string) (*AddressView, error) {
	u, err := s.users.Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	cfg, err := s.settings.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("get email settings: %w", err)
	}
	v := &AddressView{Directory: u.Directory, EmailOn: cfg.Enabled, CanConfirm: cfg.Enabled && s.env.PublicURL != "",
		Delivery: u.Delivery, MinSeverity: u.MinSeverity,
		DailyDigestTime: cfg.DailyDigestTime, Zone: s.location(ctx).String()}
	if u.Email != nil && u.Source != nil {
		v.Email, v.Source = *u.Email, *u.Source
	}
	if p, err := s.users.PendingConfirmation(ctx, userID); err == nil {
		if p.ExpiresAt.After(s.now()) {
			v.Pending = &PendingView{Email: p.Email, ExpiresAt: p.ExpiresAt, ByAdmin: p.Source == db.EmailSourceAdmin}
		}
	} else if !errors.Is(err, db.ErrEmailConfirmationNotFound) {
		return nil, fmt.Errorf("get pending address: %w", err)
	}
	return v, nil
}

// newToken makes a confirmation token and its stored hash.
func newToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate a confirmation token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// startConfirmation records a pending change and queues the email that carries
// its link to the NEW address - so only someone who can read that mailbox can
// confirm it. If anything after the record fails, the record is removed, so no
// half-started confirmation is left behind.
func (s *Service) startConfirmation(ctx context.Context, userID, address string, source db.EmailSource, requestedBy *string) error {
	cfg, err := s.settings.Get(ctx)
	if err != nil {
		return fmt.Errorf("get email settings: %w", err)
	}
	if !cfg.Enabled {
		return ErrDisabled
	}
	if s.env.PublicURL == "" {
		return ErrNoPublicURL
	}
	if p, err := s.users.PendingConfirmation(ctx, userID); err == nil && s.now().Sub(p.CreatedAt) < confirmationCooldown {
		return ErrTooSoon
	}
	token, hash, err := newToken()
	if err != nil {
		return err
	}
	now := s.now()
	if err := s.users.PutConfirmation(ctx, db.EmailConfirmation{
		UserID: userID, Email: address, TokenHash: hash, Source: source, RequestedBy: requestedBy,
		CreatedAt: now, ExpiresAt: now.Add(confirmationTTL),
	}); err != nil {
		return fmt.Errorf("record the pending change: %w", err)
	}
	link := s.env.PublicURL + "/account/email/confirm?token=" + token
	if err := s.Enqueue(ctx, Message{
		Kind: db.EmailKindConfirmation, To: address, UserID: &userID,
		Subject: "[Sparky] Confirm your email address", Body: confirmationBody(link, source == db.EmailSourceAdmin),
	}); err != nil {
		_, _ = s.users.DeleteConfirmation(ctx, userID)
		return fmt.Errorf("queue the confirmation email: %w", err)
	}
	return nil
}

func confirmationBody(link string, byAdmin bool) string {
	who := "You asked"
	if byAdmin {
		who = "A Sparky Admin asked"
	}
	return who + " to use this address for Sparky alert emails.\n\n" +
		"To confirm it, sign in to Sparky as yourself and open this link:\n\n" + link + "\n\n" +
		"The link works for 24 hours, and only while you are signed in as the account it was meant for. " +
		"Until you confirm, alert email keeps going to your current address, if you have one.\n\n" +
		"If you did not expect this, ignore this email and nothing will change.\n"
}

// RequestMyAddress starts a change of a local account's own address: a
// confirmation link is emailed to the new address and nothing changes until it
// is opened. An AD-backed account cannot do this (ErrDirectoryManaged). Audited
// as requested_email_confirmation.
func (s *Service) RequestMyAddress(ctx context.Context, userID, address string) error {
	u, err := s.users.Get(ctx, userID)
	if err != nil {
		return err
	}
	if u.Directory {
		return ErrDirectoryManaged
	}
	addr, err := mailer.ParseAddress(address)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSettings, err)
	}
	if u.Email != nil && *u.Email == addr {
		return ErrUnchanged
	}
	if err := s.startConfirmation(ctx, userID, addr, db.EmailSourceSelf, nil); err != nil {
		return err
	}
	if err := s.audit.Record(ctx, &userID, false, "requested_email_confirmation", "user", userID, map[string]any{"email": addr}); err != nil {
		_, _ = s.users.DeleteConfirmation(ctx, userID)
		return fmt.Errorf("record audit: %w", err)
	}
	return nil
}

// ClearMyAddress removes a local account's own address (and any change waiting
// to be confirmed). Audited as cleared_email_address; a failed audit write puts
// the address back.
func (s *Service) ClearMyAddress(ctx context.Context, userID string) error {
	u, err := s.users.Get(ctx, userID)
	if err != nil {
		return err
	}
	if u.Directory {
		return ErrDirectoryManaged
	}
	if err := s.setLocalAudited(ctx, u, nil, db.EmailSourceSelf, &userID, false, "cleared_email_address", map[string]any{}); err != nil {
		return err
	}
	_, _ = s.users.DeleteConfirmation(ctx, userID)
	return nil
}

// setLocalAudited changes a local account's address and records it, undoing the
// change if the record cannot be written.
func (s *Service) setLocalAudited(ctx context.Context, before *db.UserEmail, next *string, source db.EmailSource, actorID *string, superAdmin bool, action string, detail map[string]any) error {
	changed, err := s.users.SetLocal(ctx, before.UserID, next, source)
	if err != nil {
		return fmt.Errorf("update the address: %w", err)
	}
	if !changed {
		return nil
	}
	if before.Email != nil {
		detail["previous_email"] = *before.Email
	}
	if next != nil {
		detail["email"] = *next
	}
	if err := s.audit.Record(ctx, actorID, superAdmin, action, "user", before.UserID, detail); err != nil {
		auditErr := fmt.Errorf("record audit: %w", err)
		prevSource := source
		if before.Source != nil {
			prevSource = *before.Source
		}
		if _, revertErr := s.users.SetLocal(ctx, before.UserID, before.Email, prevSource); revertErr != nil {
			return fmt.Errorf("%w (revert also failed: %v)", auditErr, revertErr)
		}
		return auditErr
	}
	return nil
}

// ConfirmationView is what the confirm page shows before the person presses the
// button.
type ConfirmationView struct {
	Email     string
	ExpiresAt time.Time
	ByAdmin   bool
}

// lookup finds the pending change for a token, only if it belongs to userID and
// has not expired.
func (s *Service) lookup(ctx context.Context, userID, token string) (*db.EmailConfirmation, error) {
	if token == "" || len(token) > 200 {
		return nil, ErrConfirmationNotFound
	}
	c, err := s.users.ConfirmationByTokenHash(ctx, hashToken(token))
	if errors.Is(err, db.ErrEmailConfirmationNotFound) {
		return nil, ErrConfirmationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("look up the confirmation: %w", err)
	}
	if c.UserID != userID {
		// Someone else's link: not found, the same as a wrong token, so a link
		// does not reveal whose it is.
		return nil, ErrConfirmationNotFound
	}
	if !c.ExpiresAt.After(s.now()) {
		return nil, ErrConfirmationExpired
	}
	return c, nil
}

// PreviewConfirmation reads what a token would confirm, without confirming it:
// opening the link in a browser (or a mail scanner opening it for you) must not
// change anything. It is the signed-in person's own link or nothing.
func (s *Service) PreviewConfirmation(ctx context.Context, userID, token string) (*ConfirmationView, error) {
	c, err := s.lookup(ctx, userID, token)
	if err != nil {
		return nil, err
	}
	return &ConfirmationView{Email: c.Email, ExpiresAt: c.ExpiresAt, ByAdmin: c.Source == db.EmailSourceAdmin}, nil
}

// ConfirmAddress applies the pending change a token stands for, if it belongs to
// userID and is still valid, and returns the address now in use. Audited as
// confirmed_email_address; a failed audit write puts the old address back and
// leaves the link usable.
func (s *Service) ConfirmAddress(ctx context.Context, userID, token string) (string, error) {
	c, err := s.lookup(ctx, userID, token)
	if err != nil {
		return "", err
	}
	u, err := s.users.Get(ctx, userID)
	if err != nil {
		return "", err
	}
	if u.Directory {
		return "", ErrDirectoryManaged
	}
	next := c.Email
	if err := s.setLocalAudited(ctx, u, &next, c.Source, &userID, false, "confirmed_email_address", map[string]any{"source": string(c.Source)}); err != nil {
		return "", err
	}
	_, _ = s.users.DeleteConfirmation(ctx, userID)
	return c.Email, nil
}

// SetUserResult says what SetUserAddress did.
type SetUserResult struct {
	// Pending is true when the address was not applied but a confirmation link
	// was emailed to it, and takes effect when its owner opens the link.
	Pending bool
	// Cleared is true when the address was removed.
	Cleared bool
}

// SetUserAddress sets, changes or clears a local account's address on an
// Admin's behalf. With requireConfirmation the address is not used until its
// owner opens the link emailed to it; without, it is used at once. An AD-backed
// account cannot be set (ErrDirectoryManaged). An empty address clears it.
// Requires rbac.CanSetUserEmail. Audited as set_user_email.
func (s *Service) SetUserAddress(ctx context.Context, actor rbac.Actor, targetUserID, address string, requireConfirmation bool) (SetUserResult, error) {
	if !rbac.CanSetUserEmail(actor) {
		return SetUserResult{}, rbac.ErrNotPermitted
	}
	u, err := s.users.Get(ctx, targetUserID)
	if err != nil {
		return SetUserResult{}, err
	}
	if u.Directory {
		return SetUserResult{}, ErrDirectoryManaged
	}
	var actorID *string
	if !actor.IsSuperAdmin {
		actorID = &actor.UserID
	}

	if address == "" {
		if err := s.setLocalAudited(ctx, u, nil, db.EmailSourceAdmin, actorID, actor.IsSuperAdmin, "set_user_email", map[string]any{"cleared": true}); err != nil {
			return SetUserResult{}, err
		}
		_, _ = s.users.DeleteConfirmation(ctx, targetUserID)
		return SetUserResult{Cleared: true}, nil
	}
	addr, err := mailer.ParseAddress(address)
	if err != nil {
		return SetUserResult{}, fmt.Errorf("%w: %v", ErrInvalidSettings, err)
	}

	if requireConfirmation {
		if u.Email != nil && *u.Email == addr {
			return SetUserResult{}, ErrUnchanged
		}
		if err := s.startConfirmation(ctx, targetUserID, addr, db.EmailSourceAdmin, actorID); err != nil {
			return SetUserResult{}, err
		}
		detail := map[string]any{"email": addr, "require_confirmation": true}
		if err := s.audit.Record(ctx, actorID, actor.IsSuperAdmin, "set_user_email", "user", targetUserID, detail); err != nil {
			_, _ = s.users.DeleteConfirmation(ctx, targetUserID)
			return SetUserResult{}, fmt.Errorf("record audit: %w", err)
		}
		return SetUserResult{Pending: true}, nil
	}

	if u.Email != nil && *u.Email == addr && u.Source != nil && *u.Source == db.EmailSourceAdmin {
		return SetUserResult{}, ErrUnchanged
	}
	if err := s.setLocalAudited(ctx, u, &addr, db.EmailSourceAdmin, actorID, actor.IsSuperAdmin, "set_user_email", map[string]any{"require_confirmation": false}); err != nil {
		return SetUserResult{}, err
	}
	// An address applied directly supersedes anything waiting to be confirmed.
	_, _ = s.users.DeleteConfirmation(ctx, targetUserID)
	return SetUserResult{}, nil
}

// Addresses returns every user's address, keyed by user id, for the Users page.
// Requires rbac.CanSetUserEmail.
func (s *Service) Addresses(ctx context.Context, actor rbac.Actor) (map[string]*db.UserEmail, error) {
	if !rbac.CanSetUserEmail(actor) {
		return nil, rbac.ErrNotPermitted
	}
	return s.users.ListAll(ctx)
}
