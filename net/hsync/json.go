package hsync

import (
	"encoding/json"
	"io"
)

func decodeJSON(r io.Reader, out any) error {
	return json.NewDecoder(r).Decode(out)
}
