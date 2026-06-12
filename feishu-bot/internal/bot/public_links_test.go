package bot

import "testing"

func TestMarkdownLink(t *testing.T) {
	got := markdownLink("doc/foo[bar].md", "https://docs.pingcap.com/tidb/stable/foo/")
	want := "`doc/foo[bar].md`: https://docs.pingcap.com/tidb/stable/foo/"
	if got != want {
		t.Fatalf("unexpected markdownLink: got=%q want=%q", got, want)
	}
}

func TestWrapBareURLsForMarkdown_TrimsTrailingPunct(t *testing.T) {
	in := "see (https://example.com/foo). next"
	got := wrapBareURLsForMarkdown(in)
	want := "see (https://example.com/foo ). next"
	if got != want {
		t.Fatalf("unexpected wrap: got=%q want=%q", got, want)
	}
}

func TestWrapBareURLsForMarkdown_SkipsCodeFences(t *testing.T) {
	in := "ok\n```sh\ncurl https://example.com/foo).\n```\noutside https://example.com/bar)."
	got := wrapBareURLsForMarkdown(in)
	want := "ok\n```sh\ncurl https://example.com/foo).\n```\noutside https://example.com/bar )."
	if got != want {
		t.Fatalf("unexpected wrap: got=%q want=%q", got, want)
	}
}

func TestWrapBareURLsForMarkdown_SkipsExistingMarkdownLinks(t *testing.T) {
	in := "[x](https://example.com/foo) end"
	got := wrapBareURLsForMarkdown(in)
	if got != in {
		t.Fatalf("expected no change for existing markdown link: got=%q", got)
	}
}

func TestRewriteMarkdownLinksToAutolinks_Basic(t *testing.T) {
	in := "see [TiDB docs](https://docs.pingcap.com/tidb/stable/) end"
	got := rewriteMarkdownLinksToAutolinks(in)
	want := "see `TiDB docs`: https://docs.pingcap.com/tidb/stable/ end"
	if got != want {
		t.Fatalf("unexpected rewrite: got=%q want=%q", got, want)
	}
}

func TestRewriteMarkdownLinksToAutolinks_SkipsCodeFences(t *testing.T) {
	in := "ok\n```md\n[x](https://example.com/foo)\n```\noutside [y](https://example.com/bar)\n"
	got := rewriteMarkdownLinksToAutolinks(in)
	want := "ok\n```md\n[x](https://example.com/foo)\n```\noutside `y`: https://example.com/bar\n"
	if got != want {
		t.Fatalf("unexpected rewrite: got=%q want=%q", got, want)
	}
}

func TestRewriteMarkdownLinksToAutolinks_SkipsImages(t *testing.T) {
	in := "![img](https://example.com/a.png) and [link](https://example.com)"
	got := rewriteMarkdownLinksToAutolinks(in)
	want := "![img](https://example.com/a.png) and `link`: https://example.com"
	if got != want {
		t.Fatalf("unexpected rewrite: got=%q want=%q", got, want)
	}
}

func TestUnwrapAngleBracketAutolinks(t *testing.T) {
	in := "see <https://example.com/foo> end"
	got := unwrapAngleBracketAutolinks(in)
	want := "see https://example.com/foo end"
	if got != want {
		t.Fatalf("unexpected unwrap: got=%q want=%q", got, want)
	}
}
