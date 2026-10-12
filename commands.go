// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Der eingebaute Befehls-Bot antwortet auf Nachrichten, die mit dem Präfix beginnen
// (Standard „!“). Es gibt fertige Befehle (Hilfe, Würfel, Münze, Uhrzeit, Online, Auswahl)
// und beliebig viele eigene Text-Befehle. Antworten sind Vorlagen mit Platzhaltern:
//
//	{user} Name der Person   {room} Kanal   {args} alles nach dem Befehl
//	{result} Ergebnis des Befehls (Würfelzahl, Liste …)   {time} {date} {online}
//
// Formatierung und Emotes (:name:) funktionieren in den Antworten wie im Chat.

const (
	CmdBotID = "cmdbot"

	CmdText   = "text"
	CmdHelp   = "help"
	CmdDice   = "dice"
	CmdCoin   = "coin"
	CmdTime   = "time"
	CmdOnline = "online"
	CmdChoose = "choose"
	CmdPoll   = "poll" // Umfrage: die Person selbst postet eine Umfrage (kein Bot-Text)
)

var cmdKinds = map[string]bool{CmdText: true, CmdHelp: true, CmdDice: true, CmdCoin: true, CmdTime: true, CmdOnline: true, CmdChoose: true, CmdPoll: true}

type Command struct {
	ID          string   `json:"id"`
	Trigger     string   `json:"trigger"`
	Aliases     []string `json:"aliases"`
	Kind        string   `json:"kind"`
	Response    string   `json:"response"`
	Description string   `json:"description"`
	Cooldown    int      `json:"cooldown"`  // Sekunden je Kanal (0 = keine)
	TeamOnly    bool     `json:"teamOnly"`  // nur Personen mit Team-Abzeichen
	Rooms       []string `json:"rooms"`     // leer = alle Kanäle
	Reply       bool     `json:"reply"`     // als Antwort auf die Nachricht (mit Zitat)
	Ephemeral   bool     `json:"ephemeral"` // nur für die Person sichtbar, nichts landet im Kanal
	Enabled     bool     `json:"enabled"`
}

type CommandsConfig struct {
	Version  int       `json:"version,omitempty"` // für neue fertige Befehle nach Updates
	Enabled  bool      `json:"enabled"`
	Prefix   string    `json:"prefix"`
	BotName  string    `json:"botName"`
	Commands []Command `json:"commands"`
}

var (
	cmdTriggerRe   = regexp.MustCompile(`^[\p{Ll}\p{N}_-]{1,24}$`)
	ErrCmdTrigger  = errors.New("Befehl: 1–24 Zeichen, Kleinbuchstaben, Ziffern, - und _")
	ErrCmdDup      = errors.New("Dieser Befehl oder Alias ist mehrfach vergeben")
	ErrCmdKind     = errors.New("Unbekannte Art von Befehl")
	ErrCmdResponse = errors.New("Antwort: 1–1000 Zeichen")
	ErrCmdPrefix   = errors.New("Präfix muss eines dieser Zeichen sein: ! / . ? $ #")
	ErrCmdCooldown = errors.New("Abklingzeit: 0 bis 3600 Sekunden")
	ErrCmdDesc     = errors.New("Beschreibung: höchstens 100 Zeichen")
	ErrCmdTooMany  = errors.New("Höchstens 200 Befehle")
)

const commandsVersion = 2

func pollCommand() Command {
	return Command{ID: "c_umfrage", Trigger: "umfrage", Aliases: []string{"poll", "abstimmung"}, Kind: CmdPoll, Enabled: true,
		Description: "Umfrage starten: !umfrage Frage? | Antwort 1 | Antwort 2"}
}

func defaultCommands() CommandsConfig {
	return CommandsConfig{Version: commandsVersion, Enabled: true, Prefix: "!", BotName: "Chat-Bot", Commands: []Command{
		{ID: "c_hilfe", Trigger: "hilfe", Aliases: []string{"help", "befehle"}, Kind: CmdHelp, Enabled: true, Ephemeral: true,
			Description: "Zeigt alle Befehle", Response: "Das kann ich:\n{result}"},
		{ID: "c_regeln", Trigger: "regeln", Aliases: []string{"rules"}, Kind: CmdText, Enabled: true, Cooldown: 30, Ephemeral: true,
			Description: "Die Regeln im Chat",
			Response:    "📜 **Unsere Regeln:** Sei freundlich und respektvoll, kein Spam, keine Werbung, keine persönlichen Daten. Danke, {user}!"},
		{ID: "c_wuerfel", Trigger: "würfel", Aliases: []string{"wuerfel", "dice"}, Kind: CmdDice, Enabled: true, Reply: true,
			Description: "Würfeln, z. B. !würfel 20", Response: "🎲 {user} würfelt eine **{result}**."},
		{ID: "c_muenze", Trigger: "münze", Aliases: []string{"muenze", "coin"}, Kind: CmdCoin, Enabled: true, Reply: true,
			Description: "Kopf oder Zahl", Response: "🪙 **{result}**!"},
		{ID: "c_zeit", Trigger: "zeit", Aliases: []string{"uhrzeit", "time"}, Kind: CmdTime, Enabled: true, Ephemeral: true,
			Description: "Aktuelle Uhrzeit", Response: "🕒 Es ist {time} Uhr am {date}."},
		{ID: "c_online", Trigger: "online", Aliases: []string{"wer"}, Kind: CmdOnline, Enabled: true, Cooldown: 10,
			Description: "Wer ist gerade hier?", Response: "👥 {result}"},
		pollCommand(),
		{ID: "c_waehle", Trigger: "wähle", Aliases: []string{"waehle", "choose"}, Kind: CmdChoose, Enabled: true, Reply: true,
			Description: "Zufällig wählen: !wähle Pizza | Pasta", Response: "🤔 Ich nehme: **{result}**"},
	}}
}

type Commands struct {
	mu      sync.Mutex
	cfg     CommandsConfig
	last    map[string]time.Time // Befehl + Kanal → letzte Ausführung
	changed func()
	rnd     func(n int) int
}

func NewCommands() *Commands {
	return &Commands{cfg: defaultCommands(), last: map[string]time.Time{}, rnd: func(n int) int { return rand.IntN(n) }}
}

func (c *Commands) Config() CommandsConfig {
	c.mu.Lock()
	defer c.mu.Unlock()
	return copyCommands(c.cfg)
}

func copyCommands(cfg CommandsConfig) CommandsConfig {
	out := cfg
	out.Commands = make([]Command, len(cfg.Commands))
	for i, cmd := range cfg.Commands {
		cmd.Aliases = append([]string{}, cmd.Aliases...)
		cmd.Rooms = append([]string{}, cmd.Rooms...)
		out.Commands[i] = cmd
	}
	return out
}

// BotName liefert den Namen des Bots (für die Namensreservierung).
func (c *Commands) BotName() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg.BotName
}

func normTrigger(s string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(s), "!/.?$#")))
}

func validateCommands(in CommandsConfig) (CommandsConfig, error) {
	if in.Prefix == "" {
		in.Prefix = "!"
	}
	if len(in.Prefix) != 1 || !strings.Contains("!/.?$#", in.Prefix) {
		return in, ErrCmdPrefix
	}
	name, ok := CleanName(in.BotName)
	if !ok {
		return in, ErrInvalidName
	}
	in.BotName = name
	if len(in.Commands) > 200 {
		return in, ErrCmdTooMany
	}
	seen := map[string]bool{}
	ids := map[string]bool{}
	for i := range in.Commands {
		cmd := &in.Commands[i]
		cmd.Trigger = normTrigger(cmd.Trigger)
		if !cmdTriggerRe.MatchString(cmd.Trigger) {
			return in, fmt.Errorf("%w (%q)", ErrCmdTrigger, cmd.Trigger)
		}
		if !cmdKinds[cmd.Kind] {
			return in, ErrCmdKind
		}
		var aliases []string
		for _, a := range cmd.Aliases {
			a = normTrigger(a)
			if a == "" {
				continue
			}
			if !cmdTriggerRe.MatchString(a) {
				return in, fmt.Errorf("%w (%q)", ErrCmdTrigger, a)
			}
			aliases = append(aliases, a)
		}
		cmd.Aliases = aliases
		if cmd.Aliases == nil {
			cmd.Aliases = []string{}
		}
		for _, t := range append([]string{cmd.Trigger}, cmd.Aliases...) {
			if seen[t] {
				return in, fmt.Errorf("%w: %s%s", ErrCmdDup, in.Prefix, t)
			}
			seen[t] = true
		}
		cmd.Response = strings.TrimSpace(cmd.Response)
		if n := len([]rune(cmd.Response)); n < 1 || n > 1000 {
			if !(cmd.Kind != CmdText && n == 0) {
				return in, fmt.Errorf("%w (%s%s)", ErrCmdResponse, in.Prefix, cmd.Trigger)
			}
			cmd.Response = "{result}"
		}
		cmd.Description = strings.Join(strings.Fields(cmd.Description), " ")
		if len([]rune(cmd.Description)) > 100 {
			return in, ErrCmdDesc
		}
		if cmd.Cooldown < 0 || cmd.Cooldown > 3600 {
			return in, ErrCmdCooldown
		}
		var rooms []string
		for _, r := range cmd.Rooms {
			if r = NormRoom(r); r != "" {
				if !ValidRoom(r) {
					return in, ErrInvalidRoom
				}
				rooms = append(rooms, r)
			}
		}
		cmd.Rooms = rooms
		if cmd.Rooms == nil {
			cmd.Rooms = []string{}
		}
		if cmd.ID == "" || ids[cmd.ID] {
			cmd.ID = "c_" + randomHex(4)
		}
		ids[cmd.ID] = true
	}
	if in.Commands == nil {
		in.Commands = []Command{}
	}
	return in, nil
}

func (c *Commands) Set(in CommandsConfig) (CommandsConfig, error) {
	out, err := validateCommands(in)
	if err != nil {
		return CommandsConfig{}, err
	}
	c.mu.Lock()
	c.cfg = copyCommands(out)
	c.mu.Unlock()
	if c.changed != nil {
		c.changed()
	}
	return out, nil
}

func (c *Commands) restore(in CommandsConfig) error {
	if in.Version < 2 { // Update: Umfragen kamen dazu
		has := false
		for _, cmd := range in.Commands {
			has = has || cmd.Kind == CmdPoll || cmd.Trigger == "umfrage"
		}
		if !has {
			in.Commands = append(in.Commands, pollCommand())
		}
		in.Version = commandsVersion
	}
	out, err := validateCommands(in)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.cfg = out
	c.mu.Unlock()
	return nil
}

func (c *Commands) renameRoom(old, neu string) {
	c.mu.Lock()
	changed := false
	for i := range c.cfg.Commands {
		for j, r := range c.cfg.Commands[i].Rooms {
			if r == old {
				c.cfg.Commands[i].Rooms[j] = neu
				changed = true
			}
		}
	}
	c.mu.Unlock()
	if changed && c.changed != nil {
		c.changed()
	}
}

// PublicCommand ist die Sicht im Chat (für die Vervollständigung).
type PublicCommand struct {
	Trigger     string `json:"trigger"`
	Description string `json:"description,omitempty"`
	TeamOnly    bool   `json:"teamOnly,omitempty"`
}

type PublicCommands struct {
	Enabled  bool            `json:"enabled"`
	Prefix   string          `json:"prefix"`
	BotName  string          `json:"botName"`
	Commands []PublicCommand `json:"commands"`
}

func (c *Commands) Public(room string) PublicCommands {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := PublicCommands{Enabled: c.cfg.Enabled, Prefix: c.cfg.Prefix, BotName: c.cfg.BotName, Commands: []PublicCommand{}}
	if !c.cfg.Enabled {
		return out
	}
	for _, cmd := range c.cfg.Commands {
		if cmd.Enabled && cmd.inRoom(room) {
			out.Commands = append(out.Commands, PublicCommand{Trigger: cmd.Trigger, Description: cmd.Description, TeamOnly: cmd.TeamOnly})
		}
	}
	return out
}

func (cmd Command) inRoom(room string) bool {
	if len(cmd.Rooms) == 0 || room == "" {
		return true
	}
	for _, r := range cmd.Rooms {
		if r == room {
			return true
		}
	}
	return false
}

// CmdContext beschreibt, wo und von wem ein Befehl kommt.
type CmdContext struct {
	Room   string
	User   string
	Team   bool
	UserID string // für die Abklingzeit privater Befehle (je Person)
	Now    time.Time
	Online func() Event // Online-Liste des Kanals
	DryRun bool         // Testfeld: keine Abklingzeit
	// Tr übersetzt eine deutsche Meldung (Schlüssel srv.<Format>) in die Sprache der Antwort:
	// öffentliche Antworten in der Standardsprache, private in der Sprache der Person.
	Tr func(msg string) string
}

func (ctx CmdContext) tr(msg string) string {
	if ctx.Tr == nil {
		return msg
	}
	return ctx.Tr(msg)
}

// CmdResult ist die Antwort des Bots.
type CmdResult struct {
	Matched bool   `json:"matched"`
	Command string `json:"command,omitempty"`
	Text    string `json:"text,omitempty"`
	Reply   bool   `json:"reply,omitempty"`
	// Ephemeral: Befehl und Antwort sieht nur die Person selbst (auch bei Abklingzeit)
	Ephemeral bool   `json:"ephemeral,omitempty"`
	Note      string `json:"note,omitempty"` // Testfeld: warum keine Antwort
}

// find sucht den Befehl zu text (mit Zeichen davor) und liefert die Argumente.
func (cfg CommandsConfig) find(text string) (*Command, string) {
	word, args, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(text), cfg.Prefix), " ")
	word = strings.ToLower(strings.TrimSpace(word))
	for i := range cfg.Commands {
		cc := &cfg.Commands[i]
		if cc.Trigger == word {
			return cc, strings.TrimSpace(args)
		}
		for _, a := range cc.Aliases {
			if a == word {
				return cc, strings.TrimSpace(args)
			}
		}
	}
	return nil, ""
}

// PollArgs: Ist text ein Umfrage-Befehl, den diese Person hier nutzen darf? Liefert die Argumente.
func (c *Commands) PollArgs(text, room string, team bool) (string, string, bool) {
	c.mu.Lock()
	cfg := c.cfg
	c.mu.Unlock()
	if !cfg.Enabled || !strings.HasPrefix(strings.TrimSpace(text), cfg.Prefix) {
		return "", "", false
	}
	cmd, args := cfg.find(text)
	if cmd == nil || cmd.Kind != CmdPoll || !cmd.Enabled || !cmd.inRoom(room) || (cmd.TeamOnly && !team) {
		return "", "", false
	}
	return args, cfg.Prefix + cmd.Trigger, true
}

// IsEphemeral: Ist text ein privater Befehl, den diese Person hier nutzen darf?
// (ohne Nebenwirkungen – entscheidet, ob die Nachricht überhaupt in den Kanal geht)
func (c *Commands) IsEphemeral(text, room string, team bool) bool {
	c.mu.Lock()
	cfg := c.cfg
	c.mu.Unlock()
	if !cfg.Enabled || !strings.HasPrefix(strings.TrimSpace(text), cfg.Prefix) {
		return false
	}
	cmd, _ := cfg.find(text)
	return cmd != nil && cmd.Ephemeral && cmd.Enabled && cmd.inRoom(room) && (!cmd.TeamOnly || team)
}

// Run prüft, ob text ein Befehl ist, und liefert ggf. die Antwort.
func (c *Commands) Run(text string, ctx CmdContext) CmdResult {
	c.mu.Lock()
	cfg := c.cfg
	c.mu.Unlock()
	if !cfg.Enabled {
		return CmdResult{Note: "Der Befehls-Bot ist ausgeschaltet."}
	}
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, cfg.Prefix) {
		return CmdResult{Note: "Kein Befehl – Nachrichten müssen mit „" + cfg.Prefix + "“ beginnen."}
	}
	cmd, args := cfg.find(text)
	if cmd == nil {
		return CmdResult{Note: "Unbekannter Befehl."}
	}
	res := CmdResult{Command: cmd.Trigger}
	switch {
	case !cmd.Enabled:
		res.Note = "Dieser Befehl ist ausgeschaltet."
		return res
	case !cmd.inRoom(ctx.Room):
		res.Note = fmt.Sprintf("Dieser Befehl gilt in #%s nicht.", ctx.Room)
		return res
	case cmd.TeamOnly && !ctx.Team:
		res.Note = "Nur für das Team."
		return res
	}
	res.Ephemeral = cmd.Ephemeral
	if !ctx.DryRun && cmd.Cooldown > 0 {
		key := cmd.ID + "\x00" + ctx.Room
		if cmd.Ephemeral { // private Antworten bremsen nur die Person selbst
			key += "\x00" + ctx.UserID
		}
		c.mu.Lock()
		last, ok := c.last[key]
		if wait := time.Duration(cmd.Cooldown)*time.Second - ctx.Now.Sub(last); ok && wait > 0 {
			c.mu.Unlock()
			res.Note = "Abklingzeit läuft noch."
			if cmd.Ephemeral {
				res.Note = fmt.Sprintf("Bitte noch %d s warten, dann geht %s%s wieder.", int(wait.Seconds()+0.999), cfg.Prefix, cmd.Trigger)
			}
			return res
		}
		c.last[key] = ctx.Now
		c.mu.Unlock()
	}

	result, usage := c.result(cmd, cfg, args, ctx)
	res.Matched, res.Reply = true, cmd.Reply
	if usage != "" { // falsche Eingabe: kurzer Hinweis statt Vorlage
		res.Text, res.Reply = usage, true
		return res
	}
	online := ""
	if ctx.Online != nil && strings.Contains(cmd.Response, "{online}") {
		online = strconv.Itoa(ctx.Online().Online)
	}
	res.Text = strings.NewReplacer(
		"{user}", ctx.User, "{room}", ctx.Room, "{args}", args, "{result}", result,
		"{time}", ctx.Now.Format("15:04"), "{date}", ctx.Now.Format("02.01.2006"), "{online}", online,
	).Replace(cmd.Response)
	if len([]rune(res.Text)) > maxTextLen {
		res.Text = string([]rune(res.Text)[:maxTextLen-1]) + "…"
	}
	return res
}

func (c *Commands) result(cmd *Command, cfg CommandsConfig, args string, ctx CmdContext) (result, usage string) {
	switch cmd.Kind {
	case CmdHelp:
		var lines []string
		for _, cc := range cfg.Commands {
			if !cc.Enabled || !cc.inRoom(ctx.Room) || (cc.TeamOnly && !ctx.Team) {
				continue
			}
			line := "**" + cfg.Prefix + cc.Trigger + "**"
			if cc.Description != "" {
				line += " – " + cc.Description
			}
			lines = append(lines, line)
		}
		return strings.Join(lines, "\n"), ""
	case CmdDice:
		sides := 6
		if args != "" {
			n, err := strconv.Atoi(strings.TrimPrefix(strings.Fields(args)[0], "d"))
			if err != nil || n < 2 || n > 1000 {
				return "", ctx.tr(fmt.Sprintf("Bitte eine Zahl zwischen 2 und 1000 angeben, z. B. %s 20", cfg.Prefix+cmd.Trigger)) //i18n
			}
			sides = n
		}
		return strconv.Itoa(c.rnd(sides) + 1), ""
	case CmdCoin:
		if c.rnd(2) == 0 {
			return ctx.tr("Kopf"), "" //i18n
		}
		return ctx.tr("Zahl"), "" //i18n
	case CmdTime:
		return ctx.Now.Format("15:04"), ""
	case CmdOnline:
		if ctx.Online == nil {
			return "", ""
		}
		ev := ctx.Online()
		names := make([]string, 0, len(ev.Users))
		for _, u := range ev.Users {
			names = append(names, u.Name)
		}
		if len(names) > 15 {
			names = append(names[:15], ctx.tr(fmt.Sprintf("%d weitere", len(ev.Users)-15))) //i18n
		}
		if ev.Guests == 1 {
			names = append(names, ctx.tr("1 Gast")) //i18n
		} else if ev.Guests > 1 {
			names = append(names, ctx.tr(fmt.Sprintf("%d Gäste", ev.Guests))) //i18n
		}
		return fmt.Sprintf("%d online: %s", ev.Online, joinList(ctx, names)), ""
	case CmdChoose:
		var opts []string
		sep := "|"
		if !strings.Contains(args, "|") {
			sep = ","
			if !strings.Contains(args, ",") {
				sep = " oder "
			}
		}
		for _, o := range strings.Split(args, sep) {
			if o = strings.TrimSpace(o); o != "" {
				opts = append(opts, o)
			}
		}
		if len(opts) < 2 {
			return "", ctx.tr(fmt.Sprintf("Gib mindestens zwei Möglichkeiten an, z. B. %s Pizza | Pasta", cfg.Prefix+cmd.Trigger)) //i18n
		}
		return opts[c.rnd(len(opts))], ""
	case CmdPoll:
		if _, err := ParsePoll(args); err != nil {
			return "", ctx.tr(err.Error()) + " " + ctx.tr(fmt.Sprintf("So geht's: %s Frage? | Antwort 1 | Antwort 2", cfg.Prefix+cmd.Trigger)) //i18n
		}
		return "", ctx.tr("(Erstellt eine Umfrage von dir – keine Antwort des Bots.)") //i18n
	}
	return "", ""
}

// joinList: „a, b und c“ in der Sprache der Antwort.
func joinList(ctx CmdContext, list []string) string {
	switch len(list) {
	case 0:
		return ctx.tr("niemand") //i18n
	case 1:
		return list[0]
	}
	return ctx.tr(fmt.Sprintf("%s und %s", strings.Join(list[:len(list)-1], ", "), list[len(list)-1])) //i18n
}
