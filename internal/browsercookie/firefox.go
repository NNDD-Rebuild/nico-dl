package browsercookie

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func firefoxRoots() []string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		return []string{filepath.Join(os.Getenv("APPDATA"), "Mozilla", "Firefox")}
	case "darwin":
		return []string{filepath.Join(home, "Library", "Application Support", "Firefox")}
	}
	return []string{
		filepath.Join(home, ".mozilla", "firefox"),
		filepath.Join(home, "snap", "firefox", "common", ".mozilla", "firefox"),
		filepath.Join(home, ".var", "app", "org.mozilla.firefox", ".mozilla", "firefox"),
	}
}

type iniSection map[string]string

// parseINI は profiles.ini を [セクション名] → キー/値 の順序付きで読む。
func parseINI(path string) ([]struct {
	Name string
	Kv   iniSection
}, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []struct {
		Name string
		Kv   iniSection
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		ln := strings.TrimSpace(sc.Text())
		switch {
		case ln == "" || strings.HasPrefix(ln, ";") || strings.HasPrefix(ln, "#"):
		case strings.HasPrefix(ln, "[") && strings.HasSuffix(ln, "]"):
			out = append(out, struct {
				Name string
				Kv   iniSection
			}{ln[1 : len(ln)-1], iniSection{}})
		default:
			if i := strings.IndexByte(ln, '='); i > 0 && len(out) > 0 {
				out[len(out)-1].Kv[ln[:i]] = ln[i+1:]
			}
		}
	}
	return out, sc.Err()
}

// firefoxProfileDir は使用するプロファイルのフォルダを返す。
func firefoxProfileDir(o Options) (string, error) {
	roots := firefoxRoots()
	if o.UserDataDir != "" {
		roots = []string{o.UserDataDir}
	}
	var tried []string
	for _, root := range roots {
		secs, err := parseINI(filepath.Join(root, "profiles.ini"))
		if err != nil {
			tried = append(tried, root)
			continue
		}
		resolve := func(s iniSection) string {
			p := s["Path"]
			if s["IsRelative"] != "0" {
				p = filepath.Join(root, filepath.FromSlash(p))
			}
			return p
		}
		if o.Profile != "" {
			for _, s := range secs {
				if !strings.HasPrefix(s.Name, "Profile") {
					continue
				}
				p := resolve(s.Kv)
				if s.Kv["Name"] == o.Profile || filepath.Base(p) == o.Profile {
					return p, nil
				}
			}
			continue
		}
		// 現行 Firefox: [Install*] の Default が実際に使われるプロファイル
		for _, s := range secs {
			if strings.HasPrefix(s.Name, "Install") && s.Kv["Default"] != "" {
				return resolve(iniSection{"Path": s.Kv["Default"], "IsRelative": "1"}), nil
			}
		}
		for _, s := range secs {
			if strings.HasPrefix(s.Name, "Profile") && s.Kv["Default"] == "1" {
				return resolve(s.Kv), nil
			}
		}
	}
	if o.Profile != "" {
		return "", fmt.Errorf("Firefox プロファイル %q が見つかりません", o.Profile)
	}
	return "", errors.New("Firefox のプロファイルが見つかりません (-browser-dir で Firefox フォルダを指定可)")
}

func loadFirefox(o Options) ([]Cookie, error) {
	dir, err := firefoxProfileDir(o)
	if err != nil {
		return nil, err
	}
	db, cleanup, err := openCopy(filepath.Join(dir, "cookies.sqlite"))
	if err != nil {
		return nil, err
	}
	defer cleanup()

	rows, err := db.Query(`SELECT name, value, host, path, isSecure, isHttpOnly, expiry
		FROM moz_cookies WHERE host = ? OR host LIKE ?`, domainSuffix, "%."+domainSuffix)
	if err != nil {
		return nil, fmt.Errorf("moz_cookies 読込: %w", err)
	}
	defer rows.Close()
	var out []Cookie
	for rows.Next() {
		var c Cookie
		var secure, httpOnly int
		if err := rows.Scan(&c.Name, &c.Value, &c.Domain, &c.Path, &secure, &httpOnly, &c.Expires); err != nil {
			return nil, err
		}
		c.Secure, c.HTTPOnly = secure != 0, httpOnly != 0
		out = append(out, c)
	}
	return out, rows.Err()
}
