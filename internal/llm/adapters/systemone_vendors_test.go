package adapters

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ai-process/ktms/internal/llm"
)

// vendorCall records what one System One vendor was sent and answers like it.
type vendorCall struct {
	path, auth, apiKey, ctype string
	body                      map[string]json.RawMessage
}

func vendorServer(t *testing.T, reply string, got *vendorCall) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.auth, got.apiKey, got.ctype = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("api-key"), r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &got.body); err != nil {
			t.Errorf("body is not JSON: %v", err)
		}
		_, _ = io.WriteString(w, reply)
	}))
}

const vendorReply = `{"model":"m","answers":{
	"ok":{"type":"noul","noul":0.8},
	"wear":{"type":"score","score":1.6,"probabilities":{"0":0.1,"1":0.2,"2":0.7},"confidence":0.6,"legend":{"0":"intact","1":"cracks","2":"potholes"}}},
	"usage":{"input_tokens":1200,"output_tokens":0}}`

func vendorQuestions() map[string]*llm.JudgeQuestion {
	return map[string]*llm.JudgeQuestion{
		"ok":   {Type: llm.JudgeNoul, Instructions: "is `road` usable?"},
		"wear": {Type: llm.JudgeScore, Instructions: "how worn?", Levels: []string{"intact", "cracks", "potholes"}},
	}
}

func checkVendorAnswers(t *testing.T, res *llm.JudgeResult) {
	t.Helper()
	if res.Answers["ok"].Noul != 0.8 || res.Answers["wear"].Score != 1.6 || res.Answers["wear"].Legend["2"] != "potholes" || res.Usage.Input != 1200 {
		t.Errorf("result = %+v", res)
	}
}

// Liquid takes images as a top-level array of data URLs on /decisions/v1/systemone.
func TestLiquidSendsImagesArray(t *testing.T) {
	var got vendorCall
	srv := vendorServer(t, vendorReply, &got)
	defer srv.Close()
	a := NewSystemOneAdapter(LiquidProfile, srv.URL, "liquid_k", "d1.2")
	res, err := a.Judge(context.Background(), &llm.JudgeRequest{
		StateJSON:  `{"camera":"front"}`,
		StateMedia: []*llm.JudgeMediaPart{{Type: llm.JudgeMediaImage, Name: "road", Data: pngOf(t, 640, 480)}},
		Questions:  vendorQuestions(),
	}, "u", nil)
	if err != nil {
		t.Fatal(err)
	}
	checkVendorAnswers(t, res)
	if got.path != "/decisions/v1/systemone" || got.auth != "Bearer liquid_k" || got.ctype != "application/json" {
		t.Errorf("path %q auth %q type %q", got.path, got.auth, got.ctype)
	}
	var images []string
	_ = json.Unmarshal(got.body["images"], &images)
	if len(images) != 1 || !strings.HasPrefix(images[0], "data:image/png;base64,") {
		t.Errorf("images = %.60v", images)
	}
	if string(got.body["state"]) != `{"images":["road"],"state":{"camera":"front"}}` || string(got.body["model"]) != `"d1.2"` {
		t.Errorf("state %s model %s", got.body["state"], got.body["model"])
	}
	if _, ok := got.body["assets"]; ok {
		t.Error("assets sent to an inline-images vendor")
	}
}

// Unnamed images go as they are: no state wrapper to point names at.
func TestLiquidUnnamedImagesKeepState(t *testing.T) {
	var got vendorCall
	srv := vendorServer(t, vendorReply, &got)
	defer srv.Close()
	_, err := NewSystemOneAdapter(LiquidProfile, srv.URL, "k", "d1.2").Judge(context.Background(), &llm.JudgeRequest{
		StateMedia: []*llm.JudgeMediaPart{{Type: llm.JudgeMediaImage, Data: pngOf(t, 640, 480)}},
		Questions:  vendorQuestions(),
	}, "u", nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.body["state"]) != `{}` {
		t.Errorf("state = %s", got.body["state"])
	}
}

// Liquid answers a box question about exactly one image: more is refused before the call.
func TestLiquidBoxTakesOneImage(t *testing.T) {
	a := NewSystemOneAdapter(LiquidProfile, "http://unused", "k", "d1.2")
	img := &llm.JudgeMediaPart{Type: llm.JudgeMediaImage, Data: pngOf(t, 640, 480)}
	_, err := a.Judge(context.Background(), &llm.JudgeRequest{
		StateMedia: []*llm.JudgeMediaPart{img, img},
		Questions:  map[string]*llm.JudgeQuestion{"hole": {Type: llm.JudgeBox, Instructions: "the pothole"}},
	}, "u", nil)
	if !errors.Is(err, llm.ErrInvalidMedia) || !strings.Contains(err.Error(), "box question") {
		t.Fatalf("err = %v", err)
	}
}

// Cloudflare puts the model in the path of the account's prefix and may wrap
// the answer in its {"result", "success"} envelope.
func TestCloudflareModelPathAndEnvelope(t *testing.T) {
	var got vendorCall
	srv := vendorServer(t, `{"result":`+vendorReply+`,"success":true,"errors":[],"messages":[]}`, &got)
	defer srv.Close()
	res, err := NewSystemOneAdapter(CloudflareProfile, srv.URL+"/client/v4/accounts/acc/ai/run/@cf/cloudflare", "cf_t", "clef-flash").Judge(
		context.Background(), &llm.JudgeRequest{StateJSON: `"a road"`, Questions: vendorQuestions()}, "u", nil)
	if err != nil {
		t.Fatal(err)
	}
	checkVendorAnswers(t, res)
	if got.path != "/client/v4/accounts/acc/ai/run/@cf/cloudflare/clef-flash" || got.auth != "Bearer cf_t" || string(got.body["model"]) != `"clef-flash"` {
		t.Errorf("path %q auth %q model %s", got.path, got.auth, got.body["model"])
	}
}

func TestCloudflareTakesAtMostFourImages(t *testing.T) {
	img := &llm.JudgeMediaPart{Type: llm.JudgeMediaImage, Data: pngOf(t, 640, 480)}
	err := NewSystemOneAdapter(CloudflareProfile, "http://unused", "k", "clef").CheckJudgeMedia([]*llm.JudgeMediaPart{img, img, img, img, img}, nil)
	if !errors.Is(err, llm.ErrInvalidMedia) {
		t.Fatalf("err = %v", err)
	}
}

// Perplexity takes the System One body on /v1/decisions with images as
// image_url parts inside the state, each named image announced before it.
func TestPerplexityImagesAsStateParts(t *testing.T) {
	var got vendorCall
	srv := vendorServer(t, vendorReply, &got)
	defer srv.Close()
	res, err := NewSystemOneAdapter(PerplexityProfile, srv.URL, "pplx-k", "pplx-decider-flash-v1.3").Judge(context.Background(), &llm.JudgeRequest{
		StateJSON:  `{"camera":"front"}`,
		StateMedia: []*llm.JudgeMediaPart{{Type: llm.JudgeMediaImage, Name: "road", Data: jpegOf(t, 640, 480)}},
		Questions:  vendorQuestions(),
	}, "u", nil)
	if err != nil {
		t.Fatal(err)
	}
	checkVendorAnswers(t, res)
	if got.path != "/v1/decisions" || got.auth != "Bearer pplx-k" {
		t.Errorf("path %q auth %q", got.path, got.auth)
	}
	var parts []map[string]any
	if err := json.Unmarshal(got.body["state"], &parts); err != nil || len(parts) != 3 {
		t.Fatalf("state = %.200s (%v)", got.body["state"], err)
	}
	url, _ := parts[2]["image_url"].(map[string]any)["url"].(string)
	if parts[0]["text"] != `{"camera":"front"}` || parts[1]["text"] != "Image `road`:" || !strings.HasPrefix(url, "data:image/jpeg;base64,") {
		t.Errorf("parts = %.300v", parts)
	}
	if _, ok := got.body["images"]; ok {
		t.Error("top-level images sent to Perplexity")
	}
}

// Microsoft Foundry: the key in api-key on the deployment's provider path, text only.
func TestMicrosoftFoundryTextOnly(t *testing.T) {
	var got vendorCall
	srv := vendorServer(t, vendorReply, &got)
	defer srv.Close()
	a := NewSystemOneAdapter(MicrosoftProfile, srv.URL, "foundry-k", "decision-1")
	if _, err := a.Judge(context.Background(), &llm.JudgeRequest{StateJSON: `{}`, Questions: vendorQuestions()}, "u", nil); err != nil {
		t.Fatal(err)
	}
	if got.path != "/providers/microsoft/v1/systemone" || got.apiKey != "foundry-k" || got.auth != "" {
		t.Errorf("path %q api-key %q authorization %q", got.path, got.apiKey, got.auth)
	}
	_, err := a.Judge(context.Background(), &llm.JudgeRequest{
		StateMedia: []*llm.JudgeMediaPart{{Type: llm.JudgeMediaImage, Data: pngOf(t, 640, 480)}},
		Questions:  vendorQuestions(),
	}, "u", nil)
	if !errors.Is(err, llm.ErrJudgmentUnsupported) {
		t.Errorf("images on a text-only vendor: %v", err)
	}
}

// Errors name the vendor they came from.
func TestSystemOneErrorNamesVendor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down","type":"too_many_requests"}}`)
	}))
	defer srv.Close()
	_, err := NewSystemOneAdapter(PerplexityProfile, srv.URL, "k", "pplx").Judge(context.Background(),
		&llm.JudgeRequest{StateJSON: `{}`, Questions: vendorQuestions()}, "u", nil)
	var se *SystemOneError
	if !errors.As(err, &se) || !strings.HasPrefix(err.Error(), "perplexity: 429") || !llm.IsRetryableError(err) {
		t.Fatalf("err = %v", err)
	}
}

// Cloudflare without its envelope reads as a bare System One reply, and its
// images go as a data URL array.
func TestCloudflareImagesAndBareReply(t *testing.T) {
	var got vendorCall
	srv := vendorServer(t, vendorReply, &got)
	defer srv.Close()
	res, err := NewSystemOneAdapter(CloudflareProfile, srv.URL, "t", "clef").Judge(context.Background(), &llm.JudgeRequest{
		StateMedia: []*llm.JudgeMediaPart{{Type: llm.JudgeMediaImage, Data: jpegOf(t, 640, 480)}},
		Questions:  vendorQuestions(),
	}, "u", nil)
	if err != nil {
		t.Fatal(err)
	}
	checkVendorAnswers(t, res)
	var images []string
	_ = json.Unmarshal(got.body["images"], &images)
	if len(images) != 1 || !strings.HasPrefix(images[0], "data:image/jpeg;base64,") {
		t.Errorf("images = %.60v", images)
	}
}

// Only named images get a label: generated references stay inside the proxy.
func TestPerplexityLabelsOnlyNamedImages(t *testing.T) {
	var got vendorCall
	srv := vendorServer(t, vendorReply, &got)
	defer srv.Close()
	_, err := NewSystemOneAdapter(PerplexityProfile, srv.URL, "k", "pplx").Judge(context.Background(), &llm.JudgeRequest{
		StateMedia: []*llm.JudgeMediaPart{
			{Type: llm.JudgeMediaImage, Name: "road", Data: pngOf(t, 640, 480)},
			{Type: llm.JudgeMediaImage, Data: pngOf(t, 640, 480)},
		},
		Questions: vendorQuestions(),
	}, "u", nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(got.body["state"]); strings.Contains(s, "ktms-image") || !strings.Contains(s, "Image `road`:") || strings.Count(s, `"type":"image_url"`) != 2 {
		t.Errorf("state = %.300s", s)
	}
}

// Each vendor's documented limits are checked before the call.
func TestSystemOneVendorImageLimits(t *testing.T) {
	img := &llm.JudgeMediaPart{Type: llm.JudgeMediaImage, Data: pngOf(t, 640, 480)}
	nine := []*llm.JudgeMediaPart{img, img, img, img, img, img, img, img, img}
	if err := NewSystemOneAdapter(LiquidProfile, "http://unused", "k", "d1.2").CheckJudgeMedia(nine, nil); !errors.Is(err, llm.ErrInvalidMedia) {
		t.Errorf("liquid 9 images: %v", err)
	}
	if err := NewSystemOneAdapter(PerplexityProfile, "http://unused", "k", "pplx").CheckJudgeMedia(nine, nil); !errors.Is(err, llm.ErrInvalidMedia) {
		t.Errorf("perplexity 9 images: %v", err)
	}
	big := &llm.JudgeMediaPart{Type: llm.JudgeMediaImage, Data: pngOf(t, 4100, 4100)} // 16.8 MP
	err := NewSystemOneAdapter(CloudflareProfile, "http://unused", "k", "clef").CheckJudgeMedia([]*llm.JudgeMediaPart{big}, nil)
	if !errors.Is(err, llm.ErrInvalidMedia) || !strings.Contains(err.Error(), "within 1..16000000") {
		t.Errorf("cloudflare 16.8 MP: %v", err)
	}
	err = NewSystemOneAdapter(PerplexityProfile, "http://unused", "k", "pplx").CheckJudgeMedia([]*llm.JudgeMediaPart{big}, nil)
	if !errors.Is(err, llm.ErrInvalidMedia) {
		t.Errorf("perplexity 16.8 MP: %v", err)
	}
}

// 403 keeps its body (it says which permission is missing); only 401 is hidden.
func TestSystemOneErrorHidesOnly401(t *testing.T) {
	if s := (&SystemOneError{Vendor: "liquid", Status: 403, Body: "model not enabled"}).Error(); s != "liquid: 403 model not enabled" {
		t.Errorf("403 = %q", s)
	}
	if s := (&SystemOneError{Vendor: "liquid", Status: 401, Body: "bad key liquid_secret"}).Error(); strings.Contains(s, "secret") {
		t.Errorf("401 = %q", s)
	}
}

// One vendor's limits are not the request's fault: another model may take it.
// A name used twice is wrong for every vendor and ends the chain.
func TestSystemOneVendorLimitsYield(t *testing.T) {
	img := &llm.JudgeMediaPart{Type: llm.JudgeMediaImage, Data: pngOf(t, 640, 480)}
	nine := []*llm.JudgeMediaPart{img, img, img, img, img, img, img, img, img}
	err := NewSystemOneAdapter(LiquidProfile, "http://unused", "k", "d1.2").CheckJudgeMedia(nine, nil)
	if !errors.Is(err, llm.ErrInvalidMedia) || !llm.ShouldSkipToNextModel(err) {
		t.Errorf("9 images on Liquid: %v, skip %v", err, llm.ShouldSkipToNextModel(err))
	}
	err = NewTypeSafeAdapter("http://unused", "k", "jev").CheckJudgeMedia([]*llm.JudgeMediaPart{{Type: llm.JudgeMediaImage, Data: pngOf(t, 200, 200)}}, nil)
	if !llm.ShouldSkipToNextModel(err) {
		t.Errorf("too small for TypeSafe: %v should yield", err)
	}
	named := &llm.JudgeMediaPart{Type: llm.JudgeMediaImage, Name: "a", Data: pngOf(t, 640, 480)}
	err = NewSystemOneAdapter(LiquidProfile, "http://unused", "k", "d1.2").CheckJudgeMedia([]*llm.JudgeMediaPart{named, named}, nil)
	if !errors.Is(err, llm.ErrInvalidMedia) || llm.ShouldSkipToNextModel(err) {
		t.Errorf("name twice: %v should end the chain", err)
	}
}

// Liquid's 4.5 MB is the whole body, images included as base64.
func TestLiquidBodyLimitCountsBase64(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	noisy := noisyPNG(t, 1000, 900) // about 3.6 MB, about 4.8 MB as base64
	if len(noisy) >= 4_500_000 {
		t.Fatalf("test image is %d bytes; it should fit raw", len(noisy))
	}
	_, err := NewSystemOneAdapter(LiquidProfile, srv.URL, "k", "d1.2").Judge(context.Background(), &llm.JudgeRequest{
		StateMedia: []*llm.JudgeMediaPart{{Type: llm.JudgeMediaImage, Data: noisy}},
		Questions:  vendorQuestions(),
	}, "u", nil)
	if !errors.Is(err, llm.ErrInvalidMedia) || !llm.ShouldSkipToNextModel(err) || !strings.Contains(err.Error(), "max 4500000") || called {
		t.Errorf("err = %v, called %v", err, called)
	}
}

// The rollout pre-check knows about box questions, so it refuses what the call would.
func TestLiquidBoxPrecheck(t *testing.T) {
	img := &llm.JudgeMediaPart{Type: llm.JudgeMediaImage, Data: pngOf(t, 640, 480)}
	a := NewSystemOneAdapter(LiquidProfile, "http://unused", "k", "d1.2")
	box := map[string]*llm.JudgeQuestion{"hole": {Type: llm.JudgeBox, Instructions: "the pothole"}}
	if err := a.CheckJudgeMedia([]*llm.JudgeMediaPart{img, img}, box); !errors.Is(err, llm.ErrInvalidMedia) {
		t.Errorf("box with 2 images: %v", err)
	}
	if err := a.CheckJudgeMedia([]*llm.JudgeMediaPart{img, img}, vendorQuestions()); err != nil {
		t.Errorf("no box: %v", err)
	}
}

// Format, animation and EXIF rules are TypeSafe's; other vendors get only what
// their docs state (Cloudflare: PNG, JPEG or WebP).
func TestSystemOneFormatsPerVendor(t *testing.T) {
	webp, _ := base64.StdEncoding.DecodeString("UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==") // 1x1 lossless
	rotated := withOrientation(jpegOf(t, 640, 480), 6)
	gif := gifOf(t, 640, 480)
	check := func(p SystemOneProfile, data []byte) error {
		return NewSystemOneAdapter(p, "http://unused", "k", "m").CheckJudgeMedia([]*llm.JudgeMediaPart{{Type: llm.JudgeMediaImage, Data: data}}, nil)
	}
	for _, tc := range []struct {
		name    string
		profile SystemOneProfile
		data    []byte
		ok      bool
	}{
		{"perplexity rotated jpeg", PerplexityProfile, rotated, true},
		{"liquid gif", LiquidProfile, gif, true},
		{"cloudflare webp", CloudflareProfile, webp, true},
		{"cloudflare gif", CloudflareProfile, gif, false},
		{"typesafe webp", TypeSafeProfile, webp, false},
		{"typesafe rotated jpeg", TypeSafeProfile, rotated, false},
		{"liquid text", LiquidProfile, []byte("hello"), false},
	} {
		if err := check(tc.profile, tc.data); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok %v", tc.name, err, tc.ok)
		}
	}
	var got vendorCall
	srv := vendorServer(t, vendorReply, &got)
	defer srv.Close()
	_, err := NewSystemOneAdapter(CloudflareProfile, srv.URL, "k", "clef").Judge(context.Background(), &llm.JudgeRequest{
		StateMedia: []*llm.JudgeMediaPart{{Type: llm.JudgeMediaImage, Data: webp}}, Questions: vendorQuestions()}, "u", nil)
	var images []string
	_ = json.Unmarshal(got.body["images"], &images)
	if err != nil || len(images) != 1 || !strings.HasPrefix(images[0], "data:image/webp;base64,") {
		t.Errorf("err %v images %.40v", err, images)
	}
}

// Generated names stay inside the proxy: an unnamed image is null in the list.
func TestLiquidUnnamedImageIsNullNextToNamed(t *testing.T) {
	var got vendorCall
	srv := vendorServer(t, vendorReply, &got)
	defer srv.Close()
	_, err := NewSystemOneAdapter(LiquidProfile, srv.URL, "k", "d1.2").Judge(context.Background(), &llm.JudgeRequest{
		StateMedia: []*llm.JudgeMediaPart{
			{Type: llm.JudgeMediaImage, Name: "road", Data: pngOf(t, 640, 480)},
			{Type: llm.JudgeMediaImage, Data: pngOf(t, 640, 480)},
		},
		Questions: vendorQuestions(),
	}, "u", nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(got.body["state"]); s != `{"images":["road",null]}` {
		t.Errorf("state = %s", s)
	}
}

// A model id goes into the path as one escaped segment.
func TestCloudflareEscapesModelInPath(t *testing.T) {
	var uri string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uri = r.RequestURI
		_, _ = io.WriteString(w, vendorReply)
	}))
	defer srv.Close()
	_, _ = NewSystemOneAdapter(CloudflareProfile, srv.URL+"/run/@cf/cloudflare", "k", "@cf/x?y").Judge(context.Background(),
		&llm.JudgeRequest{StateJSON: `{}`, Questions: vendorQuestions()}, "u", nil)
	if uri != "/run/@cf/cloudflare/@cf%2Fx%3Fy" {
		t.Errorf("request uri = %q", uri)
	}
}

func noisyPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	r := rand.New(rand.NewPCG(1, 2))
	for i := range img.Pix {
		img.Pix[i] = byte(r.Uint32())
	}
	var b bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.NoCompression}).Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func gifOf(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := gif.Encode(&b, image.NewPaletted(image.Rect(0, 0, w, h), color.Palette{color.Black, color.White}), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
