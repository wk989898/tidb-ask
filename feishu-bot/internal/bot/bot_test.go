package bot

import (
	"testing"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func TestParsePostContent_Direct(t *testing.T) {
	raw := `{"title":"","content":[[{"tag":"text","text":"hello world"}]]}`
	got := parsePostContent(&raw)
	if got != "hello world" {
		t.Fatalf("unexpected parse result: %q", got)
	}
}

func TestParsePostContent_LocaleWrapped(t *testing.T) {
	raw := `{"zh_cn":{"title":"标题","content":[[{"tag":"text","text":"正文"}]]}}`
	got := parsePostContent(&raw)
	want := "标题\n正文"
	if got != want {
		t.Fatalf("unexpected parse result: got=%q want=%q", got, want)
	}
}

func TestParsePostContent_PostWrapper(t *testing.T) {
	raw := `{"post":{"zh_cn":{"title":"","content":[[{"tag":"text","text":"a"},{"tag":"at","user_id":"ou_xxx","user_name":"bot"},{"tag":"text","text":"b"}]]}}}`
	got := parsePostContent(&raw)
	if got != "ab" {
		t.Fatalf("unexpected parse result: %q", got)
	}
}

func TestExtractLanguageOverride(t *testing.T) {
	cases := []struct {
		in        string
		wantLang  string
		wantClean string
	}{
		{"en hello", "en", "hello"},
		{"EN: hello", "en", "hello"},
		{"/en hello", "en", "hello"},
		{"zh 你好", "zh", "你好"},
		{"zh-cn：你好", "zh", "你好"},
		{"中文：你好", "zh", "你好"},
		{"英文 hello", "en", "hello"},
		{"lang=en: hello", "en", "hello"},
		{"language = zh 你好", "zh", "你好"},
		{"reply=english hello", "en", "hello"},
		{"en", "en", ""},
		{"", "", ""},
	}

	for _, tc := range cases {
		lang, clean := extractLanguageOverride(tc.in)
		if lang != tc.wantLang || clean != tc.wantClean {
			t.Fatalf("extractLanguageOverride(%q) got=(%q,%q) want=(%q,%q)", tc.in, lang, clean, tc.wantLang, tc.wantClean)
		}
	}

	// No override: return original text unchanged.
	in := "encryption key rotation"
	lang, clean := extractLanguageOverride(in)
	if lang != "" || clean != in {
		t.Fatalf("no-override case got=(%q,%q) want=(%q,%q)", lang, clean, "", in)
	}
}

func TestIsBotMentionedByOpenID(t *testing.T) {
	s := func(v string) *string { return &v }

	mentions := []*larkim.MentionEvent{
		{Id: &larkim.UserId{OpenId: s("ou_other")}},
		{Id: &larkim.UserId{OpenId: s("ou_bot")}},
	}

	if !isBotMentionedByOpenID(mentions, "ou_bot") {
		t.Fatalf("expected bot to be mentioned")
	}
	if !isBotMentionedByOpenID(mentions, " ou_bot ") {
		t.Fatalf("expected bot to be mentioned (with trimming)")
	}
	if isBotMentionedByOpenID(mentions, "ou_missing") {
		t.Fatalf("expected bot to NOT be mentioned")
	}
	if isBotMentionedByOpenID(nil, "ou_bot") {
		t.Fatalf("expected nil mentions to be false")
	}
	if isBotMentionedByOpenID(mentions, "") {
		t.Fatalf("expected empty bot open_id to be false")
	}
}
