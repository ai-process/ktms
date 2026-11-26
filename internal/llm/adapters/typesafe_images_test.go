package adapters

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ai-process/ktms/internal/llm"
)

func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.White)
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func jpegOf(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// withOrientation inserts an EXIF APP1 segment with orientation o after SOI.
func withOrientation(jpg []byte, o uint16) []byte {
	tiff := []byte("II*\x00\x08\x00\x00\x00") // little endian, IFD0 at 8
	ifd := make([]byte, 2+12+4)
	binary.LittleEndian.PutUint16(ifd[0:], 1)      // one entry
	binary.LittleEndian.PutUint16(ifd[2:], 0x0112) // orientation
	binary.LittleEndian.PutUint16(ifd[4:], 3)      // SHORT
	binary.LittleEndian.PutUint32(ifd[6:], 1)      // count
	binary.LittleEndian.PutUint16(ifd[10:], o)
	payload := append([]byte("Exif\x00\x00"), append(tiff, ifd...)...)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(payload)+2))
	seg = append(seg, payload...)
	return append(append([]byte{0xFF, 0xD8}, seg...), jpg[2:]...)
}

func TestTypeSafeImageChecks(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string // substring of the error, "" for a pass
	}{
		{"png ok", pngOf(t, 640, 480), ""},
		{"jpeg ok", jpegOf(t, 1568, 882), ""},
		{"too wide", pngOf(t, 1920, 1080), "1920x1080"},
		{"too small", pngOf(t, 200, 400), "200x400"},
		{"not an image", []byte("hello"), "not a PNG or JPEG"},
		{"rotated", withOrientation(jpegOf(t, 640, 480), 6), "EXIF orientation 6"},
		{"upright tag", withOrientation(jpegOf(t, 640, 480), 1), ""},
	}
	for _, c := range cases {
		_, err := typeSafeImageFormat(0, c.data)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want) || !errors.Is(err, llm.ErrInvalidMedia)):
			t.Errorf("%s: err = %v, want an invalid-media error with %q", c.name, err, c.want)
		}
	}
}

// Images go as multipart assets with the opt-in header; a named image is
// referred to by name, an unnamed one is listed next to the caller's state.
func TestTypeSafeJudgeSendsImagesAndBox(t *testing.T) {
	var got typeSafeRequest
	var files map[string]int
	var header, ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header, ctype = r.Header.Get(typeSafeImagesHeader), r.Header.Get("Content-Type")
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("not multipart: %v", err)
			return
		}
		files = map[string]int{}
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(p)
			if p.FormName() == "request" {
				_ = json.Unmarshal(b, &got)
			} else {
				files[p.FormName()] = len(b)
			}
		}
		_, _ = io.WriteString(w, `{"model":"jev-1.14.0","answers":{
			"mug":{"type":"box","box":{"top":0.1,"bottom":0.4,"left":0.2,"right":0.5},"confidence":null},
			"ok":{"type":"noul","noul":0.9,"confidence":0.7}},
			"usage":{"input_tokens":812,"output_tokens":0}}`)
	}))
	defer srv.Close()

	a := NewTypeSafeAdapter(srv.URL, "k", "jev-1.14.0")
	res, err := a.Judge(context.Background(), &llm.JudgeRequest{
		StateJSON: `{"road":"kitchen"}`,
		StateMedia: []*llm.JudgeMediaPart{
			{Type: llm.JudgeMediaImage, Name: "kitchen", Data: pngOf(t, 640, 480)},
			{Type: llm.JudgeMediaImage, Data: jpegOf(t, 400, 400)},
		},
		Questions: map[string]*llm.JudgeQuestion{
			"mug": {Type: llm.JudgeBox, Instructions: "the coffee mug in `road`"},
			"ok":  {Type: llm.JudgeNoul, Instructions: "is the frame usable?"},
		},
	}, "u", nil)
	if err != nil {
		t.Fatal(err)
	}
	if header != "true" || !strings.HasPrefix(ctype, "multipart/form-data") {
		t.Errorf("header %q, content type %q", header, ctype)
	}
	if got.Assets["kitchen"].Part == "" || got.Assets["ktms-image-2"].Format != "jpg" || len(files) != 2 {
		t.Errorf("assets %+v, files %v", got.Assets, files)
	}
	if string(got.State) != `{"images":["ktms-image-2"],"state":{"road":"kitchen"}}` {
		t.Errorf("state = %s", got.State)
	}
	if got.Questions["mug"].Type != "box" || got.Questions["mug"].Criteria != nil {
		t.Errorf("box question = %+v", got.Questions["mug"])
	}
	box := res.Answers["mug"]
	if box.Box == nil || box.Box.Right != 0.5 || !box.ConfidenceUnset {
		t.Errorf("box answer = %+v", box)
	}
	if ok := res.Answers["ok"]; ok.ConfidenceUnset || ok.Confidence != 0.7 {
		t.Errorf("noul answer = %+v", ok)
	}
}

// A bad image is the caller's to fix: no vendor call, an invalid-media error.
func TestTypeSafeJudgeRejectsBadImageBeforeTheCall(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	a := NewTypeSafeAdapter(srv.URL, "k", "jev-1.14.0")
	_, err := a.Judge(context.Background(), &llm.JudgeRequest{
		StateMedia: []*llm.JudgeMediaPart{{Type: llm.JudgeMediaImage, Data: pngOf(t, 1920, 1080)}},
		Questions:  map[string]*llm.JudgeQuestion{"q": {Type: llm.JudgeNoul, Instructions: "?"}},
	}, "u", nil)
	if !errors.Is(err, llm.ErrInvalidMedia) || called {
		t.Fatalf("err = %v, called = %v", err, called)
	}
}

// Audio has no slot in System One: the chain moves on.
func TestTypeSafeJudgeYieldsOnAudio(t *testing.T) {
	a := NewTypeSafeAdapter("http://unused", "k", "jev-1.14.0")
	_, err := a.Judge(context.Background(), &llm.JudgeRequest{
		StateMedia: []*llm.JudgeMediaPart{{Type: llm.JudgeMediaAudio, Data: []byte("x"), Format: "wav"}},
		Questions:  map[string]*llm.JudgeQuestion{"q": {Type: llm.JudgeNoul, Instructions: "?"}},
	}, "u", nil)
	if !errors.Is(err, llm.ErrJudgmentUnsupported) {
		t.Fatalf("err = %v", err)
	}
}
