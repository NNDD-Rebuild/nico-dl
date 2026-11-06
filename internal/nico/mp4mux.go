package nico

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
)

// 映像 fMP4 と音声 fMP4 (init + moof/mdat の連結) を
// 通常の MP4 (ftyp → moov → mdat、サンプル表つき) に結合する。
// 想定外の構造 (base_data_offset 指定、複数 traf など) は黙って処理せずエラーにする。

// mbox は MP4 の box。kids が非 nil の場合はコンテナ。
type mbox struct {
	typ  string
	data []byte
	kids []*mbox
}

var mp4Containers = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true, "edts": true,
	"mvex": true, "moof": true, "traf": true,
}

func parseBoxes(buf []byte) ([]*mbox, error) {
	var out []*mbox
	for len(buf) > 0 {
		if len(buf) < 8 {
			return nil, errors.New("mp4: box ヘッダ不足")
		}
		size := uint64(binary.BigEndian.Uint32(buf))
		typ := string(buf[4:8])
		hdr := uint64(8)
		if size == 1 {
			if len(buf) < 16 {
				return nil, errors.New("mp4: box ヘッダ不足")
			}
			size = binary.BigEndian.Uint64(buf[8:])
			hdr = 16
		} else if size == 0 {
			size = uint64(len(buf))
		}
		if size < hdr || size > uint64(len(buf)) {
			return nil, fmt.Errorf("mp4: box %q のサイズが不正", typ)
		}
		b := &mbox{typ: typ}
		body := buf[hdr:size]
		if mp4Containers[typ] {
			kids, err := parseBoxes(body)
			if err != nil {
				return nil, err
			}
			b.kids = kids
			if b.kids == nil {
				b.kids = []*mbox{}
			}
		} else {
			b.data = body
		}
		out = append(out, b)
		buf = buf[size:]
	}
	return out, nil
}

func (b *mbox) find(typ string) *mbox {
	for _, k := range b.kids {
		if k.typ == typ {
			return k
		}
	}
	return nil
}

func (b *mbox) size() int {
	n := 8 + len(b.data)
	for _, k := range b.kids {
		n += k.size()
	}
	return n
}

func (b *mbox) appendTo(dst []byte) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(b.size()))
	dst = append(dst, b.typ...)
	dst = append(dst, b.data...)
	for _, k := range b.kids {
		dst = k.appendTo(dst)
	}
	return dst
}

func fullBox(typ string, version byte, flags uint32, payload []byte) *mbox {
	d := make([]byte, 4, 4+len(payload))
	d[0] = version
	d[1], d[2], d[3] = byte(flags>>16), byte(flags>>8), byte(flags)
	return &mbox{typ: typ, data: append(d, payload...)}
}

type mp4Sample struct {
	size  uint32
	dur   uint32
	flags uint32
	cto   int32
}

type mp4Chunk struct {
	srcOff  int64
	srcLen  int64
	first   int // samples 内の先頭 index
	count   int
	dts     uint64 // 先頭サンプルの DTS (media timescale)
	dstOff  uint64
	trackNo int
}

type mp4Track struct {
	f          *os.File
	trak       *mbox
	movieTS    uint32 // 元 mvhd の timescale
	mediaTS    uint32
	trackID    uint32
	samples    []mp4Sample
	chunks     []mp4Chunk
	mediaDur   uint64
	trexDur    uint32
	trexSize   uint32
	trexFlags  uint32
	handlerTyp string
	mvhd       *mbox
}

type trexDefaults struct{ dur, size, flags uint32 }

func readBoxHeader(f *os.File, off, fileSize int64) (typ string, hdr, size int64, err error) {
	var h [16]byte
	n, rerr := f.ReadAt(h[:], off)
	if n < 8 {
		if rerr == nil || rerr == io.EOF {
			return "", 0, 0, io.EOF
		}
		return "", 0, 0, rerr
	}
	sz := int64(binary.BigEndian.Uint32(h[:]))
	typ = string(h[4:8])
	hdr = 8
	if sz == 1 {
		if n < 16 {
			return "", 0, 0, errors.New("mp4: box ヘッダ不足")
		}
		sz = int64(binary.BigEndian.Uint64(h[8:]))
		hdr = 16
	} else if sz == 0 {
		sz = fileSize - off
	}
	if sz < hdr || off+sz > fileSize {
		return "", 0, 0, fmt.Errorf("mp4: box %q のサイズが不正", typ)
	}
	return typ, hdr, sz, nil
}

func readAtFull(f *os.File, off, n int64) ([]byte, error) {
	b := make([]byte, n)
	if _, err := f.ReadAt(b, off); err != nil {
		return nil, err
	}
	return b, nil
}

// loadMP4Track は fMP4 (init + fragments) を読み、サンプル表とチャンク (fragment 単位) を作る。
func loadMP4Track(path string) (*mp4Track, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	t := &mp4Track{f: f}
	ok := false
	defer func() {
		if !ok {
			f.Close()
		}
	}()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	fileSize := st.Size()

	var trex trexDefaults
	haveMoov := false
	var dtsRun uint64
	for off := int64(0); off < fileSize; {
		typ, hdr, size, err := readBoxHeader(f, off, fileSize)
		if err != nil {
			return nil, err
		}
		switch typ {
		case "moov":
			if haveMoov {
				return nil, errors.New("mp4: moov が複数ある")
			}
			buf, err := readAtFull(f, off+hdr, size-hdr)
			if err != nil {
				return nil, err
			}
			if trex, err = t.loadMoov(buf); err != nil {
				return nil, err
			}
			haveMoov = true
		case "moof":
			if !haveMoov {
				return nil, errors.New("mp4: moov より前に moof がある")
			}
			buf, err := readAtFull(f, off+hdr, size-hdr)
			if err != nil {
				return nil, err
			}
			if err := t.loadMoof(buf, off, size, trex, &dtsRun); err != nil {
				return nil, err
			}
		}
		off += size
	}
	if !haveMoov {
		return nil, errors.New("mp4: moov がない")
	}
	if len(t.samples) == 0 {
		return nil, errors.New("mp4: サンプルがない")
	}
	for _, s := range t.samples {
		t.mediaDur += uint64(s.dur)
	}
	ok = true
	return t, nil
}

func (t *mp4Track) loadMoov(buf []byte) (trexDefaults, error) {
	var trex trexDefaults
	boxes, err := parseBoxes(buf)
	if err != nil {
		return trex, err
	}
	var mvhd, trak, mvex *mbox
	traks := 0
	for _, b := range boxes {
		switch b.typ {
		case "mvhd":
			mvhd = b
		case "trak":
			trak = b
			traks++
		case "mvex":
			mvex = b
		}
	}
	if mvhd == nil || trak == nil || mvex == nil {
		return trex, errors.New("mp4: mvhd/trak/mvex がない")
	}
	if traks != 1 {
		return trex, fmt.Errorf("mp4: trak が %d 個 (1 個のみ対応)", traks)
	}
	if len(mvhd.data) < 20 {
		return trex, errors.New("mp4: mvhd が短い")
	}
	if mvhd.data[0] == 1 {
		t.movieTS = binary.BigEndian.Uint32(mvhd.data[20:])
	} else {
		t.movieTS = binary.BigEndian.Uint32(mvhd.data[12:])
	}
	tkhd := trak.find("tkhd")
	mdia := trak.find("mdia")
	if tkhd == nil || mdia == nil || mdia.find("mdhd") == nil || mdia.find("minf") == nil || mdia.find("hdlr") == nil {
		return trex, errors.New("mp4: trak の構造が不正")
	}
	if tkhd.data[0] == 1 {
		t.trackID = binary.BigEndian.Uint32(tkhd.data[20:])
	} else {
		t.trackID = binary.BigEndian.Uint32(tkhd.data[12:])
	}
	mdhd := mdia.find("mdhd").data
	if mdhd[0] == 1 {
		t.mediaTS = binary.BigEndian.Uint32(mdhd[20:])
	} else {
		t.mediaTS = binary.BigEndian.Uint32(mdhd[12:])
	}
	if t.movieTS == 0 || t.mediaTS == 0 {
		return trex, errors.New("mp4: timescale が 0")
	}
	if h := mdia.find("hdlr").data; len(h) >= 12 {
		t.handlerTyp = string(h[8:12])
	}
	if mdia.find("minf").find("stbl") == nil || mdia.find("minf").find("stbl").find("stsd") == nil {
		return trex, errors.New("mp4: stsd がない")
	}
	for _, k := range mvex.kids {
		if k.typ != "trex" || len(k.data) < 24 {
			continue
		}
		if binary.BigEndian.Uint32(k.data[4:]) == t.trackID {
			trex = trexDefaults{
				dur:   binary.BigEndian.Uint32(k.data[12:]),
				size:  binary.BigEndian.Uint32(k.data[16:]),
				flags: binary.BigEndian.Uint32(k.data[20:]),
			}
		}
	}
	t.trak = trak
	t.mvhd = mvhd
	return trex, nil
}

func (t *mp4Track) loadMoof(buf []byte, moofOff, moofSize int64, trex trexDefaults, dtsRun *uint64) error {
	boxes, err := parseBoxes(buf)
	if err != nil {
		return err
	}
	var traf *mbox
	n := 0
	for _, b := range boxes {
		if b.typ == "traf" {
			traf = b
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("mp4: moof 内の traf が %d 個 (1 個のみ対応)", n)
	}
	tfhd := traf.find("tfhd")
	if tfhd == nil || len(tfhd.data) < 8 {
		return errors.New("mp4: tfhd がない")
	}
	tflags := binary.BigEndian.Uint32(tfhd.data) & 0xffffff
	if binary.BigEndian.Uint32(tfhd.data[4:]) != t.trackID {
		return errors.New("mp4: tfhd の track_ID が trak と一致しない")
	}
	if tflags&0x1 != 0 {
		return errors.New("mp4: base_data_offset 指定は未対応")
	}
	p := 8
	need := func(n int) ([]byte, error) {
		if p+n > len(tfhd.data) {
			return nil, errors.New("mp4: tfhd が短い")
		}
		b := tfhd.data[p : p+n]
		p += n
		return b, nil
	}
	defDur, defSize, defFlags := trex.dur, trex.size, trex.flags
	if tflags&0x2 != 0 {
		if _, err := need(4); err != nil {
			return err
		}
	}
	if tflags&0x8 != 0 {
		b, err := need(4)
		if err != nil {
			return err
		}
		defDur = binary.BigEndian.Uint32(b)
	}
	if tflags&0x10 != 0 {
		b, err := need(4)
		if err != nil {
			return err
		}
		defSize = binary.BigEndian.Uint32(b)
	}
	if tflags&0x20 != 0 {
		b, err := need(4)
		if err != nil {
			return err
		}
		defFlags = binary.BigEndian.Uint32(b)
	}

	moofEnd := moofOff + moofSize
	next := moofEnd + 8 // data_offset 省略時の既定: mdat 本体の先頭
	for _, k := range traf.kids {
		if k.typ != "trun" {
			continue
		}
		d := k.data
		if len(d) < 8 {
			return errors.New("mp4: trun が短い")
		}
		ver := d[0]
		fl := binary.BigEndian.Uint32(d) & 0xffffff
		cnt := int(binary.BigEndian.Uint32(d[4:]))
		q := 8
		rd := func() (uint32, error) {
			if q+4 > len(d) {
				return 0, errors.New("mp4: trun が短い")
			}
			v := binary.BigEndian.Uint32(d[q:])
			q += 4
			return v, nil
		}
		dataOff := next
		if fl&0x1 != 0 {
			v, err := rd()
			if err != nil {
				return err
			}
			dataOff = moofOff + int64(int32(v))
		}
		firstFlags, haveFirst := uint32(0), false
		if fl&0x4 != 0 {
			v, err := rd()
			if err != nil {
				return err
			}
			firstFlags, haveFirst = v, true
		}
		if cnt == 0 {
			continue
		}
		ch := mp4Chunk{srcOff: dataOff, first: len(t.samples), count: cnt, dts: *dtsRun}
		var total int64
		for i := 0; i < cnt; i++ {
			s := mp4Sample{size: defSize, dur: defDur, flags: defFlags}
			if i == 0 && haveFirst {
				s.flags = firstFlags
			}
			if fl&0x100 != 0 {
				if s.dur, err = rd(); err != nil {
					return err
				}
			}
			if fl&0x200 != 0 {
				if s.size, err = rd(); err != nil {
					return err
				}
			}
			if fl&0x400 != 0 {
				if s.flags, err = rd(); err != nil {
					return err
				}
			}
			if fl&0x800 != 0 {
				v, err := rd()
				if err != nil {
					return err
				}
				s.cto = int32(v)
				if ver == 0 && v > 0x7fffffff {
					return errors.New("mp4: trun の cto が範囲外")
				}
			}
			total += int64(s.size)
			*dtsRun += uint64(s.dur)
			t.samples = append(t.samples, s)
		}
		ch.srcLen = total
		t.chunks = append(t.chunks, ch)
		next = dataOff + total
	}
	return nil
}

// buildTrak は stbl を作り直した trak を返す。
func (t *mp4Track) buildTrak(trackID uint32, outMovieTS uint32, co64 bool) (*mbox, uint64, error) {
	trak := t.trak
	tkhd := trak.find("tkhd")
	mdia := trak.find("mdia")
	minf := mdia.find("minf")
	stbl := minf.find("stbl")

	// stbl
	var stts, ctts, stsz, stss, stsc, stco []byte
	var sttsN, cttsN, stssN, stscN uint32
	var ctsNeg, ctsAny bool
	for _, s := range t.samples {
		if s.cto != 0 {
			ctsAny = true
		}
		if s.cto < 0 {
			ctsNeg = true
		}
	}
	for i := 0; i < len(t.samples); {
		j := i
		for j < len(t.samples) && t.samples[j].dur == t.samples[i].dur {
			j++
		}
		stts = binary.BigEndian.AppendUint32(stts, uint32(j-i))
		stts = binary.BigEndian.AppendUint32(stts, t.samples[i].dur)
		sttsN++
		i = j
	}
	if ctsAny {
		for i := 0; i < len(t.samples); {
			j := i
			for j < len(t.samples) && t.samples[j].cto == t.samples[i].cto {
				j++
			}
			ctts = binary.BigEndian.AppendUint32(ctts, uint32(j-i))
			ctts = binary.BigEndian.AppendUint32(ctts, uint32(t.samples[i].cto))
			cttsN++
			i = j
		}
	}
	same := true
	for _, s := range t.samples {
		if s.size != t.samples[0].size {
			same = false
			break
		}
	}
	if same {
		stsz = binary.BigEndian.AppendUint32(stsz, t.samples[0].size)
		stsz = binary.BigEndian.AppendUint32(stsz, uint32(len(t.samples)))
	} else {
		stsz = binary.BigEndian.AppendUint32(stsz, 0)
		stsz = binary.BigEndian.AppendUint32(stsz, uint32(len(t.samples)))
		for _, s := range t.samples {
			stsz = binary.BigEndian.AppendUint32(stsz, s.size)
		}
	}
	allSync := true
	for i, s := range t.samples {
		if s.flags&0x10000 == 0 {
			stss = binary.BigEndian.AppendUint32(stss, uint32(i+1))
			stssN++
		} else {
			allSync = false
		}
	}
	var prevCount int
	for i, c := range t.chunks {
		if i == 0 || c.count != prevCount {
			stsc = binary.BigEndian.AppendUint32(stsc, uint32(i+1))
			stsc = binary.BigEndian.AppendUint32(stsc, uint32(c.count))
			stsc = binary.BigEndian.AppendUint32(stsc, 1)
			stscN++
			prevCount = c.count
		}
	}
	for _, c := range t.chunks {
		if co64 {
			stco = binary.BigEndian.AppendUint64(stco, c.dstOff)
		} else {
			stco = binary.BigEndian.AppendUint32(stco, uint32(c.dstOff))
		}
	}
	cnt := func(n uint32, p []byte) []byte { return append(binary.BigEndian.AppendUint32(nil, n), p...) }

	newStbl := &mbox{typ: "stbl", kids: []*mbox{stbl.find("stsd")}}
	newStbl.kids = append(newStbl.kids, fullBox("stts", 0, 0, cnt(sttsN, stts)))
	if ctsAny {
		v := byte(0)
		if ctsNeg {
			v = 1
		}
		newStbl.kids = append(newStbl.kids, fullBox("ctts", v, 0, cnt(cttsN, ctts)))
	}
	newStbl.kids = append(newStbl.kids, fullBox("stsc", 0, 0, cnt(stscN, stsc)))
	newStbl.kids = append(newStbl.kids, fullBox("stsz", 0, 0, stsz))
	if co64 {
		newStbl.kids = append(newStbl.kids, fullBox("co64", 0, 0, cnt(uint32(len(t.chunks)), stco)))
	} else {
		newStbl.kids = append(newStbl.kids, fullBox("stco", 0, 0, cnt(uint32(len(t.chunks)), stco)))
	}
	if !allSync {
		newStbl.kids = append(newStbl.kids, fullBox("stss", 0, 0, cnt(stssN, stss)))
	}

	newMinf := &mbox{typ: "minf"}
	for _, k := range minf.kids {
		if k.typ == "stbl" {
			newMinf.kids = append(newMinf.kids, newStbl)
		} else {
			newMinf.kids = append(newMinf.kids, k)
		}
	}

	// mdhd: duration を実サンプル合計にする
	newMdia := &mbox{typ: "mdia"}
	for _, k := range mdia.kids {
		switch k.typ {
		case "minf":
			newMdia.kids = append(newMdia.kids, newMinf)
		case "mdhd":
			d := append([]byte(nil), k.data...)
			if d[0] == 1 {
				binary.BigEndian.PutUint64(d[24:], t.mediaDur)
			} else {
				binary.BigEndian.PutUint32(d[16:], uint32(t.mediaDur))
			}
			newMdia.kids = append(newMdia.kids, &mbox{typ: "mdhd", data: d})
		default:
			newMdia.kids = append(newMdia.kids, k)
		}
	}

	// movie timescale に換算した再生時間
	conv := func(v uint64, from, to uint32) uint64 {
		return (v*uint64(to) + uint64(from)/2) / uint64(from)
	}
	trackDur := conv(t.mediaDur, t.mediaTS, outMovieTS)

	// edts: segment_duration を出力の movie timescale に直す。0 は末尾まで。
	var newEdts *mbox
	if edts := trak.find("edts"); edts != nil {
		if elst := edts.find("elst"); elst != nil {
			ne, dur, err := rescaleElst(elst, t, outMovieTS)
			if err != nil {
				return nil, 0, err
			}
			newEdts = &mbox{typ: "edts", kids: []*mbox{ne}}
			trackDur = dur
		}
	}

	// tkhd: track_ID と duration
	td := append([]byte(nil), tkhd.data...)
	if td[0] == 1 {
		binary.BigEndian.PutUint32(td[20:], trackID)
		binary.BigEndian.PutUint64(td[28:], trackDur)
	} else {
		binary.BigEndian.PutUint32(td[12:], trackID)
		binary.BigEndian.PutUint32(td[20:], uint32(trackDur))
	}

	out := &mbox{typ: "trak", kids: []*mbox{{typ: "tkhd", data: td}}}
	if newEdts != nil {
		out.kids = append(out.kids, newEdts)
	}
	out.kids = append(out.kids, newMdia)
	return out, trackDur, nil
}

func rescaleElst(elst *mbox, t *mp4Track, outTS uint32) (*mbox, uint64, error) {
	d := elst.data
	if len(d) < 8 {
		return nil, 0, errors.New("mp4: elst が短い")
	}
	ver := d[0]
	n := int(binary.BigEndian.Uint32(d[4:]))
	esz := 12
	if ver == 1 {
		esz = 20
	}
	if len(d) < 8+n*esz {
		return nil, 0, errors.New("mp4: elst が短い")
	}
	out := append([]byte(nil), d[:8]...)
	var total uint64
	for i := 0; i < n; i++ {
		e := d[8+i*esz:]
		var segDur uint64
		var mediaTime int64
		if ver == 1 {
			segDur = binary.BigEndian.Uint64(e)
			mediaTime = int64(binary.BigEndian.Uint64(e[8:]))
		} else {
			segDur = uint64(binary.BigEndian.Uint32(e))
			mediaTime = int64(int32(binary.BigEndian.Uint32(e[4:])))
		}
		rate := e[esz-4 : esz]
		if segDur == 0 && i == n-1 {
			// 末尾まで (fMP4 では不明な場合に 0)
			if mediaTime >= 0 && uint64(mediaTime) < t.mediaDur {
				segDur = (t.mediaDur - uint64(mediaTime)) * uint64(outTS) / uint64(t.mediaTS)
			}
		} else {
			segDur = (segDur*uint64(outTS) + uint64(t.movieTS)/2) / uint64(t.movieTS)
		}
		total += segDur
		if ver == 1 {
			out = binary.BigEndian.AppendUint64(out, segDur)
			out = binary.BigEndian.AppendUint64(out, uint64(mediaTime))
		} else {
			out = binary.BigEndian.AppendUint32(out, uint32(segDur))
			out = binary.BigEndian.AppendUint32(out, uint32(int32(mediaTime)))
		}
		out = append(out, rate...)
	}
	return &mbox{typ: "elst", data: out}, total, nil
}

// muxMP4 は映像 fMP4 と音声 fMP4 を、通常の MP4 として out に書く。
// 失敗・中止時は out を削除する。
func muxMP4(ctx context.Context, videoPath, audioPath, out string) (err error) {
	v, err := loadMP4Track(videoPath)
	if err != nil {
		return fmt.Errorf("映像: %w", err)
	}
	defer v.f.Close()
	a, err := loadMP4Track(audioPath)
	if err != nil {
		return fmt.Errorf("音声: %w", err)
	}
	defer a.f.Close()

	// track_ID の衝突回避
	vID, aID := v.trackID, a.trackID
	if aID == vID {
		aID = vID + 1
	}
	nextID := vID
	if aID > nextID {
		nextID = aID
	}
	nextID++

	// チャンクを DTS (秒) 順に並べる
	type ref struct {
		t *mp4Track
		i int
	}
	var order []ref
	for i := range v.chunks {
		order = append(order, ref{v, i})
	}
	for i := range a.chunks {
		order = append(order, ref{a, i})
	}
	sort.SliceStable(order, func(x, y int) bool {
		cx, cy := order[x].t.chunks[order[x].i], order[y].t.chunks[order[y].i]
		// cx.dts/tsx < cy.dts/tsy
		return cx.dts*uint64(order[y].t.mediaTS) < cy.dts*uint64(order[x].t.mediaTS)
	})

	var dataLen uint64
	for _, r := range order {
		dataLen += uint64(r.t.chunks[r.i].srcLen)
	}
	co64 := dataLen > 0xf0000000
	mdatHdr := uint64(8)
	if dataLen+8 > 0xffffffff {
		mdatHdr = 16
	}

	ftyp := &mbox{typ: "ftyp", data: append(append([]byte("isom"), 0, 0, 2, 0), []byte("isomiso2avc1mp41")...)}

	build := func() ([]byte, error) {
		vt, vd, err := v.buildTrak(vID, v.movieTS, co64)
		if err != nil {
			return nil, err
		}
		at, ad, err := a.buildTrak(aID, v.movieTS, co64)
		if err != nil {
			return nil, err
		}
		dur := vd
		if ad > dur {
			dur = ad
		}
		md := append([]byte(nil), v.mvhd.data...)
		if md[0] == 1 {
			binary.BigEndian.PutUint64(md[24:], dur)
			binary.BigEndian.PutUint32(md[len(md)-4:], nextID)
		} else {
			binary.BigEndian.PutUint32(md[16:], uint32(dur))
			binary.BigEndian.PutUint32(md[len(md)-4:], nextID)
		}
		moov := &mbox{typ: "moov", kids: []*mbox{{typ: "mvhd", data: md}, vt, at}}
		return moov.appendTo(nil), nil
	}

	// moov サイズを確定してからチャンクの書込位置を決める
	moov, err := build()
	if err != nil {
		return err
	}
	base := uint64(ftyp.size()+len(moov)) + mdatHdr
	var pos uint64
	for _, r := range order {
		r.t.chunks[r.i].dstOff = base + pos
		pos += uint64(r.t.chunks[r.i].srcLen)
	}
	if moov, err = build(); err != nil {
		return err
	}

	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(out)
		}
	}()

	hdr := ftyp.appendTo(nil)
	hdr = append(hdr, moov...)
	if mdatHdr == 16 {
		hdr = binary.BigEndian.AppendUint32(hdr, 1)
		hdr = append(hdr, "mdat"...)
		hdr = binary.BigEndian.AppendUint64(hdr, dataLen+16)
	} else {
		hdr = binary.BigEndian.AppendUint32(hdr, uint32(dataLen+8))
		hdr = append(hdr, "mdat"...)
	}
	if _, err = f.Write(hdr); err != nil {
		return err
	}
	buf := make([]byte, 256<<10)
	for _, r := range order {
		if err = ctx.Err(); err != nil {
			return err
		}
		c := r.t.chunks[r.i]
		if _, err = io.CopyBuffer(f, io.NewSectionReader(r.t.f, c.srcOff, c.srcLen), buf); err != nil {
			return err
		}
	}
	return nil
}
