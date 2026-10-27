// SPDX-License-Identifier: AGPL-3.0-or-later

package mailer

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRelay is a minimal SMTP server for the tests: enough of the protocol to
// exercise the client's TLS, login and error handling, with every command's
// reply overridable.
type fakeRelay struct {
	t        *testing.T
	listener net.Listener
	tlsCfg   *tls.Config

	// What it offers.
	startTLS bool
	authMech string // "PLAIN", "LOGIN", "PLAIN LOGIN" or ""
	implicit bool   // speak TLS from the first byte

	// Scripted replies; the key is the command word (EHLO, MAIL, RCPT, DATA,
	// END for the reply after the message body, AUTH). The greeting is "greet".
	reply map[string]string

	// The password it expects ("" accepts any).
	user, pass string
	hang       bool // accept, then never speak

	mu        sync.Mutex
	mails     []receivedMail
	creds     []string // "user:pass" for each login that arrived
	commands  []string // every command word seen, in order
	sawTLS    bool
	plaintext []string // commands that arrived before any TLS
}

type receivedMail struct {
	from, to string
	data     string
}

// testCA writes a self-signed certificate for 127.0.0.1 and localhost to a PEM
// file, and returns the server's TLS config and the file's path.
func testCA(t *testing.T) (*tls.Config, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test relay"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}, path
}

func startRelay(t *testing.T, relay *fakeRelay) *fakeRelay {
	t.Helper()
	relay.t = t
	if relay.reply == nil {
		relay.reply = map[string]string{}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	relay.listener = l
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go relay.serve(conn)
		}
	}()
	return relay
}

func (r *fakeRelay) port() int { return r.listener.Addr().(*net.TCPAddr).Port }

// config is a client Config aimed at this relay.
func (r *fakeRelay) config(mode TLSMode) Config {
	return Config{
		Host: "127.0.0.1", Port: r.port(), TLSMode: mode,
		FromAddress: "sparky@example.com", FromName: "Sparky",
		Timeout: 5 * time.Second,
	}
}

func (r *fakeRelay) say(w *bufio.Writer, line string) {
	_, _ = w.WriteString(line + "\r\n")
	_ = w.Flush()
}

func (r *fakeRelay) replyOr(key, def string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.reply[key]; ok {
		return v
	}
	return def
}

func (r *fakeRelay) serve(raw net.Conn) {
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
	if r.hang {
		time.Sleep(10 * time.Second)
		return
	}
	var conn net.Conn = raw
	encrypted := false
	if r.implicit {
		conn = tls.Server(raw, r.tlsCfg)
		encrypted = true
		r.mu.Lock()
		r.sawTLS = true
		r.mu.Unlock()
	}
	br, bw := bufio.NewReader(conn), bufio.NewWriter(conn)
	r.say(bw, r.replyOr("greet", "220 relay ready"))

	var from, to string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		word := strings.ToUpper(strings.Fields(line + " x")[0])
		r.mu.Lock()
		r.commands = append(r.commands, word)
		if !encrypted {
			r.plaintext = append(r.plaintext, word)
		}
		r.mu.Unlock()

		switch word {
		case "EHLO", "HELO":
			if v := r.replyOr("EHLO", ""); v != "" {
				r.say(bw, v)
				continue
			}
			lines := []string{"250-relay"}
			if r.startTLS && !encrypted {
				lines = append(lines, "250-STARTTLS")
			}
			if r.authMech != "" {
				lines = append(lines, "250-AUTH "+r.authMech)
			}
			lines = append(lines, "250 8BITMIME")
			for _, l := range lines {
				_, _ = bw.WriteString(l + "\r\n")
			}
			_ = bw.Flush()
		case "STARTTLS":
			r.say(bw, "220 go ahead")
			conn = tls.Server(raw, r.tlsCfg)
			br, bw = bufio.NewReader(conn), bufio.NewWriter(conn)
			encrypted = true
			r.mu.Lock()
			r.sawTLS = true
			r.mu.Unlock()
		case "AUTH":
			r.handleAuth(br, bw, line)
		case "MAIL":
			from = between(line, '<', '>')
			r.say(bw, r.replyOr("MAIL", "250 ok"))
		case "RCPT":
			to = between(line, '<', '>')
			r.say(bw, r.replyOr("RCPT", "250 ok"))
		case "DATA":
			if v := r.replyOr("DATA", "354 go"); !strings.HasPrefix(v, "354") {
				r.say(bw, v)
				continue
			}
			r.say(bw, "354 go")
			var data strings.Builder
			for {
				l, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				data.WriteString(strings.TrimPrefix(l, ".")) // undo dot-stuffing
			}
			end := r.replyOr("END", "250 queued")
			if strings.HasPrefix(end, "250") {
				r.mu.Lock()
				r.mails = append(r.mails, receivedMail{from: from, to: to, data: data.String()})
				r.mu.Unlock()
			}
			r.say(bw, end)
		case "QUIT":
			r.say(bw, "221 bye")
			return
		default:
			r.say(bw, "502 not implemented")
		}
	}
}

func (r *fakeRelay) handleAuth(br *bufio.Reader, bw *bufio.Writer, line string) {
	parts := strings.Fields(line)
	if len(parts) < 2 {
		r.say(bw, "501 syntax")
		return
	}
	var user, pass string
	switch strings.ToUpper(parts[1]) {
	case "PLAIN":
		enc := ""
		if len(parts) > 2 {
			enc = parts[2]
		} else {
			r.say(bw, "334 ")
			l, _ := br.ReadString('\n')
			enc = strings.TrimSpace(l)
		}
		dec, _ := base64.StdEncoding.DecodeString(enc)
		f := strings.Split(string(dec), "\x00")
		if len(f) == 3 {
			user, pass = f[1], f[2]
		}
	case "LOGIN":
		r.say(bw, "334 "+base64.StdEncoding.EncodeToString([]byte("Username:")))
		l, _ := br.ReadString('\n')
		u, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(l))
		r.say(bw, "334 "+base64.StdEncoding.EncodeToString([]byte("Password:")))
		l, _ = br.ReadString('\n')
		p, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(l))
		user, pass = string(u), string(p)
	default:
		r.say(bw, "504 unsupported")
		return
	}
	r.mu.Lock()
	r.creds = append(r.creds, user+":"+pass)
	r.mu.Unlock()
	if (r.user != "" && user != r.user) || (r.pass != "" && pass != r.pass) {
		r.say(bw, "535 5.7.8 authentication failed")
		return
	}
	r.say(bw, "235 ok")
}

func between(s string, open, close byte) string {
	i, j := strings.IndexByte(s, open), strings.LastIndexByte(s, close)
	if i < 0 || j <= i {
		return ""
	}
	return s[i+1 : j]
}

func (r *fakeRelay) delivered() []receivedMail {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]receivedMail(nil), r.mails...)
}

func (r *fakeRelay) credentials() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.creds...)
}

func (r *fakeRelay) saw(word string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.commands {
		if c == word {
			return true
		}
	}
	return false
}

func (r *fakeRelay) String() string { return fmt.Sprintf("relay on %d", r.port()) }
