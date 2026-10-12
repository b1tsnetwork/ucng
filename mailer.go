// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// E-Mail-Versand per SMTP (nur Modus „hosted“): Bestätigung der Adresse, Passwort zurücksetzen.

var ErrNoSMTP = errors.New("Kein E-Mail-Versand eingerichtet (SMTP im Admin-Bereich unter „Konten“)")

// sendMail verschickt eine einfache Text-Mail. Für Tests austauschbar.
var sendMail = func(c SMTPConfig, to, subject, body string) error {
	if !c.Enabled() {
		return ErrNoSMTP
	}
	msg := buildMail(c.From, to, subject, body, time.Now())
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	var err error
	tlsCfg := &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}
	if c.Security == "tls" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	cl, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer cl.Close()
	if c.Security == "starttls" {
		if ok, _ := cl.Extension("STARTTLS"); !ok {
			return errors.New("Der Mailserver unterstützt kein STARTTLS")
		}
		if err := cl.StartTLS(tlsCfg); err != nil {
			return err
		}
	}
	if c.User != "" {
		// PlainAuth verweigert unverschlüsselte Verbindungen (außer zu localhost) – gewollt.
		if err := cl.Auth(smtp.PlainAuth("", c.User, c.Password, c.Host)); err != nil {
			return err
		}
	}
	if err := cl.Mail(addrOf(c.From)); err != nil {
		return err
	}
	if err := cl.Rcpt(to); err != nil {
		return err
	}
	w, err := cl.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return cl.Quit()
}

func buildMail(from, to, subject, body string, now time.Time) []byte {
	var b bytes.Buffer
	id := make([]byte, 12)
	rand.Read(id)
	domain := "ucng"
	if i := strings.LastIndex(addrOf(from), "@"); i >= 0 {
		domain = addrOf(from)[i+1:]
	}
	hdr := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	hdr("From", encodeAddr(from))
	hdr("To", to)
	hdr("Subject", mime.QEncoding.Encode("utf-8", subject))
	hdr("Date", now.Format(time.RFC1123Z))
	hdr("Message-ID", "<"+hex.EncodeToString(id)+"@"+domain+">")
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", "text/plain; charset=utf-8")
	hdr("Content-Transfer-Encoding", "quoted-printable")
	hdr("Auto-Submitted", "auto-generated")
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	qp.Write([]byte(strings.ReplaceAll(body, "\n", "\r\n")))
	qp.Close()
	return b.Bytes()
}

// encodeAddr kodiert den Anzeigenamen in "Name <adresse>".
func encodeAddr(s string) string {
	if i := strings.LastIndex(s, "<"); i > 0 && strings.HasSuffix(s, ">") {
		name := strings.Trim(strings.TrimSpace(s[:i]), `"`)
		return mime.QEncoding.Encode("utf-8", name) + " " + s[i:]
	}
	return s
}

// --- Texte ---

func (s *Server) baseURL() string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL
	}
	return "http://localhost" + displayAddr(s.cfg.Addr)
}

// Texte der E-Mails stehen in den Sprachdateien (mail.*); lang ist die Sprache der Anfrage.
func (s *Server) mailVerify(lang, name, email, ticket string) error {
	cat := s.hub.I18n()
	vars := map[string]any{"name": name, "title": s.hub.Settings().Title, "link": s.baseURL() + "/api/account/verify?t=" + ticket}
	return sendMail(s.hub.Hosted().Config().SMTP, email, cat.T(lang, "mail.verify_subject", vars), cat.T(lang, "mail.verify_body", vars))
}

func (s *Server) mailReset(lang, name, email, ticket string) error {
	cat := s.hub.I18n()
	vars := map[string]any{"name": name, "title": s.hub.Settings().Title, "link": s.baseURL() + "/?reset=" + ticket}
	return sendMail(s.hub.Hosted().Config().SMTP, email, cat.T(lang, "mail.reset_subject", vars), cat.T(lang, "mail.reset_body", vars))
}
