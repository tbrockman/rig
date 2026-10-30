package main

import (
	"context"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Following a log that rig can only read whole or by its tail. Incus has no
// stream for a console log, and the agent's files are read with a bounded
// exec, so -f polls and prints what each read adds to the last.

// tailLines keeps the last n lines of s; n <= 0 keeps everything.
func tailLines(s string, n int) string {
	if n <= 0 {
		return s
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "")
}

// consoleDelta is what cur adds to prev. A console log is a bounded buffer
// that drops its head as it grows, so after it wraps cur is a tail of prev
// with more after it: find where cur's start sits in prev, earliest first so
// the overlap is the longest. Nothing in common means a new buffer, such as a
// VM that restarted, and all of it is new.
func consoleDelta(prev, cur string) string {
	if cur == "" {
		return ""
	}
	for from := 0; ; {
		i := strings.IndexByte(prev[from:], cur[0])
		if i < 0 {
			return cur
		}
		k := from + i
		if strings.HasPrefix(cur, prev[k:]) {
			return cur[len(prev)-k:]
		}
		from = k + 1
	}
}

// newSince is the part of window not already shown. window is the latest tail
// of a stream and seen what was shown of it, so the longest run that ends seen
// and starts window is the part read twice. seen must reach further back than
// window does, which followPolled ensures by seeding it with a whole read.
func newSince(seen, window []string) []string {
	for m := min(len(seen), len(window)); m > 0; m-- {
		if slices.Equal(window[:m], seen[len(seen)-m:]) {
			return window[m:]
		}
	}
	return window
}

// followPolled shows what each read of a stream adds, every interval, until
// interrupted. seen is what the first read returned, shown or not.
func followPolled(every time.Duration, seen []string, read func() ([]string, error), show func(string)) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(every):
		}
		window, err := read()
		if err != nil {
			return err
		}
		fresh := newSince(seen, window)
		for _, s := range fresh {
			show(s)
		}
		seen = append(seen, fresh...)
		if len(seen) > 2000 {
			seen = seen[len(seen)-2000:]
		}
	}
}

// followConsole is followPolled for a console log, which is read whole.
func followConsole(every time.Duration, prev string, read func() (string, error), show func(string)) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(every):
		}
		cur, err := read()
		if err != nil {
			return err
		}
		if d := consoleDelta(prev, cur); d != "" {
			show(d)
		}
		prev = cur
	}
}
