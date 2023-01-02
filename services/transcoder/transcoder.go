package transcoder

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/anacrolix/missinggo"
	"github.com/anacrolix/missinggo/pubsub"
	"github.com/anacrolix/missinggo/v2/resource"
	"github.com/dustin/go-humanize"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// 54228
func hashStrings(ss []string) []byte {
	h := md5.New()
	for _, s := range ss {
		h.Write([]byte(s))
	}
	return h.Sum(nil)
}

func (t *Transcoder) cacheFile(name string) (err error) {
	dstLoc, err := t.RP.NewInstance(filepath.Base(name))
	if err != nil {
		return
	}
	srcFile, err := os.Open(name)
	if err != nil {
		return
	}
	defer srcFile.Close()
	return dstLoc.Put(srcFile)
}

func reencodeURL(s string) string {
	url_, err := url.Parse(s)
	if err != nil || url_.Scheme == "" {
		// Why?
		return s
	}
	url_.RawQuery = url_.Query().Encode()
	return url_.String()
}

func ffmpegArgs(tempFilePath, progressListenerUrl, outputName, outputFilePath string, transcodeOpts []string) []string {
	return append(
		[]string{"nice", "ffmpeg", "-hide_banner", "-i", tempFilePath},
		append(
			transcodeOpts,
			"-progress", (&url.URL{
				Scheme: "http",
				Host:   progressListenerUrl,
				Path:   "/",
				RawQuery: (url.Values{
					"id": {outputName},
				}).Encode(),
			}).String(), "-y", outputFilePath,
		)...,
	)
}

func progressUpdater(op *operation) func(func(*Progress)) {
	return func(f func(*Progress)) {
		op.mu.Lock()
		defer op.mu.Unlock()
		before := op.Progress
		f(&op.Progress)
		if op.Progress != before {
			// log.Printf("%#v", op.Progress)
			op.sendEvent()
		}
	}
}

func (t *Transcoder) transcode(outputName, inputURL string, opts []string) (err error) {
	op := &operation{
		sendEvent: func() { t.events.Publish(struct{}{}) },
	}
	defer op.sendEvent()
	t.mu.Lock()
	// The operation needs to be visible for progress operations?
	t.operations[outputName] = op
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.operations, outputName)
		t.mu.Unlock()
	}()
	outputFilePath := filepath.Join(t.OutputDir, outputName)
	defer os.Remove(outputFilePath)

	tempFilePath := outputFilePath + ".input"
	outputLogFilePath := outputFilePath + ".log"
	err = transcode(inputURL, tempFilePath, outputLogFilePath, outputName, ffmpegArgs(
		tempFilePath,
		t.progressListener.Addr().String(),
		outputName,
		outputFilePath,
		opts,
	), progressUpdater(op))
	if err != nil {
		return
	}
	// Only remove the output log file if the operation succeeded. Note that
	// it is cached later.
	defer os.Remove(outputLogFilePath)

	log.Printf("completed %s: size: %s", outputName, func() string {
		fi, err := os.Stat(outputFilePath)
		if err != nil {
			return err.Error()
		}
		return humanize.Bytes(uint64(fi.Size()))
	}())
	started := time.Now()
	go t.cacheFile(outputLogFilePath)
	err = t.cacheFile(outputFilePath)
	if err != nil {
		return
	}
	log.Printf("stored files for %s in %s", outputName, time.Since(started))
	return
}

type operation struct {
	mu        sync.Mutex
	Progress  Progress
	sendEvent func()
}

type Transcoder struct {
	sf missinggo.SingleFlight
	RP resource.Provider
	// Where ffmpeg creates files.
	OutputDir        string
	progressListener net.Listener
	progressHandler  progressHandler
	mu               sync.Mutex
	operations       map[string]*operation
	events           *pubsub.PubSub
}

func (t *Transcoder) Init() {
	t.operations = make(map[string]*operation)
	var err error
	t.progressListener, err = net.Listen("tcp", "localhost:0")
	if err != nil {
		panic(err)
	}
	t.progressHandler.onInfo = func(id, key, value string) {
		if key != progressInfoOutTimeKey {
			return
		}
		t.mu.Lock()
		op := t.operations[id]
		t.mu.Unlock()
		if op == nil {
			return
		}
		progressUpdater(op)(func(p *Progress) {
			p.ConvertPos = parseProgressInfoOutTime(value)
		})
	}
	t.progressHandler.onProgress = func(id string) {
		t.mu.Lock()
		op := t.operations[id]
		t.mu.Unlock()
		if op == nil {
			return
		}
		op.sendEvent()
	}

	t.events = pubsub.NewPubSub()
	go func() {
		panic(http.Serve(t.progressListener, &t.progressHandler))
	}()
}

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
}

const progressInfoOutTimeKey = "out_time_ms"

func parseProgressInfoOutTime(s string) time.Duration {
	i64, err := strconv.ParseInt(s, 0, 64)
	if err != nil && s != "" {
		panic(err)
	}
	return time.Duration(i64) * time.Microsecond
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

func (t *Transcoder) serveEvents(w http.ResponseWriter, r *http.Request, outputName string, outputLoc resource.Instance) {
	sub := t.events.Subscribe()
	defer sub.Close()
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		log.Printf("error accepting transcoder events websocket: %v", err)
		return
	}
	defer conn.Close(websocket.StatusGoingAway, "deferred close")
	writeProgress := func() {
		p := t.getProgress(outputLoc, outputName)
		err := wsjson.Write(context.TODO(), conn, p)
		switch err {
		case io.ErrClosedPipe:
		case nil:
		default:
			log.Printf("error encoding transcoder event: %s", err)
		}
	}
	writeProgress()
	for {
		select {
		case v, ok := <-sub.Values:
			// Last I checked the pubsub just receives dummy values, to notify of an event on all
			// operations.
			if v != struct{}{} {
				panic(v)
			}
			if !ok {
				panic("subscription closed")
			}
			writeProgress()
		case <-r.Context().Done():
			return
		}
	}
}

func (t *Transcoder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	i := reencodeURL(q.Get("i"))
	f := q.Get("f")
	opts := q["opt"]
	outputName := fmt.Sprintf("%x.%s", hashStrings(append([]string{i}, opts...)), f)
	outputLoc, err := t.RP.NewInstance(outputName)
	if err != nil {
		log.Print(err)
		http.Error(w, "bad output location", http.StatusInternalServerError)
		return
	}
	if r.URL.Path == "/events" {
		t.serveEvents(w, r, outputName, outputLoc)
		return
	}
	// Ensure no-one else is operating on this outputName.
	sf := t.sf.Lock(outputName)
	for {
		if rs := resource.ReadSeeker(outputLoc); rs != nil {
			// We got a handle to it in the storage, it should be complete and
			// ready to go.
			sf.Unlock()
			http.ServeContent(w, r, outputName, time.Time{}, rs)
			return
		}
		err = t.transcode(outputName, i, opts)
		if err != nil {
			sf.Unlock()
			log.Printf("error transcoding %q: %s", outputName, err)
			http.Error(w, "error transcoding", http.StatusInternalServerError)
			return
		}
	}
}
