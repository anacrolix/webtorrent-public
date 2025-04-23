package transcoder

import (
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/missinggo/v2/resource"
)

type Progress struct {
	Ready            bool
	Downloading      bool
	DownloadProgress float64
	Probing          bool
	Converting       bool
	ConvertPos       time.Duration
	InputDuration    time.Duration
	Queued           bool
	Storing          bool
	StoreProgress    g.Option[float64]
}

func (t *Transcoder) getProgress(outputLoc resource.Instance, outputName string) g.Option[Progress] {
	if resource.Exists(outputLoc) {
		return g.Some(Progress{
			Ready: true,
		})
	}
	t.mu.Lock()
	op := t.operations[outputName]
	t.mu.Unlock()
	if op == nil {
		return g.None[Progress]()
	}
	op.mu.Lock()
	defer op.mu.Unlock()
	return g.Some(op.Progress)
}
