package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nico-dl/internal/browsercookie"
	"nico-dl/internal/nico"
)

type opts struct {
	outDir      string
	session     string
	cookieFile  string
	browser     string
	browserProf string
	browserDir  string
	videoOnly   bool
	commentOnly bool
	infoOnly    bool
	thumbOnly   bool
	noComment   bool
	allComments bool
	easy        bool
	audioOnly   bool
	thumbInfo   bool
	thumbImg    bool
	force       bool
	subdir      bool
	retry       int
	jobs        int
	maxHeight   int
	limitMbps   float64
	whoami      bool
	sleep       time.Duration
	wait429     time.Duration
}

func main() {
	initConsole()
	var o opts
	flag.StringVar(&o.outDir, "o", ".", "保存先ディレクトリ")
	flag.StringVar(&o.session, "session", os.Getenv("NICO_USER_SESSION"), "user_session cookie 値 (env: NICO_USER_SESSION)")
	flag.StringVar(&o.cookieFile, "cookies", "", "Netscape 形式 cookie ファイル")
	flag.StringVar(&o.browser, "browser", "", "ブラウザから cookie を取得: firefox")
	flag.StringVar(&o.browserProf, "browser-profile", "", "ブラウザのプロファイル名 (既定: 使用中のもの)")
	flag.StringVar(&o.browserDir, "browser-dir", "", "ブラウザのプロファイル置き場を明示 (User Data / Firefox フォルダ)")
	flag.BoolVar(&o.videoOnly, "video-only", false, "動画 (.mp4) のみ取得")
	flag.BoolVar(&o.commentOnly, "comment-only", false, "コメントのみ取得")
	flag.BoolVar(&o.infoOnly, "info-only", false, "動画情報 ([ThumbInfo].xml) のみ取得")
	flag.BoolVar(&o.thumbOnly, "thumb-only", false, "サムネイル ([ThumbImg].jpeg) のみ取得")
	flag.BoolVar(&o.noComment, "no-comment", false, "コメントを取得しない")
	flag.BoolVar(&o.allComments, "all-comments", false, "過去ログを遡って全量取得 (既存 XML があれば差分のみ)")
	flag.BoolVar(&o.easy, "easy", false, "かんたんコメントも取得 (-all-comments 時)")
	flag.BoolVar(&o.audioOnly, "audio-only", false, "音声 (.m4a) のみ取得")
	flag.BoolVar(&o.thumbInfo, "thumbinfo", true, "[ThumbInfo].xml を保存")
	flag.BoolVar(&o.thumbImg, "thumbimg", true, "サムネイル画像 [ThumbImg].jpeg を保存")
	flag.BoolVar(&o.force, "force", false, "既存の動画/コメントがあっても取り直す")
	flag.BoolVar(&o.subdir, "subdir", true, "マイリスト/投稿者/シリーズ指定時、一覧名のサブフォルダに保存")
	flag.BoolVar(&o.whoami, "whoami", false, "ログイン状態 (アカウント名・プレミアム) を確認して終了")
	flag.IntVar(&o.retry, "retry", 2, "動画ごとの失敗時リトライ回数")
	flag.IntVar(&o.jobs, "jobs", 1, "同時に処理する動画数")
	flag.IntVar(&o.maxHeight, "max-height", 0, "映像の最大高さ (例: 720。0 で制限なし。満たす画質が無ければ最低画質)")
	flag.Float64Var(&o.limitMbps, "limit-mbps", 0, "DL 速度の上限 (Mbps。0 で無制限)")
	flag.DurationVar(&o.sleep, "sleep", 0, "動画ごとの待機時間 (例: 3s)")
	flag.DurationVar(&o.wait429, "wait429", 185*time.Second, "429 時の待機時間")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"usage: nico-dl [flags] <動画ID|URL|マイリスト/ユーザー/シリーズURL>...\n"+
				"  例: sm9 / https://www.nicovideo.jp/watch/sm9 / mylist/123 / user/456 / series/789\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() == 0 && !o.whoami {
		flag.Usage()
		os.Exit(2)
	}
	if o.commentOnly && o.noComment {
		fmt.Fprintln(os.Stderr, "-comment-only と -no-comment は同時指定不可")
		os.Exit(2)
	}
	if o.videoOnly && o.audioOnly {
		fmt.Fprintln(os.Stderr, "-video-only と -audio-only は同時指定不可")
		os.Exit(2)
	}
	if o.jobs < 1 {
		o.jobs = 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cl := nico.NewClient()
	cl.Wait429 = o.wait429
	cl.MaxHeight = o.maxHeight
	cl.SetRateLimitMbps(o.limitMbps)
	cl.SetUserSession(o.session)
	if o.browser != "" {
		n, err := cl.LoadBrowserCookies(browsercookie.Options{Browser: o.browser, Profile: o.browserProf, UserDataDir: o.browserDir})
		if err != nil {
			fmt.Fprintln(os.Stderr, "ブラウザ cookie 取得失敗:", err)
			os.Exit(1)
		}
		fmt.Printf("%s から cookie %d 件を取得 (user_session あり)\n", o.browser, n)
	}
	if o.cookieFile != "" {
		if err := cl.LoadNetscapeCookies(o.cookieFile); err != nil {
			fmt.Fprintln(os.Stderr, "cookie 読込失敗:", err)
			os.Exit(1)
		}
	}

	if cl.LoggedIn {
		p, err := cl.Whoami(ctx)
		var ae *nico.APIError
		switch {
		case err == nil:
			kind := "一般会員"
			if p.IsPremium {
				kind = "プレミアム会員"
			}
			fmt.Printf("ログイン中: %s (ID %s, %s)\n", p.Nickname, p.ID, kind)
		case errors.As(err, &ae) && (ae.Status == 401 || ae.Status == 403):
			fmt.Fprintln(os.Stderr, "警告: user_session が無効または期限切れです (ゲスト扱いで続行)。ブラウザで再ログインしてください")
			cl.LoggedIn = false
		default:
			fmt.Fprintln(os.Stderr, "警告: ログイン状態を確認できませんでした:", err)
		}
	} else {
		fmt.Fprintln(os.Stderr, "警告: 未ログイン (ゲスト)。会員限定/高画質/過去ログは取得不可")
	}
	if o.whoami {
		if !cl.LoggedIn {
			os.Exit(1)
		}
		return
	}

	var targets []target
	failed := int32(0)
	for _, arg := range flag.Args() {
		ts, err := expand(ctx, cl, arg, &o)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] 展開失敗: %v\n", arg, err)
			failed++
			continue
		}
		targets = append(targets, ts...)
	}

	ch := make(chan target)
	var wg sync.WaitGroup
	for i := 0; i < o.jobs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			first := true
			for t := range ch {
				if ctx.Err() != nil {
					continue
				}
				if !first && o.sleep > 0 {
					select {
					case <-ctx.Done():
					case <-time.After(o.sleep):
					}
				}
				first = false
				if err := runWithRetry(ctx, cl, t, &o); err != nil {
					printf(true, "[%s] 失敗: %v\n", t.id, err)
					atomic.AddInt32(&failed, 1)
				}
			}
		}()
	}
	for _, t := range targets {
		ch <- t
	}
	close(ch)
	wg.Wait()

	if ctx.Err() != nil {
		os.Exit(130)
	}
	if atomic.LoadInt32(&failed) > 0 {
		os.Exit(1)
	}
}

var printMu sync.Mutex

// printf は並列実行時に行が混ざらないよう排他して出力する。
func printf(stderr bool, format string, a ...any) {
	printMu.Lock()
	defer printMu.Unlock()
	if stderr {
		fmt.Fprintf(os.Stderr, format, a...)
	} else {
		fmt.Printf(format, a...)
	}
}

type target struct {
	id     string
	outDir string
}

// expand は引数を動画 ID の列に展開する。
func expand(ctx context.Context, cl *nico.Client, arg string, o *opts) ([]target, error) {
	if kind, id, ok := nico.ParseListURL(arg); ok {
		fmt.Printf("[%s/%s] 一覧取得\n", kind, id)
		l, err := cl.FetchList(ctx, kind, id)
		if err != nil {
			return nil, err
		}
		dir := o.outDir
		if o.subdir {
			name := nico.Sanitize(l.Name)
			if name == "" {
				name = string(kind) + "-" + id
			}
			dir = filepath.Join(o.outDir, name)
		}
		fmt.Printf("  %q: %d 件\n", l.Name, len(l.VideoIDs))
		ts := make([]target, 0, len(l.VideoIDs))
		for _, v := range l.VideoIDs {
			ts = append(ts, target{id: v, outDir: dir})
		}
		return ts, nil
	}
	id := nico.ExtractID(arg)
	if id == "" {
		return nil, errors.New("ID 不明")
	}
	return []target{{id: id, outDir: o.outDir}}, nil
}

func runWithRetry(ctx context.Context, cl *nico.Client, t target, o *opts) error {
	var err error
	for try := 0; try <= o.retry; try++ {
		if try > 0 {
			printf(false, "[%s] リトライ %d/%d\n", t.id, try, o.retry)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Second):
			}
		}
		err = run(ctx, cl, t, o)
		if err == nil || ctx.Err() != nil || errors.Is(err, errPermanent) || isWatchDenied(err) {
			return err
		}
		printf(true, "[%s] エラー: %v\n", t.id, err)
	}
	return err
}

var errPermanent = errors.New("永続エラー")

// existingBase は outDir 内の既存ファイルから「タイトル - [id]」部分を探す (タイトル変更に追従)。
func existingBase(dir, id string) string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	tag := " - [" + id + "]"
	for _, e := range ents {
		n := e.Name()
		for _, ext := range []string{".mp4", ".m4a", ".xml"} {
			if strings.HasSuffix(n, tag+ext) {
				return strings.TrimSuffix(n, ext)
			}
		}
	}
	return ""
}

func exists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Size() > 0
}

func run(ctx context.Context, cl *nico.Client, t target, o *opts) error {
	id := t.id
	logf := func(s string) {
		if o.jobs > 1 && !strings.HasPrefix(s, "["+id+"]") {
			printf(false, "[%s] %s\n", id, strings.TrimSpace(s))
		} else if o.jobs > 1 {
			printf(false, "%s\n", s)
		} else {
			printf(false, "%s\n", s)
		}
	}
	logf(fmt.Sprintf("[%s] watch 取得", id))
	w, err := cl.FetchWatch(ctx, id)
	if err != nil {
		return err
	}
	if w.Video.IsDeleted {
		return fmt.Errorf("削除済み動画: %w", errPermanent)
	}
	if err := os.MkdirAll(t.outDir, 0o755); err != nil {
		return err
	}
	name := existingBase(t.outDir, id)
	if name == "" {
		name = nico.BaseName(w.Video.Title, id)
	}
	base := filepath.Join(t.outDir, name)
	logf(fmt.Sprintf("[%s] %s", id, w.Video.Title))

	// *-only 指定があればそれだけ、なければ通常 (動画 + コメント + 動画情報 + サムネ)
	anyOnly := o.videoOnly || o.commentOnly || o.infoOnly || o.thumbOnly || o.audioOnly
	doVideo := o.videoOnly || o.audioOnly || !anyOnly
	doComment := (o.commentOnly || !anyOnly) && !o.noComment
	doInfo := o.infoOnly || (!anyOnly && o.thumbInfo)
	doThumb := o.thumbOnly || (!anyOnly && o.thumbImg)

	if doInfo {
		if err := nico.WriteThumbInfoXML(base+"[ThumbInfo].xml", w); err != nil {
			return err
		}
	}
	if doThumb && (o.force || !exists(base+"[ThumbImg].jpeg")) {
		if err := cl.DownloadThumbnail(ctx, w, base+"[ThumbImg].jpeg"); err != nil {
			logf("  サムネイル取得失敗 (続行): " + err.Error())
		}
	}

	if doComment {
		if err := fetchComments(ctx, cl, w, base, o, logf); err != nil {
			return fmt.Errorf("コメント: %w", err)
		}
	}

	if doVideo {
		ext := ".mp4"
		if o.audioOnly {
			ext = ".m4a"
		}
		dst := base + ext
		if !o.force && exists(dst) {
			logf("  動画は取得済み (スキップ): " + filepath.Base(dst))
			return nil
		}
		tmp := dst + ".part"
		logf("  動画取得")
		if err := cl.DownloadVideo(ctx, w, tmp, o.audioOnly, logf); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("動画: %w", err)
		}
		if err := os.Rename(tmp, dst); err != nil {
			return err
		}
		logf("  完了: " + dst)
	}
	return nil
}

// fetchComments は既存 XML があれば統合 (差分更新) して書き出す。
func fetchComments(ctx context.Context, cl *nico.Client, w *nico.Watch, base string, o *opts, logf func(string)) error {
	id := w.Video.ID
	mainPath, ownerPath := base+".xml", base+"[Owner].xml"

	var oldMain, oldOwner []nico.Comment
	if !o.force {
		var err error
		if oldMain, err = nico.ReadCommentXML(mainPath); err != nil {
			return fmt.Errorf("既存 XML 読込: %w", err)
		}
		if oldOwner, err = nico.ReadCommentXML(ownerPath); err != nil {
			return fmt.Errorf("既存 Owner XML 読込: %w", err)
		}
	}
	old := append(append([]nico.Comment{}, oldMain...), oldOwner...)

	var cms, nowList []nico.Comment
	var err error
	if o.allComments {
		stopAt := nico.MaxDate(old)
		// 既存 XML が先頭 (no<5) まで揃っていなければ、過去ログが未取得か途中で終わっているので差分にせず全量取得する
		if stopAt > 0 && !hasHead(oldMain) {
			logf(fmt.Sprintf("  既存 %d 件は過去ログが不完全 (最古 no=%d) → 全量取得", len(old), minNo(oldMain)))
			stopAt = 0
		} else if stopAt > 0 {
			logf(fmt.Sprintf("  コメント差分取得 (既存 %d 件)", len(old)))
		} else {
			logf("  コメント全量取得")
		}
		// 今コメも取り、nicoru 数などの更新を反映する
		cms, err = cl.FetchComments(ctx, w)
		if err != nil {
			return err
		}
		nowList = cms
		more, err := cl.FetchAllComments(ctx, w, o.easy, stopAt, o.wait429, func() (*nico.Watch, error) {
			return cl.FetchWatch(ctx, id)
		}, logf)
		if err != nil {
			return err
		}
		cms = nico.MergeComments(cms, more)
	} else {
		logf("  コメント取得")
		if cms, err = cl.FetchComments(ctx, w); err != nil {
			return err
		}
		nowList = cms
	}

	if err := nico.WriteNowCommentJSON(base+"[NowComment].json", nowList); err != nil {
		return err
	}
	var newMain, newOwner []nico.Comment
	for _, c := range cms {
		if c.Fork == "owner" {
			newOwner = append(newOwner, c)
		} else {
			newMain = append(newMain, c)
		}
	}
	mergedMain := nico.MergeComments(oldMain, newMain)
	mergedOwner := nico.MergeComments(oldOwner, newOwner)
	if err := nico.WriteCommentXML(mainPath, id, mergedMain); err != nil {
		return err
	}
	if len(mergedOwner) > 0 {
		if err := nico.WriteCommentXML(ownerPath, id, mergedOwner); err != nil {
			return err
		}
	}
	logf(fmt.Sprintf("  コメント %d 件 (新規 %d) / 投コメ %d",
		len(mergedMain), len(mergedMain)-len(oldMain), len(mergedOwner)))
	return nil
}

// isWatchDenied は watch API が 400/401/403/404 を返した (削除・制限) か。リトライしても無駄。
func isWatchDenied(err error) bool {
	var ae *nico.APIError
	if !errors.As(err, &ae) || !strings.Contains(ae.URL, "/api/watch/") {
		return false
	}
	switch ae.Status {
	case 400, 401, 403, 404:
		return true
	}
	return false
}

func minNo(cms []nico.Comment) int {
	m := 0
	for i, c := range cms {
		if i == 0 || c.No < m {
			m = c.No
		}
	}
	return m
}

// hasHead は FetchAllComments の終了条件 (最古 no<5) まで取得済みかを返す。
func hasHead(cms []nico.Comment) bool {
	return len(cms) > 0 && minNo(cms) < 5
}
