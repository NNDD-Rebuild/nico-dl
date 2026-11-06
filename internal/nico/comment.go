package nico

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// flexString は JSON の文字列/数値どちらも文字列として受ける。
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" {
		s = ""
	}
	*f = flexString(s)
	return nil
}

type Comment struct {
	Thread    string
	Fork      string // main / owner / easy
	No        int
	VposMs    int
	Body      string
	Commands  []string
	UserID    string
	IsPremium bool
	Score     int
	Date      int64 // unix 秒
	NicoruCnt int
}

type rawThread struct {
	ID       flexString `json:"id"`
	Fork     flexString `json:"fork"`
	Comments []struct {
		No        int      `json:"no"`
		VposMs    int      `json:"vposMs"`
		Body      string   `json:"body"`
		Commands  []string `json:"commands"`
		UserID    string   `json:"userId"`
		IsPremium bool     `json:"isPremium"`
		Score     int      `json:"score"`
		PostedAt  string   `json:"postedAt"`
		NicoruCnt int      `json:"nicoruCount"`
	} `json:"comments"`
}

// postThreads は 1 回のリクエスト。バースト制限を守り、429 を受けたら残量を捨てる。
func (c *Client) postThreads(ctx context.Context, w *Watch, params json.RawMessage, additionals map[string]any) ([]Comment, error) {
	server := w.Comment.NvComment.Server
	if server == "" {
		server = defaultCommentServer
	}
	if len(params) == 0 {
		return nil, errors.New("nvComment.params なし")
	}
	body := map[string]any{
		"params":      params,
		"threadKey":   w.Comment.NvComment.ThreadKey,
		"additionals": additionals,
	}
	if err := c.commentLimit.take(ctx, 1); err != nil {
		return nil, err
	}
	data, err := c.postJSON(ctx, server+"/v1/threads", nil, body)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == 429 {
			c.commentLimit.drain()
		}
		return nil, err
	}
	var r struct {
		Data struct {
			Threads []rawThread `json:"threads"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	var out []Comment
	for _, t := range r.Data.Threads {
		for _, x := range t.Comments {
			ts, _ := time.Parse(time.RFC3339, x.PostedAt)
			out = append(out, Comment{
				Thread: string(t.ID), Fork: string(t.Fork), No: x.No, VposMs: x.VposMs,
				Body: x.Body, Commands: x.Commands, UserID: x.UserID, IsPremium: x.IsPremium,
				Score: x.Score, Date: ts.Unix(), NicoruCnt: x.NicoruCnt,
			})
		}
	}
	return out, nil
}

// FetchComments は今コメ (現在のコメント) を取得する。429 は待機して最大 5 回リトライする。
func (c *Client) FetchComments(ctx context.Context, w *Watch) ([]Comment, error) {
	wait := c.Wait429
	if wait <= 0 {
		wait = 185 * time.Second
	}
	for try := 0; ; try++ {
		cms, err := c.postThreads(ctx, w, w.Comment.NvComment.Params, map[string]any{})
		var ae *APIError
		if err != nil && errors.As(err, &ae) && ae.Status == 429 && try < 5 {
			if serr := sleepCtx(ctx, wait); serr != nil {
				return nil, serr
			}
			continue
		}
		return cms, err
	}
}

// FetchAllComments は target ごとに過去ログを遡って全量取得する。
// stopAt > 0 なら、その unix 秒以前に到達した時点で打ち切る (差分取得)。
// refresh は EXPIRED_TOKEN 時に watch を取り直すための関数。
func (c *Client) FetchAllComments(ctx context.Context, w *Watch, includeEasy bool, stopAt int64, wait429 time.Duration,
	refresh func() (*Watch, error), log func(string)) ([]Comment, error) {

	var p struct {
		Targets  []map[string]any `json:"targets"`
		Language string           `json:"language"`
	}
	if err := json.Unmarshal(w.Comment.NvComment.Params, &p); err != nil {
		return nil, fmt.Errorf("params 解析: %w", err)
	}
	// owner を先に処理
	sort.SliceStable(p.Targets, func(i, j int) bool {
		return fmt.Sprint(p.Targets[i]["fork"]) == "owner" && fmt.Sprint(p.Targets[j]["fork"]) != "owner"
	})

	seen := map[string]bool{}
	var all []Comment
	for _, tg := range p.Targets {
		fork := fmt.Sprint(tg["fork"])
		if fork == "easy" && !includeEasy {
			continue
		}
		params, _ := json.Marshal(map[string]any{"targets": []any{tg}, "language": p.Language})
		when := time.Now().Unix()
		count429, countExp := 0, 0
		for round := 0; round < 10000; round++ {
			batch, err := c.postThreads(ctx, w, params, map[string]any{"when": when, "res_from": -1000})
			if err != nil {
				var ae *APIError
				if errors.As(err, &ae) {
					if ae.Status == 429 && count429 < 5 {
						count429++
						log(fmt.Sprintf("  429: %v 待機", wait429))
						if err := sleepCtx(ctx, wait429); err != nil {
							return all, err
						}
						continue
					}
					if ae.ErrorCode == "EXPIRED_TOKEN" && countExp < 3 && refresh != nil {
						countExp++
						nw, rerr := refresh()
						if rerr != nil {
							return all, rerr
						}
						w = nw
						continue
					}
				}
				if round == 0 && errors.As(err, &ae) && ae.ErrorCode == "INVALID_TOKEN" && !c.LoggedIn {
					// 過去ログ (when 指定) はゲストだと拒否される。今コメだけで続行する
					log("  警告: 過去ログ取得は要ログインの可能性 (INVALID_TOKEN)。今コメのみ")
					break
				}
				if round == 0 {
					return all, err
				}
				log("  取得中断: " + err.Error())
				break
			}
			count429, countExp = 0, 0
			if len(batch) == 0 {
				break
			}
			minNo, minDate := batch[0].No, batch[0].Date
			for _, cm := range batch {
				k := cm.Thread + ":" + strconv.Itoa(cm.No)
				if !seen[k] {
					seen[k] = true
					all = append(all, cm)
				}
				if cm.No < minNo {
					minNo = cm.No
				}
				if cm.Date < minDate {
					minDate = cm.Date
				}
			}
			log(fmt.Sprintf("  %s: %d 件 (最古 no=%d)", fork, len(all), minNo))
			if minNo < 5 {
				break
			}
			if stopAt > 0 && minDate <= stopAt { // 差分取得: 既存の最新コメント時刻に到達
				break
			}
			if minDate >= when {
				minDate = when - 1
			}
			when = minDate
			if err := sleepCtx(ctx, 1500*time.Millisecond); err != nil { // 429 回避の簡易レート制限
				return all, err
			}
		}
	}
	return all, nil
}

var xmlBad = regexp.MustCompile("[\x00-\x08\x0B\x0C\x0E-\x1F￾￿]")

func xmlText(s string) string {
	s = xmlBad.ReplaceAllString(s, "")
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func xmlAttr(s string) string {
	return strings.ReplaceAll(xmlText(s), `"`, "&quot;")
}

func forkNum(f string) int {
	switch f {
	case "owner":
		return 1
	case "easy":
		return 2
	}
	return 0
}

// isAnon は匿名ユーザー ID (nv...: 形式) か。ID 形式は未検証。
func isAnon(uid string) bool {
	return strings.HasPrefix(uid, "nv") && strings.Contains(uid, ":")
}

// WriteCommentXML は NNDD 互換 XML を書き出す。fork は数値 (main=省略, owner=1, easy=2) で出力する。
func WriteCommentXML(path, videoID string, cms []Comment) error {
	sort.SliceStable(cms, func(i, j int) bool {
		if cms[i].Thread != cms[j].Thread {
			return cms[i].Thread < cms[j].Thread
		}
		if ri, rj := forkRank(cms[i].Fork), forkRank(cms[j].Fork); ri != rj {
			return ri < rj
		}
		return cms[i].No < cms[j].No
	})
	var sb strings.Builder
	sb.WriteString("<?xml version=\"1.0\" encoding=\"utf-8\"?>\n")
	if videoID != "" {
		fmt.Fprintf(&sb, "<!--BoonSutazioData=%s-->\n", videoID)
	}
	sb.WriteString("<packet>\n")
	now := time.Now().Unix()

	type grp struct {
		thread, fork string
		cms          []Comment
	}
	var groups []*grp
	idx := map[string]*grp{}
	for _, cm := range cms {
		k := cm.Thread + "/" + cm.Fork
		g, ok := idx[k]
		if !ok {
			g = &grp{thread: cm.Thread, fork: cm.Fork}
			idx[k] = g
			groups = append(groups, g)
		}
		g.cms = append(g.cms, cm)
	}
	for _, g := range groups {
		last := g.cms[len(g.cms)-1].No
		if g.fork == "main" || g.fork == "" {
			fmt.Fprintf(&sb, "<thread resultcode=\"0\" thread=\"%s\" last_res=\"%d\" ticket=\"0\" revision=\"1\" server_time=\"%d\"/>\n", xmlAttr(g.thread), last, now)
			fmt.Fprintf(&sb, "<leaf thread=\"%s\" count=\"%d\"/>\n", xmlAttr(g.thread), len(g.cms))
		} else {
			fmt.Fprintf(&sb, "<thread resultcode=\"0\" thread=\"%s\" ticket=\"0\" revision=\"1\" fork=\"%d\" server_time=\"%d\"/>\n", xmlAttr(g.thread), forkNum(g.fork), now)
		}
	}
	sb.WriteString("<ngups/>\n")
	for _, cm := range cms {
		anon := 0
		if isAnon(cm.UserID) {
			anon = 1
		}
		fmt.Fprintf(&sb, "<chat thread=\"%s\"", xmlAttr(cm.Thread))
		if f := forkNum(cm.Fork); f != 0 {
			fmt.Fprintf(&sb, " fork=\"%d\"", f)
		}
		fmt.Fprintf(&sb, " no=\"%d\" vpos=\"%d\" date=\"%d\" date_usec=\"0\" user_id=\"%s\" mail=\"%s\" score=\"%d\" anonymity=\"%d\" nicoru=\"%d\"",
			cm.No, cm.VposMs/10, cm.Date, xmlAttr(cm.UserID), xmlAttr(strings.Join(cm.Commands, " ")), cm.Score, anon, cm.NicoruCnt)
		if cm.IsPremium {
			sb.WriteString(` premium="1"`)
		}
		fmt.Fprintf(&sb, ">%s</chat>\n", xmlText(cm.Body))
	}
	sb.WriteString("</packet>\n")
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}

// WriteThumbInfoXML は watch 情報から [ThumbInfo].xml を合成する。
func WriteThumbInfoXML(path string, w *Watch) error {
	v := w.Video
	var sb strings.Builder
	sb.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<nicovideo_thumb_response status=\"ok\"><thumb>\n")
	el := func(n, val string) { fmt.Fprintf(&sb, "<%s>%s</%s>\n", n, xmlText(val), n) }
	thumb := v.Thumbnail.LargeURL
	if thumb == "" {
		thumb = v.Thumbnail.URL
	}
	el("video_id", v.ID)
	el("title", v.Title)
	el("description", html.UnescapeString(v.Description))
	el("thumbnail_url", thumb)
	el("first_retrieve", v.RegisteredAt)
	el("length", fmt.Sprintf("%d:%02d", v.Duration/60, v.Duration%60))
	el("view_counter", strconv.Itoa(v.Count.View))
	el("comment_num", strconv.Itoa(v.Count.Comment))
	el("mylist_counter", strconv.Itoa(v.Count.Mylist))
	sb.WriteString("<tags domain=\"jp\">")
	for _, t := range w.Tag.Items {
		fmt.Fprintf(&sb, "<tag>%s</tag>", xmlText(t.Name))
	}
	sb.WriteString("</tags>\n")
	if w.Owner != nil {
		el("user_id", w.Owner.ID.String())
		el("user_nickname", w.Owner.Nickname)
		el("user_icon_url", w.Owner.IconURL)
	}
	if w.Channel != nil {
		el("ch_id", w.Channel.ID)
		el("ch_name", w.Channel.Name)
	}
	sb.WriteString("</thumb></nicovideo_thumb_response>\n")
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}

// forkRank は XML 内のスレッド並び順 (main → owner → easy)。
func forkRank(f string) int {
	switch f {
	case "owner":
		return 1
	case "easy":
		return 2
	}
	return 0
}
