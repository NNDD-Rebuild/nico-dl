package nico

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"nico-dl/internal/browsercookie"
	"os"
	"strings"
	"time"
)

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36"

// Client はニコニコ API 用の HTTP クライアント。cookie は jar で保持する。
type Client struct {
	http     *http.Client
	LoggedIn bool

	Wait429   time.Duration // コメント API が 429 のときの待機 (0 なら既定 185 秒)
	MaxHeight int           // 映像の最大高さ (0 なら制限なし)

	commentLimit *tokenBucket // コメント API のバースト制限 (NNDD-RE と同じ: 容量 60、0.4/秒)
	dlLimit      *tokenBucket // DL 速度制限 (nil なら無制限)
}

// APIError は HTTP エラーと meta.errorCode を保持する。
type APIError struct {
	Status    int
	ErrorCode string
	URL       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("HTTP %d (%s) %s", e.Status, e.ErrorCode, e.URL)
}

func NewClient() *Client {
	jar, _ := cookiejar.New(nil)
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       60 * time.Second,
		MaxIdleConnsPerHost:   8,
		ForceAttemptHTTP2:     true,
	}
	return &Client{http: &http.Client{Jar: jar, Transport: tr}, commentLimit: newTokenBucket(60, 0.4)}
}

// SetUserSession は user_session cookie を設定する。
func (c *Client) SetUserSession(v string) {
	if v == "" {
		return
	}
	u, _ := url.Parse("https://www.nicovideo.jp/")
	c.http.Jar.SetCookies(u, []*http.Cookie{{
		Name: "user_session", Value: v, Domain: ".nicovideo.jp", Path: "/",
		Secure: true, HttpOnly: true,
	}})
	c.LoggedIn = true
}

// LoadNetscapeCookies は Netscape 形式の cookie ファイルを読み込む。
func (c *Client) LoadNetscapeCookies(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		httpOnly := false
		if strings.HasPrefix(line, "#HttpOnly_") {
			line = strings.TrimPrefix(line, "#HttpOnly_")
			httpOnly = true
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p := strings.Split(line, "\t")
		if len(p) < 7 {
			continue
		}
		domain := p[0]
		if !strings.HasSuffix(strings.TrimPrefix(domain, "."), "nicovideo.jp") {
			continue
		}
		ck := &http.Cookie{Name: p[5], Value: p[6], Domain: domain, Path: p[2], Secure: p[3] == "TRUE", HttpOnly: httpOnly}
		u := &url.URL{Scheme: "https", Host: strings.TrimPrefix(domain, "."), Path: "/"}
		c.http.Jar.SetCookies(u, []*http.Cookie{ck})
		if p[5] == "user_session" {
			c.LoggedIn = true
		}
	}
	return sc.Err()
}

func (c *Client) do(ctx context.Context, method, rawURL string, hdr map[string]string, body []byte) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, method, rawURL, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", "https://www.nicovideo.jp/")
	req.Header.Set("Accept", "application/json, */*;q=0.8") // application/json 単体は 406 になる
	req.Header.Set("X-Frontend-Id", "6")
	req.Header.Set("X-Frontend-Version", "0")
	req.Header.Set("X-Niconico-Language", "ja-jp")
	req.Header.Set("X-Request-With", "https://www.nicovideo.jp")
	req.Header.Set("X-Client-Os-Type", "others")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := c.http.Do(req.WithContext(rctx))
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var m struct {
			Meta struct {
				ErrorCode string `json:"errorCode"`
			} `json:"meta"`
		}
		_ = json.Unmarshal(data, &m)
		return nil, &APIError{Status: res.StatusCode, ErrorCode: m.Meta.ErrorCode, URL: rawURL}
	}
	return data, nil
}

func (c *Client) get(ctx context.Context, u string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, u, nil, nil)
}

func (c *Client) postJSON(ctx context.Context, u string, hdr map[string]string, v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	h := map[string]string{"Content-Type": "application/json"}
	for k, val := range hdr {
		h[k] = val
	}
	return c.do(ctx, http.MethodPost, u, h, b)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// LoadBrowserCookies はブラウザ (firefox) から nicovideo.jp の cookie を取り込む。
func (c *Client) LoadBrowserCookies(o browsercookie.Options) (int, error) {
	cs, err := browsercookie.Load(o)
	if err != nil {
		return 0, err
	}
	now := time.Now().Unix()
	n := 0
	for _, bc := range cs {
		if bc.Expires > 0 && bc.Expires < now {
			continue // 期限切れ
		}
		ck := &http.Cookie{Name: bc.Name, Value: bc.Value, Domain: bc.Domain, Path: bc.Path,
			Secure: bc.Secure, HttpOnly: bc.HTTPOnly}
		if bc.Expires > 0 {
			ck.Expires = time.Unix(bc.Expires, 0)
		}
		host := strings.TrimPrefix(bc.Domain, ".")
		c.http.Jar.SetCookies(&url.URL{Scheme: "https", Host: host, Path: "/"}, []*http.Cookie{ck})
		if bc.Name == "user_session" {
			c.LoggedIn = true
		}
		n++
	}
	if !c.LoggedIn {
		return n, fmt.Errorf("user_session が期限切れです。%s で再ログインしてください", o.Browser)
	}
	return n, nil
}
