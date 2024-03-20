package transcoder

import (
	"bufio"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/anacrolix/log"
)

type progressHandler struct {
	onInfo     func(id, key, value string)
	onProgress func(id string)
}

func (me *progressHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	sr := bufio.NewScanner(r.Body)
	for sr.Scan() {
		ss := strings.SplitN(sr.Text(), "=", 2)
		key := ss[0]
		if key == "progress" {
			me.onProgress(id)
		} else {
			me.onInfo(id, key, ss[1])
		}
	}
	err := sr.Err()
	if err != nil {
		log.Printf("error scanning ffmpeg progress: %s", err)
	}
	me.onProgress(id)
}

const progressInfoOutTimeKey = "out_time_ms"

func parseProgressInfoOutTime(s string) (ok time.Duration, err error) {
	i64, err := strconv.ParseInt(s, 0, 64)
	if s == "" {
		err = nil
	}
	ok = time.Duration(i64) * time.Microsecond
	return
}
