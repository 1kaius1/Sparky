// SPDX-License-Identifier: AGPL-3.0-or-later

// Package mailer sends one email through an SMTP relay. It supports only a
// properly configured relay - with a user name and password, or an anonymous
// relay that trusts the server's address - and never sends directly to
// recipients' mail servers (PLANNING.md, 2026-10-26: deliverability needs SPF,
// DKIM and reverse DNS, the standard library cannot DKIM-sign, and outbound
// port 25 is commonly blocked; a local MTA on localhost covers the small case).
//
// The standard library's net/smtp is enough, so there is no new dependency.
// What this package adds on top of it are the guarantees an alert system needs
// and net/smtp does not give by itself:
//
//   - the relay's certificate is always verified (there is no option to skip
//     it), against the system roots or an extra CA file;
//   - a connection that was meant to be encrypted is never quietly sent in the
//     clear: if STARTTLS is asked for and the relay does not offer it, nothing
//     is sent;
//   - a password is never sent over an unencrypted connection to anything but
//     the local machine;
//   - no header can be injected through an address or a subject.
package mailer

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"time"
)

// TLSMode says how the connection to the relay is protected.
type TLSMode string

const (
	// TLSNone is a plain connection, for a relay on localhost or a trusted
	// internal network. Credentials are refused over it unless the relay is on
	// this machine.
	TLSNone TLSMode = "none"

	// TLSStartTLS connects in the clear and upgrades with STARTTLS (usually
	// port 587). A relay that does not offer STARTTLS is an error, not a
	// downgrade.
	TLSStartTLS TLSMode = "starttls"

	// TLSImplicit speaks TLS from the first byte (usually port 465).
	TLSImplicit TLSMode = "tls"
)

// Valid reports whether m is one of the three modes.
func (m TLSMode) Valid() bool {
	return m == TLSNone || m == TLSStartTLS || m == TLSImplicit
}

// DefaultTimeout bounds one whole delivery attempt: connecting, the
// conversation and the transfer. A relay that stalls cannot hold the queue.
const DefaultTimeout = 60 * time.Second

// Config is everything needed to reach a relay and who mail is from.
type Config struct {
	Host    string
	Port    int
	TLSMode TLSMode

	// Username and Password are used if Username is set; an anonymous relay
	// leaves both empty.
	Username string
	Password string

	// CAFile is a PEM file of extra authorities to trust for the relay's
	// certificate, "" for the system roots only.
	CAFile string

	FromAddress string
	FromName    string

	// HelloName is what this server calls itself in EHLO, "" for "localhost".
	HelloName string

	// Timeout bounds one attempt; zero means DefaultTimeout.
	Timeout time.Duration

	// dial is a hook for tests; nil uses a net.Dialer.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Message is one email: plain text, to a single recipient.
type Message struct {
	To      string
	Subject string
	Body    string
}

var (
	// ErrNotConfigured is returned when the config cannot be used at all.
	ErrNotConfigured = errors.New("email is not configured")

	// ErrPermanent is wrapped by a failure that retrying cannot fix: the relay
	// refused the recipient or the message itself (a 5xx reply to RCPT TO or the
	// end of DATA). Everything else - an unreachable relay, a certificate
	// problem, a login the relay refused, a 4xx - is worth retrying, because an
	// operator may fix it and queued mail should not be lost to a settings
	// mistake.
	ErrPermanent = errors.New("the relay refused this message")
)

// IsPermanent reports whether err is a failure that retrying cannot fix.
func IsPermanent(err error) bool { return errors.Is(err, ErrPermanent) }

// Validate checks the parts of the config that do not need a network.
func (c Config) Validate() error {
	switch {
	case strings.TrimSpace(c.Host) == "" || strings.ContainsAny(c.Host, " \t\r\n/"):
		return fmt.Errorf("%w: the relay host is missing or malformed", ErrNotConfigured)
	case c.Port < 1 || c.Port > 65535:
		return fmt.Errorf("%w: the port must be between 1 and 65535", ErrNotConfigured)
	case !c.TLSMode.Valid():
		return fmt.Errorf("%w: unknown TLS mode %q", ErrNotConfigured, c.TLSMode)
	}
	if _, err := ParseAddress(c.FromAddress); err != nil {
		return fmt.Errorf("%w: the from address: %v", ErrNotConfigured, err)
	}
	if hasControl(c.FromName) {
		return fmt.Errorf("%w: the from name has control characters", ErrNotConfigured)
	}
	return nil
}

// ParseAddress validates an email address and returns it in plain
// "local@domain" form. It accepts only one bare address - no display name, no
// list, no comment - with a dotted domain, at most 254 bytes, and no control
// characters, because the result goes straight into SMTP commands and headers.
func ParseAddress(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("the address is empty")
	}
	if len(s) > 254 || hasControl(s) || strings.ContainsAny(s, "<>,;\"") {
		return "", errors.New("the address is not valid")
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || a.Name != "" {
		return "", errors.New("the address is not valid")
	}
	at := strings.LastIndex(s, "@")
	if at < 1 || !strings.Contains(s[at+1:], ".") || strings.HasSuffix(s, ".") {
		return "", errors.New("the address needs a domain such as example.com")
	}
	return s, nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// tlsConfig builds the verification settings: the system roots, plus the extra
// CA file if there is one. InsecureSkipVerify is never set.
func (c Config) tlsConfig() (*tls.Config, error) {
	cfg := &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}
	if c.CAFile == "" {
		return cfg, nil
	}
	pem, err := os.ReadFile(c.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read the CA file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("the CA file holds no certificates")
	}
	cfg.RootCAs = pool
	return cfg, nil
}

// Send delivers m through the relay. It returns nil only once the relay has
// accepted the message.
func (c Config) Send(ctx context.Context, m Message) error {
	if err := c.Validate(); err != nil {
		return err
	}
	to, err := ParseAddress(m.To)
	if err != nil {
		return fmt.Errorf("the recipient: %w", err)
	}
	raw, err := c.build(to, m)
	if err != nil {
		return err
	}
	tlsCfg, err := c.tlsConfig()
	if err != nil {
		return err
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	dial := c.dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", addr, err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	encrypted := false
	if c.TLSMode == TLSImplicit {
		tc := tls.Client(conn, tlsCfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("TLS handshake with %s: %w", addr, err)
		}
		conn, encrypted = tc, true
	}

	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		return fmt.Errorf("start the conversation with %s: %w", addr, err)
	}
	defer client.Close()

	hello := c.HelloName
	if hello == "" {
		hello = "localhost"
	}
	if err := client.Hello(hello); err != nil {
		return fmt.Errorf("EHLO: %w", err)
	}

	if c.TLSMode == TLSStartTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			// Never downgrade: the point of asking for STARTTLS is that the
			// mail and any password are not sent in the clear.
			return fmt.Errorf("%s does not offer STARTTLS, so nothing was sent (use a relay that does, or set the TLS mode to none for a relay on a trusted network)", addr)
		}
		if err := client.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("STARTTLS with %s: %w", addr, err)
		}
		encrypted = true
	}

	if c.Username != "" {
		if err := c.authenticate(client, encrypted); err != nil {
			return err
		}
	}

	if err := client.Mail(c.FromAddress); err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return classify(fmt.Errorf("RCPT TO: %w", err))
	}
	w, err := client.Data()
	if err != nil {
		return classify(fmt.Errorf("DATA: %w", err))
	}
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("send the message: %w", err)
	}
	if err := w.Close(); err != nil {
		return classify(fmt.Errorf("end of message: %w", err))
	}
	// The relay has the message. A failure to say goodbye changes nothing.
	_ = client.Quit()
	return nil
}

// classify marks a relay's 5xx reply to the recipient or the message as
// permanent.
func classify(err error) error {
	var tpErr *textproto.Error
	if errors.As(err, &tpErr) && tpErr.Code >= 500 && tpErr.Code < 600 {
		return fmt.Errorf("%w: %v", ErrPermanent, err)
	}
	return err
}

// authenticate logs in with PLAIN, or LOGIN if that is all the relay offers. It
// refuses to send a password over an unencrypted connection to anything but
// this machine.
func (c Config) authenticate(client *smtp.Client, encrypted bool) error {
	if !authAllowed(c.Host, encrypted) {
		return fmt.Errorf("refusing to send the relay password in the clear to %s: use STARTTLS or TLS", c.Host)
	}
	ok, mechs := client.Extension("AUTH")
	if !ok {
		return fmt.Errorf("a user name is set but %s does not offer authentication", c.Host)
	}
	var auth smtp.Auth
	switch {
	case hasMech(mechs, "PLAIN"):
		auth = plainAuth{user: c.Username, pass: c.Password}
	case hasMech(mechs, "LOGIN"):
		auth = &loginAuth{user: c.Username, pass: c.Password}
	default:
		return fmt.Errorf("%s offers no login method this server supports (it offers: %s)", c.Host, mechs)
	}
	if err := client.Auth(auth); err != nil {
		return fmt.Errorf("log in to the relay: %w", err)
	}
	return nil
}

// authAllowed reports whether a password may be sent: over an encrypted
// connection always, unencrypted only to the local machine.
func authAllowed(host string, encrypted bool) bool {
	if encrypted {
		return true
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func hasMech(list, want string) bool {
	for _, m := range strings.Fields(list) {
		if strings.EqualFold(m, want) {
			return true
		}
	}
	return false
}

// plainAuth is SMTP AUTH PLAIN. net/smtp's own PlainAuth would do, but then two
// places would decide whether a password may be sent; authAllowed is the one.
type plainAuth struct{ user, pass string }

func (a plainAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.pass), nil
}

// Next is called by net/smtp for every reply, including the final 235 success
// (more is false then); only a real challenge is an error.
func (a plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("unexpected challenge")
	}
	return nil, nil
}

// loginAuth is the older AUTH LOGIN some relays still offer instead of PLAIN
// (Exchange does), which net/smtp does not implement.
type loginAuth struct{ user, pass string }

func (a *loginAuth) Start(*smtp.ServerInfo) (string, []byte, error) { return "LOGIN", nil, nil }

func (a *loginAuth) Next(challenge []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(challenge))) {
	case "username:":
		return []byte(a.user), nil
	case "password:":
		return []byte(a.pass), nil
	}
	return nil, fmt.Errorf("unexpected LOGIN challenge %q", challenge)
}

// build assembles the message with CRLF line ends, a quoted-printable UTF-8
// body and an RFC 2047 encoded subject. No header takes anything from the
// caller without being checked for line breaks first.
func (c Config) build(to string, m Message) ([]byte, error) {
	if hasControl(strings.NewReplacer("\t", " ").Replace(m.Subject)) {
		return nil, errors.New("the subject has control characters")
	}
	id := make([]byte, 12)
	if _, err := rand.Read(id); err != nil {
		return nil, fmt.Errorf("make a message id: %w", err)
	}
	domain := "localhost"
	if at := strings.LastIndex(c.FromAddress, "@"); at >= 0 {
		domain = c.FromAddress[at+1:]
	}
	from := (&mail.Address{Name: c.FromName, Address: c.FromAddress}).String()

	var b strings.Builder
	write := func(name, value string) { b.WriteString(name + ": " + value + "\r\n") }
	write("From", from)
	write("To", to)
	write("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	write("Date", time.Now().Format(time.RFC1123Z))
	write("Message-ID", "<"+hex.EncodeToString(id)+"@"+domain+">")
	write("MIME-Version", "1.0")
	write("Content-Type", "text/plain; charset=utf-8")
	write("Content-Transfer-Encoding", "quoted-printable")
	// Tells autoresponders and mail rules this is machine-sent and must not be
	// answered or looped.
	write("Auto-Submitted", "auto-generated")
	b.WriteString("\r\n")

	qp := quotedprintable.NewWriter(&stringWriter{&b})
	if _, err := io.WriteString(qp, m.Body); err != nil {
		return nil, fmt.Errorf("encode the body: %w", err)
	}
	if err := qp.Close(); err != nil {
		return nil, fmt.Errorf("encode the body: %w", err)
	}
	return []byte(b.String()), nil
}

type stringWriter struct{ b *strings.Builder }

func (w *stringWriter) Write(p []byte) (int, error) { return w.b.Write(p) }
