// Command latencyproxy is a TCP proxy that adds a fixed one-way delay to traffic in
// both directions, so a local PostgreSQL behaves like one that is -delay away
// (round-trip time = 2 x delay).
//
// Data is pipelined, not throttled: every chunk read from one side is written to the
// other side `delay` after it was read, preserving order. A new connection also pays
// one extra round trip before its first client bytes are forwarded, to account for the
// TCP handshake a remote server would cost. TLS is not emulated; with sslmode=require a
// real remote server costs further round trips per connection.
//
//	latencyproxy -listen 127.0.0.1:25446 -upstream 127.0.0.1:25445 -delay 1ms
package main

import (
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

type chunk struct {
	data []byte
	due  time.Time
}

func main() {
	listen := flag.String("listen", "127.0.0.1:25446", "address to listen on")
	upstream := flag.String("upstream", "127.0.0.1:5432", "address to forward to")
	delay := flag.Duration("delay", 0, "one-way delay added in each direction")
	flag.Parse()

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("proxying %s -> %s with %s one-way delay (RTT +%s)", *listen, *upstream, *delay, 2**delay)

	for {
		c, err := ln.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go handle(c, *upstream, *delay)
	}
}

func handle(client net.Conn, upstream string, delay time.Duration) {
	server, err := net.Dial("tcp", upstream)
	if err != nil {
		log.Printf("dial %s: %v", upstream, err)
		_ = client.Close()
		return
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pipe(server, client, delay, 2*delay) }()
	go func() { defer wg.Done(); pipe(client, server, delay, 0) }()
	wg.Wait()
	_ = client.Close()
	_ = server.Close()
}

// pipe copies src to dst, delivering each chunk delay after it was read. The first
// chunk is held for an extra `first` to model connection setup.
func pipe(dst, src net.Conn, delay, first time.Duration) {
	ch := make(chan chunk, 1024)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for c := range ch {
			if d := time.Until(c.due); d > 0 {
				time.Sleep(d)
			}
			if _, err := dst.Write(c.data); err != nil {
				// The peer is gone: unblock the reader and drain what it queued.
				_ = src.Close()
				for range ch {
				}
				return
			}
		}
	}()

	extra := first
	buf := make([]byte, 64*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			ch <- chunk{data: data, due: time.Now().Add(delay + extra)}
			extra = 0
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				log.Printf("read: %v", err)
			}
			break
		}
	}
	close(ch)
	<-done
	// Propagate the half-close after the queued data has been delivered.
	if tc, ok := dst.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	} else {
		_ = dst.Close()
	}
}
