package nico

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// requestTimeout は 1 リクエスト (本文の読み切りまで) の上限。
const requestTimeout = 120 * time.Second

// tokenBucket はトークンバケット方式のレート制限。
type tokenBucket struct {
	mu       sync.Mutex
	capacity float64
	rate     float64 // 1 秒あたりの補充量
	tokens   float64
	last     time.Time
}

func newTokenBucket(capacity, rate float64) *tokenBucket {
	return &tokenBucket{capacity: capacity, rate: rate, tokens: capacity, last: time.Now()}
}

// take は n トークンを消費する。足りなければ補充まで待つ。
func (b *tokenBucket) take(ctx context.Context, n float64) error {
	for {
		b.mu.Lock()
		now := time.Now()
		b.tokens += now.Sub(b.last).Seconds() * b.rate
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.last = now
		if b.tokens >= n || n > b.capacity {
			b.tokens -= n
			b.mu.Unlock()
			return nil
		}
		wait := time.Duration((n - b.tokens) / b.rate * float64(time.Second))
		b.mu.Unlock()
		if err := sleepCtx(ctx, wait); err != nil {
			return err
		}
	}
}

// drain は 429 を受けたときに残りトークンを捨てる。
func (b *tokenBucket) drain() {
	b.mu.Lock()
	b.tokens = 0
	b.last = time.Now()
	b.mu.Unlock()
}

// SetRateLimitMbps は DL 速度の上限 (Mbps) を設定する。0 以下で無制限。
func (c *Client) SetRateLimitMbps(mbps float64) {
	if mbps <= 0 {
		c.dlLimit = nil
		return
	}
	bps := mbps * 1e6 / 8
	capacity := bps
	if capacity < 1<<20 {
		capacity = 1 << 20 // 1 セグメント分は一度に消費できるようにする
	}
	c.dlLimit = newTokenBucket(capacity, bps)
}

func (c *Client) throttle(ctx context.Context, n int) error {
	if c.dlLimit == nil {
		return nil
	}
	return c.dlLimit.take(ctx, float64(n))
}

// Profile は users/me の要約。
type Profile struct {
	ID        string
	Nickname  string
	IsPremium bool
}

// Whoami はログイン中のアカウントを返す。未ログイン・期限切れは APIError (401/403)。
func (c *Client) Whoami(ctx context.Context) (*Profile, error) {
	data, err := c.get(ctx, nvapiBase+"/v1/users/me")
	if err != nil {
		return nil, err
	}
	var r struct {
		Data struct {
			User struct {
				ID        json.Number `json:"id"`
				Nickname  string      `json:"nickname"`
				IsPremium bool        `json:"isPremium"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	u := r.Data.User
	if u.ID.String() == "" {
		return nil, fmt.Errorf("アカウント情報を取得できません")
	}
	return &Profile{ID: u.ID.String(), Nickname: u.Nickname, IsPremium: u.IsPremium}, nil
}
