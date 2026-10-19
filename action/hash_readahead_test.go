package action

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"
)

func readAheadData(n int) []byte {
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i*31 + i>>8)
	}
	return data
}

func TestReadAheadCopyHashesExactlyWhatIoCopyDoes(t *testing.T) {
	e := NewEngine(EngineDeps{})
	for _, size := range []int{0, 1, hashReadChunk - 1, hashReadChunk, hashReadChunk + 1, 5*hashReadChunk + 7} {
		data := readAheadData(size)
		want := sha256.Sum256(data)
		for name, wrap := range map[string]func(io.Reader) io.Reader{
			"plain":    func(r io.Reader) io.Reader { return r },
			"half":     iotest.HalfReader,
			"one byte": func(r io.Reader) io.Reader { return io.LimitReader(iotest.OneByteReader(r), int64(size)) },
			"data+eof": iotest.DataErrReader,
		} {
			if name == "one byte" && size > 1<<16 {
				continue
			}
			h := sha256.New()
			n, err := e.hashMedia(context.Background(), h, wrap(bytes.NewReader(data)))
			if err != nil || n != int64(size) {
				t.Fatalf("%d bytes via %s: read %d, %v", size, name, n, err)
			}
			if got := fmt.Sprintf("%x", h.Sum(nil)); got != fmt.Sprintf("%x", want) {
				t.Fatalf("%d bytes via %s: digest %s, want %x", size, name, got, want)
			}
		}
	}
}

// A read error is returned only after every byte before it was written, so a
// short file never hashes as if it were complete.
func TestReadAheadCopyReportsAReadErrorAfterTheBytesBeforeIt(t *testing.T) {
	e := NewEngine(EngineDeps{})
	boom := errors.New("link dropped")
	data := readAheadData(3*hashReadChunk + 5)
	var out bytes.Buffer
	n, err := e.hashMedia(context.Background(), &out, io.MultiReader(bytes.NewReader(data), iotest.ErrReader(boom)))
	if !errors.Is(err, boom) {
		t.Fatalf("error %v, want the read error", err)
	}
	if n != int64(len(data)) || !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("wrote %d bytes before the error, want %d", n, len(data))
	}
}

type readTracker struct {
	r        io.Reader
	returned atomic.Bool
	late     atomic.Int32
}

func (r *readTracker) Read(p []byte) (int, error) {
	if r.returned.Load() {
		r.late.Add(1)
	}
	return r.r.Read(p)
}

type failingWriter struct{ after int }

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.after <= 0 {
		return 0, errors.New("disk full")
	}
	w.after--
	return len(p), nil
}

// hashMedia is called with a file that the caller closes right after it
// returns: nothing may still be reading it, whichever way it ended.
func TestReadAheadCopyStopsReadingBeforeItReturns(t *testing.T) {
	e := NewEngine(EngineDeps{})
	src := &readTracker{r: bytes.NewReader(readAheadData(32 * hashReadChunk))}
	if _, err := e.hashMedia(context.Background(), &failingWriter{after: 2}, src); err == nil || err.Error() != "disk full" {
		t.Fatalf("write error %v", err)
	}
	src.returned.Store(true)
	time.Sleep(20 * time.Millisecond)
	if got := src.late.Load(); got != 0 {
		t.Fatalf("%d reads after hashMedia returned", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	src = &readTracker{r: bytes.NewReader(readAheadData(32 * hashReadChunk))}
	cancel()
	if _, err := e.hashMedia(ctx, io.Discard, src); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled hash returned %v", err)
	}
	src.returned.Store(true)
	time.Sleep(20 * time.Millisecond)
	if got := src.late.Load(); got != 0 {
		t.Fatalf("%d reads after a cancelled hashMedia returned", got)
	}
}

// slowReader costs a fixed latency per Read, like a network mount round trip.
type slowReader struct {
	r     io.Reader
	delay time.Duration
}

func (r *slowReader) Read(p []byte) (int, error) {
	time.Sleep(r.delay)
	return r.r.Read(p)
}

// Reading ahead only helps where reads have latency and not just bandwidth.
// This keeps that claim honest: with the same per-call latency, large
// pipelined reads must beat 32 KiB reads by a wide margin. The thresholds are
// loose because the numbers are simulated, not a measurement of any NAS.
func TestReadAheadCopyBeatsSmallSerialReadsWhenReadsHaveLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	e := NewEngine(EngineDeps{})
	data := readAheadData(16 << 20)
	const latency = 300 * time.Microsecond
	start := time.Now()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, struct{ io.Reader }{&slowReader{r: bytes.NewReader(data), delay: latency}}, make([]byte, 32<<10)); err != nil {
		t.Fatal(err)
	}
	serial := time.Since(start)
	start = time.Now()
	h2 := sha256.New()
	if _, err := e.hashMedia(context.Background(), h2, &slowReader{r: bytes.NewReader(data), delay: latency}); err != nil {
		t.Fatal(err)
	}
	pipelined := time.Since(start)
	if !bytes.Equal(h.Sum(nil), h2.Sum(nil)) {
		t.Fatal("digests differ")
	}
	t.Logf("16 MiB with %v per read: 32 KiB serial %v, 1 MiB read-ahead %v", latency, serial, pipelined)
	if pipelined*2 > serial {
		t.Fatalf("read-ahead %v is not clearly faster than serial %v", pipelined, serial)
	}
}
