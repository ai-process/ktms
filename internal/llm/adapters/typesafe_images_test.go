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

// Structured instructions and criteria reach System One as they are; Decisions
// reads them as text.
func TestStructuredQuestions(t *testing.T) {
	q := map[string]*llm.JudgeQuestion{"defect": {
		Type:             llm.JudgeChoice,
		InstructionsJSON: json.RawMessage(`["What defect is in", "road", "?"]`),
		CriteriaJSON:     json.RawMessage(`{"pothole":{"description":"a hole","examples":["ex-hole"]},"crack":"ex-crack"}`),
	}}
	ts := toTypeSafeQuestions(q)["defect"]
	b, _ := json.Marshal(ts)
	if !strings.Contains(string(b), `"instructions":["What defect is in","road","?"]`) || !strings.Contains(string(b), `"examples":["ex-hole"]`) {
		t.Errorf("typesafe question = %s", b)
	}
	flat := flatQuestion(q["defect"])
	if flat.Instructions != `["What defect is in","road","?"]` || flat.Options["crack"] != "ex-crack" ||
		flat.Options["pothole"] != `{"description":"a hole","examples":["ex-hole"]}` {
		t.Errorf("decisions question = %+v", flat)
	}
}

func TestTypeSafeLongContextHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(typeSafeLongContextHeader)
		_, _ = io.WriteString(w, `{"answers":{"q":{"type":"noul","noul":0.5}}}`)
	}))
	defer srv.Close()
	a := NewTypeSafeAdapter(srv.URL, "k", "jev-1.14.0").WithLongContext(true)
	if _, err := a.Judge(context.Background(), &llm.JudgeRequest{StateJSON: `{}`,
		Questions: map[string]*llm.JudgeQuestion{"q": {Type: llm.JudgeNoul, Instructions: "?"}}}, "u", nil); err != nil {
		t.Fatal(err)
	}
	if got != "true" {
		t.Errorf("long context header = %q", got)
	}
}

// A long-context model with images sends both opt-in headers.
func TestTypeSafeImagesAndLongContextHeaders(t *testing.T) {
	var images, long string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		images, long = r.Header.Get(typeSafeImagesHeader), r.Header.Get(typeSafeLongContextHeader)
		_, _ = io.WriteString(w, `{"answers":{"q":{"type":"noul","noul":0.5}}}`)
	}))
	defer srv.Close()
	a := NewTypeSafeAdapter(srv.URL, "k", "jev-1.14.0").WithLongContext(true)
	if _, err := a.Judge(context.Background(), &llm.JudgeRequest{
		StateMedia: []*llm.JudgeMediaPart{{Type: llm.JudgeMediaImage, Name: "road", Data: pngOf(t, 640, 480)}},
		Questions:  map[string]*llm.JudgeQuestion{"q": {Type: llm.JudgeNoul, Instructions: "is `road` usable?"}}}, "u", nil); err != nil {
		t.Fatal(err)
	}
	if images != "true" || long != "true" {
		t.Errorf("headers images=%q long=%q", images, long)
	}
}

// The pre-call check agrees with what the call itself would refuse or yield on.
func TestTypeSafeCheckJudgeMedia(t *testing.T) {
	a := NewTypeSafeAdapter("http://unused", "k", "jev-1.14.0")
	img := func(name string, w, h int) *llm.JudgeMediaPart {
		return &llm.JudgeMediaPart{Type: llm.JudgeMediaImage, Name: name, Data: pngOf(t, w, h)}
	}
	if err := a.CheckJudgeMedia([]*llm.JudgeMediaPart{img("", 640, 480), img("road", 640, 480)}); err != nil {
		t.Errorf("valid media: %v", err)
	}
	if err := a.CheckJudgeMedia([]*llm.JudgeMediaPart{img("", 640, 480), {Type: llm.JudgeMediaText, Text: "t"}}); !errors.Is(err, llm.ErrJudgmentUnsupported) {
		t.Errorf("text part: %v", err)
	}
	err := a.CheckJudgeMedia([]*llm.JudgeMediaPart{img("", 640, 480), img("ktms-image-1", 640, 480)})
	var im *llm.InvalidMediaError
	if !errors.As(err, &im) || im.Part != 1 || err.Error() != `state_media[1]: name "ktms-image-1" is used twice` {
		t.Errorf("generated name clash: %v", err)
	}
	if err := NewTrackingAdapter(a, nil, "typesafe", "jev-1.14.0").CheckJudgeMedia([]*llm.JudgeMediaPart{img("", 1920, 1080)}); !errors.Is(err, llm.ErrInvalidMedia) {
		t.Errorf("tracking adapter does not forward: %v", err)
	}
}

// System One requires a state and refuses null: a missing one goes as {}.
func TestTypeSafeMissingStateIsEmptyObject(t *testing.T) {
	var states []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got typeSafeRequest
		if mr, err := r.MultipartReader(); err == nil {
			for p, err := mr.NextPart(); err == nil; p, err = mr.NextPart() {
				if p.FormName() == "request" {
					_ = json.NewDecoder(p).Decode(&got)
				}
			}
		} else {
			_ = json.NewDecoder(r.Body).Decode(&got)
		}
		states = append(states, string(got.State))
		_, _ = io.WriteString(w, `{"answers":{"q":{"type":"noul","noul":0.5}}}`)
	}))
	defer srv.Close()
	a := NewTypeSafeAdapter(srv.URL, "k", "jev-1.14.0")
	q := map[string]*llm.JudgeQuestion{"q": {Type: llm.JudgeNoul, Instructions: "is `road` usable?"}}
	for _, req := range []*llm.JudgeRequest{
		{StateMedia: []*llm.JudgeMediaPart{{Type: llm.JudgeMediaImage, Name: "road", Data: pngOf(t, 640, 480)}}, Questions: q},
		{StateJSON: "null", StateMedia: []*llm.JudgeMediaPart{{Type: llm.JudgeMediaImage, Name: "road", Data: pngOf(t, 640, 480)}}, Questions: q},
		{Questions: q},
		{StateJSON: " null ", Questions: q},
		{StateMedia: []*llm.JudgeMediaPart{{Type: llm.JudgeMediaImage, Data: pngOf(t, 640, 480)}}, Questions: q},
	} {
		if _, err := a.Judge(context.Background(), req, "u", nil); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"{}", "{}", "{}", "{}", `"ktms-image-1"`} // an unnamed image is the state itself
	for i, s := range states {
		if s != want[i] {
			t.Errorf("call %d: state = %s, want %s", i, s, want[i])
		}
	}
}

// Criteria given as {"description", "examples"} come back as objects in the
// legend; the answer reads them as their description.
func TestTypeSafeLegendObjects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"answers":{"wear":{"type":"score","score":1.7,"legend":{
			"0":"intact","1":{"description":"potholes","examples":["ex-pothole"]},"2":{ "examples": [ "x" ] }}}}}`)
	}))
	defer srv.Close()
	res, err := NewTypeSafeAdapter(srv.URL, "k", "jev-1.14.0").Judge(context.Background(), &llm.JudgeRequest{StateJSON: `{}`,
		Questions: map[string]*llm.JudgeQuestion{"wear": {Type: llm.JudgeScore, Instructions: "?", Levels: []string{"a", "b", "c"}}}}, "u", nil)
	if err != nil {
		t.Fatal(err)
	}
	l := res.Answers["wear"].Legend
	if l["0"] != "intact" || l["1"] != "potholes" || l["2"] != `{"examples":["x"]}` {
		t.Errorf("legend = %v", l)
	}
}

// System One refuses an image nothing mentions: a named image no question or
// state names is listed next to the state like an unnamed one.
func TestTypeSafeListsUnmentionedNamedImages(t *testing.T) {
	var got typeSafeRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("not multipart: %v", err)
			return
		}
		for p, err := mr.NextPart(); err == nil; p, err = mr.NextPart() {
			if p.FormName() == "request" {
				_ = json.NewDecoder(p).Decode(&got)
			}
		}
		_, _ = io.WriteString(w, `{"answers":{"q":{"type":"noul","noul":0.5}}}`)
	}))
	defer srv.Close()
	a := NewTypeSafeAdapter(srv.URL, "k", "jev-1.14.0")
	img := func(name string) *llm.JudgeMediaPart {
		return &llm.JudgeMediaPart{Type: llm.JudgeMediaImage, Name: name, Data: pngOf(t, 640, 480)}
	}
	for _, tc := range []struct {
		name, state string
		media       []*llm.JudgeMediaPart
		want        string
	}{
		{"only the marked one is asked about", `{"task":"check"}`, []*llm.JudgeMediaPart{img("original"), img("marked")},
			`{"images":["original"],"state":{"task":"check"}}`},
		{"no state", "", []*llm.JudgeMediaPart{img("original"), img("marked")}, `"original"`},
		{"state names it", `{"before":"original"}`, []*llm.JudgeMediaPart{img("original"), img("marked")}, `{"before":"original"}`},
		{"with an unnamed one", `{}`, []*llm.JudgeMediaPart{img("original"), img(""), img("marked")},
			`{"images":["original","ktms-image-2"],"state":{}}`},
	} {
		got = typeSafeRequest{}
		_, err := a.Judge(context.Background(), &llm.JudgeRequest{StateJSON: tc.state, StateMedia: tc.media,
			Questions: map[string]*llm.JudgeQuestion{"q": {Type: llm.JudgeNoul, Instructions: "is the sign in `marked` labelled right?"}}}, "u", nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if string(got.State) != tc.want {
			t.Errorf("%s: state = %s, want %s", tc.name, got.State, tc.want)
		}
	}
}

// A judgment beyond the model's context is the caller's to split: bad_request,
// terminal, not a vendor failure.
func TestTypeSafeMaxTokensIsJudgeTooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"detail":{"error_type":"max_tokens_exceeded"}}`)
	}))
	defer srv.Close()
	_, err := NewTypeSafeAdapter(srv.URL, "k", "jev-1.14.0").Judge(context.Background(), &llm.JudgeRequest{
		StateMedia: []*llm.JudgeMediaPart{{Type: llm.JudgeMediaImage, Name: "road", Data: pngOf(t, 640, 480)}},
		Questions:  map[string]*llm.JudgeQuestion{"q": {Type: llm.JudgeNoul, Instructions: "is `road` usable?"}}}, "u", nil)
	if !errors.Is(err, llm.ErrJudgeTooLarge) || llm.IsRetryableError(err) || llm.ShouldSkipToNextModel(err) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "max_tokens_exceeded") || !strings.Contains(err.Error(), "fewer") {
		t.Errorf("message = %q", err.Error())
	}
}
