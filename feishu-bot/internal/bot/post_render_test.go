package bot

import (
	"encoding/json"
	"testing"
)

func TestRenderPostRichText_SingleURL(t *testing.T) {
	_, raw := renderPostRichText("## TiCDC docs\n\nTiCDC docs: https://docs.pingcap.com/tidb/stable/ticdc/")

	var parsed map[string]postOutPayload
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("unmarshal post content: %v", err)
	}
	p, ok := parsed["zh_cn"]
	if !ok {
		t.Fatalf("missing zh_cn payload")
	}
	if p.Title != "TiCDC docs" {
		t.Fatalf("unexpected title: %q", p.Title)
	}
	if len(p.Content) != 1 {
		t.Fatalf("unexpected rows: %d", len(p.Content))
	}
	row := p.Content[0]
	if len(row) < 2 {
		t.Fatalf("expected at least 2 elements, got %d", len(row))
	}
	if row[0].Tag != "text" || row[0].Text != "TiCDC docs: " {
		t.Fatalf("unexpected first element: %+v", row[0])
	}
	if row[1].Tag != "a" || row[1].Href != "https://docs.pingcap.com/tidb/stable/ticdc/" || row[1].Text != "https://docs.pingcap.com/tidb/stable/ticdc/" {
		t.Fatalf("unexpected link element: %+v", row[1])
	}
}

func TestRenderPostRichText_MultipleURLsKeepsSpace(t *testing.T) {
	_, raw := renderPostRichText("https://example.com/a https://example.com/b")

	var parsed map[string]postOutPayload
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("unmarshal post content: %v", err)
	}
	row := parsed["zh_cn"].Content[0]
	// Expect: a + text(" ") + a (or merged in some equivalent that preserves the space)
	var rendered string
	for _, el := range row {
		switch el.Tag {
		case "text":
			rendered += el.Text
		case "a":
			rendered += el.Text
		}
	}
	if rendered != "https://example.com/a https://example.com/b" {
		t.Fatalf("space between urls lost: got=%q", rendered)
	}
}

func TestRenderPostRichText_Bullets(t *testing.T) {
	_, raw := renderPostRichText("- item https://example.com")

	var parsed map[string]postOutPayload
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("unmarshal post content: %v", err)
	}
	row := parsed["zh_cn"].Content[0]
	if len(row) == 0 || row[0].Tag != "text" {
		t.Fatalf("unexpected first element: %+v", row)
	}
	if row[0].Text != "• item " {
		t.Fatalf("unexpected bullet normalization: %q", row[0].Text)
	}
}

func TestRenderPostRichText_DoesNotLinkifyAnglePlaceholder(t *testing.T) {
	_, raw := renderPostRichText("TiCDC server: http://<ticdc-host>:8300")

	var parsed map[string]postOutPayload
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("unmarshal post content: %v", err)
	}
	row := parsed["zh_cn"].Content[0]
	for _, el := range row {
		if el.Tag == "a" {
			t.Fatalf("unexpected link element for placeholder url: %+v", el)
		}
	}
}
