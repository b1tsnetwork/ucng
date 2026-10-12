// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Betriebsarten. Beide stecken im selben Programm – umgeschaltet wird mit MODE.
//
//   - simple: wie bisher. Namen werden ohne Konto reserviert (Token im Browser). Ideal für
//     Intranet, Vereine, private Nutzung.
//   - hosted: Nutzerkonten mit Passwort, optional 2FA (TOTP), E-Mail-Bestätigung über SMTP,
//     Ende-zu-Ende-verschlüsselte Direktnachrichten und Föderation (name@domain).
const (
	ModeSimple = "simple"
	ModeHosted = "hosted"
)

func (s *Server) hosted() bool { return s.cfg.Mode == ModeHosted }
