package helps

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// DeleteJSONFieldsIfPresent avoids sjson's full-buffer copy for absent fields.
func DeleteJSONFieldsIfPresent(body []byte, paths ...string) []byte {
	for _, path := range paths {
		if gjson.GetBytes(body, path).Exists() {
			body, _ = sjson.DeleteBytes(body, path)
		}
	}
	return body
}
