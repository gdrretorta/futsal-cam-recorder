package config

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/mail"
	"strings"
	"time"
)

// Role decides what a user can do in the web page.
type Role string

const (
	// RoleAdmin can do everything: recorder, users, settings, logs, updates.
	RoleAdmin Role = "admin"
	// RoleUserManager can use the recorder and manage users other than admins.
	RoleUserManager Role = "userManager"
	// RoleUser can only use the recorder.
	RoleUser Role = "user"
)

// ParseRole accepts the JSON names case-insensitively.
func ParseRole(s string) (Role, error) {
	for _, r := range []Role{RoleAdmin, RoleUserManager, RoleUser} {
		if strings.EqualFold(s, string(r)) {
			return r, nil
		}
	}
	return "", ErrInvalidRole
}

// CanManageUsers reports whether the role may open the users page.
func (r Role) CanManageUsers() bool { return r == RoleAdmin || r == RoleUserManager }

// User is someone who can log in to the web page. ID never changes, so a
// session survives a change of email.
type User struct {
	ID           string `json:"id"`
	Email        string `json:"email"`
	Role         Role   `json:"role"`
	PasswordHash string `json:"passwordHash"`
	// MustChangePassword is set when someone else chose the password (new
	// user, password reset): until the user picks their own, they can only
	// change it.
	MustChangePassword bool `json:"mustChangePassword,omitempty"`
	// InviteHash is the sha256 (hex) of the token in the invite link sent to
	// a new user, who has no password until they open it and choose one.
	// Only the hash is kept, so config.json never holds a usable link.
	InviteHash string `json:"inviteHash,omitempty"`
	// InviteExpires is when the invite link stops working (Unix seconds).
	InviteExpires int64 `json:"inviteExpires,omitempty"`
}

// InvitePending reports whether the user was invited and hasn't chosen a
// password yet.
func (u User) InvitePending() bool { return u.InviteHash != "" && u.PasswordHash == "" }

// InviteDuration is how long an invite link works.
const InviteDuration = 7 * 24 * time.Hour

// MigratedAdminEmail is the email given to the admin created from the single
// admin password of earlier versions (see migrateUsers).
const MigratedAdminEmail = "joao@retorta.com"

var (
	ErrInvalidEmail  = errors.New("email inválido")
	ErrInvalidRole   = errors.New("perfil inválido")
	ErrEmailTaken    = errors.New("já existe um utilizador com este email")
	ErrUserNotFound  = errors.New("utilizador não encontrado")
	ErrLastAdmin     = errors.New("tem de existir pelo menos um administrador")
	ErrBadLogin      = errors.New("email ou palavra-passe errados")
	ErrBadInvite     = errors.New("este convite não é válido ou já foi usado; peça um novo a quem o convidou")
	ErrNotInvited    = errors.New("este utilizador já escolheu a palavra-passe")
	ErrInviteExpired = errors.New("este convite expirou; peça um novo a quem o convidou")
)

// NormalizeEmail trims and lowercases an email and checks its format.
func NormalizeEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email || addr.Name != "" {
		return "", ErrInvalidEmail
	}
	return email, nil
}

func validateUsers(users []User) error {
	ids := map[string]bool{}
	emails := map[string]bool{}
	for i := range users {
		u := &users[i]
		email, err := NormalizeEmail(u.Email)
		if err != nil {
			return err
		}
		u.Email = email
		if _, err := ParseRole(string(u.Role)); err != nil {
			return err
		}
		if u.ID == "" || ids[u.ID] {
			return errors.New("utilizador com id em falta ou repetido")
		}
		if emails[u.Email] {
			return ErrEmailTaken
		}
		ids[u.ID], emails[u.Email] = true, true
	}
	return nil
}

func newUserID() string { return randomHex(8) }

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func inviteHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// newInvite sets a fresh invite on u and returns the token for the link.
func newInvite(u *User, now time.Time) string {
	token := randomHex(32)
	u.InviteHash = inviteHash(token)
	u.InviteExpires = now.Add(InviteDuration).Unix()
	return token
}

// migrateUsers turns the single admin password of earlier versions into an
// admin user with the same password, so the owner keeps access after the
// automatic update. It reports whether cfg changed.
func migrateUsers(cfg *Config) bool {
	if cfg.AdminPasswordHash == "" {
		return false
	}
	if len(cfg.Users) == 0 {
		cfg.Users = []User{{ID: newUserID(), Email: MigratedAdminEmail, Role: RoleAdmin, PasswordHash: cfg.AdminPasswordHash}}
	}
	cfg.AdminPasswordHash = ""
	return true
}

func findUser(users []User, match func(User) bool) int {
	for i, u := range users {
		if match(u) {
			return i
		}
	}
	return -1
}

func adminCount(users []User) int {
	n := 0
	for _, u := range users {
		if u.Role == RoleAdmin {
			n++
		}
	}
	return n
}

// UserByID returns the user with that id.
func (s *Store) UserByID(id string) (User, bool) {
	users := s.Get().Users
	if i := findUser(users, func(u User) bool { return u.ID == id }); i >= 0 {
		return users[i], true
	}
	return User{}, false
}

// HasUsers reports whether login is required.
func (s *Store) HasUsers() bool { return len(s.Get().Users) > 0 }

// HasAdmin reports whether some user is an admin.
func (s *Store) HasAdmin() bool { return adminCount(s.Get().Users) > 0 }

// dummyHash makes a login with an unknown email take as long as one with a
// wrong password, so the timing doesn't reveal which emails exist.
var dummyHash, _ = HashPassword("not a real password")

// Authenticate checks an email and password.
func (s *Store) Authenticate(email, password string) (User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	users := s.Get().Users
	i := findUser(users, func(u User) bool { return u.Email == email })
	if i < 0 {
		CheckPassword(dummyHash, password)
		return User{}, ErrBadLogin
	}
	if !CheckPassword(users[i].PasswordHash, password) {
		return User{}, ErrBadLogin
	}
	return users[i], nil
}

// AddUser creates a user. mustChange makes them pick a new password at the
// first login (see User.MustChangePassword).
func (s *Store) AddUser(email string, role Role, password string, mustChange bool) (User, error) {
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}
	return s.addUser(User{Email: email, Role: role, PasswordHash: hash, MustChangePassword: mustChange})
}

// InviteUser creates a user without a password, and returns the token of the
// invite link with which they choose it (see AcceptInvite).
func (s *Store) InviteUser(email string, role Role) (User, string, error) {
	u := User{Email: email, Role: role}
	token := newInvite(&u, time.Now())
	u, err := s.addUser(u)
	return u, token, err
}

func (s *Store) addUser(u User) (User, error) {
	email, err := NormalizeEmail(u.Email)
	if err != nil {
		return User{}, err
	}
	role, err := ParseRole(string(u.Role))
	if err != nil {
		return User{}, err
	}
	u.ID, u.Email, u.Role = newUserID(), email, role
	return u, s.Edit(func(c *Config) error {
		if findUser(c.Users, func(x User) bool { return x.Email == email }) >= 0 {
			return ErrEmailTaken
		}
		if role != RoleAdmin && adminCount(c.Users) == 0 {
			return errors.New("o primeiro utilizador tem de ser administrador")
		}
		c.Users = append(c.Users, u)
		return nil
	})
}

// UserChanges are the fields to change in UpdateUser; nil keeps the value.
type UserChanges struct {
	Email    *string
	Role     *Role
	Password *string
	// MustChangePassword is only applied together with Password.
	MustChangePassword bool
}

// UpdateUser changes a user. check, when not nil, sees the user before and
// after the change and can refuse it (permission rules of the caller).
func (s *Store) UpdateUser(id string, ch UserChanges, check func(before, after User) error) (User, error) {
	var after User
	if ch.Email != nil {
		email, err := NormalizeEmail(*ch.Email)
		if err != nil {
			return User{}, err
		}
		ch.Email = &email
	}
	if ch.Role != nil {
		role, err := ParseRole(string(*ch.Role))
		if err != nil {
			return User{}, err
		}
		ch.Role = &role
	}
	hash := ""
	if ch.Password != nil {
		var err error
		if hash, err = HashPassword(*ch.Password); err != nil {
			return User{}, err
		}
	}
	err := s.Edit(func(c *Config) error {
		i := findUser(c.Users, func(u User) bool { return u.ID == id })
		if i < 0 {
			return ErrUserNotFound
		}
		before := c.Users[i]
		after = before
		if ch.Email != nil {
			if j := findUser(c.Users, func(u User) bool { return u.Email == *ch.Email }); j >= 0 && j != i {
				return ErrEmailTaken
			}
			after.Email = *ch.Email
		}
		if ch.Role != nil {
			after.Role = *ch.Role
		}
		if hash != "" {
			after.PasswordHash = hash
			after.MustChangePassword = ch.MustChangePassword
			after.InviteHash, after.InviteExpires = "", 0
		}
		if check != nil {
			if err := check(before, after); err != nil {
				return err
			}
		}
		admins := adminCount(c.Users)
		c.Users[i] = after
		if admins > 0 && adminCount(c.Users) == 0 {
			return ErrLastAdmin
		}
		return nil
	})
	return after, err
}

// Reinvite sends a new invite to a user who hasn't chosen a password yet:
// the old link stops working. check works as in UpdateUser.
func (s *Store) Reinvite(id string, check func(User) error) (User, string, error) {
	var u User
	var token string
	err := s.Edit(func(c *Config) error {
		i := findUser(c.Users, func(u User) bool { return u.ID == id })
		if i < 0 {
			return ErrUserNotFound
		}
		if check != nil {
			if err := check(c.Users[i]); err != nil {
				return err
			}
		}
		if c.Users[i].PasswordHash != "" {
			return ErrNotInvited
		}
		token = newInvite(&c.Users[i], time.Now())
		u = c.Users[i]
		return nil
	})
	return u, token, err
}

// InvitedUser returns the user an invite token belongs to, if it still works.
func (s *Store) InvitedUser(token string) (User, error) {
	return invited(s.Get().Users, token, time.Now())
}

func invited(users []User, token string, now time.Time) (User, error) {
	if token == "" {
		return User{}, ErrBadInvite
	}
	h := inviteHash(token)
	i := findUser(users, func(u User) bool { return u.InvitePending() && u.InviteHash == h })
	switch {
	case i < 0:
		return User{}, ErrBadInvite
	case now.Unix() > users[i].InviteExpires:
		return User{}, ErrInviteExpired
	}
	return users[i], nil
}

// AcceptInvite sets the password of an invited user; the link then stops
// working.
func (s *Store) AcceptInvite(token, password string) (User, error) {
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}
	var u User
	err = s.Edit(func(c *Config) error {
		found, err := invited(c.Users, token, time.Now())
		if err != nil {
			return err
		}
		i := findUser(c.Users, func(u User) bool { return u.ID == found.ID })
		c.Users[i].PasswordHash, c.Users[i].MustChangePassword = hash, false
		c.Users[i].InviteHash, c.Users[i].InviteExpires = "", 0
		u = c.Users[i]
		return nil
	})
	return u, err
}

// DeleteUser removes a user. check works as in UpdateUser.
func (s *Store) DeleteUser(id string, check func(User) error) error {
	return s.Edit(func(c *Config) error {
		i := findUser(c.Users, func(u User) bool { return u.ID == id })
		if i < 0 {
			return ErrUserNotFound
		}
		if check != nil {
			if err := check(c.Users[i]); err != nil {
				return err
			}
		}
		admins := adminCount(c.Users)
		c.Users = append(c.Users[:i], c.Users[i+1:]...)
		if admins > 0 && adminCount(c.Users) == 0 {
			return ErrLastAdmin
		}
		return nil
	})
}

// SetUser creates the user, or sets the role and password of an existing one
// (used by the add-user command on the PC).
func (s *Store) SetUser(email string, role Role, password string) (User, error) {
	norm, err := NormalizeEmail(email)
	if err != nil {
		return User{}, err
	}
	users := s.Get().Users
	if i := findUser(users, func(u User) bool { return u.Email == norm }); i >= 0 {
		return s.UpdateUser(users[i].ID, UserChanges{Role: &role, Password: &password}, nil)
	}
	return s.AddUser(norm, role, password, false)
}

// SetAdminPassword sets the password of the first admin (used by the
// set-admin-password command on the PC, kept for compatibility).
func (s *Store) SetAdminPassword(password string) error {
	users := s.Get().Users
	i := findUser(users, func(u User) bool { return u.Role == RoleAdmin })
	if i < 0 {
		return errors.New("não existe nenhum administrador; crie um com: futsal-cam-recorder.exe add-user <email> admin")
	}
	_, err := s.UpdateUser(users[i].ID, UserChanges{Password: &password}, nil)
	return err
}
