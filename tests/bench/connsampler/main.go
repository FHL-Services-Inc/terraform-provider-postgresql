// Command connsampler samples the number of concurrent PostgreSQL connections a
// client holds, so tests/bench/run.sh can report the peak during each plan.
//
// It keeps one connection open (connection settings from the usual PG* environment
// variables) and every -interval runs one query against pg_stat_activity. Each
// sample is a tab-separated line on stdout:
//
//	<unix time> <connections> <idle connections> <per-database counts>
//
// for example "1731571200.123	12	9	bench:10,postgres:2". Only client backends with
// the given application_name are counted; the provider sets "Terraform provider"
// (fallback_application_name). The sampler's own connection uses a different
// application_name and is excluded by pid as well. It runs until it is killed.
//
//	connsampler -interval 100ms > samples.tsv
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/lib/pq"
)

const query = `
WITH c AS (
	SELECT datname, state
	FROM pg_stat_activity
	WHERE backend_type = 'client backend'
	  AND application_name = $1
	  AND pid <> pg_backend_pid()
)
SELECT
	(SELECT count(*) FROM c),
	(SELECT count(*) FROM c WHERE state = 'idle'),
	coalesce((SELECT string_agg(datname || ':' || n, ',' ORDER BY datname)
	          FROM (SELECT coalesce(datname, '-') AS datname, count(*) AS n FROM c GROUP BY 1) d), '')`

func main() {
	interval := flag.Duration("interval", 100*time.Millisecond, "time between samples")
	app := flag.String("application-name", "Terraform provider", "application_name of the connections to count")
	flag.Parse()

	// Only the connection parameters come from the environment; the sampler names
	// itself so that it never matches -application-name.
	db, err := sql.Open("postgres", "application_name=pg-tf-bench-sampler")
	if err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	stmt, err := db.Prepare(query)
	if err != nil {
		log.Fatal(err)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	tick := time.NewTicker(*interval)
	defer tick.Stop()

	for {
		var total, idle int
		var perDB string
		if err := stmt.QueryRow(*app).Scan(&total, &idle, &perDB); err != nil {
			log.Fatal(err)
		}
		now := float64(time.Now().UnixNano()) / 1e9
		if _, err := fmt.Printf("%.3f\t%d\t%d\t%s\n", now, total, idle, perDB); err != nil {
			log.Fatal(err)
		}
		select {
		case <-stop:
			_ = db.Close()
			return
		case <-tick.C:
		}
	}
}
