package helps

import (
	"bytes"
	"testing"

	"github.com/tidwall/sjson"
)

func TestDeleteJSONFieldsIfPresentKeepsNoopAndDeletionSemantics(t *testing.T) {
	fields := []string{"previous_response_id", "prompt_cache_retention", "safety_identifier", "stream_options"}
	absent := []byte(`{"model":"muse","input":[{"text":"retained"}]}`)
	got := DeleteJSONFieldsIfPresent(absent, fields...)
	if &got[0] != &absent[0] {
		t.Fatal("absent fields must retain buffer")
	}
	for _, raw := range []string{`{"previous_response_id":null,"input":[],"stream_options":{}}`, `{"previous_response_id":"a","previous_response_id":"b","prompt_cache_retention":"1h","safety_identifier":"x","input":[]}`} {
		input := []byte(raw)
		expected := bytes.Clone(input)
		for _, key := range fields {
			expected, _ = sjson.DeleteBytes(expected, key)
		}
		if got := DeleteJSONFieldsIfPresent(input, fields...); !bytes.Equal(got, expected) {
			t.Fatalf("changed deletion semantics: %s", got)
		}
	}
}
