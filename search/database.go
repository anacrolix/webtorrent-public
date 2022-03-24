package search

import (
	"context"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/tracker/udp"
	"github.com/dustin/go-humanize"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

func DatabaseInfos(ctx context.Context, conn *sqlite.Conn, query string) (ret Result) {
	query = escapeQuery(query)
	ret.Err = sqlitex.Exec(conn, `
        with
            match_info_hash as (
                select distinct info_file.info_id
                from info_fts(?, 'bm(2,1)')
                join info_file on info_fts.rowid=info_file.rowid
				limit 100000), -- We can't limit on final_info, because it will scan trying to find scrape_datetime is not null
            match_info as (
                select * from info where info_id in match_info_hash),
            final_info as (
                select * from match_info where scrape_datetime is not null
                union all select * from (
					select * from match_info where scrape_datetime is null limit 222))
        select
            infohash_hex,
            name,
            obtained_datetime,
            (select sum(length) from info_file where info_file.info_id=final_info.info_id),
            completed,
            leechers,
            seeders,
			scrape_datetime is not null
        from final_info
		order by seeders desc
		limit 100`,
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
			})
			return nil
		}, query)
	if ret.Err == nil {
		ret.Err = sqlitex.Exec(conn, `
			select count(*) from (
				select distinct info_file.info_id
				from info_fts(?)
				join info_file on info_fts.rowid=info_file.rowid
			)`,
			func(stmt *sqlite.Stmt) error {
				ret.Total = stmt.ColumnInt64(0)
				return nil
			},
			query,
		)
	}
	return
}

func escapeQuery(s string) string {
	fs := strings.Fields(s)
	for i, f := range fs {
		fs[i] = fmt.Sprintf(`"%s"`, strings.ReplaceAll(f, `"`, `""`))
	}
	return strings.Join(fs, " ")
}

func nonBreakingSpaces(s string) string {
	return strings.ReplaceAll(s, " ", "&nbsp;")
}
