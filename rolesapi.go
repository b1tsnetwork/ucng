// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"net/http"
	"strings"
)

// GET /api/roles – Rollen und wer sie trägt (öffentlich: die Abzeichen sieht ohnehin jeder).
func (s *Server) handleRolesPublic(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.publicRoles())
}

func (s *Server) publicRoles() PublicRoles {
	out := PublicRoles{Roles: s.hub.Roles().List(), Users: map[string][]string{}}
	for id, roles := range s.users.RoleMap() {
		if f := s.hub.Roles().Filter(roles); len(f) > 0 {
			out.Users[id] = f
		}
	}
	return out
}

// GET /api/admin/roles
func (s *Server) handleAdminRoles(w http.ResponseWriter, r *http.Request) {
	pr := s.publicRoles()
	counts := map[string]int{}
	for _, ids := range pr.Users {
		for _, id := range ids {
			counts[id]++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"roles": pr.Roles, "counts": counts})
}

// PUT /api/admin/roles {"roles":[{"id","name","color","colorName"}]} – alle Rollen in Reihenfolge.
func (s *Server) handleAdminRolesSet(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Roles []Role `json:"roles"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := s.hub.Roles().Set(in.Roles)
	if err != nil {
		fail(w, err)
		return
	}
	names := make([]string, len(out))
	for i, x := range out {
		names[i] = x.Name
	}
	s.log(r, AuditEntry{Action: "roles.update", Detail: strings.Join(names, ", ")})
	writeJSON(w, http.StatusOK, out)
}

// POST /api/admin/users/{id}/roles {"roles":["r_…"]}
func (s *Server) handleAdminUserRoles(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Roles []string `json:"roles"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	for _, id := range in.Roles {
		if !s.hub.Roles().Has(id) {
			fail(w, ErrNoRole)
			return
		}
	}
	id := r.PathValue("id")
	u, err := s.users.SetRoles(id, s.hub.Roles().Filter(in.Roles))
	if err != nil {
		fail(w, err)
		return
	}
	names := []string{}
	for _, role := range s.hub.Roles().List() {
		for _, x := range u.Roles {
			if x == role.ID {
				names = append(names, role.Name)
			}
		}
	}
	detail := strings.Join(names, ", ")
	if detail == "" {
		detail = "keine"
	}
	s.log(r, AuditEntry{Action: "user.roles", UserID: id, UserName: u.Name, Detail: detail})
	s.hub.RefreshViewer(id, u.Team, u.Roles)
	s.hub.Notify("roles")
	writeJSON(w, http.StatusOK, u)
}
