package nico

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// ReadCommentXML は NNDD 互換コメント XML を読み込む。ファイルが無ければ nil, nil。
// fork は数値 (1=owner, 2=easy) と文字列 (main/owner/easy) の両方を受ける。
func ReadCommentXML(path string) ([]Comment, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dec := xml.NewDecoder(f)
	dec.Strict = false
	var out []Comment
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "chat" {
			continue
		}
		at := map[string]string{}
		for _, a := range se.Attr {
			at[a.Name.Local] = a.Value
		}
		var body strings.Builder
		for {
			t, err := dec.Token()
			if err != nil {
				return nil, err
			}
			if cd, ok := t.(xml.CharData); ok {
				body.Write(cd)
				continue
			}
			if _, ok := t.(xml.EndElement); ok {
				break
			}
		}
		no, _ := strconv.Atoi(at["no"])
		vpos, _ := strconv.Atoi(at["vpos"])
		date, _ := strconv.ParseInt(at["date"], 10, 64)
		score, _ := strconv.Atoi(at["score"])
		nicoru, _ := strconv.Atoi(at["nicoru"])
		fork := "main"
		switch at["fork"] {
		case "1", "owner":
			fork = "owner"
		case "2", "easy":
			fork = "easy"
		}
		var cmds []string
		if at["mail"] != "" {
			cmds = strings.Fields(at["mail"])
		}
		out = append(out, Comment{
			Thread: at["thread"], Fork: fork, No: no, VposMs: vpos * 10, Body: body.String(),
			Commands: cmds, UserID: at["user_id"], IsPremium: at["premium"] == "1",
			Score: score, Date: date, NicoruCnt: nicoru,
		})
	}
	return out, nil
}

// MaxDate は最新コメントの unix 秒。空なら 0。
func MaxDate(cms []Comment) int64 {
	var m int64
	for _, c := range cms {
		if c.Date > m {
			m = c.Date
		}
	}
	return m
}

// MergeComments は (thread, fork, no) で重複排除して統合する。同一キーは新しい方 (nicoru 等が更新済み) を採用。
func MergeComments(old, fresh []Comment) []Comment {
	key := func(c Comment) string { return c.Thread + "/" + c.Fork + "/" + strconv.Itoa(c.No) }
	idx := map[string]int{}
	var out []Comment
	for _, list := range [][]Comment{old, fresh} {
		for _, c := range list {
			k := key(c)
			if i, ok := idx[k]; ok {
				out[i] = c
				continue
			}
			idx[k] = len(out)
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].No < out[j].No })
	return out
}

// WriteNowCommentJSON は今コメの no 一覧を [NowComment].json に書く (NNDD-RE 互換: 整数配列)。
func WriteNowCommentJSON(path string, cms []Comment) error {
	nos := make([]int, 0, len(cms))
	for _, c := range cms {
		nos = append(nos, c.No)
	}
	sort.Ints(nos)
	b, err := json.Marshal(nos)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
