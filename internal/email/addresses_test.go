// SPDX-License-Identifier: AGPL-3.0-or-later

package email

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// fakeUsers is the users' addresses and pending confirmations in memory, with
// the same rule the database enforces: a directory address belongs to an AD
// account only, a local one to a local account only.
type fakeUsers struct {
	mu       sync.Mutex
	now      func() time.Time
	users    map[string]*db.UserEmail
	pending  map[string]*db.EmailConfirmation
	setErr   error
	putErr   error
	setLocal int
}

func newFakeUsers(now func() time.Time) *fakeUsers {
	return &fakeUsers{now: now, users: map[string]*db.UserEmail{}, pending: map[string]*db.EmailConfirmation{}}
}

func (f *fakeUsers) addAD(id string) { f.users[id] = &db.UserEmail{UserID: id, Directory: true} }
func (f *fakeUsers) addLocal(id string) {
	f.users[id] = &db.UserEmail{UserID: id}
}

func (f *fakeUsers) Get(_ context.Context, id string) (*db.UserEmail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return nil, db.ErrUserNotFound
	}
	c := *u
	return &c, nil
}

func (f *fakeUsers) ListAll(context.Context) (map[string]*db.UserEmail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]*db.UserEmail{}
	for id, u := range f.users {
		c := *u
		out[id] = &c
	}
	return out, nil
}

func (f *fakeUsers) SetDirectory(_ context.Context, id string, email *string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return false, f.setErr
	}
	u := f.users[id]
	if u == nil || !u.Directory {
		return false, nil
	}
	var src *db.EmailSource
	if email != nil {
		s := db.EmailSourceDirectory
		src = &s
	}
	changed := !sameStr(u.Email, email) || !sameSrc(u.Source, src)
	u.Email, u.Source = email, src
	return changed, nil
}

func (f *fakeUsers) SetLocal(_ context.Context, id string, email *string, source db.EmailSource) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setLocal++
	if f.setErr != nil {
		return false, f.setErr
	}
	u := f.users[id]
	if u == nil || u.Directory {
		return false, nil
	}
	var src *db.EmailSource
	if email != nil {
		src = &source
	}
	changed := !sameStr(u.Email, email) || !sameSrc(u.Source, src)
	u.Email, u.Source = email, src
	return changed, nil
}

func sameStr(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}
func sameSrc(a, b *db.EmailSource) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func (f *fakeUsers) PutConfirmation(_ context.Context, c db.EmailConfirmation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return f.putErr
	}
	f.pending[c.UserID] = &c
	return nil
}

func (f *fakeUsers) PendingConfirmation(_ context.Context, id string) (*db.EmailConfirmation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.pending[id]; ok {
		cc := *c
		return &cc, nil
	}
	return nil, db.ErrEmailConfirmationNotFound
}

func (f *fakeUsers) ConfirmationByTokenHash(_ context.Context, hash string) (*db.EmailConfirmation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.pending {
		if c.TokenHash == hash {
			cc := *c
			return &cc, nil
		}
	}
	return nil, db.ErrEmailConfirmationNotFound
}

func (f *fakeUsers) DeleteConfirmation(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.pending[id]
	delete(f.pending, id)
	return ok, nil
}

func (f *fakeUsers) email(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u := f.users[id]; u != nil && u.Email != nil {
		return *u.Email
	}
	return ""
}

func (f *fakeUsers) source(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u := f.users[id]; u != nil && u.Source != nil {
		return string(*u.Source)
	}
	return ""
}

const (
	adUser    = "ad-user-1"
	localUser = "local-user-1"
)

var tokenInLink = regexp.MustCompile(`https://sparky\.example\.com/account/email/confirm\?token=([A-Za-z0-9_-]+)`)

// lastConfirmationMail returns the newest queued confirmation email and the
// token in its link.
func (f *fixture) lastConfirmationMail(t *testing.T) (*db.Email, string) {
	t.Helper()
	for i := len(f.queue.rows) - 1; i >= 0; i-- {
		if e := f.queue.rows[i]; e.Kind == db.EmailKindConfirmation {
			m := tokenInLink.FindStringSubmatch(e.Body)
			if m == nil {
				t.Fatalf("the confirmation email has no link: %q", e.Body)
			}
			return e, m[1]
		}
	}
	t.Fatal("no confirmation email was queued")
	return nil, ""
}

func (f *fixture) addressFixture() {
	f.enable()
	f.users.addAD(adUser)
	f.users.addLocal(localUser)
}

func TestRecordDirectoryEmail(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.addressFixture()

	f.svc.RecordDirectoryEmail(ctx, adUser, "jane.smith@example.org")
	if f.users.email(adUser) != "jane.smith@example.org" || f.users.source(adUser) != "directory" {
		t.Fatalf("address = %q (%s)", f.users.email(adUser), f.users.source(adUser))
	}
	// A change in the directory is picked up at the next sign-in.
	f.svc.RecordDirectoryEmail(ctx, adUser, "  jane@example.org ")
	if f.users.email(adUser) != "jane@example.org" {
		t.Errorf("after a change in AD: %q", f.users.email(adUser))
	}
	// AD dropping the address, or holding something that is not an address,
	// leaves no address - never a half-valid one.
	for _, raw := range []string{"", "not an address", "jane@example.org\r\nBcc: x@y.com", "Jane <jane@example.org>"} {
		f.svc.RecordDirectoryEmail(ctx, adUser, "jane@example.org")
		f.svc.RecordDirectoryEmail(ctx, adUser, raw)
		if f.users.email(adUser) != "" {
			t.Errorf("directory value %q left the address %q", raw, f.users.email(adUser))
		}
	}
	// A local account is never given the directory's address, and a store
	// failure never reaches the caller.
	f.svc.RecordDirectoryEmail(ctx, localUser, "jane@example.org")
	if f.users.email(localUser) != "" {
		t.Error("a local account was given a directory address")
	}
	f.users.setErr = errors.New("db down")
	f.svc.RecordDirectoryEmail(ctx, adUser, "jane@example.org") // must not panic or block
	if len(f.audit.calls) != 0 {
		t.Error("the directory refresh was audited; it is data arriving, not a person acting")
	}
}

func TestMyAddress(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.addressFixture()

	v, err := f.svc.MyAddress(ctx, localUser)
	if err != nil || v.Directory || v.Email != "" || v.Pending != nil || !v.EmailOn || !v.CanConfirm {
		t.Fatalf("a new local account: %+v, %v", v, err)
	}
	f.users.users[adUser].Email, f.users.users[adUser].Source = ptr("ad@example.org"), srcPtr(db.EmailSourceDirectory)
	if v, _ := f.svc.MyAddress(ctx, adUser); !v.Directory || v.Email != "ad@example.org" || v.Source != db.EmailSourceDirectory {
		t.Errorf("an AD account: %+v", v)
	}

	f.users.pending[localUser] = &db.EmailConfirmation{UserID: localUser, Email: "new@example.org", Source: db.EmailSourceAdmin, ExpiresAt: f.clock.Add(time.Hour)}
	if v, _ := f.svc.MyAddress(ctx, localUser); v.Pending == nil || v.Pending.Email != "new@example.org" || !v.Pending.ByAdmin {
		t.Errorf("a pending change: %+v", v.Pending)
	}
	f.users.pending[localUser].ExpiresAt = f.clock.Add(-time.Minute)
	if v, _ := f.svc.MyAddress(ctx, localUser); v.Pending != nil {
		t.Error("an expired pending change is still shown")
	}

	f.svc.env.PublicURL = ""
	if v, _ := f.svc.MyAddress(ctx, localUser); v.CanConfirm {
		t.Error("CanConfirm is true with no public URL")
	}
	f.settings.cfg.Enabled = false
	if v, _ := f.svc.MyAddress(ctx, localUser); v.EmailOn {
		t.Error("EmailOn is true with email off")
	}
	if _, err := f.svc.MyAddress(ctx, "nobody"); !errors.Is(err, db.ErrUserNotFound) {
		t.Errorf("unknown user: %v", err)
	}
}

func ptr(s string) *string                    { return &s }
func srcPtr(s db.EmailSource) *db.EmailSource { return &s }

func TestRequestMyAddress_SendsALinkToTheNewAddressAndChangesNothingYet(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.addressFixture()
	f.users.users[localUser].Email, f.users.users[localUser].Source = ptr("old@example.org"), srcPtr(db.EmailSourceSelf)

	if err := f.svc.RequestMyAddress(ctx, localUser, "  New@Example.org "); err != nil {
		t.Fatal(err)
	}
	if f.users.email(localUser) != "old@example.org" {
		t.Errorf("the address changed before it was confirmed: %q", f.users.email(localUser))
	}
	mail, token := f.lastConfirmationMail(t)
	if mail.ToAddress != "New@Example.org" || mail.UserID == nil || *mail.UserID != localUser || !strings.Contains(mail.Subject, "Confirm") {
		t.Errorf("mail = %+v", mail)
	}
	if len(token) < 40 {
		t.Errorf("token %q is too short to be unguessable", token)
	}
	p := f.users.pending[localUser]
	if p == nil || p.Email != "New@Example.org" || p.Source != db.EmailSourceSelf || p.RequestedBy != nil || !p.ExpiresAt.Equal(f.clock.Add(24*time.Hour)) {
		t.Fatalf("pending = %+v", p)
	}
	if p.TokenHash == token || strings.Contains(p.TokenHash, token) || p.TokenHash != hashToken(token) {
		t.Error("the token was stored, or not as its SHA-256")
	}
	calls := f.audit.calls
	if len(calls) != 1 || calls[0].action != "requested_email_confirmation" || calls[0].actorID == nil || *calls[0].actorID != localUser || calls[0].detail["email"] != "New@Example.org" {
		t.Errorf("audit = %+v", calls)
	}
	if strings.Contains(strings.ToLower(mail.Body), "old@example.org") {
		t.Error("the confirmation email names the old address")
	}
}

func TestRequestMyAddress_Refusals(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		prepare func(*fixture)
		user    string
		address string
		want    error
	}{
		"an AD account":      {func(f *fixture) {}, adUser, "x@example.org", ErrDirectoryManaged},
		"not an address":     {func(f *fixture) {}, localUser, "nope", ErrInvalidSettings},
		"an injected header": {func(f *fixture) {}, localUser, "x@example.org\r\nBcc: y@z.com", ErrInvalidSettings},
		"the address in use": {func(f *fixture) {
			f.users.users[localUser].Email, f.users.users[localUser].Source = ptr("x@example.org"), srcPtr(db.EmailSourceSelf)
		}, localUser, "x@example.org", ErrUnchanged},
		"email turned off": {func(f *fixture) { f.settings.cfg.Enabled = false }, localUser, "x@example.org", ErrDisabled},
		"no public URL":    {func(f *fixture) { f.svc.env.PublicURL = "" }, localUser, "x@example.org", ErrNoPublicURL},
		"an unknown user":  {func(f *fixture) {}, "nobody", "x@example.org", db.ErrUserNotFound},
	}
	for name, c := range cases {
		f := newFixture(t)
		f.addressFixture()
		c.prepare(f)
		if err := f.svc.RequestMyAddress(ctx, c.user, c.address); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
		if len(f.queue.rows) != 0 || len(f.users.pending) != 0 || len(f.audit.calls) != 0 {
			t.Errorf("%s: a refused request queued mail, recorded a change or was audited", name)
		}
	}
}

func TestRequestMyAddress_CooldownAndCleanUpAfterFailure(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.addressFixture()
	if err := f.svc.RequestMyAddress(ctx, localUser, "a@example.org"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RequestMyAddress(ctx, localUser, "b@example.org"); !errors.Is(err, ErrTooSoon) {
		t.Errorf("a second request at once: %v, want ErrTooSoon", err)
	}
	if len(f.queue.rows) != 1 {
		t.Error("the refused request still queued mail")
	}
	f.clock = f.clock.Add(61 * time.Second)
	if err := f.svc.RequestMyAddress(ctx, localUser, "b@example.org"); err != nil {
		t.Fatalf("after the cooldown: %v", err)
	}
	// The earlier link stops working: the new request replaced it.
	_, firstToken := func() (*db.Email, string) {
		m := tokenInLink.FindStringSubmatch(f.queue.rows[0].Body)
		return f.queue.rows[0], m[1]
	}()
	if _, err := f.svc.PreviewConfirmation(ctx, localUser, firstToken); !errors.Is(err, ErrConfirmationNotFound) {
		t.Errorf("a replaced link still works: %v", err)
	}

	// Audit failure removes the pending change, so no unrecorded link works.
	f2 := newFixture(t)
	f2.addressFixture()
	f2.audit.err = errors.New("audit down")
	if err := f2.svc.RequestMyAddress(ctx, localUser, "a@example.org"); err == nil {
		t.Fatal("succeeded without an audit record")
	}
	if len(f2.users.pending) != 0 {
		t.Error("a pending change survived a failed audit write")
	}
}

func TestConfirmAddress(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.addressFixture()
	_ = f.svc.RequestMyAddress(ctx, localUser, "new@example.org")
	_, token := f.lastConfirmationMail(t)
	f.audit.calls = nil

	// Looking at the link changes nothing.
	view, err := f.svc.PreviewConfirmation(ctx, localUser, token)
	if err != nil || view.Email != "new@example.org" || view.ByAdmin {
		t.Fatalf("preview: %+v, %v", view, err)
	}
	if f.users.email(localUser) != "" || len(f.audit.calls) != 0 {
		t.Error("previewing a link changed the address or was audited")
	}

	// Someone else, signed in, cannot use it - and it is still good for its owner.
	f.users.addLocal("other-user")
	if _, err := f.svc.ConfirmAddress(ctx, "other-user", token); !errors.Is(err, ErrConfirmationNotFound) {
		t.Errorf("another account's attempt: %v, want ErrConfirmationNotFound", err)
	}
	if f.users.email("other-user") != "" || f.users.email(localUser) != "" {
		t.Error("someone else's attempt changed an address")
	}

	got, err := f.svc.ConfirmAddress(ctx, localUser, token)
	if err != nil || got != "new@example.org" {
		t.Fatalf("confirm: %q, %v", got, err)
	}
	if f.users.email(localUser) != "new@example.org" || f.users.source(localUser) != "self" || len(f.users.pending) != 0 {
		t.Errorf("after confirming: %q (%s), pending %d", f.users.email(localUser), f.users.source(localUser), len(f.users.pending))
	}
	calls := f.audit.calls
	if len(calls) != 1 || calls[0].action != "confirmed_email_address" || calls[0].detail["email"] != "new@example.org" || calls[0].detail["source"] != "self" {
		t.Errorf("audit = %+v", calls)
	}
	// One use only.
	if _, err := f.svc.ConfirmAddress(ctx, localUser, token); !errors.Is(err, ErrConfirmationNotFound) {
		t.Errorf("reusing a link: %v", err)
	}
}

func TestConfirmAddress_BadExpiredAndUnrecordedLinks(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.addressFixture()
	for _, token := range []string{"", "nope", strings.Repeat("a", 300), "../../etc/passwd"} {
		if _, err := f.svc.ConfirmAddress(ctx, localUser, token); !errors.Is(err, ErrConfirmationNotFound) {
			t.Errorf("token %q: %v", token, err)
		}
	}

	_ = f.svc.RequestMyAddress(ctx, localUser, "new@example.org")
	_, token := f.lastConfirmationMail(t)
	f.clock = f.clock.Add(25 * time.Hour)
	if _, err := f.svc.ConfirmAddress(ctx, localUser, token); !errors.Is(err, ErrConfirmationExpired) {
		t.Errorf("after 25 hours: %v, want ErrConfirmationExpired", err)
	}
	if f.users.email(localUser) != "" {
		t.Error("an expired link set the address")
	}

	// If the change cannot be recorded, the old address is put back and the
	// link still works once recording does.
	f2 := newFixture(t)
	f2.addressFixture()
	f2.users.users[localUser].Email, f2.users.users[localUser].Source = ptr("old@example.org"), srcPtr(db.EmailSourceSelf)
	_ = f2.svc.RequestMyAddress(ctx, localUser, "new@example.org")
	_, token = f2.lastConfirmationMail(t)
	f2.audit.err = errors.New("audit down")
	if _, err := f2.svc.ConfirmAddress(ctx, localUser, token); err == nil {
		t.Fatal("confirmed without an audit record")
	}
	if f2.users.email(localUser) != "old@example.org" || f2.users.source(localUser) != "self" || len(f2.users.pending) != 1 {
		t.Errorf("not reverted: %q (%s), pending %d", f2.users.email(localUser), f2.users.source(localUser), len(f2.users.pending))
	}
	f2.audit.err = nil
	if got, err := f2.svc.ConfirmAddress(ctx, localUser, token); err != nil || got != "new@example.org" {
		t.Errorf("retry: %q, %v", got, err)
	}
}

func TestClearMyAddress(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.addressFixture()
	f.users.users[localUser].Email, f.users.users[localUser].Source = ptr("old@example.org"), srcPtr(db.EmailSourceSelf)
	f.users.pending[localUser] = &db.EmailConfirmation{UserID: localUser, Email: "p@example.org", ExpiresAt: f.clock.Add(time.Hour)}

	if err := f.svc.ClearMyAddress(ctx, adUser); !errors.Is(err, ErrDirectoryManaged) {
		t.Errorf("AD account: %v", err)
	}
	if err := f.svc.ClearMyAddress(ctx, localUser); err != nil {
		t.Fatal(err)
	}
	if f.users.email(localUser) != "" || len(f.users.pending) != 0 {
		t.Errorf("after clearing: %q, pending %d", f.users.email(localUser), len(f.users.pending))
	}
	if c := f.audit.calls; len(c) != 1 || c[0].action != "cleared_email_address" || c[0].detail["previous_email"] != "old@example.org" {
		t.Errorf("audit = %+v", c)
	}

	f2 := newFixture(t)
	f2.addressFixture()
	f2.users.users[localUser].Email, f2.users.users[localUser].Source = ptr("old@example.org"), srcPtr(db.EmailSourceSelf)
	f2.audit.err = errors.New("audit down")
	if err := f2.svc.ClearMyAddress(ctx, localUser); err == nil {
		t.Fatal("cleared without an audit record")
	}
	if f2.users.email(localUser) != "old@example.org" {
		t.Errorf("not restored: %q", f2.users.email(localUser))
	}
}

func TestSetUserAddress_AdminSetsDirectlyOrAsksForConfirmation(t *testing.T) {
	ctx := context.Background()

	// Without confirmation: used at once, source admin.
	f := newFixture(t)
	f.addressFixture()
	f.users.pending[localUser] = &db.EmailConfirmation{UserID: localUser, Email: "p@example.org", ExpiresAt: f.clock.Add(time.Hour)}
	res, err := f.svc.SetUserAddress(ctx, admin, localUser, " bo@example.org ", false)
	if err != nil || res.Pending || res.Cleared {
		t.Fatalf("direct set: %+v, %v", res, err)
	}
	if f.users.email(localUser) != "bo@example.org" || f.users.source(localUser) != "admin" || len(f.users.pending) != 0 {
		t.Errorf("after: %q (%s), pending %d", f.users.email(localUser), f.users.source(localUser), len(f.users.pending))
	}
	if len(f.queue.rows) != 0 {
		t.Error("a direct set sent a confirmation email")
	}
	c := f.audit.calls[len(f.audit.calls)-1]
	if c.action != "set_user_email" || c.actorID == nil || *c.actorID != "admin-1" || c.detail["require_confirmation"] != false || c.detail["email"] != "bo@example.org" || c.objectType != "user" {
		t.Errorf("audit = %+v", c)
	}

	// With confirmation: nothing changes until the owner opens the link.
	f = newFixture(t)
	f.addressFixture()
	res, err = f.svc.SetUserAddress(ctx, admin, localUser, "bo@example.org", true)
	if err != nil || !res.Pending {
		t.Fatalf("confirmed set: %+v, %v", res, err)
	}
	if f.users.email(localUser) != "" {
		t.Error("the address was applied before it was confirmed")
	}
	mail, token := f.lastConfirmationMail(t)
	if mail.ToAddress != "bo@example.org" || !strings.Contains(mail.Body, "A Sparky Admin asked") {
		t.Errorf("mail = %+v", mail)
	}
	if p := f.users.pending[localUser]; p.Source != db.EmailSourceAdmin || p.RequestedBy == nil || *p.RequestedBy != "admin-1" {
		t.Errorf("pending = %+v", p)
	}
	if v, _ := f.svc.PreviewConfirmation(ctx, localUser, token); v == nil || !v.ByAdmin {
		t.Errorf("preview = %+v", v)
	}
	if _, err := f.svc.ConfirmAddress(ctx, localUser, token); err != nil {
		t.Fatal(err)
	}
	if f.users.email(localUser) != "bo@example.org" || f.users.source(localUser) != "admin" {
		t.Errorf("after the owner confirmed: %q (%s)", f.users.email(localUser), f.users.source(localUser))
	}
	if c := f.audit.calls[0]; c.action != "set_user_email" || c.detail["require_confirmation"] != true {
		t.Errorf("first audit = %+v", c)
	}

	// The break-glass SuperAdmin is recorded as such, with no requester id.
	f = newFixture(t)
	f.addressFixture()
	if _, err := f.svc.SetUserAddress(ctx, breakGlass, localUser, "bo@example.org", true); err != nil {
		t.Fatal(err)
	}
	if p := f.users.pending[localUser]; p.RequestedBy != nil {
		t.Errorf("break-glass requested_by = %v", p.RequestedBy)
	}
	if c := f.audit.calls[0]; c.actorID != nil || !c.superAdmin {
		t.Errorf("break-glass audit = %+v", c)
	}
}

func TestSetUserAddress_ClearRefusalsAndFailures(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.addressFixture()
	f.users.users[localUser].Email, f.users.users[localUser].Source = ptr("old@example.org"), srcPtr(db.EmailSourceAdmin)

	if _, err := f.svc.SetUserAddress(ctx, developer, localUser, "x@example.org", false); !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("developer: %v", err)
	}
	if _, err := f.svc.Addresses(ctx, developer); !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("developer Addresses: %v", err)
	}
	if _, err := f.svc.SetUserAddress(ctx, admin, adUser, "x@example.org", false); !errors.Is(err, ErrDirectoryManaged) {
		t.Errorf("AD target: %v", err)
	}
	if _, err := f.svc.SetUserAddress(ctx, admin, "nobody", "x@example.org", false); !errors.Is(err, db.ErrUserNotFound) {
		t.Errorf("unknown target: %v", err)
	}
	if _, err := f.svc.SetUserAddress(ctx, admin, localUser, "nope", false); !errors.Is(err, ErrInvalidSettings) {
		t.Errorf("bad address: %v", err)
	}
	if _, err := f.svc.SetUserAddress(ctx, admin, localUser, "old@example.org", false); !errors.Is(err, ErrUnchanged) {
		t.Errorf("same address: %v", err)
	}
	f.settings.cfg.Enabled = false
	if _, err := f.svc.SetUserAddress(ctx, admin, localUser, "x@example.org", true); !errors.Is(err, ErrDisabled) {
		t.Errorf("confirmation with email off: %v", err)
	}
	f.settings.cfg.Enabled = true
	f.svc.env.PublicURL = ""
	if _, err := f.svc.SetUserAddress(ctx, admin, localUser, "x@example.org", true); !errors.Is(err, ErrNoPublicURL) {
		t.Errorf("confirmation with no public URL: %v", err)
	}
	if f.users.email(localUser) != "old@example.org" || len(f.users.pending) != 0 || len(f.audit.calls) != 0 {
		t.Error("a refused request changed or audited something")
	}
	// A direct set needs neither email nor a public URL.
	if _, err := f.svc.SetUserAddress(ctx, admin, localUser, "x@example.org", false); err != nil {
		t.Errorf("direct set with email off: %v", err)
	}

	// Clearing.
	res, err := f.svc.SetUserAddress(ctx, admin, localUser, "", false)
	if err != nil || !res.Cleared || f.users.email(localUser) != "" {
		t.Errorf("clear: %+v, %v, %q", res, err, f.users.email(localUser))
	}
	if c := f.audit.calls[len(f.audit.calls)-1]; c.action != "set_user_email" || c.detail["cleared"] != true {
		t.Errorf("clear audit = %+v", c)
	}

	// A failed audit write puts the address back.
	f2 := newFixture(t)
	f2.addressFixture()
	f2.users.users[localUser].Email, f2.users.users[localUser].Source = ptr("old@example.org"), srcPtr(db.EmailSourceSelf)
	f2.audit.err = errors.New("audit down")
	if _, err := f2.svc.SetUserAddress(ctx, admin, localUser, "new@example.org", false); err == nil {
		t.Fatal("set without an audit record")
	}
	if f2.users.email(localUser) != "old@example.org" || f2.users.source(localUser) != "self" {
		t.Errorf("not restored: %q (%s)", f2.users.email(localUser), f2.users.source(localUser))
	}
	if _, err := f2.svc.SetUserAddress(ctx, admin, localUser, "new@example.org", true); err == nil || len(f2.users.pending) != 0 {
		t.Errorf("a confirmed set with a failed audit left a pending change (%d)", len(f2.users.pending))
	}
}

func TestAddresses(t *testing.T) {
	f := newFixture(t)
	f.addressFixture()
	got, err := f.svc.Addresses(context.Background(), admin)
	if err != nil || len(got) != 2 || !got[adUser].Directory || got[localUser].Directory {
		t.Errorf("Addresses() = %+v, %v", got, err)
	}
}
