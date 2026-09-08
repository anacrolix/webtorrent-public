package transcoder

import (
	"encoding/json"
	"testing"

	"github.com/go-quicktest/qt"
)

func TestJSONNaN(t *testing.T) {
	var zero float64
	b, err := json.Marshal(float64(0) / zero)
	qt.Assert(t, qt.IsNotNil(err))
	qt.Assert(t, qt.HasLen(b, 0))
}

func TestHashStrings(t *testing.T) {
	partsHash := hashStrings([]string{"h", "el", "lo"})
	oneHash := hashStrings([]string{"hello"})
	qt.Check(t, qt.Not(qt.DeepEquals(partsHash, oneHash)))
	qt.Check(t, qt.HasLen(partsHash, hashStringsSize))
	qt.Check(t, qt.HasLen(oneHash, hashStringsSize))
}
