package main

import (
	"net/url"
	"testing"
)

func TestURLString(t *testing.T) {
	v, _ := url.ParseQuery(`i=http://webtorrent.anacrolix.link/30764610642571b3c01af11d6ce60cfa164d7ee3/file?path=Season%204%2fStar%20Trek%20The%20Next%20Generation%20Season%204%20Episode%2011%20-%20Data%27s%20Day.avi`)
	i := v.Get("i")
	t.Log(i)
	url_, _ := url.Parse(i)
	t.Logf("%#v", url_)
	url_.RawQuery = url_.Query().Encode()
	t.Log(url_.String())
	t.Log(url.QueryEscape(i))
}
