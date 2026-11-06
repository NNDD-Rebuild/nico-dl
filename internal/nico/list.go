package nico

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
)

const nvapiBase = "https://nvapi.nicovideo.jp"
const listPageSize = 100

// Listing は一覧系 (マイリスト/投稿動画/シリーズ) の展開結果。
type Listing struct {
	Name     string
	VideoIDs []string
}

// ListKind は一覧の種類。
type ListKind string

const (
	KindMylist ListKind = "mylist"
	KindUser   ListKind = "user"
	KindSeries ListKind = "series"
)

var listRe = regexp.MustCompile(`(mylist|user|series)/(\d+)`)

// ParseListURL は URL/文字列が一覧なら種類と ID を返す。
func ParseListURL(s string) (ListKind, string, bool) {
	m := listRe.FindStringSubmatch(s)
	if m == nil {
		return "", "", false
	}
	return ListKind(m[1]), m[2], true
}

// paged は page=1.. を順に取得し、parse が返す (ids, total, name) を集める。
func (c *Client) paged(ctx context.Context, mk func(page int) string,
	parse func(data []byte) (ids []string, total int, name string, err error)) (*Listing, error) {

	l := &Listing{}
	total := -1
	for page := 1; page < 1000; page++ {
		data, err := c.get(ctx, mk(page))
		if err != nil {
			return l, err
		}
		ids, t, name, err := parse(data)
		if err != nil {
			return l, err
		}
		if name != "" && l.Name == "" {
			l.Name = name
		}
		if t >= 0 {
			total = t
		}
		l.VideoIDs = append(l.VideoIDs, ids...)
		if len(ids) == 0 || (total >= 0 && len(l.VideoIDs) >= total) {
			break
		}
	}
	return l, nil
}

// FetchMylist は公開マイリスト (ログイン時は自分のマイリストを先に試す) の動画 ID を全件取得する。
func (c *Client) FetchMylist(ctx context.Context, id string) (*Listing, error) {
	// 削除済みで video が欠落する項目があるため、ids 数ではなく item 数で終了判定する
	cands := []func(int) string{}
	if c.LoggedIn {
		cands = append(cands, func(p int) string {
			return fmt.Sprintf("%s/v1/users/me/mylists/%s?pageSize=%d&page=%d", nvapiBase, url.PathEscape(id), listPageSize, p)
		})
	}
	cands = append(cands, func(p int) string {
		return fmt.Sprintf("%s/v2/mylists/%s?pageSize=%d&page=%d", nvapiBase, url.PathEscape(id), listPageSize, p)
	})

	var lastErr error
	for _, mk := range cands {
		l, err := c.fetchMylistWith(ctx, mk)
		if err == nil {
			return l, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("マイリスト %s の取得失敗: %w", id, lastErr)
}

func (c *Client) fetchMylistWith(ctx context.Context, mk func(int) string) (*Listing, error) {
	l := &Listing{}
	count := 0
	for page := 1; page < 1000; page++ {
		data, err := c.get(ctx, mk(page))
		if err != nil {
			return nil, err
		}
		var r struct {
			Data struct {
				Mylist struct {
					Name  string `json:"name"`
					Total int    `json:"totalItemCount"`
					Items []struct {
						Video *struct {
							ID string `json:"id"`
						} `json:"video"`
					} `json:"items"`
				} `json:"mylist"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		m := r.Data.Mylist
		if l.Name == "" {
			l.Name = m.Name
		}
		for _, it := range m.Items {
			if it.Video != nil && it.Video.ID != "" {
				l.VideoIDs = append(l.VideoIDs, it.Video.ID)
			}
		}
		count += len(m.Items)
		if len(m.Items) == 0 || count >= m.Total {
			break
		}
	}
	return l, nil
}

// FetchUserVideos はユーザー投稿動画の ID を全件取得する。
func (c *Client) FetchUserVideos(ctx context.Context, userID string) (*Listing, error) {
	return c.paged(ctx, func(p int) string {
		q := url.Values{
			"sortKey": {"registeredAt"}, "sortOrder": {"desc"},
			"pageSize": {strconv.Itoa(listPageSize)}, "page": {strconv.Itoa(p)},
			"sensitiveContents": {"mask"}, // センシティブ動画も含める (名前は sensitiveContents)
		}
		return fmt.Sprintf("%s/v3/users/%s/videos?%s", nvapiBase, url.PathEscape(userID), q.Encode())
	}, func(data []byte) ([]string, int, string, error) {
		var r struct {
			Data struct {
				TotalCount int `json:"totalCount"`
				Items      []struct {
					Essential struct {
						ID string `json:"id"`
					} `json:"essential"`
				} `json:"items"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, -1, "", err
		}
		var ids []string
		for _, it := range r.Data.Items {
			if it.Essential.ID != "" {
				ids = append(ids, it.Essential.ID)
			}
		}
		return ids, r.Data.TotalCount, "user-" + userID, nil
	})
}

// FetchSeries はシリーズ内の動画 ID を全件取得する。
func (c *Client) FetchSeries(ctx context.Context, seriesID string) (*Listing, error) {
	return c.paged(ctx, func(p int) string {
		return fmt.Sprintf("%s/v2/series/%s?pageSize=%d&page=%d", nvapiBase, url.PathEscape(seriesID), listPageSize, p)
	}, func(data []byte) ([]string, int, string, error) {
		var r struct {
			Data struct {
				TotalCount int `json:"totalCount"`
				Detail     struct {
					Title string `json:"title"`
				} `json:"detail"`
				Items []struct {
					Video struct {
						ID string `json:"id"`
					} `json:"video"`
				} `json:"items"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, -1, "", err
		}
		var ids []string
		for _, it := range r.Data.Items {
			if it.Video.ID != "" {
				ids = append(ids, it.Video.ID)
			}
		}
		return ids, r.Data.TotalCount, r.Data.Detail.Title, nil
	})
}

// FetchList は種類に応じて一覧を展開する。
func (c *Client) FetchList(ctx context.Context, kind ListKind, id string) (*Listing, error) {
	switch kind {
	case KindMylist:
		return c.FetchMylist(ctx, id)
	case KindUser:
		return c.FetchUserVideos(ctx, id)
	case KindSeries:
		return c.FetchSeries(ctx, id)
	}
	return nil, fmt.Errorf("未対応の一覧: %s", kind)
}
