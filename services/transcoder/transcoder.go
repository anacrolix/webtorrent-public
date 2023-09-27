package transcoder

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"resenje.org/singleflight"
	"strconv"
	"sync"
	"time"

	"github.com/anacrolix/log"
	"github.com/anacrolix/missinggo/v2/pubsub"
	"github.com/anacrolix/missinggo/v2/resource"
	"github.com/dustin/go-humanize"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

var hashStringsSize = md5.Size

// 54228
func hashStrings(ss []string) []byte {
	h := md5.New()
	var b []byte
	for _, s := range ss {
		b = h.Sum(b[:0])
		h.Write(b)
		h.Write([]byte(s))
	}
	return h.Sum(b[:0])
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

func ffmpegArgs(
	tempFilePath, progressListenerUrl, outputName, outputFilePath string,
	outputOpts, inputOpts []string,
) (ret []string) {
	_, err := exec.LookPath("nice")
	if err == nil {
		// Windows does not have nice (things lol).
		ret = append(ret, "nice")
	}
	ret = append(ret, "ffmpeg", "-hide_banner")
	ret = append(ret, inputOpts...)
	ret = append(ret, "-i", tempFilePath)
	ret = append(ret, outputOpts...)
	ret = append(ret,
		"-progress", (&url.URL{
			Scheme: "http",
			Host:   progressListenerUrl,
			Path:   "/",
			RawQuery: (url.Values{
				"id": {outputName},
			}).Encode(),
		}).String(),
		"-y", outputFilePath)
	return
}

func (op *operation) updateProgress(f func(p *Progress)) {
	op.mu.Lock()
	defer op.mu.Unlock()
	before := op.Progress
	f(&op.Progress)
	if op.Progress != before {
		// log.Printf("%#v", op.Progress)
		op.sendEvent()
	}
}

func (t *Transcoder) transcode(
	ctx context.Context,
	outputName,
	inputURL string,
	opts, iopts []string,
) (err error) {
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
	err = transcode(
		ctx,
		inputURL,
		tempFilePath,
		outputLogFilePath,
		outputName,
		ffmpegArgs(
			tempFilePath,
			t.progressListener.Addr().String(),
			outputName,
			outputFilePath,
			opts,
			iopts,
		),
		op.updateProgress,
	)
	if err != nil {
		if ctx.Err() != nil {
			os.Remove(outputLogFilePath)
		}
		return
	}
	// Only remove the output log file if the operation succeeded. Note that it is cached later.
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
	op.updateProgress(func(p *Progress) {
		p.Storing = true
	})
	defer op.updateProgress(func(p *Progress) {
		p.Storing = false
	})
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
	sf singleflight.Group[string, struct{}]
	RP resource.Provider
	// Where ffmpeg creates files.
	OutputDir        string
	progressListener net.Listener
	progressHandler  progressHandler
	mu               sync.Mutex
	operations       map[string]*operation
	events           pubsub.PubSub[struct{}]
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
		op.updateProgress(func(p *Progress) {
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
	iopts := q["iopt"]
	outputName := fmt.Sprintf(
		"%x.%s",
		hashStrings(append(iopts, append(opts, i)...)),
		f,
	)
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
	for {
		if rs := resource.ReadSeeker(outputLoc); rs != nil {
			http.ServeContent(w, r, outputName, time.Time{}, rs)
			return
		}
		_, _, err := t.sf.Do(
			r.Context(),
			outputName,
			func(ctx context.Context) (_ struct{}, err error) {
				err = t.transcode(ctx, outputName, i, opts, iopts)
				if err != nil {
					log.Printf("error transcoding %q: %s", outputName, err)
				}
				return
			},
		)
		if err != nil {
			http.Error(w, "error transcoding", http.StatusInternalServerError)
			return
		}
	}
}
