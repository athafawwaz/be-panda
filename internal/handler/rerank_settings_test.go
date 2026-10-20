package handler

import "testing"

func TestValidateRerankSettings(t *testing.T) {
	five, nine := 5, 9
	cases := []struct {
		req  rerankSettingsRequest
		want bool // true = valid
	}{
		{rerankSettingsRequest{Provider: "none"}, true},
		{rerankSettingsRequest{Provider: ""}, true},
		{rerankSettingsRequest{Provider: "cohere", BaseURL: "https://api.cohere.com/v2", Model: "rerank-v3.5"}, true},
		{rerankSettingsRequest{Provider: "voyage", BaseURL: "https://api.voyageai.com/v1", Model: "rerank-2", Candidates: &five}, true},
		{rerankSettingsRequest{Provider: "openai", BaseURL: "https://x", Model: "m"}, false},
		{rerankSettingsRequest{Provider: "jina", BaseURL: "api.jina.ai", Model: "m"}, false},
		{rerankSettingsRequest{Provider: "jina", BaseURL: "https://api.jina.ai/v1"}, false},
		{rerankSettingsRequest{Provider: "jina", BaseURL: "https://api.jina.ai/v1", Model: "m", TopN: &nine}, false},
	}
	for _, c := range cases {
		if got := validateRerankSettings(c.req) == ""; got != c.want {
			t.Errorf("%+v: valid=%v, seharusnya %v (%s)", c.req, got, c.want, validateRerankSettings(c.req))
		}
	}
}
