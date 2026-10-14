package main

import (
	"reflect"
	"testing"
)

func TestOnlyModelFlag(t *testing.T) {
	s := newSettings("test")
	if err := s.fs.Parse([]string{"--only-model", "a:1b, b:2b", "--only-model=c:3b", "--only-model", ","}); err != nil {
		t.Fatal(err)
	}
	want := []string{"a:1b", "b:2b", "c:3b"}
	if !reflect.DeepEqual([]string(s.onlyModels), want) {
		t.Errorf("only-model = %v, want %v", s.onlyModels, want)
	}
}
