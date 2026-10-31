package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An install upgraded from 2.6 has only adminPasswordHash: it must become an
// admin user with the same password, saved to disk.
func TestMigrateAdminPassword(t *testing.T) {
	dir := t.TempDir()
	hash, _ := HashPassword("old admin pw")
	legacy := `{"title":"Futsal","port":8080,"camera":{"host":"cam","port":554},"recordingsDir":"recordings","autoUpdate":true,"adminPasswordHash":"` + hash + `"}`
	os.WriteFile(filepath.Join(dir, FileName), []byte(legacy), 0o600)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.Authenticate(" JOAO@retorta.com ", "old admin pw")
	if err != nil || u.Role != RoleAdmin || u.Email != MigratedAdminEmail {
		t.Fatalf("migrated admin = %+v, %v", u, err)
	}
	var onDisk Config
	data, _ := os.ReadFile(filepath.Join(dir, FileName))
	json.Unmarshal(data, &onDisk)
	if onDisk.AdminPasswordHash != "" || len(onDisk.Users) != 1 || onDisk.Users[0].ID != u.ID || onDisk.Title != "Futsal" {
		t.Fatalf("not saved: %s", data)
	}
	// Opening again keeps the same user (same id: sessions stay valid).
	s2, _ := Open(dir)
	if got, _ := s2.UserByID(u.ID); got.Email != MigratedAdminEmail {
		t.Fatal("user id changed on reopen")
	}
}

func TestUsersCRUD(t *testing.T) {
	s, _ := Open(t.TempDir())
	if s.HasUsers() {
		t.Fatal("new install has users")
	}
	if _, err := s.AddUser("u@x.pt", RoleUser, "password1", false); err == nil {
		t.Fatal("first user must be an admin")
	}
	admin, err := s.AddUser("Admin@X.pt", RoleAdmin, "password1", false)
	if err != nil || admin.Email != "admin@x.pt" {
		t.Fatalf("add admin = %+v %v", admin, err)
	}
	for _, c := range []struct {
		email, pw string
		role      Role
		want      error
	}{
		{"admin@x.pt", "password1", RoleUser, ErrEmailTaken},
		{"not an email", "password1", RoleUser, ErrInvalidEmail},
		{"Name <n@x.pt>", "password1", RoleUser, ErrInvalidEmail},
		{"n@x.pt", "short", RoleUser, ErrWeakPassword},
		{"n@x.pt", "password1", "boss", ErrInvalidRole},
	} {
		if _, err := s.AddUser(c.email, c.role, c.pw, false); !errors.Is(err, c.want) {
			t.Errorf("AddUser(%q, %q) = %v, want %v", c.email, c.role, err, c.want)
		}
	}
	user, err := s.AddUser("u@x.pt", RoleUser, "password1", false)
	if err != nil {
		t.Fatal(err)
	}

	// The last admin can't be demoted or deleted.
	r := RoleUser
	if _, err := s.UpdateUser(admin.ID, UserChanges{Role: &r}, nil); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("demote last admin = %v", err)
	}
	if err := s.DeleteUser(admin.ID, nil); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("delete last admin = %v", err)
	}
	// Email clash on update.
	taken := "admin@x.pt"
	if _, err := s.UpdateUser(user.ID, UserChanges{Email: &taken}, nil); !errors.Is(err, ErrEmailTaken) {
		t.Errorf("email clash = %v", err)
	}
	// The check callback can refuse.
	boom := errors.New("no")
	if _, err := s.UpdateUser(user.ID, UserChanges{}, func(_, _ User) error { return boom }); err != boom {
		t.Errorf("check ignored: %v", err)
	}
	// A real change.
	pw, email := "new password", "U2@x.pt"
	if _, err := s.UpdateUser(user.ID, UserChanges{Email: &email, Password: &pw}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate("u2@x.pt", "new password"); err != nil {
		t.Errorf("login after change: %v", err)
	}
	if _, err := s.Authenticate("u2@x.pt", "password1"); !errors.Is(err, ErrBadLogin) {
		t.Errorf("old password still works: %v", err)
	}
	if _, err := s.Authenticate("nobody@x.pt", "password1"); !errors.Is(err, ErrBadLogin) {
		t.Errorf("unknown email: %v", err)
	}
	if err := s.DeleteUser(user.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.UserByID(user.ID); ok {
		t.Fatal("user not deleted")
	}
	if err := s.DeleteUser(user.ID, nil); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("delete twice = %v", err)
	}
}

func TestSetUserAndAdminPassword(t *testing.T) {
	s, _ := Open(t.TempDir())
	if err := s.SetAdminPassword("password1"); err == nil || !strings.Contains(err.Error(), "add-user") {
		t.Fatalf("no admin yet: %v", err)
	}
	if _, err := s.SetUser("a@x.pt", RoleAdmin, "password1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetUser("A@x.pt", RoleAdmin, "password2"); err != nil {
		t.Fatal(err)
	}
	if len(s.Get().Users) != 1 {
		t.Fatal("SetUser duplicated the user")
	}
	if err := s.SetAdminPassword("password3"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate("a@x.pt", "password3"); err != nil {
		t.Fatal(err)
	}
}

// Get hands out copies: changing one must not change the store.
func TestGetCopiesUsers(t *testing.T) {
	s, _ := Open(t.TempDir())
	s.AddUser("a@x.pt", RoleAdmin, "password1", false)
	cfg := s.Get()
	cfg.Users[0].Role = RoleUser
	if s.Get().Users[0].Role != RoleAdmin {
		t.Fatal("Get shares the users slice")
	}
}
