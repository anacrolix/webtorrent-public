package search

import (
	"context"
	"fmt"
	"html/template"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/tracker/udp"
	"github.com/dustin/go-humanize"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type InfosQuery struct {
	Query  string
	Limit  int
	Offset int
	DoTags bool
}

func DatabaseInfos(ctx context.Context, conn *sqlite.Conn, iq InfosQuery) (ret Result) {
	query := EscapeQuery(iq.Query)
	// This version of the query requires that info_fts be clustered by the intended result
	// ordering, such as seeders desc, length desc.
	conn.SetInterrupt(ctx.Done())
	ret.Err = sqlitex.Exec(conn, `
        with
            matched_info_id as (
                select distinct info_file.info_id
                from info_fts(?)
                join info_file on info_fts.rowid=info_file.rowid
				limit ? offset ? ), 
            matched_info as (
                select * from info where info_id in matched_info_id )
        select
            infohash_hex,
            name,
            obtained_datetime,
            (select sum(length) from info_file where info_file.info_id=matched_info.info_id),
            completed,
            leechers,
            seeders,
			scrape_datetime is not null
        from matched_info`,
		func(stmt *sqlite.Stmt) error {
			infohashHex := stmt.ColumnText(0)
			infoName := stmt.ColumnText(1)
			age := stmt.ColumnText(2)
			size := humanize.Bytes(uint64(stmt.ColumnInt64(3)))
			m := metainfo.Magnet{
				DisplayName: infoName,
			}
			// Mon Jan 2 15:04:05 -0700 MST 2006
			ageTime, _ := time.Parse("2006-01-02 15:04:05", age)
			err := m.InfoHash.FromHexString(infohashHex)
			if err != nil {
				panic(fmt.Errorf("parsing infohash hex %q: %w", infohashHex, err))
			}
			var exts sort.StringSlice
			var veryNice bool
			if iq.DoTags {
				infoFiles, _, err := infoFilesFromDatabase(conn, m.InfoHash)
				if err != nil {
					panic(err)
				}
				exts = mapStringEmptyStructToSlice(infoDistinctFileExts(infoFiles))
				exts.Sort()
				veryNice = infoLargestFileExt(infoFiles) == ".mp4"
			}
			ret.Items = append(ret.Items, ResultItem{
				Name:                   infoName,
				Magnet:                 m.String(),
				TrustedOriginResultUrl: template.URL(m.String()),
				Age: template.HTML(fmt.Sprintf("%v (%v)",
					age,
					nonBreakingSpaces(strings.TrimSpace(humanize.RelTime(
						ageTime, time.Now(), "", "in the future"))))),
				OriginTag: "dht",
				SwarmInfo: udp.ScrapeInfohashResult{
					Completed: stmt.ColumnInt32(4),
					Leechers:  stmt.ColumnInt32(5),
					Seeders:   stmt.ColumnInt32(6),
				},
				NoSwarmInfo: stmt.ColumnInt64(7) == 0,
				Size:        size,
				Tags:        exts,
				TagsOk:      iq.DoTags,
				VeryNice:    veryNice,
			})
			return nil
		}, query, iq.Limit, iq.Offset)
	if ret.Err == nil {
		ret.Total = int64(len(ret.Items))
		// This is too expensive to calculate!
		//ret.Err = sqlitex.Exec(conn, `
		//	select count(1) from (
		//		select distinct info_file.info_id
		//		from info_fts(?)
		//		join info_file on info_fts.rowid=info_file.rowid
		//	)`,
		//	func(stmt *sqlite.Stmt) error {
		//		ret.Total = stmt.ColumnInt64(0)
		//		return nil
		//	},
		//	query,
		//)
	}
	return
}

// Escape query for use as a sqlite FTS5 query.
func EscapeQuery(s string) string {
	fs := strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		// From info_fts' tokenize tokenchars argument
		case '&', '-', '\'':
			return false
		}
		// sqlite FTS5 4.3.1. Unicode61 Tokenizer: "By default all space and punctuation characters,
		// as defined by Unicode 6.1, are considered separators, and all other characters as token
		// characters."
		return unicode.IsSpace(r) || unicode.IsPunct(r)
	})
	for i, f := range fs {
		fs[i] = fmt.Sprintf(`"%s"`, strings.ReplaceAll(f, `"`, `""`))
	}
	return strings.Join(fs, " ")
}

func nonBreakingSpaces(s string) string {
	return strings.ReplaceAll(s, " ", "&nbsp;")
}

func infoLargestFileExt(info infoFiles) string {
	var ret string
	var largest int64
	for _, f := range info.Files {
		if f.Length >= largest {
			ret = path.Ext(f.DisplayPath)
			largest = f.Length
		}
	}
	return ret
}

func mapStringEmptyStructToSlice(m map[string]struct{}) (ret []string) {
	for s := range m {
		ret = append(ret, s)
	}
	return
}

// The files-only part of a torrent info.
type infoFiles struct {
	Name  string
	Files []infoFile
}

type infoFile struct {
	Length      int64
	DisplayPath string
}

func infoFilesFromDatabase(c *sqlite.Conn, ih torrent.InfoHash) (ret infoFiles, ok bool, err error) {
	err = sqlitex.Exec(c, `
		select name
		from info where infohash_hex=?`,
		func(stmt *sqlite.Stmt) error {
			ok = true
			ret.Name = stmt.ColumnText(0)
			return nil
		},
		ih.HexString(),
	)
	if err != nil {
		return
	}
	if !ok {
		return
	}
	err = sqlitex.Exec(c, `
		select length, path from info_file where info_id=(select info_id from info where infohash_hex=?) order by file_index`,
		func(stmt *sqlite.Stmt) error {
			dp := ret.Name
			if stmt.ColumnType(1) != sqlite.TypeNull {
				dp = stmt.ColumnText(1)
			}
			ret.Files = append(ret.Files, infoFile{
				Length:      stmt.ColumnInt64(0),
				DisplayPath: dp,
			})
			return nil
		},
		ih.HexString())
	return
}

func infoDistinctFileExts(info infoFiles) (exts map[string]struct{}) {
	exts = make(map[string]struct{})
	for _, f := range info.Files {
		ext := path.Ext(f.DisplayPath)
		if regexp.MustCompile(`\.r\d{2}`).MatchString(ext) {
			continue
		}
		exts[ext] = struct{}{}
	}
	return
}
