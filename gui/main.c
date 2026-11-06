/*
 * nico-dl-gui: nico-dl.exe を呼び出す Win32 ネイティブ GUI (C言語)。
 *
 * 同じフォルダの nico-dl.exe を子プロセスとして起動し、標準出力をログ欄に流す。
 * 設定は exe と同じフォルダの nico-dl-gui.ini に保存する (user_session は保存しない)。
 */
#ifndef UNICODE
#define UNICODE
#endif
#ifndef _UNICODE
#define _UNICODE
#endif
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <shlobj.h>
#include <commdlg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <wchar.h>

#define WM_APP_LOG  (WM_APP + 1) /* lParam: wchar_t* (malloc 済み、受信側で free) */
#define WM_APP_DONE (WM_APP + 2) /* wParam: 終了コード */

enum {
    ID_URLS = 100, ID_OUTDIR, ID_OUTDIR_BTN, ID_BROWSER, ID_PROFILE, ID_COOKIES, ID_COOKIES_BTN,
    ID_SESSION, ID_SLEEP, ID_VIDEO_ONLY, ID_COMMENT_ONLY, ID_INFO_ONLY, ID_THUMB_ONLY, ID_NO_COMMENT, ID_ALL_COMMENTS, ID_EASY,
    ID_AUDIO_ONLY, ID_FORCE, ID_JOBS, ID_LIMIT, ID_MAXH, ID_WHOAMI, ID_START, ID_STOP, ID_LOG
};

#define NCHECK 9
static const struct { int id; const wchar_t *label; const wchar_t *key; const wchar_t *flag; } checks[NCHECK] = {
    { ID_VIDEO_ONLY,   L"動画のみ",               L"video_only",   L"-video-only" },
    { ID_COMMENT_ONLY, L"コメントのみ",           L"comment_only", L"-comment-only" },
    { ID_INFO_ONLY,    L"動画情報のみ",           L"info_only",    L"-info-only" },
    { ID_THUMB_ONLY,   L"サムネのみ",             L"thumb_only",   L"-thumb-only" },
    { ID_AUDIO_ONLY,   L"音声のみ (.m4a)",        L"audio_only",   L"-audio-only" },
    { ID_NO_COMMENT,   L"コメントを取得しない",   L"no_comment",   L"-no-comment" },
    { ID_ALL_COMMENTS, L"過去ログ全量・差分",     L"all_comments", L"-all-comments" },
    { ID_EASY,         L"かんたんコメントも",     L"easy",         L"-easy" },
    { ID_FORCE,        L"取得済みも取り直す",     L"force",        L"-force" },
};

static const wchar_t *browsers[] = { L"使わない", L"Firefox" };
static const wchar_t *browserArg[] = { NULL, L"firefox" };

static HINSTANCE g_inst;
static HWND g_wnd;
static HFONT g_font;
static wchar_t g_dir[MAX_PATH];     /* exe のあるフォルダ */
static wchar_t g_ini[MAX_PATH];
static HANDLE g_job, g_proc, g_thread;
static volatile LONG g_running;

static HWND ctl(int id) { return GetDlgItem(g_wnd, id); }

/* ---------- 文字列ユーティリティ ---------- */

static void get_text(int id, wchar_t *buf, int cap)
{
    GetWindowTextW(ctl(id), buf, cap);
}

static void trim(wchar_t *s)
{
    size_t n = wcslen(s), i = 0;
    while (n > 0 && iswspace(s[n - 1])) s[--n] = 0;
    while (s[i] && iswspace(s[i])) i++;
    if (i) memmove(s, s + i, (wcslen(s + i) + 1) * sizeof(wchar_t));
}

/* CommandLineToArgvW 規則で引数を引用符付きで追記する */
static int append_arg(wchar_t *cmd, size_t cap, const wchar_t *arg)
{
    size_t len = wcslen(cmd);
    wchar_t *p = cmd + len;
    size_t need = wcslen(arg) * 2 + 4;
    if (len + need >= cap) return 0;
    *p++ = L' ';
    *p++ = L'"';
    for (const wchar_t *a = arg;; a++) {
        size_t bs = 0;
        while (*a == L'\\') { a++; bs++; }
        if (*a == 0) {
            for (size_t i = 0; i < bs * 2; i++) *p++ = L'\\';
            break;
        } else if (*a == L'"') {
            for (size_t i = 0; i < bs * 2 + 1; i++) *p++ = L'\\';
            *p++ = L'"';
        } else {
            for (size_t i = 0; i < bs; i++) *p++ = L'\\';
            *p++ = *a;
        }
    }
    *p++ = L'"';
    *p = 0;
    return 1;
}

static void log_append(const wchar_t *s)
{
    HWND e = ctl(ID_LOG);
    SendMessageW(e, EM_SETSEL, (WPARAM)-1, (LPARAM)-1);
    SendMessageW(e, EM_REPLACESEL, FALSE, (LPARAM)s);
    SendMessageW(e, EM_SCROLLCARET, 0, 0);
}

/* UTF-8 → UTF-16。LF は CRLF にする (EDIT コントロール用)。malloc して返す */
static wchar_t *utf8_to_edit(const char *src, int n)
{
    int w = MultiByteToWideChar(CP_UTF8, 0, src, n, NULL, 0);
    if (w <= 0) return NULL;
    wchar_t *tmp = (wchar_t *)malloc((w + 1) * sizeof(wchar_t));
    if (!tmp) return NULL;
    MultiByteToWideChar(CP_UTF8, 0, src, n, tmp, w);
    tmp[w] = 0;
    int lf = 0;
    for (int i = 0; i < w; i++) if (tmp[i] == L'\n' && (i == 0 || tmp[i - 1] != L'\r')) lf++;
    wchar_t *out = (wchar_t *)malloc((w + lf + 1) * sizeof(wchar_t));
    if (!out) { free(tmp); return NULL; }
    int j = 0;
    for (int i = 0; i < w; i++) {
        if (tmp[i] == L'\r') continue;
        if (tmp[i] == L'\n') out[j++] = L'\r';
        out[j++] = tmp[i];
    }
    out[j] = 0;
    free(tmp);
    return out;
}

/* ---------- 設定の保存・復元 ---------- */

static void ini_get(const wchar_t *key, const wchar_t *def, wchar_t *buf, DWORD cap)
{
    GetPrivateProfileStringW(L"gui", key, def, buf, cap, g_ini);
}

static void ini_put(const wchar_t *key, const wchar_t *val)
{
    WritePrivateProfileStringW(L"gui", key, val, g_ini);
}

/* INI を UTF-16 LE (BOM 付き) で作る。BOM があれば WritePrivateProfile* が Unicode のまま読み書きし、
 * 日本語パスがロケールに依らず保存される */
static void ensure_unicode_ini(void)
{
    if (GetFileAttributesW(g_ini) != INVALID_FILE_ATTRIBUTES) return;
    HANDLE f = CreateFileW(g_ini, GENERIC_WRITE, 0, NULL, CREATE_NEW, FILE_ATTRIBUTE_NORMAL, NULL);
    if (f == INVALID_HANDLE_VALUE) return;
    const unsigned char bom[2] = { 0xFF, 0xFE };
    DWORD n;
    WriteFile(f, bom, 2, &n, NULL);
    CloseHandle(f);
}

static void load_settings(void)
{
    ensure_unicode_ini();
    wchar_t buf[MAX_PATH * 2], def[MAX_PATH * 2];
    swprintf(def, MAX_PATH * 2, L"%ls\\downloads", g_dir);
    ini_get(L"outdir", def, buf, MAX_PATH * 2);
    SetWindowTextW(ctl(ID_OUTDIR), buf);
    ini_get(L"profile", L"", buf, MAX_PATH);
    SetWindowTextW(ctl(ID_PROFILE), buf);
    ini_get(L"cookies", L"", buf, MAX_PATH * 2);
    SetWindowTextW(ctl(ID_COOKIES), buf);
    ini_get(L"sleep", L"0", buf, 16);
    SetWindowTextW(ctl(ID_SLEEP), buf);
    ini_get(L"jobs", L"1", buf, 16);
    SetWindowTextW(ctl(ID_JOBS), buf);
    ini_get(L"limit", L"0", buf, 16);
    SetWindowTextW(ctl(ID_LIMIT), buf);
    ini_get(L"maxh", L"0", buf, 16);
    SetWindowTextW(ctl(ID_MAXH), buf);
    int br = GetPrivateProfileIntW(L"gui", L"browser2", -1, g_ini);
    if (br < 0) br = GetPrivateProfileIntW(L"gui", L"browser", 0, g_ini) == 3 ? 1 : 0; /* 旧形式 (Chrome/Edge/Firefox 選択) からの移行 */
    SendMessageW(ctl(ID_BROWSER), CB_SETCURSEL, br, 0);
    for (int i = 0; i < NCHECK; i++)
        CheckDlgButton(g_wnd, checks[i].id, GetPrivateProfileIntW(L"gui", checks[i].key, 0, g_ini) ? BST_CHECKED : BST_UNCHECKED);
}

static void save_settings(void)
{
    wchar_t buf[MAX_PATH * 2];
    get_text(ID_OUTDIR, buf, MAX_PATH * 2);     ini_put(L"outdir", buf);
    get_text(ID_PROFILE, buf, MAX_PATH);        ini_put(L"profile", buf);
    get_text(ID_COOKIES, buf, MAX_PATH * 2);    ini_put(L"cookies", buf);
    get_text(ID_SLEEP, buf, 16);                ini_put(L"sleep", buf);
    get_text(ID_JOBS, buf, 16);                 ini_put(L"jobs", buf);
    get_text(ID_LIMIT, buf, 16);                ini_put(L"limit", buf);
    get_text(ID_MAXH, buf, 16);                 ini_put(L"maxh", buf);
    swprintf(buf, 16, L"%d", (int)SendMessageW(ctl(ID_BROWSER), CB_GETCURSEL, 0, 0));
    ini_put(L"browser2", buf);
    for (int i = 0; i < NCHECK; i++)
        ini_put(checks[i].key, IsDlgButtonChecked(g_wnd, checks[i].id) == BST_CHECKED ? L"1" : L"0");
}

/* ---------- 子プロセス実行 ---------- */

static DWORD WINAPI reader_thread(LPVOID param)
{
    HANDLE rd = (HANDLE)param;
    char *buf = NULL;
    size_t len = 0, cap = 0;
    char chunk[4096];
    DWORD got;

    for (;;) {
        BOOL ok = ReadFile(rd, chunk, sizeof chunk, &got, NULL);
        if (ok && got > 0) {
            if (len + got + 1 > cap) {
                cap = (len + got + 1) * 2;
                buf = (char *)realloc(buf, cap);
                if (!buf) break;
            }
            memcpy(buf + len, chunk, got);
            len += got;
        }
        /* UTF-8 の分断を避けるため、改行までを送り、端数は次回に回す */
        size_t cut = 0;
        for (size_t i = len; i > 0; i--) if (buf && buf[i - 1] == '\n') { cut = i; break; }
        if (!ok || got == 0) cut = len;
        if (cut > 0) {
            wchar_t *w = utf8_to_edit(buf, (int)cut);
            if (w) PostMessageW(g_wnd, WM_APP_LOG, 0, (LPARAM)w);
            memmove(buf, buf + cut, len - cut);
            len -= cut;
        }
        if (!ok || got == 0) break;
    }
    free(buf);
    CloseHandle(rd);

    WaitForSingleObject(g_proc, INFINITE);
    DWORD code = 0;
    GetExitCodeProcess(g_proc, &code);
    PostMessageW(g_wnd, WM_APP_DONE, (WPARAM)code, 0);
    return 0;
}

static void set_running(int running)
{
    InterlockedExchange(&g_running, running);
    EnableWindow(ctl(ID_START), !running);
    EnableWindow(ctl(ID_STOP), running);
}

static void start_download(int whoami)
{
    wchar_t urls[16384], outdir[MAX_PATH * 2], tmp[MAX_PATH * 2];
    wchar_t *cmd = (wchar_t *)calloc(32768, sizeof(wchar_t));
    if (!cmd) return;

    get_text(ID_URLS, urls, 16384);
    int nurl = 0;
    for (wchar_t *p = urls; *p; p++) if (!iswspace(*p)) { nurl = 1; break; }
    if (!nurl && !whoami) {
        MessageBoxW(g_wnd, L"動画ID または URL を入力してください。", L"nico-dl", MB_ICONINFORMATION);
        free(cmd);
        return;
    }
    if (!whoami && IsDlgButtonChecked(g_wnd, ID_COMMENT_ONLY) == BST_CHECKED && IsDlgButtonChecked(g_wnd, ID_NO_COMMENT) == BST_CHECKED) {
        MessageBoxW(g_wnd, L"「コメントのみ」と「コメントを取得しない」は同時に指定できません。", L"nico-dl", MB_ICONWARNING);
        free(cmd);
        return;
    }

    wchar_t exe[MAX_PATH * 2];
    swprintf(exe, MAX_PATH * 2, L"%ls\\nico-dl.exe", g_dir);
    if (GetFileAttributesW(exe) == INVALID_FILE_ATTRIBUTES) {
        swprintf(tmp, MAX_PATH * 2, L"nico-dl.exe が見つかりません:\n%ls", exe);
        MessageBoxW(g_wnd, tmp, L"nico-dl", MB_ICONERROR);
        free(cmd);
        return;
    }

    save_settings();

    cmd[0] = 0;
    append_arg(cmd, 32768, exe);
    /* append_arg は先頭に空白を入れるため、実行ファイル名の前の空白を詰める */
    memmove(cmd, cmd + 1, wcslen(cmd) * sizeof(wchar_t));

    get_text(ID_OUTDIR, outdir, MAX_PATH * 2); trim(outdir);
    if (outdir[0]) { append_arg(cmd, 32768, L"-o"); append_arg(cmd, 32768, outdir); }

    int b = (int)SendMessageW(ctl(ID_BROWSER), CB_GETCURSEL, 0, 0);
    if (b > 0 && b < 2) {
        append_arg(cmd, 32768, L"-browser"); append_arg(cmd, 32768, browserArg[b]);
        get_text(ID_PROFILE, tmp, MAX_PATH); trim(tmp);
        if (tmp[0]) { append_arg(cmd, 32768, L"-browser-profile"); append_arg(cmd, 32768, tmp); }
    }
    get_text(ID_COOKIES, tmp, MAX_PATH * 2); trim(tmp);
    if (tmp[0]) { append_arg(cmd, 32768, L"-cookies"); append_arg(cmd, 32768, tmp); }

    for (int i = 0; i < NCHECK; i++)
        if (IsDlgButtonChecked(g_wnd, checks[i].id) == BST_CHECKED) append_arg(cmd, 32768, checks[i].flag);

    get_text(ID_SLEEP, tmp, 16); trim(tmp);
    if (tmp[0] && wcscmp(tmp, L"0") != 0) {
        wchar_t d[32];
        swprintf(d, 32, L"%ls" L"s", tmp);
        append_arg(cmd, 32768, L"-sleep"); append_arg(cmd, 32768, d);
    }

    {
        static const struct { int id; const wchar_t *flag; } nums[] = {
            { ID_JOBS, L"-jobs" }, { ID_LIMIT, L"-limit-mbps" }, { ID_MAXH, L"-max-height" }
        };
        for (int i = 0; i < 3; i++) {
            get_text(nums[i].id, tmp, 16); trim(tmp);
            if (tmp[0] && wcscmp(tmp, L"0") != 0 && !(nums[i].id == ID_JOBS && wcscmp(tmp, L"1") == 0)) {
                append_arg(cmd, 32768, nums[i].flag); append_arg(cmd, 32768, tmp);
            }
        }
    }
    if (whoami) append_arg(cmd, 32768, L"-whoami");

    /* user_session は引数に出さず環境変数で渡す (子プロセスが継承) */
    get_text(ID_SESSION, tmp, MAX_PATH * 2); trim(tmp);
    SetEnvironmentVariableW(L"NICO_USER_SESSION", tmp[0] ? tmp : NULL);

    /* URL / ID を空白・改行区切りで引数に追加 (ログイン確認時は不要) */
    for (wchar_t *p = urls; *p && !whoami;) {
        while (*p && iswspace(*p)) p++;
        wchar_t *s = p;
        while (*p && !iswspace(*p)) p++;
        if (p > s) {
            wchar_t save = *p;
            *p = 0;
            if (!append_arg(cmd, 32768, s)) {
                MessageBoxW(g_wnd, L"入力が長すぎます。", L"nico-dl", MB_ICONERROR);
                free(cmd);
                return;
            }
            *p = save;
        }
    }

    SECURITY_ATTRIBUTES sa = { sizeof sa, NULL, TRUE };
    HANDLE rd = NULL, wr = NULL;
    if (!CreatePipe(&rd, &wr, &sa, 0)) {
        MessageBoxW(g_wnd, L"パイプを作成できません。", L"nico-dl", MB_ICONERROR);
        free(cmd);
        return;
    }
    SetHandleInformation(rd, HANDLE_FLAG_INHERIT, 0);

    STARTUPINFOW si;
    PROCESS_INFORMATION pi;
    memset(&si, 0, sizeof si);
    si.cb = sizeof si;
    si.dwFlags = STARTF_USESTDHANDLES;
    si.hStdOutput = wr;
    si.hStdError = wr;
    si.hStdInput = GetStdHandle(STD_INPUT_HANDLE);
    HANDLE nul = CreateFileW(L"NUL", GENERIC_READ, FILE_SHARE_READ | FILE_SHARE_WRITE, &sa, OPEN_EXISTING, 0, NULL);
    if (nul != INVALID_HANDLE_VALUE) si.hStdInput = nul;

    if (!CreateProcessW(NULL, cmd, NULL, NULL, TRUE, CREATE_NO_WINDOW | CREATE_SUSPENDED | CREATE_UNICODE_ENVIRONMENT,
                        NULL, g_dir, &si, &pi)) {
        swprintf(tmp, MAX_PATH * 2, L"nico-dl.exe を起動できません (エラー %lu)", GetLastError());
        MessageBoxW(g_wnd, tmp, L"nico-dl", MB_ICONERROR);
        CloseHandle(rd); CloseHandle(wr);
        if (nul != INVALID_HANDLE_VALUE) CloseHandle(nul);
        SetEnvironmentVariableW(L"NICO_USER_SESSION", NULL);
        free(cmd);
        return;
    }
    CloseHandle(wr);
    if (nul != INVALID_HANDLE_VALUE) CloseHandle(nul);
    SetEnvironmentVariableW(L"NICO_USER_SESSION", NULL);

    /* ジョブに入れて、中止・終了時に 子孫プロセスごと止める */
    g_job = CreateJobObjectW(NULL, NULL);
    if (g_job) {
        JOBOBJECT_EXTENDED_LIMIT_INFORMATION li;
        memset(&li, 0, sizeof li);
        li.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
        SetInformationJobObject(g_job, JobObjectExtendedLimitInformation, &li, sizeof li);
        AssignProcessToJobObject(g_job, pi.hProcess);
    }
    g_proc = pi.hProcess;
    ResumeThread(pi.hThread);
    CloseHandle(pi.hThread);

    SetWindowTextW(ctl(ID_LOG), L"");
    log_append(L"> 開始\r\n");
    set_running(1);
    g_thread = CreateThread(NULL, 0, reader_thread, rd, 0, NULL);
    free(cmd);
}

static void stop_download(void)
{
    if (g_job) TerminateJobObject(g_job, 1);
    else if (g_proc) TerminateProcess(g_proc, 1);
}

static void cleanup_process(void)
{
    if (g_thread) { WaitForSingleObject(g_thread, 2000); CloseHandle(g_thread); g_thread = NULL; }
    if (g_proc) { CloseHandle(g_proc); g_proc = NULL; }
    if (g_job) { CloseHandle(g_job); g_job = NULL; }
}

/* ---------- ダイアログ ---------- */

static void pick_folder(void)
{
    wchar_t cur[MAX_PATH * 2], path[MAX_PATH];
    get_text(ID_OUTDIR, cur, MAX_PATH * 2);
    BROWSEINFOW bi;
    memset(&bi, 0, sizeof bi);
    bi.hwndOwner = g_wnd;
    bi.lpszTitle = L"保存先フォルダを選択";
    bi.ulFlags = BIF_RETURNONLYFSDIRS | BIF_NEWDIALOGSTYLE;
    LPITEMIDLIST pidl = SHBrowseForFolderW(&bi);
    if (pidl) {
        if (SHGetPathFromIDListW(pidl, path)) SetWindowTextW(ctl(ID_OUTDIR), path);
        CoTaskMemFree(pidl);
    }
}

static void pick_cookies(void)
{
    wchar_t path[MAX_PATH * 2] = L"";
    OPENFILENAMEW ofn;
    memset(&ofn, 0, sizeof ofn);
    ofn.lStructSize = sizeof ofn;
    ofn.hwndOwner = g_wnd;
    ofn.lpstrFilter = L"cookies.txt (Netscape 形式)\0*.txt\0すべてのファイル\0*.*\0";
    ofn.lpstrFile = path;
    ofn.nMaxFile = MAX_PATH * 2;
    ofn.Flags = OFN_FILEMUSTEXIST | OFN_PATHMUSTEXIST | OFN_HIDEREADONLY;
    if (GetOpenFileNameW(&ofn)) SetWindowTextW(ctl(ID_COOKIES), path);
}

/* ---------- ウィンドウ ---------- */

static HWND mk(const wchar_t *cls, const wchar_t *text, DWORD style, DWORD ex, int id)
{
    HWND h = CreateWindowExW(ex, cls, text, WS_CHILD | WS_VISIBLE | style, 0, 0, 10, 10, g_wnd,
                             (HMENU)(INT_PTR)id, g_inst, NULL);
    SendMessageW(h, WM_SETFONT, (WPARAM)g_font, TRUE);
    return h;
}

static HWND label(const wchar_t *text, int id)
{
    return mk(L"STATIC", text, SS_LEFT, 0, id);
}

enum { LBL_URLS = 200, LBL_OUT, LBL_BROWSER, LBL_PROFILE, LBL_COOKIES, LBL_SESSION, LBL_SLEEP, LBL_LOG,
       LBL_JOBS, LBL_LIMIT, LBL_MAXH };

static void create_controls(void)
{
    label(L"動画ID / URL (1行に1つ。マイリスト・投稿者・シリーズの URL も可)", LBL_URLS);
    mk(L"EDIT", L"", ES_MULTILINE | ES_AUTOVSCROLL | ES_WANTRETURN | WS_VSCROLL | WS_TABSTOP, WS_EX_CLIENTEDGE, ID_URLS);

    label(L"保存先:", LBL_OUT);
    mk(L"EDIT", L"", ES_AUTOHSCROLL | WS_TABSTOP, WS_EX_CLIENTEDGE, ID_OUTDIR);
    mk(L"BUTTON", L"参照...", BS_PUSHBUTTON | WS_TABSTOP, 0, ID_OUTDIR_BTN);

    label(L"ブラウザの cookie:", LBL_BROWSER);
    HWND cb = mk(L"COMBOBOX", L"", CBS_DROPDOWNLIST | WS_VSCROLL | WS_TABSTOP, 0, ID_BROWSER);
    for (int i = 0; i < 2; i++) SendMessageW(cb, CB_ADDSTRING, 0, (LPARAM)browsers[i]);
    SendMessageW(cb, CB_SETCURSEL, 0, 0);
    label(L"プロファイル:", LBL_PROFILE);
    mk(L"EDIT", L"", ES_AUTOHSCROLL | WS_TABSTOP, WS_EX_CLIENTEDGE, ID_PROFILE);

    label(L"cookies.txt:", LBL_COOKIES);
    mk(L"EDIT", L"", ES_AUTOHSCROLL | WS_TABSTOP, WS_EX_CLIENTEDGE, ID_COOKIES);
    mk(L"BUTTON", L"参照...", BS_PUSHBUTTON | WS_TABSTOP, 0, ID_COOKIES_BTN);

    label(L"user_session:", LBL_SESSION);
    mk(L"EDIT", L"", ES_AUTOHSCROLL | ES_PASSWORD | WS_TABSTOP, WS_EX_CLIENTEDGE, ID_SESSION);

    for (int i = 0; i < NCHECK; i++)
        mk(L"BUTTON", checks[i].label, BS_AUTOCHECKBOX | WS_TABSTOP, 0, checks[i].id);

    label(L"待機(秒/本):", LBL_SLEEP);
    mk(L"EDIT", L"0", ES_AUTOHSCROLL | ES_NUMBER | WS_TABSTOP, WS_EX_CLIENTEDGE, ID_SLEEP);
    label(L"同時DL数:", LBL_JOBS);
    mk(L"EDIT", L"1", ES_AUTOHSCROLL | ES_NUMBER | WS_TABSTOP, WS_EX_CLIENTEDGE, ID_JOBS);
    label(L"速度上限(Mbps):", LBL_LIMIT);
    mk(L"EDIT", L"0", ES_AUTOHSCROLL | ES_NUMBER | WS_TABSTOP, WS_EX_CLIENTEDGE, ID_LIMIT);
    label(L"最大画質(高さpx):", LBL_MAXH);
    mk(L"EDIT", L"0", ES_AUTOHSCROLL | ES_NUMBER | WS_TABSTOP, WS_EX_CLIENTEDGE, ID_MAXH);
    mk(L"BUTTON", L"ログイン確認", BS_PUSHBUTTON | WS_TABSTOP, 0, ID_WHOAMI);
    mk(L"BUTTON", L"ダウンロード開始", BS_DEFPUSHBUTTON | WS_TABSTOP, 0, ID_START);
    mk(L"BUTTON", L"中止", BS_PUSHBUTTON | WS_TABSTOP, 0, ID_STOP);
    EnableWindow(ctl(ID_STOP), FALSE);

    label(L"ログ:", LBL_LOG);
    HWND log = mk(L"EDIT", L"", ES_MULTILINE | ES_AUTOVSCROLL | ES_READONLY | WS_VSCROLL | WS_HSCROLL | WS_TABSTOP,
                  WS_EX_CLIENTEDGE, ID_LOG);
    SendMessageW(log, EM_SETLIMITTEXT, 8 * 1024 * 1024, 0);
}

#define M 8
static void place(int id, int x, int y, int w, int h)
{
    MoveWindow(ctl(id), x, y, w, h, TRUE);
}

static void layout(void)
{
    RECT rc;
    GetClientRect(g_wnd, &rc);
    int W = rc.right, H = rc.bottom, y = M;
    const int LH = 16, EH = 22, BW = 76;

    place(LBL_URLS, M, y, W - 2 * M, LH); y += LH + 2;
    place(ID_URLS, M, y, W - 2 * M, 80); y += 80 + 6;

    place(LBL_OUT, M, y + 3, 88, LH);
    place(ID_OUTDIR, M + 92, y, W - 2 * M - 92 - BW - 4, EH);
    place(ID_OUTDIR_BTN, W - M - BW, y, BW, EH); y += EH + 4;

    int half = (W - 2 * M) / 2;
    place(LBL_BROWSER, M, y + 3, 88, LH);
    place(ID_BROWSER, M + 92, y, half - 92 - 8, 200);
    place(LBL_PROFILE, M + half, y + 3, 70, LH);
    place(ID_PROFILE, M + half + 74, y, W - 2 * M - half - 74, EH); y += EH + 4;

    place(LBL_COOKIES, M, y + 3, 88, LH);
    place(ID_COOKIES, M + 92, y, W - 2 * M - 92 - BW - 4, EH);
    place(ID_COOKIES_BTN, W - M - BW, y, BW, EH); y += EH + 4;

    place(LBL_SESSION, M, y + 3, 88, LH);
    place(ID_SESSION, M + 92, y, W - 2 * M - 92, EH); y += EH + 6;

    int cw = (W - 2 * M) / 3;
    for (int i = 0; i < NCHECK; i++)
        place(checks[i].id, M + (i % 3) * cw, y + (i / 3) * 22, cw - 4, 20);
    y += 3 * 22 + 6;

    {
        int x = M;
        place(LBL_JOBS, x, y + 3, 64, LH);       place(ID_JOBS, x + 66, y, 36, EH);  x += 112;
        place(LBL_LIMIT, x, y + 3, 100, LH);     place(ID_LIMIT, x + 102, y, 40, EH); x += 152;
        place(LBL_MAXH, x, y + 3, 110, LH);      place(ID_MAXH, x + 112, y, 46, EH);  x += 168;
        place(LBL_SLEEP, x, y + 3, 110, LH);     place(ID_SLEEP, x + 112, y, 40, EH);
    }
    y += EH + 6;

    place(ID_WHOAMI, M, y - 2, 100, 26);
    place(ID_STOP, W - M - 70, y - 2, 70, 26);
    place(ID_START, W - M - 70 - 6 - 130, y - 2, 130, 26); y += 30;

    place(LBL_LOG, M, y, 60, LH); y += LH + 2;
    int lh = H - y - M;
    if (lh < 40) lh = 40;
    place(ID_LOG, M, y, W - 2 * M, lh);
}

static LRESULT CALLBACK wnd_proc(HWND h, UINT msg, WPARAM wp, LPARAM lp)
{
    switch (msg) {
    case WM_CREATE:
        g_wnd = h;
        create_controls();
        load_settings();
        layout();
        return 0;
    case WM_SIZE:
        layout();
        return 0;
    case WM_GETMINMAXINFO: {
        MINMAXINFO *mm = (MINMAXINFO *)lp;
        mm->ptMinTrackSize.x = 700;
        mm->ptMinTrackSize.y = 500;
        return 0;
    }
    case WM_COMMAND:
        switch (LOWORD(wp)) {
        case ID_START: if (!g_running) start_download(0); break;
        case ID_WHOAMI: if (!g_running) start_download(1); break;
        case ID_STOP: if (g_running) stop_download(); break;
        case ID_OUTDIR_BTN: pick_folder(); break;
        case ID_COOKIES_BTN: pick_cookies(); break;
        }
        return 0;
    case WM_APP_LOG: {
        wchar_t *s = (wchar_t *)lp;
        log_append(s);
        free(s);
        return 0;
    }
    case WM_APP_DONE: {
        wchar_t m[64];
        cleanup_process();
        swprintf(m, 64, L"> 終了 (コード %d)\r\n", (int)wp);
        log_append(m);
        set_running(0);
        return 0;
    }
    case WM_CLOSE:
        if (g_running) {
            if (MessageBoxW(h, L"ダウンロード中です。中止して終了しますか?", L"nico-dl", MB_YESNO | MB_ICONQUESTION) != IDYES)
                return 0;
            stop_download();
            if (g_thread) WaitForSingleObject(g_thread, 3000);
        }
        save_settings();
        DestroyWindow(h);
        return 0;
    case WM_DESTROY:
        PostQuitMessage(0);
        return 0;
    }
    return DefWindowProcW(h, msg, wp, lp);
}

int WINAPI wWinMain(HINSTANCE inst, HINSTANCE prev, PWSTR cmdline, int show)
{
    (void)prev; (void)cmdline;
    g_inst = inst;
    OleInitialize(NULL);

    GetModuleFileNameW(NULL, g_dir, MAX_PATH);
    wchar_t *sl = wcsrchr(g_dir, L'\\');
    if (sl) *sl = 0;
    swprintf(g_ini, MAX_PATH, L"%ls\\nico-dl-gui.ini", g_dir);

    NONCLIENTMETRICSW ncm;
    memset(&ncm, 0, sizeof ncm);
    ncm.cbSize = sizeof ncm;
    if (SystemParametersInfoW(SPI_GETNONCLIENTMETRICS, sizeof ncm, &ncm, 0))
        g_font = CreateFontIndirectW(&ncm.lfMessageFont);
    if (!g_font) g_font = (HFONT)GetStockObject(DEFAULT_GUI_FONT);

    WNDCLASSW wc;
    memset(&wc, 0, sizeof wc);
    wc.lpfnWndProc = wnd_proc;
    wc.hInstance = inst;
    wc.hCursor = LoadCursorW(NULL, IDC_ARROW);
    wc.hbrBackground = (HBRUSH)(COLOR_BTNFACE + 1);
    wc.lpszClassName = L"NicoDlGui";
    wc.hIcon = LoadIconW(NULL, IDI_APPLICATION);
    RegisterClassW(&wc);

    HWND w = CreateWindowExW(0, wc.lpszClassName, L"nico-dl - ニコニコ動画ダウンローダ", WS_OVERLAPPEDWINDOW,
                             CW_USEDEFAULT, CW_USEDEFAULT, 720, 640, NULL, NULL, inst, NULL);
    if (!w) return 1;
    ShowWindow(w, show);
    UpdateWindow(w);

    MSG m;
    while (GetMessageW(&m, NULL, 0, 0) > 0) {
        if (!IsDialogMessageW(w, &m)) {
            TranslateMessage(&m);
            DispatchMessageW(&m);
        }
    }
    OleUninitialize();
    return (int)m.wParam;
}
