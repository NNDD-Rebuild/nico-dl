package nico

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExtractID(t *testing.T) {
	cases := map[string]string{
		"sm9":                                   "sm9",
		"https://www.nicovideo.jp/watch/sm9":    "sm9",
		"https://nico.ms/so12345?ref=x":         "so12345",
		"https://www.nicovideo.jp/watch/nm1234": "nm1234",
		"123456":                                "123456",
		"":                                      "",
	}
	for in, want := range cases {
		if got := ExtractID(in); got != want {
			t.Errorf("ExtractID(%q)=%q want %q", in, got, want)
		}
	}
}

func TestParseListURL(t *testing.T) {
	for in, want := range map[string][2]string{
		"mylist/123":                               {"mylist", "123"},
		"https://www.nicovideo.jp/user/456/video":  {"user", "456"},
		"https://www.nicovideo.jp/series/789":      {"series", "789"},
		"https://www.nicovideo.jp/user/1/mylist/9": {"user", "1"},
	} {
		k, id, ok := ParseListURL(in)
		if !ok || string(k) != want[0] || id != want[1] {
			t.Errorf("ParseListURL(%q)=%v,%v,%v", in, k, id, ok)
		}
	}
	if _, _, ok := ParseListURL("sm9"); ok {
		t.Error("動画 ID は一覧ではない")
	}
}

func TestSanitize(t *testing.T) {
	if got := Sanitize(`a/b\c:d*e?f"g<h>i|j`); got != "a_b_c_d_e_f_g_h_i_j" {
		t.Errorf("got %q", got)
	}
	if got := Sanitize("  末尾ドット...  "); got != "末尾ドット" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("あ", 150)
	if n := len([]rune(Sanitize(long))); n != 100 {
		t.Errorf("rune 数 %d", n)
	}
	if got := BaseName("", "sm9"); got != "sm9 - [sm9]" {
		t.Errorf("got %q", got)
	}
}

func TestParseMasterAndVariant(t *testing.T) {
	master := `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio-aac-192kbps",NAME="Main",URI="audio/a.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=1000000,RESOLUTION=640x360,AUDIO="audio-aac-192kbps"
video/low.m3u8?x=1
#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1280x720,AUDIO="audio-aac-192kbps"
video/high.m3u8?x=1
`
	m, err := parseMaster("https://d.example.com/hls/master.m3u8?s=1", master)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Streams) != 2 || m.Streams[0].Bandwidth != 3000000 {
		t.Fatalf("帯域降順になっていない: %+v", m.Streams)
	}
	if m.Streams[0].URL != "https://d.example.com/hls/video/high.m3u8?x=1" {
		t.Errorf("相対 URL 解決: %s", m.Streams[0].URL)
	}
	if m.Audios["audio-aac-192kbps"] != "https://d.example.com/hls/audio/a.m3u8" {
		t.Errorf("音声 URL: %v", m.Audios)
	}

	variant := `#EXTM3U
#EXT-X-MAP:URI="init.cmfv?a=b"
#EXT-X-KEY:METHOD=AES-128,URI="https://k.example.com/key",IV=0x1F
#EXTINF:6.0,
1.cmfv?a=b
#EXTINF:6.0,
2.cmfv?a=b
#EXT-X-ENDLIST
`
	v, err := parseVariant("https://d.example.com/hls/video/high.m3u8", variant)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Segments) != 2 || v.MapURL != "https://d.example.com/hls/video/init.cmfv?a=b" || v.KeyURL != "https://k.example.com/key" {
		t.Fatalf("variant: %+v", v)
	}
	if len(v.IV) != 16 || v.IV[15] != 0x1F || v.IV[0] != 0 {
		t.Errorf("IV は 32 桁へ左ゼロ埋めされるべき: %x", v.IV)
	}
	if _, err := parseVariant("https://x/", "#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"k\"\n"); err == nil {
		t.Error("未対応の暗号化はエラーになるべき")
	}
}

func TestDecryptAES128(t *testing.T) {
	key := bytes.Repeat([]byte{3}, 16)
	iv := bytes.Repeat([]byte{9}, 16)
	plain := []byte("fragmented mp4 segment data...")
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	buf := append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	blk, _ := aes.NewCipher(key)
	cipher.NewCBCEncrypter(blk, iv).CryptBlocks(buf, buf)
	got, err := decryptAES128(key, iv, buf)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("got=%q err=%v", got, err)
	}
	if _, err := decryptAES128(key, iv, buf[:len(buf)-1]); err == nil {
		t.Error("ブロック長不正はエラーになるべき")
	}
}

func TestPickBest(t *testing.T) {
	ss := []Stream{
		{ID: "v1", IsAvailable: true, QualityLevel: 1, Height: 360},
		{ID: "v2", IsAvailable: true, QualityLevel: 2, Height: 720},
		{ID: "v3", IsAvailable: true, QualityLevel: 3, Height: 1080},
		{ID: "v4", IsAvailable: false, QualityLevel: 4, Height: 1440},
	}
	if s, _ := pickBest(ss, 0); s.ID != "v3" {
		t.Errorf("上限なし: %s", s.ID)
	}
	if s, _ := pickBest(ss, 720); s.ID != "v2" {
		t.Errorf("上限 720: %s", s.ID)
	}
	if _, ok := pickBest(ss, 240); ok {
		t.Error("上限未満が無ければ ok=false")
	}
	if s, _ := pickLowest(ss); s.ID != "v1" {
		t.Errorf("最低画質: %s", s.ID)
	}
}

func TestCommentXMLRoundtrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.xml")
	in := []Comment{
		{Thread: "100", Fork: "easy", No: 2, VposMs: 12340, Body: "易 <&> \"q\"", UserID: "nvc:abc", Date: 1700000000, NicoruCnt: 3},
		{Thread: "100", Fork: "main", No: 5, VposMs: 100, Body: "本文", Commands: []string{"184", "red"}, UserID: "12345", IsPremium: true, Score: -1, Date: 1700000100},
		{Thread: "100", Fork: "main", No: 1, VposMs: 0, Body: "a\x01b", UserID: "9", Date: 1700000050},
	}
	if err := WriteCommentXML(p, "sm9", in); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	s := string(raw)
	if !strings.Contains(s, "<!--BoonSutazioData=sm9-->") || !strings.Contains(s, `fork="2"`) {
		t.Errorf("XML 内容が想定と違う:\n%s", s)
	}
	if strings.Index(s, ">本文<") > strings.Index(s, "易 &lt;&amp;&gt;") {
		t.Error("main スレッドが easy より前に並ぶべき")
	}
	got, err := ReadCommentXML(p)
	if err != nil || len(got) != 3 {
		t.Fatalf("read: n=%d err=%v", len(got), err)
	}
	by := map[int]Comment{}
	for _, c := range got {
		by[c.No] = c
	}
	if c := by[2]; c.Fork != "easy" || c.Body != `易 <&> "q"` || c.VposMs != 12340 || c.NicoruCnt != 3 {
		t.Errorf("easy 復元: %+v", c)
	}
	if c := by[5]; c.Fork != "main" || !c.IsPremium || strings.Join(c.Commands, " ") != "184 red" || c.Score != -1 {
		t.Errorf("main 復元: %+v", c)
	}
	if c := by[1]; c.Body != "ab" {
		t.Errorf("不正な制御文字は除去されるべき: %q", c.Body)
	}
	if cs, err := ReadCommentXML(filepath.Join(dir, "none.xml")); cs != nil || err != nil {
		t.Errorf("無いファイルは nil,nil: %v %v", cs, err)
	}
}

func TestMergeComments(t *testing.T) {
	old := []Comment{{Thread: "1", Fork: "main", No: 1, Body: "old", NicoruCnt: 0}, {Thread: "1", Fork: "main", No: 3, Body: "x"}}
	fresh := []Comment{{Thread: "1", Fork: "main", No: 1, Body: "old", NicoruCnt: 5}, {Thread: "1", Fork: "main", No: 2, Body: "new"}}
	got := MergeComments(old, fresh)
	if len(got) != 3 || got[0].No != 1 || got[1].No != 2 || got[2].No != 3 || got[0].NicoruCnt != 5 {
		t.Errorf("merge: %+v", got)
	}
	if MaxDate([]Comment{{Date: 5}, {Date: 9}, {Date: 1}}) != 9 {
		t.Error("MaxDate")
	}
}

func TestNowCommentJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "n.json")
	if err := WriteNowCommentJSON(p, []Comment{{No: 3}, {No: 1}}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "[1,3]" {
		t.Errorf("got %s", b)
	}
}

func TestTokenBucket(t *testing.T) {
	b := newTokenBucket(2, 20) // 容量 2、毎秒 20
	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := b.take(ctx, 1); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 30*time.Millisecond || d > 500*time.Millisecond {
		t.Errorf("3 個目は補充待ちになるはず: %v", d)
	}
	b.drain()
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	b2 := newTokenBucket(1, 0.01)
	b2.drain()
	if err := b2.take(ctx2, 1); err == nil {
		t.Error("キャンセルで中断されるべき")
	}
}

func TestParseAttrs(t *testing.T) {
	m := parseAttrs(`METHOD=AES-128,URI="https://a/b?c=d,e",IV=0xAB`)
	if m["METHOD"] != "AES-128" || m["URI"] != "https://a/b?c=d,e" || m["IV"] != "0xAB" {
		t.Errorf("%v", m)
	}
}
