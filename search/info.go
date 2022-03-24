package search

import (
	"html/template"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/tracker/udp"
)

type Result struct {
	Items []ResultItem
	Total int64
	Err   error
}

// Represents a single search result item.
type ResultItem struct {
	Name        string
	Magnet      string
	SwarmInfo   udp.ScrapeInfohashResult
	NoSwarmInfo bool
	// The search result page on the origin.
	OriginResultURL string
	// The origin URL but from a source we trust to return from a executed template.
	TrustedOriginResultUrl template.URL
	Size                   string
	Age                    interface{}
	OriginTag              string
	Trusted                bool
}

func (sr ResultItem) InfoHash() metainfo.Hash {
	m, _ := metainfo.ParseMagnetUri(sr.Magnet)
	return m.InfoHash
}
