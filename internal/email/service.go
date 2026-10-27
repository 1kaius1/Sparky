// SPDX-License-Identifier: AGPL-3.0-or-later

// Package email decides who is emailed what and delivers it. Sparky sends mail
// only through an SMTP relay (internal/mailer), and never in the request that
// caused it: a message is queued in the database and a worker delivers it,
// retrying with backoff and recording failures, so a mail outage never blocks
// the application. See PLANNING.md's 2026-10-26 Decisions Log.
//
// This file holds the Admin settings, the test send and the queue; the worker
// is in worker.go.
package email

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/mailer"
	"github.com/1kaius1/Sparky/internal/rbac"
)

const (
	// settingsAuditObjectID stands in for email_settings' singleton row in an
	// audit_log entry - object_id is a real uuid, so the conventional all-zero
	// uuid is used, as for the other singleton settings rows.
	settingsAuditObjectID = "00000000-0000-0000-0000-000000000000"

	// maxSubjectLen and maxBodyLen bound what a caller may queue, in bytes.
	maxSubjectLen = 300
	maxBodyLen    = 50000
)

var (
	// ErrInvalidSettings is wrapped by a refused settings change; its message
	// is safe to show an Admin.
	ErrInvalidSettings = errors.New("invalid email settings")

	// ErrDisabled is returned by Enqueue when email is turned off. Callers
	// treat it as "nothing to do", not as a failure.
	ErrDisabled = errors.New("email is turned off")

	// ErrSendFailed wraps the relay's or the network's reason a test send did
	// not go; the message is for an Admin.
	ErrSendFailed = errors.New("the test email could not be sent")
)

// Env is the part of the configuration that lives in the environment, not the
// database: the relay's password, an extra CA file, and the address people
// reach this server at (for the links in emails).
type Env struct {
	SMTPPassword string
	SMTPCAFile   string
	PublicURL    string
}

// settingsStore is the subset of *db.EmailSettingsRepository this package needs.
type settingsStore interface {
	Get(ctx context.Context) (*db.EmailSettings, error)
	Update(ctx context.Context, s db.EmailSettings) error
	Restore(ctx context.Context, prev *db.EmailSettings) error
}

// queueStore is the subset of *db.EmailQueueRepository this package needs.
type queueStore interface {
	Enqueue(ctx context.Context, in db.NewEmail) (*db.Email, error)
	Claim(ctx context.Context, limit int, lease time.Duration) ([]*db.Email, error)
	MarkSent(ctx context.Context, id string) error
	MarkRetry(ctx context.Context, id string, next time.Time, reason string) error
	MarkFailed(ctx context.Context, id string, reason string) error
	FailOlderThan(ctx context.Context, cutoff time.Time, reason string) (int64, error)
	DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
	Stats(ctx context.Context) (*db.EmailQueueStats, error)
	Recent(ctx context.Context, limit int) ([]*db.Email, error)
}

// auditRecorder is the subset of *audit.Recorder this package needs.
type auditRecorder interface {
	Record(ctx context.Context, actorID *string, isSuperAdminAction bool, action, objectType, objectID string, detail map[string]any) error
}

// sendFunc delivers one message; mailer.Config.Send in production, a fake in
// tests.
type sendFunc func(ctx context.Context, cfg mailer.Config, m mailer.Message) error

// Service holds the email settings, queues mail and delivers it.
type Service struct {
	settings settingsStore
	queue    queueStore
	users    userEmailStore
	audit    auditRecorder
	env      Env
	hello    string
	logger   *log.Logger
	now      func() time.Time
	send     sendFunc

	// sendTimeout bounds one delivery attempt by the worker. A batch of
	// batchSize attempts must fit well inside the lease Claim takes, so a stalled
	// relay cannot make the same mail look abandoned and be sent twice.
	sendTimeout time.Duration
}

// NewService constructs a Service. hello is what this server calls itself in
// EHLO ("" for localhost).
func NewService(settings settingsStore, queue queueStore, users userEmailStore, audit auditRecorder, env Env, hello string, logger *log.Logger) *Service {
	return &Service{
		settings: settings, queue: queue, users: users, audit: audit, env: env, hello: hello, logger: logger,
		now:         time.Now,
		send:        func(ctx context.Context, cfg mailer.Config, m mailer.Message) error { return cfg.Send(ctx, m) },
		sendTimeout: 30 * time.Second,
	}
}

// PublicURL is the address emails link to, "" when none is configured.
func (s *Service) PublicURL() string { return s.env.PublicURL }

// mailerConfig turns saved settings and the environment into a relay config.
func (s *Service) mailerConfig(cfg *db.EmailSettings) mailer.Config {
	return mailer.Config{
		Host: cfg.Host, Port: cfg.Port, TLSMode: mailer.TLSMode(cfg.TLSMode),
		Username: cfg.Username, Password: s.env.SMTPPassword, CAFile: s.env.SMTPCAFile,
		FromAddress: cfg.FromAddress, FromName: cfg.FromName, HelloName: s.hello,
		Timeout: s.sendTimeout,
	}
}

// SettingsView is what the Settings page shows: the saved settings, what the
// environment provides, and how delivery has been going.
type SettingsView struct {
	Settings *db.EmailSettings

	// PasswordSet, CAFileSet and PublicURL say what the environment provides
	// (SMTP_PASSWORD, SMTP_CA_FILE, SPARKY_PUBLIC_URL). The password itself is
	// never shown.
	PasswordSet bool
	CAFileSet   bool
	PublicURL   string

	Stats  *db.EmailQueueStats
	Recent []*db.Email
}

// Settings returns the settings and the delivery summary. Requires
// rbac.CanManageEmail.
func (s *Service) Settings(ctx context.Context, actor rbac.Actor) (*SettingsView, error) {
	if !rbac.CanManageEmail(actor) {
		return nil, rbac.ErrNotPermitted
	}
	cfg, err := s.settings.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("get email settings: %w", err)
	}
	stats, err := s.queue.Stats(ctx)
	if err != nil {
		return nil, fmt.Errorf("get email queue stats: %w", err)
	}
	recent, err := s.queue.Recent(ctx, 10)
	if err != nil {
		return nil, fmt.Errorf("list recent email: %w", err)
	}
	return &SettingsView{
		Settings: cfg, PasswordSet: s.env.SMTPPassword != "", CAFileSet: s.env.SMTPCAFile != "", PublicURL: s.env.PublicURL,
		Stats: stats, Recent: recent,
	}, nil
}

// SettingsInput is what the Settings form submits.
type SettingsInput struct {
	Enabled     bool
	Host        string
	Port        int
	TLSMode     db.EmailTLSMode
	Username    string
	FromAddress string
	FromName    string
}

// UpdateSettings saves the relay settings. Requires rbac.CanManageEmail.
// Audited as updated_email_settings (never with the password, which is not
// stored here); a failed audit write reverts the change. Turning email on
// needs a usable relay and from address, so "on" always means "can try to
// send".
func (s *Service) UpdateSettings(ctx context.Context, actor rbac.Actor, in SettingsInput) error {
	if !rbac.CanManageEmail(actor) {
		return rbac.ErrNotPermitted
	}
	in.Host = strings.TrimSpace(in.Host)
	in.Username = strings.TrimSpace(in.Username)
	in.FromAddress = strings.TrimSpace(in.FromAddress)
	in.FromName = strings.TrimSpace(in.FromName)

	if in.Port < 1 || in.Port > 65535 {
		return fmt.Errorf("%w: the port must be between 1 and 65535", ErrInvalidSettings)
	}
	if !in.TLSMode.Valid() {
		return fmt.Errorf("%w: choose a connection security option", ErrInvalidSettings)
	}
	if strings.ContainsAny(in.Host, " \t\r\n/") {
		return fmt.Errorf("%w: the relay host should be a name or address with no spaces or slashes", ErrInvalidSettings)
	}
	if hasControl(in.Username) || hasControl(in.FromName) || len(in.Username) > 255 || len(in.FromName) > 100 {
		return fmt.Errorf("%w: the user name or from name is too long or has control characters", ErrInvalidSettings)
	}
	if in.FromAddress != "" {
		addr, err := mailer.ParseAddress(in.FromAddress)
		if err != nil {
			return fmt.Errorf("%w: the from address: %v", ErrInvalidSettings, err)
		}
		in.FromAddress = addr
	}
	if in.Enabled {
		if in.Host == "" {
			return fmt.Errorf("%w: a relay host is needed to turn email on", ErrInvalidSettings)
		}
		if in.FromAddress == "" {
			return fmt.Errorf("%w: a from address is needed to turn email on", ErrInvalidSettings)
		}
	}
	if in.Username != "" {
		if s.env.SMTPPassword == "" {
			return fmt.Errorf("%w: a user name is set but SMTP_PASSWORD is not; set the password in the server's environment, or clear the user name for an anonymous relay", ErrInvalidSettings)
		}
		if in.TLSMode == db.EmailTLSNone && !isLocalHost(in.Host) {
			return fmt.Errorf("%w: a user name and password are never sent without encryption to another machine; choose STARTTLS or TLS", ErrInvalidSettings)
		}
	}

	prev, err := s.settings.Get(ctx)
	if err != nil {
		return fmt.Errorf("get current email settings: %w", err)
	}
	var actorID *string
	if !actor.IsSuperAdmin {
		actorID = &actor.UserID
	}
	next := db.EmailSettings{
		Enabled: in.Enabled, Host: in.Host, Port: in.Port, TLSMode: in.TLSMode, Username: in.Username,
		FromAddress: in.FromAddress, FromName: in.FromName, UpdatedBy: actorID,
	}
	if err := s.settings.Update(ctx, next); err != nil {
		return fmt.Errorf("update email settings: %w", err)
	}
	detail := map[string]any{
		"enabled": next.Enabled, "host": next.Host, "port": next.Port, "tls_mode": string(next.TLSMode),
		"username": next.Username, "from_address": next.FromAddress, "previous_enabled": prev.Enabled,
	}
	if err := s.audit.Record(ctx, actorID, actor.IsSuperAdmin, "updated_email_settings", "email_settings", settingsAuditObjectID, detail); err != nil {
		auditErr := fmt.Errorf("record audit: %w", err)
		if revertErr := s.settings.Restore(ctx, prev); revertErr != nil {
			return fmt.Errorf("%w (revert also failed: %v)", auditErr, revertErr)
		}
		return auditErr
	}
	return nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func isLocalHost(h string) bool {
	h = strings.ToLower(h)
	return h == "localhost" || h == "::1" || strings.HasPrefix(h, "127.")
}

// SendTest sends one email straight to the given address, bypassing the queue,
// using the saved relay settings - so an Admin sees the relay's own answer at
// once, and can check the settings before turning email on. Requires
// rbac.CanManageEmail. Audited as sent_test_email whether or not it went.
func (s *Service) SendTest(ctx context.Context, actor rbac.Actor, to string) error {
	if !rbac.CanManageEmail(actor) {
		return rbac.ErrNotPermitted
	}
	addr, err := mailer.ParseAddress(to)
	if err != nil {
		return fmt.Errorf("%w: the address: %v", ErrInvalidSettings, err)
	}
	cfg, err := s.settings.Get(ctx)
	if err != nil {
		return fmt.Errorf("get email settings: %w", err)
	}
	mc := s.mailerConfig(cfg)
	if err := mc.Validate(); err != nil {
		return fmt.Errorf("%w: %v; save a relay host and from address first", ErrInvalidSettings, err)
	}

	sendErr := s.send(ctx, mc, mailer.Message{
		To:      addr,
		Subject: "[Sparky] Test email",
		Body: "This is a test email from Sparky, sent because an Admin asked for one.\n\n" +
			"If you are reading it, the relay settings work. Nothing more is needed.\n",
	})

	var actorID *string
	if !actor.IsSuperAdmin {
		actorID = &actor.UserID
	}
	detail := map[string]any{"to": addr, "sent": sendErr == nil}
	if err := s.audit.Record(ctx, actorID, actor.IsSuperAdmin, "sent_test_email", "email_settings", settingsAuditObjectID, detail); err != nil {
		s.logger.Printf("email: record the test email in the audit log: %v", err)
	}
	if sendErr != nil {
		return fmt.Errorf("%w: %v", ErrSendFailed, sendErr)
	}
	return nil
}

// Message is an email to queue.
type Message struct {
	Kind    db.EmailKind
	To      string
	UserID  *string
	Subject string
	Body    string
}

// Enqueue queues a message for delivery. It returns ErrDisabled when email is
// off, and an error for an address or content that is not acceptable; it never
// talks to the relay. A queued message is delivered by the worker (Run), and a
// failure there is recorded on the row, never raised to the caller.
func (s *Service) Enqueue(ctx context.Context, m Message) error {
	cfg, err := s.settings.Get(ctx)
	if err != nil {
		return fmt.Errorf("get email settings: %w", err)
	}
	if !cfg.Enabled {
		return ErrDisabled
	}
	addr, err := mailer.ParseAddress(m.To)
	if err != nil {
		return fmt.Errorf("the recipient: %w", err)
	}
	if strings.TrimSpace(m.Subject) == "" || len(m.Subject) > maxSubjectLen || hasControl(m.Subject) {
		return errors.New("the subject is empty, too long or has control characters")
	}
	if len(m.Body) > maxBodyLen {
		return errors.New("the body is too long")
	}
	if _, err := s.queue.Enqueue(ctx, db.NewEmail{Kind: m.Kind, ToAddress: addr, UserID: m.UserID, Subject: m.Subject, Body: m.Body}); err != nil {
		return fmt.Errorf("queue the email: %w", err)
	}
	return nil
}
