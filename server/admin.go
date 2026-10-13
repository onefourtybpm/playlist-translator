package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/mail"
	"strconv"
	"time"

	"github.com/onefourtybpm/playlist-translator/server/internal/store"
)

// --- Admin: who can use the app ---

type adminMemberJSON struct {
	ID          int64  `json:"id"`
	DisplayName string `json:"displayName"`
	IsAdmin     bool   `json:"isAdmin"`
	// Spotify and Apple say which ways this member signs in.
	Spotify bool `json:"spotify"`
	Apple   bool `json:"apple"`
}

type allowedEmailJSON struct {
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
}

type accessRequestJSON struct {
	ID          int64     `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"displayName"`
	RequestedAt time.Time `json:"requestedAt"`
}

func (s *server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	members, err1 := s.store.ListMembers(ctx)
	allowed, err2 := s.store.ListAllowedEmails(ctx)
	requests, err3 := s.store.ListAccessRequests(ctx)
	if err := errors.Join(err1, err2, err3); err != nil {
		slog.Error("load admin view", "err", err)
		writeError(w, http.StatusInternalServerError, "Couldn't load members.")
		return
	}
	resp := struct {
		Members         []adminMemberJSON   `json:"members"`
		AllowedEmails   []allowedEmailJSON  `json:"allowedEmails"`
		Requests        []accessRequestJSON `json:"requests"`
		SignInWithApple bool                `json:"signInWithApple"`
	}{
		Members:         []adminMemberJSON{},
		AllowedEmails:   []allowedEmailJSON{},
		Requests:        []accessRequestJSON{},
		SignInWithApple: s.appleID != nil,
	}
	for _, m := range members {
		resp.Members = append(resp.Members, adminMemberJSON{
			ID: m.ID, DisplayName: m.DisplayName, IsAdmin: m.IsAdmin,
			Spotify: m.SpotifyUserID != "", Apple: m.AppleSub != "",
		})
	}
	for _, a := range allowed {
		resp.AllowedEmails = append(resp.AllowedEmails, allowedEmailJSON{Email: a.Email, DisplayName: a.DisplayName})
	}
	for _, q := range requests {
		resp.Requests = append(resp.Requests, accessRequestJSON{
			ID: q.ID, Email: q.Email, DisplayName: q.DisplayName, RequestedAt: q.RequestedAt,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) handleAllowEmail(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email       string `json:"email"`
		DisplayName string `json:"displayName"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Send an email and a name.")
		return
	}
	email := store.NormalizeEmail(req.Email)
	if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
		writeError(w, http.StatusBadRequest, "That doesn't look like an email address.")
		return
	}
	name := cleanName(req.DisplayName)
	if name == "" {
		writeError(w, http.StatusBadRequest, "Add a name, so you know who this is.")
		return
	}
	if err := s.store.AllowEmail(r.Context(), email, name); err != nil {
		slog.Error("allow email", "err", err)
		writeError(w, http.StatusInternalServerError, "Couldn't add that email.")
		return
	}
	slog.Info("email allowed", "by", memberFrom(r.Context()).ID)
	s.handleAdmin(w, r)
}

func (s *server) handleRemoveAllowedEmail(w http.ResponseWriter, r *http.Request) {
	if err := s.store.RemoveAllowedEmail(r.Context(), r.PathValue("email")); err != nil {
		slog.Error("remove allowed email", "err", err)
		writeError(w, http.StatusInternalServerError, "Couldn't remove that email.")
		return
	}
	s.handleAdmin(w, r)
}

func (s *server) handleApproveRequest(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "Not found.")
		return
	}
	var req struct {
		DisplayName string `json:"displayName"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Send a name for the new member.")
		return
	}
	name := cleanName(req.DisplayName)
	if name == "" {
		writeError(w, http.StatusBadRequest, "Add a name, so you know who this is.")
		return
	}
	member, err := s.store.ApproveAccessRequest(r.Context(), id, name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "That request is gone. Refresh the page.")
		return
	}
	if err != nil {
		slog.Error("approve access request", "err", err)
		writeError(w, http.StatusInternalServerError, "Couldn't approve that request.")
		return
	}
	slog.Info("access request approved", "member", member.ID, "by", memberFrom(r.Context()).ID)
	s.handleAdmin(w, r)
}

func (s *server) handleDenyRequest(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "Not found.")
		return
	}
	if err := s.store.DeleteAccessRequest(r.Context(), id); err != nil {
		slog.Error("deny access request", "err", err)
		writeError(w, http.StatusInternalServerError, "Couldn't remove that request.")
		return
	}
	s.handleAdmin(w, r)
}

func (s *server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "Not found.")
		return
	}
	member, err := s.store.GetMember(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.handleAdmin(w, r)
		return
	}
	if err != nil {
		slog.Error("load member", "err", err)
		writeError(w, http.StatusInternalServerError, "Couldn't remove that member.")
		return
	}
	if member.IsAdmin {
		writeError(w, http.StatusBadRequest, "The admin can't be removed.")
		return
	}
	if acct, err := s.store.GetAppleAccount(r.Context()); err == nil && acct.ConnectedBy == id {
		writeError(w, http.StatusBadRequest, member.DisplayName+
			" connected the Apple Music account. Disconnect Apple Music before removing them.")
		return
	}
	if err := s.store.DeleteMember(r.Context(), id); err != nil {
		slog.Error("remove member", "err", err)
		writeError(w, http.StatusInternalServerError, "Couldn't remove that member.")
		return
	}
	slog.Info("member removed", "member", id, "by", memberFrom(r.Context()).ID)
	s.handleAdmin(w, r)
}
