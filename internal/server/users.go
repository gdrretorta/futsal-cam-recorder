package server

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/gdrretorta/futsal-cam-recorder/internal/config"
	"github.com/gdrretorta/futsal-cam-recorder/internal/notify"
)

// errPermission is a refusal by the user-management rules (403).
type errPermission struct{ msg string }

func (e errPermission) Error() string { return e.msg }

var (
	errAdminsOnly = errPermission{"só um administrador pode criar ou alterar administradores e gestores de utilizadores"}
	errDeleteSelf = errPermission{"não pode apagar a sua própria conta"}
)

// canManage applies the rules of the users page: admins manage everyone, user
// managers only plain users (roles holds the role before and after a change),
// so nobody can give anyone, themselves included, more power than a user.
func canManage(actor config.User, roles ...config.Role) error {
	if actor.Role == config.RoleAdmin {
		return nil
	}
	for _, r := range roles {
		if r != config.RoleUser {
			return errAdminsOnly
		}
	}
	return nil
}

func writeUserError(w http.ResponseWriter, err error) {
	var perm errPermission
	code := http.StatusBadRequest
	switch {
	case errors.As(err, &perm):
		code = http.StatusForbidden
	case errors.Is(err, config.ErrUserNotFound):
		code = http.StatusNotFound
	case errors.Is(err, config.ErrEmailTaken), errors.Is(err, config.ErrLastAdmin), errors.Is(err, config.ErrNotInvited):
		code = http.StatusConflict
	case errors.Is(err, config.ErrInvalidEmail), errors.Is(err, config.ErrInvalidRole), errors.Is(err, config.ErrWeakPassword):
	default:
		code = http.StatusInternalServerError
	}
	writeError(w, code, err)
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	users := s.Store.Get().Users
	slices.SortFunc(users, func(a, b config.User) int { return strings.Compare(a.Email, b.Email) })
	list := make([]userDTO, 0, len(users))
	for _, u := range users {
		list = append(list, toUserDTO(u))
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	actor, _ := userFrom(r)
	var body struct {
		Email    string      `json:"email"`
		Role     config.Role `json:"role"`
		Password string      `json:"password"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	role, err := config.ParseRole(string(body.Role))
	if err != nil {
		writeUserError(w, err)
		return
	}
	if err := canManage(actor, role); err != nil {
		writeUserError(w, err)
		return
	}
	if body.Password == "" {
		// No password: the new user gets an email with a link to choose it.
		u, token, err := s.Store.InviteUser(body.Email, role)
		if err != nil {
			writeUserError(w, err)
			return
		}
		s.Log.Info("user invited", "email", u.Email, "role", u.Role, "by", actor.Email)
		writeJSON(w, http.StatusCreated, s.sendInvite(r, u, token, actor))
		return
	}
	// The creator chose the password: the new user must pick their own.
	u, err := s.Store.AddUser(body.Email, role, body.Password, true)
	if err != nil {
		writeUserError(w, err)
		return
	}
	s.Log.Info("user created", "email", u.Email, "role", u.Role, "by", actor.Email)
	writeJSON(w, http.StatusCreated, inviteDTO{userDTO: toUserDTO(u)})
}

// inviteDTO is a user just created or invited again. InviteURL is the link
// the email carries, also given to whoever invited, so they can send it
// another way when the email couldn't be sent (InviteError says why).
type inviteDTO struct {
	userDTO
	InviteSent  bool   `json:"inviteSent"`
	InviteURL   string `json:"inviteUrl,omitempty"`
	InviteError string `json:"inviteError,omitempty"`
}

// inviteHashPrefix starts the link's fragment: the token stays in the
// browser, out of the access logs of Cloudflare and of this app.
const inviteHashPrefix = "#convite="

// sendInvite emails the invite link to u.
func (s *Server) sendInvite(r *http.Request, u config.User, token string, actor config.User) inviteDTO {
	cfg := s.Store.Get()
	base := cfg.Notify.SiteURL
	if base == "" {
		scheme := "http"
		if isHTTPS(r) {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	link := base + "/" + inviteHashPrefix + token
	by := actor.Email
	if actor.ID == localUser.ID {
		by = "O administrador"
	}
	d := inviteDTO{userDTO: toUserDTO(u), InviteURL: link}
	if err := notify.Send(r.Context(), cfg.Notify, notify.InviteEmail(cfg, u.Email, link, by)); err != nil {
		s.Log.Warn("invite email not sent", "email", u.Email, "err", err)
		d.InviteError = err.Error()
		return d
	}
	s.Log.Info("invite email sent", "email", u.Email)
	d.InviteSent = true
	return d
}

// reinvite sends a new invite link (the old one stops working) to a user who
// hasn't chosen a password yet.
func (s *Server) reinvite(w http.ResponseWriter, r *http.Request) {
	actor, _ := userFrom(r)
	u, token, err := s.Store.Reinvite(r.PathValue("id"), func(u config.User) error { return canManage(actor, u.Role) })
	if err != nil {
		writeUserError(w, err)
		return
	}
	s.Log.Info("user invited again", "email", u.Email, "by", actor.Email)
	writeJSON(w, http.StatusOK, s.sendInvite(r, u, token, actor))
}

// checkInvite tells the invite page whose account the link is for. Like the
// login, a wrong token counts as a failed attempt.
func (s *Server) checkInvite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if !s.inviteAllowed(w, r) || !readJSON(w, r, &body) {
		return
	}
	u, err := s.Store.InvitedUser(body.Token)
	if err != nil {
		s.inviteFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"email": u.Email})
}

// acceptInvite sets the invited user's password and logs them in.
func (s *Server) acceptInvite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if !s.inviteAllowed(w, r) || !readJSON(w, r, &body) {
		return
	}
	u, err := s.Store.AcceptInvite(body.Token, body.Password)
	if errors.Is(err, config.ErrBadInvite) || errors.Is(err, config.ErrInviteExpired) {
		s.inviteFailed(w, r, err)
		return
	}
	if err != nil {
		writeUserError(w, err)
		return
	}
	s.auth.succeed(clientIP(r))
	s.auth.setSession(w, r, u)
	s.Log.Info("invite accepted", "email", u.Email, "ip", clientIP(r))
	d := toUserDTO(u)
	writeJSON(w, http.StatusOK, meDTO{AuthRequired: true, User: &d})
}

func (s *Server) inviteAllowed(w http.ResponseWriter, r *http.Request) bool {
	if s.auth.locked(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, errors.New("demasiadas tentativas falhadas; tente daqui a 15 minutos"))
		return false
	}
	return true
}

func (s *Server) inviteFailed(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, config.ErrBadInvite) {
		s.auth.fail(clientIP(r))
	}
	writeError(w, http.StatusGone, err)
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	actor, _ := userFrom(r)
	var body struct {
		Email    *string      `json:"email"`
		Role     *config.Role `json:"role"`
		Password *string      `json:"password"` // absent or empty keeps it
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Password != nil && *body.Password == "" {
		body.Password = nil
	}
	// A password reset by someone else must be changed at the next login.
	id := r.PathValue("id")
	ch := config.UserChanges{Email: body.Email, Role: body.Role, Password: body.Password, MustChangePassword: id != actor.ID}
	u, err := s.Store.UpdateUser(id, ch,
		func(before, after config.User) error { return canManage(actor, before.Role, after.Role) })
	if err != nil {
		writeUserError(w, err)
		return
	}
	if u.ID == actor.ID && body.Password != nil {
		s.auth.setSession(w, r, u) // keep the editor logged in
	}
	s.Log.Info("user updated", "email", u.Email, "role", u.Role, "password", body.Password != nil, "by", actor.Email)
	writeJSON(w, http.StatusOK, toUserDTO(u))
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	actor, _ := userFrom(r)
	var deleted config.User
	err := s.Store.DeleteUser(r.PathValue("id"), func(u config.User) error {
		if u.ID == actor.ID {
			return errDeleteSelf
		}
		deleted = u
		return canManage(actor, u.Role)
	})
	if err != nil {
		writeUserError(w, err)
		return
	}
	s.Log.Info("user deleted", "email", deleted.Email, "by", actor.Email)
	w.WriteHeader(http.StatusNoContent)
}
