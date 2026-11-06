package nico

import (
	"regexp"
	"strings"
)

var (
	badChars = regexp.MustCompile(`[\\/:*?"<>|\x00-\x1f]`)
	multiSp  = regexp.MustCompile(`\s+`)
)

// Sanitize はファイル名に使えない文字を除去する (NNDD-RE 互換 + 末尾ドット/空白除去)。
func Sanitize(s string) string {
	s = badChars.ReplaceAllString(s, "_")
	s = multiSp.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 100 {
		s = string(r[:100])
	}
	return strings.TrimRight(s, ". ")
}

// BaseName は NNDD-RE 互換の「タイトル - [sm12345]」を返す。
func BaseName(title, videoID string) string {
	t := Sanitize(title)
	if t == "" {
		t = videoID
	}
	return t + " - [" + videoID + "]"
}

var idRe = regexp.MustCompile(`(?:sm|nm|so)\d+|\b\d+\b`)

// ExtractID は URL または文字列から動画 ID を取り出す。
func ExtractID(s string) string {
	if i := strings.Index(s, "/watch/"); i >= 0 {
		s = s[i+len("/watch/"):]
	}
	return idRe.FindString(s)
}
