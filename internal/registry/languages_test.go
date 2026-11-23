package registry

import (
	"testing"

	"github.com/ai-process/ktms/internal/proxydb"
)

func TestValidLanguageTag(t *testing.T) {
	for _, ok := range []string{"de", "pt-BR", "zh-Hant-TW", "gsw", "*"} {
		if !ValidLanguageTag(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "german", "de_DE", "d", "de-", "-de", "de--DE", "de-toolongsubtag"} {
		if ValidLanguageTag(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestServesLanguagePair(t *testing.T) {
	opus := &proxydb.Model{LanguagesSource: []string{"en", "fr"}, LanguagesTarget: []string{"de"}}
	anyModel := &proxydb.Model{LanguagesSource: []string{"*"}, LanguagesTarget: []string{"*"}}
	star := &proxydb.Model{LanguagesSource: []string{"*"}, LanguagesTarget: []string{"de-CH"}}
	cases := []struct {
		m        *proxydb.Model
		src, dst string
		want     bool
	}{
		{opus, "en", "de", true},
		{opus, "EN", "de-AT", true},
		{opus, "", "de", true},
		{opus, "it", "de", false},
		{opus, "en", "fr", false},
		{anyModel, "ja", "ko", true},
		{star, "ja", "de-CH", true},
		{star, "ja", "de", false},
		{&proxydb.Model{LanguagesSource: []string{"*"}, LanguagesTarget: []string{"zh-Hant"}}, "en", "zh-Hant-TW", true},
		{&proxydb.Model{LanguagesSource: []string{"*"}, LanguagesTarget: []string{"zh-Hant"}}, "en", "zh-Hans", false},
		{&proxydb.Model{LanguagesSource: []string{"*"}, LanguagesTarget: []string{"de"}}, "en", "den", false},
	}
	for _, c := range cases {
		if got := ServesLanguagePair(c.m, c.src, c.dst); got != c.want {
			t.Errorf("%v/%v %s->%s = %v, want %v", c.m.LanguagesSource, c.m.LanguagesTarget, c.src, c.dst, got, c.want)
		}
	}
}

func TestValidateModelLanguages(t *testing.T) {
	m := &proxydb.Model{ID: "opus", Vendor: VendorLocal, Efforts: []string{EffortLow}, LanguagesSource: []string{"english"}}
	if err := ValidateModel(m); err == nil {
		t.Error("a language name passed validation")
	}
	m.LanguagesSource, m.PricePerMillionChars = []string{"en"}, -1
	if err := ValidateModel(m); err == nil {
		t.Error("a negative character price passed validation")
	}
	m.PricePerMillionChars, m.Capabilities = 0, []string{CapabilityTranslate}
	if err := ValidateModel(m); err == nil {
		t.Error("a translate model without target languages passed validation")
	}
	m.LanguagesTarget = []string{"de"}
	if err := ValidateModel(m); err != nil {
		t.Errorf("valid translate model: %v", err)
	}
}
