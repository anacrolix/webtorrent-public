package main

import (
	"log"
	"net/http"

	_ "github.com/anacrolix/envpprof"
	"github.com/anacrolix/missinggo/expect"
	"github.com/anacrolix/missinggo/httptoo"
	"github.com/anacrolix/missinggo/v2/filecache"
	"github.com/anacrolix/tagflag"

	"github.com/anacrolix/webtorrent-public/services/transcoder"
)

func main() {
	log.SetFlags(log.Flags() | log.Lshortfile)
	var args = struct {
		Addr string
	}{
		Addr: "localhost:54228",
	}
	tagflag.Parse(&args)
	fc, err := filecache.NewCache("filecache")
	expect.Nil(err)
	t := &transcoder.Transcoder{
		RP: fc.AsResourceProvider(),
	}
	t.Init()
	httptoo.ClientTLSConfig(http.DefaultClient).InsecureSkipVerify = true
	expect.Nil(http.ListenAndServe(args.Addr, t))
}
