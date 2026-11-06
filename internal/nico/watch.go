package nico

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"time"
)

// Watch は watch API レスポンスのうち DL に使う部分。
type Watch struct {
	ActionTrackID string
	Video         struct {
		ID           string `json:"id"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		Duration     int    `json:"duration"`
		IsDeleted    bool   `json:"isDeleted"`
		RegisteredAt string `json:"registeredAt"`
		Count        struct {
			View    int `json:"view"`
			Comment int `json:"comment"`
			Mylist  int `json:"mylist"`
		} `json:"count"`
		Thumbnail struct {
			URL       string `json:"url"`
			MiddleURL string `json:"middleUrl"`
			LargeURL  string `json:"largeUrl"`
		} `json:"thumbnail"`
	} `json:"video"`
	Tag struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	} `json:"tag"`
	Owner *struct {
		ID       json.Number `json:"id"`
		Nickname string      `json:"nickname"`
		IconURL  string      `json:"iconUrl"`
	} `json:"owner"`
	Channel *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"channel"`
	Media struct {
		Domand *struct {
			AccessRightKey string   `json:"accessRightKey"`
			Videos         []Stream `json:"videos"`
			Audios         []Stream `json:"audios"`
		} `json:"domand"`
	} `json:"media"`
	Comment struct {
		NvComment struct {
			Server    string          `json:"server"`
			ThreadKey string          `json:"threadKey"`
			Params    json.RawMessage `json:"params"`
		} `json:"nvComment"`
	} `json:"comment"`
}

// Stream は domand の映像/音声候補。
type Stream struct {
	ID           string `json:"id"`
	IsAvailable  bool   `json:"isAvailable"`
	QualityLevel int    `json:"qualityLevel"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
}

const defaultCommentServer = "https://public.nvcomment.nicovideo.jp"

func newActionTrackID() string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 10)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		b[i] = chars[n.Int64()]
	}
	return fmt.Sprintf("%s_%d", b, time.Now().UnixMilli())
}

// FetchWatch は watch API を呼ぶ。ログイン時は v3、失敗または未ログインは v3_guest。
func (c *Client) FetchWatch(ctx context.Context, videoID string) (*Watch, error) {
	atid := newActionTrackID()
	kinds := []string{"v3_guest"}
	if c.LoggedIn {
		kinds = []string{"v3", "v3_guest"}
	}
	var lastErr error
	for _, k := range kinds {
		u := fmt.Sprintf("https://www.nicovideo.jp/api/watch/%s/%s?actionTrackId=%s", k, videoID, atid)
		data, err := c.get(ctx, u)
		if err != nil {
			lastErr = err
			continue
		}
		var env struct {
			Data Watch `json:"data"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			lastErr = err
			continue
		}
		w := &env.Data
		w.ActionTrackID = atid
		if w.Video.ID == "" {
			w.Video.ID = videoID
		}
		return w, nil
	}
	return nil, fmt.Errorf("watch API 取得失敗: %w", lastErr)
}

// DownloadThumbnail はサムネイル画像を保存する。
func (c *Client) DownloadThumbnail(ctx context.Context, w *Watch, path string) error {
	u := w.Video.Thumbnail.LargeURL
	if u == "" {
		u = w.Video.Thumbnail.MiddleURL
	}
	if u == "" {
		u = w.Video.Thumbnail.URL
	}
	if u == "" {
		return nil
	}
	b, err := c.do(ctx, "GET", u, map[string]string{"Accept": "image/*,*/*;q=0.8"}, nil)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
