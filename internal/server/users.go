package server

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/joaoopereira/simple-rtsp-recorder/internal/config"
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
	case errors.Is(err, config.ErrEmailTaken), errors.Is(err, config.ErrLastAdmin):
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
	// The creator chose the password: the new user must pick their own.
	u, err := s.Store.AddUser(body.Email, role, body.Password, true)
	if err != nil {
		writeUserError(w, err)
		return
	}
	s.Log.Info("user created", "email", u.Email, "role", u.Role, "by", actor.Email)
	writeJSON(w, http.StatusCreated, toUserDTO(u))
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
