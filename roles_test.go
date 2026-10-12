// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRolesAPI(t *testing.T) {
	ts, srv := testServerWith(t, nil)
	adm := adminLogin(t, ts.URL)
	do(t, "POST", ts.URL+"/api/admin/accounts", adm, `{"name":"mo","role":"moderator","password":"mo-passwort-12"}`).Body.Close()
	mod := loginAs(t, ts.URL, "mo", "mo-passwort-12")

	// Prüfung
	expect(t, do(t, "PUT", ts.URL+"/api/admin/roles", mod, `{"roles":[]}`), 403, "Moderator")
	expect(t, do(t, "PUT", ts.URL+"/api/admin/roles", adm, `{"roles":[{"name":"Team","color":"#ff0000"}]}`), 400, "Team")
	expect(t, do(t, "PUT", ts.URL+"/api/admin/roles", adm, `{"roles":[{"name":"VIP","color":"rot"}]}`), 400, "Farbe")
	expect(t, do(t, "PUT", ts.URL+"/api/admin/roles", adm, `{"roles":[{"name":"VIP","color":"#ff0000"},{"name":"vip","color":"#00ff00"}]}`), 400, "doppelt")
	expect(t, do(t, "PUT", ts.URL+"/api/admin/roles", adm, `{"roles":[{"name":"  ","color":"#ff0000"}]}`), 400, "leer")

	roles := decode[[]Role](t, expect(t, do(t, "PUT", ts.URL+"/api/admin/roles", adm,
		`{"roles":[{"name":"  Moderator ","color":"#2E7D32","colorName":true},{"name":"VIP","color":"#c2185b"}]}`), 200, "anlegen"))
	if len(roles) != 2 || roles[0].Name != "Moderator" || roles[0].Color != "#2e7d32" || !strings.HasPrefix(roles[0].ID, "r_") || !roles[0].ColorName {
		t.Fatalf("%+v", roles)
	}
	modID, vipID := roles[0].ID, roles[1].ID

	ada := claim(t, ts.URL, "Ada")
	adaID := sess(t, ts.URL, ada).ID
	expect(t, do(t, "POST", ts.URL+"/api/admin/users/"+adaID+"/roles", adm, `{"roles":["r_gibtsnicht"]}`), 400, "unbekannte Rolle")
	expect(t, do(t, "POST", ts.URL+"/api/admin/users/"+adaID+"/roles", mod, `{"roles":["`+vipID+`"]}`), 403, "Moderator vergibt")
	u := decode[UserInfo](t, expect(t, do(t, "POST", ts.URL+"/api/admin/users/"+adaID+"/roles", adm, `{"roles":["`+vipID+`","`+modID+`"]}`), 200, "vergeben"))
	if strings.Join(u.Roles, ",") != modID+","+vipID {
		t.Fatalf("Reihenfolge wie Rollenliste: %v", u.Roles)
	}
	pub := decode[PublicRoles](t, do(t, "GET", ts.URL+"/api/roles", "", ""))
	if len(pub.Roles) != 2 || strings.Join(pub.Users[adaID], ",") != modID+","+vipID {
		t.Fatalf("%+v", pub)
	}
	// Umsortieren behält IDs, Löschen entfernt die Rolle bei allen
	roles = decode[[]Role](t, expect(t, do(t, "PUT", ts.URL+"/api/admin/roles", adm,
		`{"roles":[{"id":"`+vipID+`","name":"VIP","color":"#c2185b"}]}`), 200, "Moderator-Rolle löschen"))
	if len(roles) != 1 || roles[0].ID != vipID {
		t.Fatalf("%+v", roles)
	}
	pub = decode[PublicRoles](t, do(t, "GET", ts.URL+"/api/roles", "", ""))
	if strings.Join(pub.Users[adaID], ",") != vipID {
		t.Fatalf("%+v", pub.Users)
	}
	// Admin-Liste zählt
	var al struct {
		Counts map[string]int `json:"counts"`
	}
	json.NewDecoder(expect(t, do(t, "GET", ts.URL+"/api/admin/roles", mod, ""), 200, "Liste").Body).Decode(&al)
	if al.Counts[vipID] != 1 {
		t.Fatalf("%+v", al)
	}
	_ = srv
}

func TestRolesPersist(t *testing.T) {
	dir := t.TempDir()
	h, u, st := openStore(t, dir)
	roles, err := h.Roles().Set([]Role{{Name: "VIP", Color: "#123456"}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := u.Claim("Ada", "")
	if err != nil {
		t.Fatal(err)
	}
	u.SetRoles(res.ID, []string{roles[0].ID})
	flush(t, h, u, st)
	st.Close()
	if b, _ := os.ReadFile(filepath.Join(dir, "roles.json")); !strings.Contains(string(b), "VIP") {
		t.Fatalf("roles.json: %s", b)
	}
	h2, u2, _ := openStore(t, dir)
	if l := h2.Roles().List(); len(l) != 1 || l[0].ID != roles[0].ID {
		t.Fatalf("%+v", l)
	}
	if m := u2.RoleMap(); len(m[res.ID]) != 1 {
		t.Fatalf("%+v", m)
	}
}
