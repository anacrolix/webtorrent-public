package transcoder

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJSONNaN(t *testing.T) {
	var zero float64
	b, err := json.Marshal(float64(0) / zero)
	require.Error(t, err)
	require.Empty(t, b)
}
