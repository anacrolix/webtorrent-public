package transcoder

import (
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/anacrolix/log"
	"github.com/anacrolix/missinggo/v2/pubsub"
	"github.com/anacrolix/missinggo/v2/resource"
	"github.com/dustin/go-humanize"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
	"resenje.org/singleflight"
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

func (t *Transcoder) cacheFile(name string, progress func(f float64)) (err error) {
	dstLoc, err := t.RP.NewInstance(filepath.Base(name))
	if err != nil {
		return
	}
	srcFile, err := os.Open(name)
	if err != nil {
		return
	}
	defer srcFile.Close()
	var pw progressWriter
	pw.callback = progress
	pw.total, _ = srcFile.Seek(0, io.SeekEnd)
	srcFile.Seek(0, io.SeekStart)
	return dstLoc.Put(io.TeeReader(srcFile, &pw))
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
	go t.cacheFile(outputLogFilePath, func(float64) {})
	op.updateProgress(func(p *Progress) {
		p.Storing = true
	})
	defer op.updateProgress(func(p *Progress) {
		p.Storing = false
	})
	err = t.cacheFile(outputFilePath, func(f float64) {
		op.updateProgress(func(p *Progress) {
			p.StoreProgress.Set(f)
		})
	})
	if err != nil {
		return
	}
	log.Printf("stored files for %s in %s", outputName, time.Since(started))
	return
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
			var err error
			p.ConvertPos, err = parseProgressInfoOutTime(value)
			if err != nil {
				log.Levelf(log.Warning, "error parsing out_time_ms for operation %q: %s", id, err)
			}
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

func (t *Transcoder) serveEvents(
	w http.ResponseWriter,
	r *http.Request,
	outputName string,
	outputLoc resource.Instance,
) {
	sub := t.events.Subscribe()
	defer sub.Close()
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		log.Printf("error accepting transcoder events websocket: %v", err)
		return
	}
	defer conn.Close(websocket.StatusGoingAway, "deferred close")
	writeProgress := func() bool {
		p := t.getProgress(outputLoc, outputName)
		err := wsjson.Write(context.TODO(), conn, p)
		switch err {
		case io.ErrClosedPipe:
			return false
		case nil:
			return true
		default:
			log.Printf("error encoding transcoder event: %s", err)
			return false
		}
	}
	if !writeProgress() {
		return
	}
	for {
		select {
		case v, ok := <-sub.Values:
			if !ok {
				panic("subscription closed")
			}
			t.assertEventValue(v)
			// We don't care what the events are so throw away as many as we can to minimize
			// progress writes. TODO: Ignore events that aren't related to the outputName.
			t.drainEventSub(sub)
			if !writeProgress() {
				return
			}
			select {
			case <-time.After(100 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

func (t *Transcoder) drainEventSub(sub *pubsub.Subscription[struct{}]) {
	for {
		select {
		case v, ok := <-sub.Values:
			if !ok {
				return
			}
			t.assertEventValue(v)
		default:
			return
		}
	}
}

func (t *Transcoder) assertEventValue(value any) {
	// Last I checked the pubsub just receives dummy values, to notify of an event on all
	// operations.
	if value != struct{}{} {
		panic(value)
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
