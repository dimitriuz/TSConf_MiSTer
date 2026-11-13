package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// readLog writes lines to a file from a background goroutine. Producers never
// block: a line is dropped and counted when the queue is full.
type readLog struct {
	ch      chan string
	stop    chan struct{}
	done    chan struct{}
	dropped atomic.Int64
	once    sync.Once
}

func newReadLog(f *os.File) *readLog {
	l := &readLog{ch: make(chan string, 1<<16), stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(l.done)
		w := bufio.NewWriter(f)
		tk := time.NewTicker(100 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case s := <-l.ch:
				w.WriteString(s)
			case <-tk.C:
				w.Flush()
			case <-l.stop:
				for {
					select {
					case s := <-l.ch:
						w.WriteString(s)
					default:
						w.Flush()
						return
					}
				}
			}
		}
	}()
	return l
}

// add queues one read line: `ts_ns offset len kind lat_us`.
func (l *readLog) add(ts int64, off int64, n int, kind string, lat time.Duration) {
	if l == nil {
		return
	}
	l.line(fmt.Sprintf("%d %d %d %s %d\n", ts, off, n, kind, lat.Microseconds()))
}

func (l *readLog) line(s string) {
	if l == nil {
		return
	}
	select {
	case l.ch <- s:
	default:
		l.dropped.Add(1)
	}
}

// close flushes the queue and stops the writer; it is safe to call twice.
func (l *readLog) close() {
	if l == nil {
		return
	}
	l.once.Do(func() { close(l.stop) })
	<-l.done
	log.Printf("read log closed, dropped %d lines", l.dropped.Load())
}
