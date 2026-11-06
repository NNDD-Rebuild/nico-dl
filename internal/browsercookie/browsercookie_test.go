package browsercookie

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestFirefox(t *testing.T) {
	root := t.TempDir()
	prof := filepath.Join(root, "abc.default-release")
	if err := os.MkdirAll(prof, 0o755); err != nil {
		t.Fatal(err)
	}
	ini := "[Install4F96D1932A9F858E]\nDefault=abc.default-release\nLocked=1\n\n[Profile0]\nName=default-release\nIsRelative=1\nPath=abc.default-release\nDefault=1\n"
	if err := os.WriteFile(filepath.Join(root, "profiles.ini"), []byte(ini), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(prof, "cookies.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE moz_cookies (name TEXT, value TEXT, host TEXT, path TEXT, isSecure INTEGER, isHttpOnly INTEGER, expiry INTEGER)`,
		`INSERT INTO moz_cookies VALUES ('user_session','user_session_1_abc','.nicovideo.jp','/',1,1,1900000000)`,
		`INSERT INTO moz_cookies VALUES ('x','y','.example.com','/',0,0,0)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	cs, err := Load(Options{Browser: "firefox", UserDataDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Value != "user_session_1_abc" || !cs[0].Secure || !cs[0].HTTPOnly || cs[0].Expires != 1900000000 {
		t.Fatalf("unexpected: %+v", cs)
	}
	if _, err := Load(Options{Browser: "firefox", UserDataDir: root, Profile: "nope"}); err == nil {
		t.Fatal("存在しないプロファイルでエラーになるべき")
	}
}
