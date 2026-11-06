package nico

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	segWorkers    = 3
	segMaxRetries = 3
	segRetryWait  = 5 * time.Second
)

func pickBest(ss []Stream, maxHeight int) (Stream, bool) {
	var best Stream
	found := false
	for _, s := range ss {
		if !s.IsAvailable {
			continue
		}
		if maxHeight > 0 && s.Height > maxHeight {
			continue
		}
		if !found || s.QualityLevel > best.QualityLevel {
			best, found = s, true
		}
	}
	return best, found
}

// pickLowest は利用可能な最低画質を返す。
func pickLowest(ss []Stream) (Stream, bool) {
	var low Stream
	found := false
	for _, s := range ss {
		if s.IsAvailable && (!found || s.QualityLevel < low.QualityLevel) {
			low, found = s, true
		}
	}
	return low, found
}

// FetchContentURL は access-rights/hls から署名付きマスター m3u8 URL を得る。
func (c *Client) FetchContentURL(ctx context.Context, w *Watch, audioOnly bool) (string, error) {
	d := w.Media.Domand
	if d == nil || d.AccessRightKey == "" {
		return "", errors.New("domand 情報なし (削除済み/要ログイン/要プレミアムの可能性)")
	}
	a, okA := pickBest(d.Audios, 0)
	v, okV := pickBest(d.Videos, c.MaxHeight)
	if !okV && c.MaxHeight > 0 && !audioOnly {
		v, okV = pickLowest(d.Videos) // 上限を満たす画質が無ければ最低画質
	}
	if !okA || (!okV && !audioOnly) {
		return "", errors.New("利用可能なストリームなし")
	}
	outputs := [][]string{{v.ID, a.ID}}
	if audioOnly {
		outputs = [][]string{{a.ID}}
	}
	u := fmt.Sprintf("https://nvapi.nicovideo.jp/v1/watch/%s/access-rights/hls?actionTrackId=%s", w.Video.ID, w.ActionTrackID)
	data, err := c.postJSON(ctx, u, map[string]string{"X-Access-Right-Key": d.AccessRightKey},
		map[string]any{"outputs": outputs})
	if err != nil {
		return "", fmt.Errorf("access-rights/hls: %w", err)
	}
	var r struct {
		Data struct {
			ContentURL string `json:"contentUrl"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &r); err != nil || r.Data.ContentURL == "" {
		return "", fmt.Errorf("contentUrl 取得失敗: %s", string(data))
	}
	return r.Data.ContentURL, nil
}

type masterStream struct {
	URL       string
	Bandwidth int
	AudioGrp  string
}

type master struct {
	Streams    []masterStream
	Audios     map[string]string // GROUP-ID -> URI
	AudioFirst string
}

type segment struct{ URL string }

type variant struct {
	MapURL   string
	KeyURL   string
	IV       []byte
	Segments []segment
}

func parseAttrs(s string) map[string]string {
	m := map[string]string{}
	for len(s) > 0 {
		i := strings.IndexByte(s, '=')
		if i < 0 {
			break
		}
		k := strings.TrimSpace(s[:i])
		s = s[i+1:]
		var v string
		if strings.HasPrefix(s, `"`) {
			j := strings.IndexByte(s[1:], '"')
			if j < 0 {
				v, s = s[1:], ""
			} else {
				v, s = s[1:1+j], s[j+2:]
			}
		} else {
			j := strings.IndexByte(s, ',')
			if j < 0 {
				v, s = s, ""
			} else {
				v, s = s[:j], s[j:]
			}
		}
		s = strings.TrimPrefix(s, ",")
		m[k] = v
	}
	return m
}

func resolve(base, ref string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	return b.ResolveReference(r).String(), nil
}

func parseMaster(base, text string) (*master, error) {
	m := &master{Audios: map[string]string{}}
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		ln := strings.TrimSpace(lines[i])
		switch {
		case strings.HasPrefix(ln, "#EXT-X-MEDIA:"):
			at := parseAttrs(strings.TrimPrefix(ln, "#EXT-X-MEDIA:"))
			if at["TYPE"] == "AUDIO" && at["URI"] != "" {
				u, err := resolve(base, at["URI"])
				if err != nil {
					return nil, err
				}
				m.Audios[at["GROUP-ID"]] = u
				if m.AudioFirst == "" {
					m.AudioFirst = u
				}
			}
		case strings.HasPrefix(ln, "#EXT-X-STREAM-INF:"):
			at := parseAttrs(strings.TrimPrefix(ln, "#EXT-X-STREAM-INF:"))
			if i+1 >= len(lines) {
				break
			}
			i++
			u, err := resolve(base, strings.TrimSpace(lines[i]))
			if err != nil {
				return nil, err
			}
			bw, _ := strconv.Atoi(at["BANDWIDTH"])
			m.Streams = append(m.Streams, masterStream{URL: u, Bandwidth: bw, AudioGrp: at["AUDIO"]})
		}
	}
	sort.SliceStable(m.Streams, func(i, j int) bool { return m.Streams[i].Bandwidth > m.Streams[j].Bandwidth })
	return m, nil
}

func parseVariant(base, text string) (*variant, error) {
	v := &variant{}
	for _, raw := range strings.Split(text, "\n") {
		ln := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(ln, "#EXT-X-MAP:"):
			at := parseAttrs(strings.TrimPrefix(ln, "#EXT-X-MAP:"))
			u, err := resolve(base, at["URI"])
			if err != nil {
				return nil, err
			}
			v.MapURL = u
		case strings.HasPrefix(ln, "#EXT-X-KEY:"):
			at := parseAttrs(strings.TrimPrefix(ln, "#EXT-X-KEY:"))
			if at["METHOD"] == "AES-128" {
				u, err := resolve(base, at["URI"])
				if err != nil {
					return nil, err
				}
				v.KeyURL = u
				if iv := at["IV"]; iv != "" {
					iv = strings.TrimPrefix(strings.TrimPrefix(iv, "0x"), "0X")
					iv = strings.Repeat("0", max(0, 32-len(iv))) + iv
					b, err := hex.DecodeString(iv)
					if err != nil {
						return nil, fmt.Errorf("IV 不正: %w", err)
					}
					v.IV = b
				}
			} else if at["METHOD"] != "NONE" && at["METHOD"] != "" {
				return nil, fmt.Errorf("未対応の暗号化: %s", at["METHOD"])
			}
		case ln != "" && !strings.HasPrefix(ln, "#"):
			u, err := resolve(base, ln)
			if err != nil {
				return nil, err
			}
			v.Segments = append(v.Segments, segment{URL: u})
		}
	}
	if len(v.Segments) == 0 {
		return nil, errors.New("セグメントなし")
	}
	return v, nil
}

func decryptAES128(key, iv, data []byte) ([]byte, error) {
	if len(data)%aes.BlockSize != 0 || len(data) == 0 {
		return nil, errors.New("暗号文長が不正")
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(blk, iv).CryptBlocks(out, data)
	pad := int(out[len(out)-1])
	if pad < 1 || pad > aes.BlockSize || pad > len(out) {
		return nil, errors.New("PKCS#7 パディング不正")
	}
	return out[:len(out)-pad], nil
}

// downloadTrack は variant の init + 全セグメントを復号して dst に連結する (fMP4)。
func (c *Client) downloadTrack(ctx context.Context, variantURL, dst string, progress func(done, total int)) error {
	txt, err := c.get(ctx, variantURL)
	if err != nil {
		return fmt.Errorf("variant 取得: %w", err)
	}
	v, err := parseVariant(variantURL, string(txt))
	if err != nil {
		return err
	}
	var key, iv []byte
	if v.KeyURL != "" {
		if key, err = c.get(ctx, v.KeyURL); err != nil {
			return fmt.Errorf("鍵取得: %w", err)
		}
		if len(key) != 16 {
			return fmt.Errorf("鍵長が不正: %d", len(key))
		}
		iv = v.IV
		if iv == nil {
			iv = make([]byte, 16)
		}
	}

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if v.MapURL != "" {
		b, err := c.get(ctx, v.MapURL)
		if err != nil {
			return fmt.Errorf("init 取得: %w", err)
		}
		if _, err := out.Write(b); err != nil {
			return err
		}
	}

	n := len(v.Segments)
	results := make([][]byte, n)
	var mu sync.Mutex
	cond := sync.NewCond(&mu)
	next, written := 0, 0
	var firstErr error

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	fetch := func(s segment) ([]byte, error) {
		var last error
		for try := 0; try <= segMaxRetries; try++ {
			if try > 0 {
				if err := sleepCtx(ctx, segRetryWait); err != nil {
					return nil, err
				}
			}
			b, err := c.get(ctx, s.URL)
			if err != nil {
				last = err
				continue
			}
			if err := c.throttle(ctx, len(b)); err != nil {
				return nil, err
			}
			if key != nil {
				if b, err = decryptAES128(key, iv, b); err != nil {
					last = err
					continue
				}
			}
			return b, nil
		}
		return nil, last
	}

	var wg sync.WaitGroup
	for w := 0; w < segWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				// 書込み位置から離れすぎた先読みを抑制 (メモリ保護)
				for next < n && next-written >= segWorkers*4 && firstErr == nil {
					cond.Wait()
				}
				if next >= n || firstErr != nil {
					mu.Unlock()
					return
				}
				i := next
				next++
				mu.Unlock()

				b, err := fetch(v.Segments[i])
				mu.Lock()
				if err != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("セグメント %d: %w", i, err)
						cancel()
					}
				} else {
					results[i] = b
				}
				cond.Broadcast()
				mu.Unlock()
			}
		}()
	}

	// 順序通りに書き出し
	mu.Lock()
	for written < n && firstErr == nil {
		if results[written] == nil {
			cond.Wait()
			continue
		}
		b := results[written]
		results[written] = nil
		mu.Unlock()
		_, werr := out.Write(b)
		mu.Lock()
		if werr != nil && firstErr == nil {
			firstErr = werr
			cancel()
		}
		written++
		cond.Broadcast()
		if progress != nil {
			mu.Unlock()
			progress(written, n)
			mu.Lock()
		}
	}
	err = firstErr
	cond.Broadcast()
	mu.Unlock()
	wg.Wait()
	return err
}

// DownloadVideo は HLS を取得して out (.mp4 / 音声のみは .m4a) に保存する。
func (c *Client) DownloadVideo(ctx context.Context, w *Watch, out string, audioOnly bool, log func(string)) error {
	contentURL, err := c.FetchContentURL(ctx, w, audioOnly)
	if err != nil {
		return err
	}
	mtxt, err := c.get(ctx, contentURL)
	if err != nil {
		return fmt.Errorf("master 取得: %w", err)
	}
	m, err := parseMaster(contentURL, string(mtxt))
	if err != nil {
		return err
	}

	tmp, err := os.MkdirTemp("", "nico-dl-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	prog := func(label string) func(int, int) {
		return func(d, t int) {
			if d == t || d%20 == 0 {
				log(fmt.Sprintf("  %s %d/%d", label, d, t))
			}
		}
	}

	audioURL := m.AudioFirst
	if len(m.Streams) > 0 {
		if u, ok := m.Audios[m.Streams[0].AudioGrp]; ok {
			audioURL = u
		}
	}

	if audioOnly {
		src := audioURL
		if src == "" {
			if len(m.Streams) == 0 {
				return errors.New("音声ストリームなし")
			}
			src = m.Streams[0].URL
		}
		return c.downloadTrack(ctx, src, out, prog("audio"))
	}

	if len(m.Streams) == 0 {
		return errors.New("映像ストリームなし")
	}
	vPath := filepath.Join(tmp, "video.mp4")
	if err := c.downloadTrack(ctx, m.Streams[0].URL, vPath, prog("video")); err != nil {
		return err
	}
	if audioURL == "" {
		return os.Rename(vPath, out) // 音声が映像に含まれる場合
	}
	aPath := filepath.Join(tmp, "audio.mp4")
	if err := c.downloadTrack(ctx, audioURL, aPath, prog("audio")); err != nil {
		return err
	}
	log("  mux")
	return muxMP4(ctx, vPath, aPath, out)
}
