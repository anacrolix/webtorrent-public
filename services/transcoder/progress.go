package transcoder

import (
	"encoding/json"
	"net/http"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/log"
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

func (t *Transcoder) getProgress(outputLoc resource.Instance, outputName string) (ret Progress) {
	if resource.Exists(outputLoc) {
		ret.Ready = true
		return
	}
	t.mu.Lock()
	op := t.operations[outputName]
	t.mu.Unlock()
	if op == nil {
		return
	}
	op.mu.Lock()
	defer op.mu.Unlock()
	return op.Progress
}

func (t *Transcoder) serveProgress(w http.ResponseWriter, r *http.Request, outputName string, outputLoc resource.Instance) {
	p := t.getProgress(outputLoc, outputName)
	err := json.NewEncoder(w).Encode(p)
	if err != nil {
		log.Print(err)
	}
}
