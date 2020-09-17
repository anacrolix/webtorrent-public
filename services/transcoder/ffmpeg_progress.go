package transcoder

import (
	"bufio"
	"log"
	"net/http"
	"strings"
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
