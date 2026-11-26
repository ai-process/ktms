package server

import (
	"encoding/json"
	"testing"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

func TestDecisionsBoxAndNamedImage(t *testing.T) {
	fp, h, key := newTestAPI(t)
	body := `{"input":[{"type":"input_image","name":"road","image_url":"data:image/png;base64,iVBORw0KGgo="}],
		"questions":[{"name":"crack","type":"box","instructions":"the crack in ` + "`road`" + `"}]}`
	rec := do(h, "POST", "/v1/decisions", key, body)
	if rec.Code != 200 {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body)
	}
	if fp.judgeReq.GetQuestions()["crack"].GetType() != pb.JudgeType_JUDGE_TYPE_BOX || fp.judgeReq.GetStateMedia()[0].GetName() != "road" {
		t.Fatalf("request = %v", fp.judgeReq)
	}
	var out struct {
		Answers []struct {
			Type       string             `json:"type"`
			Box        map[string]float64 `json:"box"`
			Confidence *float64           `json:"confidence"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Answers) != 1 {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	a := out.Answers[0]
	if a.Type != "box" || a.Box["right"] != 0.5 || a.Confidence != nil {
		t.Fatalf("answer = %+v, want a box and no confidence", a)
	}
}
