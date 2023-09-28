package transcoder

import (
	"encoding/json"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stretchr/testify/require"
)

func TestJSONNaN(t *testing.T) {
	var zero float64
	b, err := json.Marshal(float64(0) / zero)
	require.Error(t, err)
	require.Empty(t, b)
}

func TestHashStrings(t *testing.T) {
	qtc := qt.New(t)
	partsHash := hashStrings([]string{"h", "el", "lo"})
	oneHash := hashStrings([]string{"hello"})
	qtc.Check(partsHash, qt.Not(qt.DeepEquals), oneHash)
	qtc.Check(partsHash, qt.HasLen, hashStringsSize)
	qtc.Check(oneHash, qt.HasLen, hashStringsSize)
}
