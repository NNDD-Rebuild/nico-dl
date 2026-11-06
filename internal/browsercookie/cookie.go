// Package browsercookie は Firefox のプロファイルから nicovideo.jp の cookie を読み出す。
package browsercookie

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // pure Go の SQLite ドライバ (cgo 不要)
)

// Cookie はブラウザから取り出した 1 件の cookie。
type Cookie struct {
	Name, Value, Domain, Path string
	Secure, HTTPOnly          bool
	Expires                   int64 // unix 秒。0 ならセッション cookie
}

// Options は読み出しの指定。
type Options struct {
	Browser     string // firefox のみ対応
	Profile     string // プロファイルのフォルダ名。空なら既定
	UserDataDir string // Firefox ディレクトリの上書き
}

// domainSuffix は取り出す cookie のドメイン。
const domainSuffix = "nicovideo.jp"

// Load はブラウザの cookie DB から nicovideo.jp の cookie を読む。
func Load(o Options) ([]Cookie, error) {
	var cs []Cookie
	var err error
	switch strings.ToLower(o.Browser) {
	case "firefox":
		cs, err = loadFirefox(o)
	default:
		return nil, fmt.Errorf("未対応のブラウザ: %q (firefox のみ。Chrome / Edge は cookies.txt か -session を使う)", o.Browser)
	}
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		if c.Name == "user_session" {
			return cs, nil
		}
	}
	return cs, fmt.Errorf("%s に nicovideo.jp の user_session がありません (ニコニコにログイン済みか、プロファイルは正しいか確認)", o.Browser)
}

// openCopy は cookie DB を (WAL 付きで) 一時ディレクトリへコピーして開く。
// ブラウザ起動中でもロックと干渉せず、元ファイルを変更しない。
func openCopy(src string) (db *sql.DB, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "nico-dl-cookie-*")
	if err != nil {
		return nil, nil, err
	}
	remove := func() { os.RemoveAll(dir) }
	dst := filepath.Join(dir, "cookies.db")
	for _, suf := range []string{"", "-wal", "-shm"} {
		if err := copyFile(src+suf, dst+suf); err != nil {
			if suf == "" {
				remove()
				return nil, nil, fmt.Errorf("cookie DB をコピーできません (ブラウザを閉じて再実行してください): %w", err)
			}
			if !os.IsNotExist(err) {
				remove()
				return nil, nil, err
			}
		}
	}
	db, err = sql.Open("sqlite", dst)
	if err != nil {
		remove()
		return nil, nil, err
	}
	return db, func() { db.Close(); remove() }, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
