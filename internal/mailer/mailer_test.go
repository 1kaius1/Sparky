// SPDX-License-Identifier: AGPL-3.0-or-later

package mailer

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func send(cfg Config, to, subject, body string) error {
	return cfg.Send(context.Background(), Message{To: to, Subject: subject, Body: body})
}

func parseMail(t *testing.T, raw string) (*mail.Message, string) {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("the relay received something that is not a message: %v\n%s", err, raw)
	}
	body, err := io.ReadAll(quotedprintable.NewReader(msg.Body))
	if err != nil {
		t.Fatal(err)
	}
	return msg, string(body)
}

func TestSend_DeliversWithAWellFormedMessage(t *testing.T) {
	relay := startRelay(t, &fakeRelay{})
	body := "Line one\n.A line that starts with a dot\nété café ✓\n" + strings.Repeat("long ", 60) + "end\n"
	if err := send(relay.config(TLSNone), "ana@example.org", "[Sparky] critical: café is down", body); err != nil {
		t.Fatal(err)
	}
	got := relay.delivered()
	if len(got) != 1 || got[0].from != "sparky@example.com" || got[0].to != "ana@example.org" {
		t.Fatalf("delivered %+v, want one mail from sparky@example.com to ana@example.org", got)
	}
	msg, text := parseMail(t, got[0].data)
	dec := new(mime.WordDecoder)
	if subj, err := dec.DecodeHeader(msg.Header.Get("Subject")); err != nil || subj != "[Sparky] critical: café is down" {
		t.Errorf("subject = %q, %v", msg.Header.Get("Subject"), err)
	}
	if from, _ := dec.DecodeHeader(msg.Header.Get("From")); from != `"Sparky" <sparky@example.com>` && from != "Sparky <sparky@example.com>" {
		t.Errorf("From = %q", from)
	}
	if msg.Header.Get("To") != "ana@example.org" || msg.Header.Get("MIME-Version") != "1.0" ||
		!strings.HasPrefix(msg.Header.Get("Content-Type"), "text/plain") || !strings.Contains(msg.Header.Get("Content-Type"), "utf-8") ||
		msg.Header.Get("Auto-Submitted") != "auto-generated" ||
		!strings.HasPrefix(msg.Header.Get("Message-ID"), "<") || !strings.HasSuffix(msg.Header.Get("Message-ID"), "@example.com>") {
		t.Errorf("headers = %v", msg.Header)
	}
	if _, err := msg.Header.Date(); err != nil {
		t.Errorf("Date header: %v", err)
	}
	if strings.ReplaceAll(text, "\r\n", "\n") != body {
		t.Errorf("body after decoding = %q, want %q", text, body)
	}
}

func TestSend_ARejectedRecipientIsPermanentButTemporaryTroubleIsNot(t *testing.T) {
	cases := map[string]struct {
		reply     map[string]string
		permanent bool
	}{
		"550 at RCPT TO (no such user)":     {map[string]string{"RCPT": "550 5.1.1 no such user"}, true},
		"554 after the message":             {map[string]string{"END": "554 5.7.1 rejected as spam"}, true},
		"451 at RCPT TO (try again)":        {map[string]string{"RCPT": "451 4.3.0 try later"}, false},
		"452 after the message":             {map[string]string{"END": "452 4.2.2 mailbox full"}, false},
		"550 at MAIL FROM (sender refused)": {map[string]string{"MAIL": "550 5.7.1 sender not allowed"}, false},
		"421 greeting (busy)":               {map[string]string{"greet": "421 too busy"}, false},
	}
	for name, c := range cases {
		relay := startRelay(t, &fakeRelay{reply: c.reply})
		err := send(relay.config(TLSNone), "ana@example.org", "s", "b")
		if err == nil {
			t.Errorf("%s: succeeded", name)
			continue
		}
		if IsPermanent(err) != c.permanent {
			t.Errorf("%s: permanent = %v, want %v (%v)", name, IsPermanent(err), c.permanent, err)
		}
		if len(relay.delivered()) != 0 {
			t.Errorf("%s: a refused message was recorded as delivered", name)
		}
	}

	// An unreachable relay is retryable.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	cfg := Config{Host: "127.0.0.1", Port: port, TLSMode: TLSNone, FromAddress: "sparky@example.com", Timeout: 2 * time.Second}
	if err := send(cfg, "ana@example.org", "s", "b"); err == nil || IsPermanent(err) {
		t.Errorf("connection refused: %v, want a retryable error", err)
	}
}

func TestSend_STARTTLSVerifiesTheCertificate(t *testing.T) {
	tlsCfg, ca := testCA(t)
	relay := startRelay(t, &fakeRelay{tlsCfg: tlsCfg, startTLS: true})

	cfg := relay.config(TLSStartTLS)
	cfg.CAFile = ca
	if err := send(cfg, "ana@example.org", "s", "b"); err != nil {
		t.Fatalf("with the CA trusted: %v", err)
	}
	if len(relay.delivered()) != 1 || !relay.sawTLS {
		t.Errorf("delivered %d, saw TLS %v", len(relay.delivered()), relay.sawTLS)
	}

	// Without the CA the certificate is not trusted, and the mail is not sent
	// anyway: there is no way to skip verification and no fallback.
	relay2 := startRelay(t, &fakeRelay{tlsCfg: tlsCfg, startTLS: true})
	err := send(relay2.config(TLSStartTLS), "ana@example.org", "s", "b")
	if err == nil || IsPermanent(err) || !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("untrusted certificate: %v, want a retryable STARTTLS failure", err)
	}
	if len(relay2.delivered()) != 0 || relay2.saw("MAIL") {
		t.Error("mail was sent over a connection whose certificate was not trusted")
	}
}

func TestSend_STARTTLSIsNeverQuietlyDowngraded(t *testing.T) {
	relay := startRelay(t, &fakeRelay{startTLS: false, authMech: "PLAIN"})
	cfg := relay.config(TLSStartTLS)
	cfg.Username, cfg.Password = "user", "secret"
	err := send(cfg, "ana@example.org", "s", "b")
	if err == nil || !strings.Contains(err.Error(), "does not offer STARTTLS") {
		t.Fatalf("got %v, want a refusal because the relay does not offer STARTTLS", err)
	}
	if relay.saw("MAIL") || relay.saw("AUTH") || len(relay.credentials()) != 0 || len(relay.delivered()) != 0 {
		t.Error("mail or a password was sent in the clear after STARTTLS was asked for")
	}
}

func TestSend_ImplicitTLS(t *testing.T) {
	tlsCfg, ca := testCA(t)
	relay := startRelay(t, &fakeRelay{tlsCfg: tlsCfg, implicit: true})
	cfg := relay.config(TLSImplicit)
	cfg.CAFile = ca
	if err := send(cfg, "ana@example.org", "s", "b"); err != nil || len(relay.delivered()) != 1 {
		t.Fatalf("with the CA trusted: %v, delivered %d", err, len(relay.delivered()))
	}

	relay2 := startRelay(t, &fakeRelay{tlsCfg: tlsCfg, implicit: true})
	if err := send(relay2.config(TLSImplicit), "ana@example.org", "s", "b"); err == nil || len(relay2.delivered()) != 0 {
		t.Errorf("an untrusted certificate was accepted: %v", err)
	}
	// Talking TLS to a relay that is not: a failed handshake, nothing sent.
	plain := startRelay(t, &fakeRelay{})
	cfg = plain.config(TLSImplicit)
	cfg.CAFile = ca
	cfg.Timeout = time.Second
	if err := send(cfg, "ana@example.org", "s", "b"); err == nil || len(plain.delivered()) != 0 {
		t.Errorf("implicit TLS to a plaintext relay: %v", err)
	}
}

func TestSend_Authentication(t *testing.T) {
	tlsCfg, ca := testCA(t)

	// PLAIN and LOGIN both work, over STARTTLS.
	for _, mech := range []string{"PLAIN", "LOGIN", "LOGIN PLAIN", "GSSAPI LOGIN"} {
		relay := startRelay(t, &fakeRelay{tlsCfg: tlsCfg, startTLS: true, authMech: mech, user: "svc", pass: "s3cret"})
		cfg := relay.config(TLSStartTLS)
		cfg.CAFile, cfg.Username, cfg.Password = ca, "svc", "s3cret"
		if err := send(cfg, "ana@example.org", "s", "b"); err != nil {
			t.Errorf("%s: %v", mech, err)
			continue
		}
		if got := relay.credentials(); len(got) != 1 || got[0] != "svc:s3cret" || len(relay.delivered()) != 1 {
			t.Errorf("%s: credentials %v, delivered %d", mech, got, len(relay.delivered()))
		}
	}

	// A wrong password is retryable (an operator can fix it) and sends nothing.
	relay := startRelay(t, &fakeRelay{tlsCfg: tlsCfg, startTLS: true, authMech: "PLAIN", user: "svc", pass: "right"})
	cfg := relay.config(TLSStartTLS)
	cfg.CAFile, cfg.Username, cfg.Password = ca, "svc", "wrong"
	err := send(cfg, "ana@example.org", "s", "b")
	if err == nil || IsPermanent(err) || !strings.Contains(err.Error(), "log in") || len(relay.delivered()) != 0 {
		t.Errorf("wrong password: %v, delivered %d", err, len(relay.delivered()))
	}

	// A user name against a relay with no AUTH, or only a mechanism we do not
	// speak, is an error that sends nothing.
	for name, mech := range map[string]string{"no AUTH offered": "", "only GSSAPI": "GSSAPI"} {
		relay := startRelay(t, &fakeRelay{tlsCfg: tlsCfg, startTLS: true, authMech: mech})
		cfg := relay.config(TLSStartTLS)
		cfg.CAFile, cfg.Username, cfg.Password = ca, "svc", "pw"
		if err := send(cfg, "ana@example.org", "s", "b"); err == nil || len(relay.delivered()) != 0 {
			t.Errorf("%s: %v, delivered %d", name, err, len(relay.delivered()))
		}
	}

	// An anonymous relay needs no login and is not offered a password.
	relay = startRelay(t, &fakeRelay{authMech: "PLAIN"})
	if err := send(relay.config(TLSNone), "ana@example.org", "s", "b"); err != nil || len(relay.credentials()) != 0 || len(relay.delivered()) != 1 {
		t.Errorf("anonymous: %v, credentials %v", err, relay.credentials())
	}
}

func TestSend_APasswordIsNeverSentInTheClearToARemoteRelay(t *testing.T) {
	relay := startRelay(t, &fakeRelay{authMech: "PLAIN"})
	cfg := relay.config(TLSNone)
	cfg.Host, cfg.Username, cfg.Password = "relay.example.test", "svc", "s3cret"
	// The name does not resolve here; connect to the fake relay regardless.
	cfg.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort("127.0.0.1", strconv.Itoa(relay.port())))
	}
	err := send(cfg, "ana@example.org", "s", "b")
	if err == nil || !strings.Contains(err.Error(), "in the clear") {
		t.Fatalf("got %v, want a refusal to send the password in the clear", err)
	}
	if len(relay.credentials()) != 0 || relay.saw("AUTH") || relay.saw("MAIL") {
		t.Error("a password or mail went out unencrypted to a remote host")
	}

	// The local machine is the one exception.
	local := startRelay(t, &fakeRelay{authMech: "PLAIN"})
	cfg = local.config(TLSNone)
	cfg.Username, cfg.Password = "svc", "pw"
	if err := send(cfg, "ana@example.org", "s", "b"); err != nil || len(local.credentials()) != 1 {
		t.Errorf("loopback relay: %v, credentials %v", err, local.credentials())
	}
}

func TestAuthAllowed(t *testing.T) {
	for _, c := range []struct {
		host      string
		encrypted bool
		want      bool
	}{
		{"relay.example.com", true, true}, {"relay.example.com", false, false}, {"10.0.0.5", false, false},
		{"localhost", false, true}, {"127.0.0.1", false, true}, {"::1", false, true}, {"127.0.0.2", false, true},
		{"localhost.evil.example", false, false}, {"0.0.0.0", false, false},
	} {
		if got := authAllowed(c.host, c.encrypted); got != c.want {
			t.Errorf("authAllowed(%q, %v) = %v, want %v", c.host, c.encrypted, got, c.want)
		}
	}
}

func TestSend_RefusesAnythingThatCouldInjectAHeaderOrACommand(t *testing.T) {
	relay := startRelay(t, &fakeRelay{})
	cfg := relay.config(TLSNone)
	bad := map[string]Message{
		"newline in the subject":          {To: "ana@example.org", Subject: "hi\r\nBcc: boss@example.org", Body: "b"},
		"lone LF in the subject":          {To: "ana@example.org", Subject: "hi\nBcc: boss@example.org", Body: "b"},
		"NUL in the subject":              {To: "ana@example.org", Subject: "hi\x00", Body: "b"},
		"command smuggled in the address": {To: "ana@example.org\r\nRCPT TO:<boss@example.org>", Subject: "s", Body: "b"},
		"two recipients":                  {To: "ana@example.org, boss@example.org", Subject: "s", Body: "b"},
		"a display name":                  {To: "Ana <ana@example.org>", Subject: "s", Body: "b"},
		"no domain":                       {To: "ana", Subject: "s", Body: "b"},
		"empty recipient":                 {To: "", Subject: "s", Body: "b"},
	}
	for name, m := range bad {
		if err := cfg.Send(context.Background(), m); err == nil {
			t.Errorf("%s: was sent", name)
		}
	}
	if relay.saw("MAIL") || relay.saw("RCPT") || len(relay.delivered()) != 0 {
		t.Errorf("a refused message reached the relay: %v", relay.commands)
	}

	cfg.FromName = "Sparky\r\nBcc: boss@example.org"
	if err := send(cfg, "ana@example.org", "s", "b"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("newline in the from name: %v, want ErrNotConfigured", err)
	}
}

func TestParseAddress(t *testing.T) {
	// Whitespace around a typed address is trimmed away; only the trimmed form
	// is ever used.
	good := []string{"ana@example.org", "a.b+tag@sub.example.co.uk", "  ana@example.org  ", "o'brien@example.org", "ana@example.org\r\n"}
	for _, a := range good {
		if got, err := ParseAddress(a); err != nil || got != strings.TrimSpace(a) {
			t.Errorf("ParseAddress(%q) = %q, %v", a, got, err)
		}
	}
	bad := []string{
		"", "   ", "ana", "@example.org", "ana@", "ana@example", "ana@example.", "ana@@example.org", "a b@example.org",
		"ana@example.org\r\nBcc: x@y.com", "ana@example.org\nBcc: x@y.com", "<ana@example.org>", "Ana <ana@example.org>", `"ana"@example.org`,
		"ana@example.org,boss@example.org", "ana@example.org;boss@example.org", "ana@exam\x00ple.org",
		strings.Repeat("a", 250) + "@example.org",
	}
	for _, a := range bad {
		if got, err := ParseAddress(a); err == nil {
			t.Errorf("ParseAddress(%q) accepted as %q", a, got)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	ok := Config{Host: "relay.example.com", Port: 587, TLSMode: TLSStartTLS, FromAddress: "sparky@example.com", FromName: "Sparky"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a valid config: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"no host":         func(c *Config) { c.Host = "" },
		"host with space": func(c *Config) { c.Host = "relay example" },
		"host with slash": func(c *Config) { c.Host = "relay.example.com/x" },
		"port 0":          func(c *Config) { c.Port = 0 },
		"port 70000":      func(c *Config) { c.Port = 70000 },
		"unknown TLS":     func(c *Config) { c.TLSMode = "ssl3" },
		"empty TLS":       func(c *Config) { c.TLSMode = "" },
		"bad from":        func(c *Config) { c.FromAddress = "sparky" },
		"no from":         func(c *Config) { c.FromAddress = "" },
		"from name LF":    func(c *Config) { c.FromName = "a\nb" },
	} {
		c := ok
		mutate(&c)
		if err := c.Validate(); !errors.Is(err, ErrNotConfigured) {
			t.Errorf("%s: %v, want ErrNotConfigured", name, err)
		}
		if err := send(c, "ana@example.org", "s", "b"); !errors.Is(err, ErrNotConfigured) {
			t.Errorf("%s: Send returned %v, want ErrNotConfigured and no connection attempt", name, err)
		}
	}
}

func TestSend_ATroubledCAFileIsAnError(t *testing.T) {
	relay := startRelay(t, &fakeRelay{})
	cfg := relay.config(TLSNone)
	cfg.CAFile = "/nonexistent/ca.pem"
	if err := send(cfg, "ana@example.org", "s", "b"); err == nil || !strings.Contains(err.Error(), "CA file") {
		t.Errorf("missing CA file: %v", err)
	}
	empty := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(empty, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.CAFile = empty
	if err := send(cfg, "ana@example.org", "s", "b"); err == nil || !strings.Contains(err.Error(), "no certificates") {
		t.Errorf("garbage CA file: %v", err)
	}
	if len(relay.delivered()) != 0 {
		t.Error("mail was sent despite an unusable CA file")
	}
}

func TestSend_AStalledRelayTimesOutAndACancelledContextStops(t *testing.T) {
	relay := startRelay(t, &fakeRelay{hang: true})
	cfg := relay.config(TLSNone)
	cfg.Timeout = 300 * time.Millisecond
	start := time.Now()
	if err := send(cfg, "ana@example.org", "s", "b"); err == nil {
		t.Fatal("a relay that never speaks was waited on successfully")
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("took %v to give up on a stalled relay", time.Since(start))
	}

	cfg.Timeout = 30 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cfg.Send(ctx, Message{To: "ana@example.org", Subject: "s", Body: "b"}); err == nil {
		t.Error("a cancelled context still sent")
	}
}
