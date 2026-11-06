# nico-dl

ニコニコ動画の動画とコメントをダウンロードする Windows 向けツールです。
CLI (`nico-dl.exe`) と、C 言語で書いた Win32 の GUI (`nico-dl-gui.exe`) があります。

## 必要なもの

- 会員限定動画・過去ログ・高画質には、ニコニコへのログインが必要です。

## ビルド

```sh
./build.sh      # dist/ に nico-dl.exe と nico-dl-gui.exe を作る (Go と x86_64-w64-mingw32-gcc が必要)
```

## GUI

`nico-dl-gui.exe` を `nico-dl.exe` と同じフォルダに置いて起動します。
動画 ID / URL (マイリスト・投稿者・シリーズの URL も可) を入力して「ダウンロード開始」を押します。
「ログイン確認」で、設定した cookie でログインできているかを確認できます。
設定は同じフォルダの `nico-dl-gui.ini` に保存されます (`user_session` は保存しません)。

## CLI

```sh
nico-dl -o 保存先 sm9                      # 動画 + コメント + ThumbInfo + サムネイル
nico-dl -video-only sm9                    # 動画 (.mp4) のみ
nico-dl -comment-only sm9                  # コメントのみ
nico-dl -info-only sm9                     # 動画情報 ([ThumbInfo].xml) のみ
nico-dl -thumb-only sm9                    # サムネイルのみ
nico-dl -all-comments -easy sm9            # 過去ログ全量 (既存 XML があれば差分のみ)
nico-dl -audio-only sm9                    # 音声 (.m4a) のみ
nico-dl mylist/2973737 user/12345 series/678   # 一覧を展開して一括 DL
nico-dl -jobs 2 -max-height 720 -limit-mbps 10 -sleep 3s mylist/2973737
nico-dl -whoami -browser firefox           # ログイン状態の確認
```

`-video-only` / `-comment-only` / `-info-only` / `-thumb-only` / `-audio-only` は、指定したものだけを取得します (複数指定は和集合。`-video-only` と `-audio-only` は同時指定不可)。
指定なしは動画 + コメント + 動画情報 + サムネイルです。取得済みの動画は `-force` で取り直せます。

主なオプションは `nico-dl -h` を参照してください。

### ログイン (cookie の渡し方)

| 方法 | 指定 |
|---|---|
| ブラウザから取得 | `-browser firefox` (`-browser-profile`、`-browser-dir` で場所を指定) |
| cookies.txt (Netscape 形式) | `-cookies cookies.txt` |
| user_session の値 | `-session 値` または環境変数 `NICO_USER_SESSION` |

ブラウザからの取得は Firefox のみ対応です。Chrome / Edge の新しい版 (v20 = app-bound encryption) の cookie は、他のプログラムからは読めない仕様のため対応していません。
Chrome / Edge を使っている場合は、拡張機能で書き出した cookies.txt か、`user_session` の値を使ってください。

## 出力ファイル (NNDD-RE 互換)

- `タイトル - [sm9].mp4` / `.m4a`
- `タイトル - [sm9].xml` (コメント)、`タイトル - [sm9][Owner].xml` (投稿者コメント)
- `タイトル - [sm9][ThumbInfo].xml`、`[ThumbImg].jpeg`、`[NowComment].json`

コメント XML の `fork` は数値 (owner=1、easy=2、main は省略) で書き出します。

## 既知の制限

- 過去ログ (`-all-comments`) はゲストだと拒否されることがあります (要ログイン)。
- 旧方式 (DMC) の配信、生放送、ランキング、検索は対象外です。
- 暗号化は AES-128 のみ対応です。
