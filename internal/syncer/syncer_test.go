package syncer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestEligibleForTransfer(t *testing.T) {
	now := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)

	if !eligibleForTransfer(now.Add(-31*time.Minute), now, QuietPeriod) {
		t.Fatal("expected file older than quiet period to be eligible")
	}

	if !eligibleForTransfer(now.Add(-30*time.Minute), now, QuietPeriod) {
		t.Fatal("expected file exactly on quiet period boundary to be eligible")
	}

	if eligibleForTransfer(now.Add(-29*time.Minute), now, QuietPeriod) {
		t.Fatal("expected recent file to be skipped")
	}
}

func TestTargetLocalPathPreservesStructure(t *testing.T) {
	localRoot := filepath.Clean(`C:\\data\\downloads`)
	got, err := targetLocalPath(localRoot, "/incoming", "/incoming/a/b/file.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := filepath.Join(localRoot, "a", "b", "file.txt")
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestRelativeRemotePathRejectsOutsideRoot(t *testing.T) {
	if _, err := relativeRemotePath("/incoming", "/other/file.txt"); err == nil {
		t.Fatal("expected error for path outside root")
	}
}

func TestIsFileInChildDirectory(t *testing.T) {
	tests := []struct {
		name       string
		remoteRoot string
		remotePath string
		want       bool
	}{
		{name: "root level file", remoteRoot: "/recordings", remotePath: "/recordings/167.log", want: false},
		{name: "child directory file", remoteRoot: "/recordings", remotePath: "/recordings/2026-05-13/cam1/video.mp4", want: true},
		{name: "outside root", remoteRoot: "/recordings", remotePath: "/other/video.mp4", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isFileInChildDirectory(tt.remoteRoot, tt.remotePath)
			if got != tt.want {
				t.Fatalf("expected %v, got %v", tt.want, got)
			}
		})
	}
}

func TestHiddenRemoteDir(t *testing.T) {
	tests := []struct {
		name       string
		remoteRoot string
		dirPath    string
		want       bool
	}{
		{name: "regular dir", remoteRoot: "/recordings", dirPath: "/recordings/2026-05-13", want: false},
		{name: "dot dir inside root", remoteRoot: "/recordings", dirPath: "/recordings/.git", want: true},
		{name: "nested dot dir", remoteRoot: "/recordings", dirPath: "/recordings/day/.cache", want: true},
		{name: "root itself is never hidden", remoteRoot: "/data/.incoming", dirPath: "/data/.incoming", want: false},
		{name: "dot dir inside dot root", remoteRoot: "/data/.incoming", dirPath: "/data/.incoming/.git", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hiddenRemoteDir(tt.remoteRoot, tt.dirPath)
			if got != tt.want {
				t.Fatalf("expected %v, got %v", tt.want, got)
			}
		})
	}
}

func TestCopyWithContextUsesWriterTo(t *testing.T) {
	reader := &writerToProbe{data: []byte("hello over sftp")}
	var dst bytes.Buffer

	written, err := copyWithContext(context.Background(), &dst, reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reader.usedWriterTo {
		t.Fatal("expected WriterTo path to be used")
	}
	if int(written) != len(reader.data) {
		t.Fatalf("expected %d bytes written, got %d", len(reader.data), written)
	}
	if dst.String() != string(reader.data) {
		t.Fatalf("expected %q, got %q", string(reader.data), dst.String())
	}
}

func TestCopyWithContextReturnsErrNoProgress(t *testing.T) {
	_, err := copyWithContext(context.Background(), io.Discard, zeroProgressReader{})
	if !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("expected io.ErrNoProgress, got %v", err)
	}
}

func TestCopyWithContextCancelsWriterToReader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := newBlockingWriterToReader()

	done := make(chan error, 1)
	go func() {
		_, err := copyWithContext(ctx, io.Discard, reader)
		done <- err
	}()

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("copyWithContext did not stop after cancellation")
	}
}

// shortReadAt simulates an SFTP server that returns fewer bytes than
// requested per ReadAt, which is the exact failure that motivated readAtFull.
type shortReadAt struct {
	data []byte
	max  int // per-call byte cap
}

func (s *shortReadAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := len(p)
	if s.max > 0 && n > s.max {
		n = s.max
	}
	if rem := len(s.data) - int(off); n > rem {
		n = rem
	}
	copy(p, s.data[off:off+int64(n)])
	if n < len(p) && off+int64(n) >= int64(len(s.data)) {
		return n, io.EOF
	}
	return n, nil
}

func (s *shortReadAt) Read(p []byte) (int, error) { return 0, io.EOF } // unused in these tests
func (s *shortReadAt) Close() error               { return nil }

func TestReadAtFullRetriesShortReads(t *testing.T) {
	data := []byte("0123456789abcdef")
	ra := &shortReadAt{data: data, max: 5}

	buf := make([]byte, len(data))
	n, err := readAtFull(ra, buf, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != len(data) {
		t.Fatalf("expected %d bytes, got %d", len(data), n)
	}
	if !bytes.Equal(buf, data) {
		t.Fatalf("expected %q, got %q", data, buf)
	}
}

func TestReadAtFullUnexpectedEOF(t *testing.T) {
	ra := &shortReadAt{data: []byte("short"), max: 3}
	buf := make([]byte, 10)
	_, err := readAtFull(ra, buf, 0)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected io.ErrUnexpectedEOF, got %v", err)
	}
}

func TestCopyRemoteFileConcurrentWritesInOrder(t *testing.T) {
	// 300 KB forces multiple chunks and multiple concurrent workers.
	size := 300 * 1024
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i * 31)
	}
	// 7KB per ReadAt forces the concurrent path to retry short reads heavily.
	src := &shortReadAt{data: data, max: 7 * 1024}
	var dst bytes.Buffer

	written, err := copyRemoteFileConcurrent(context.Background(), src, &dst, int64(size), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if written != int64(size) {
		t.Fatalf("expected %d bytes written, got %d", size, written)
	}
	if !bytes.Equal(dst.Bytes(), data) {
		t.Fatal("downloaded bytes differ from source data")
	}
}

func TestCopyRemoteFileConcurrentReportsReceiveProgress(t *testing.T) {
	// 进度回调按"已收到字节"统计：所有分块汇报的字节总和必须等于文件
	// 大小，与分块完成顺序无关。
	size := 300 * 1024
	data := make([]byte, size)
	src := &shortReadAt{data: data, max: 7 * 1024}

	var mu sync.Mutex
	var received int64
	var calls int
	_, err := copyRemoteFileConcurrent(context.Background(), src, io.Discard, int64(size), func(n int64) {
		mu.Lock()
		received += n
		calls++
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if received != int64(size) {
		t.Fatalf("expected %d received bytes reported, got %d", size, received)
	}
	if calls == 0 {
		t.Fatal("expected onReceive to be called at least once")
	}
}

func TestCopyRemoteFileConcurrentCancellation(t *testing.T) {
	size := int64(downloadChunkSize * 4)
	data := make([]byte, size)
	src := &shortReadAt{data: data, max: 1024}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	_, err := copyRemoteFileConcurrent(ctx, src, io.Discard, size, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestProgressTrackerThrottlesAndCounts(t *testing.T) {
	var lastReceived int64
	var updates int
	pt := &progressTracker{
		// 让首次 add 立即触发上报。
		lastTime: time.Now().Add(-time.Second),
		onUpdate: func(received int64, _ float64) {
			lastReceived = received
			updates++
		},
	}
	pt.add(100)
	if updates != 1 || lastReceived != 100 {
		t.Fatalf("expected first add to report 100, got updates=%d received=%d", updates, lastReceived)
	}
	// 距上次上报不足 250ms，不应再次触发，但字节数应继续累计。
	pt.add(50)
	if updates != 1 {
		t.Fatalf("expected update to be throttled, got %d updates", updates)
	}
	pt.lastTime = time.Now().Add(-time.Second)
	pt.add(25)
	if updates != 2 || lastReceived != 175 {
		t.Fatalf("expected cumulative 175, got updates=%d received=%d", updates, lastReceived)
	}
}

type writerToProbe struct {
	data         []byte
	usedWriterTo bool
	readOffset   int
}

func (p *writerToProbe) Read(b []byte) (int, error) {
	if p.readOffset >= len(p.data) {
		return 0, io.EOF
	}
	n := copy(b, p.data[p.readOffset:])
	p.readOffset += n
	return n, nil
}

func (p *writerToProbe) WriteTo(w io.Writer) (int64, error) {
	p.usedWriterTo = true
	n, err := w.Write(p.data)
	return int64(n), err
}

type zeroProgressReader struct{}

func (zeroProgressReader) Read([]byte) (int, error) {
	return 0, nil
}

type blockingWriterToReader struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingWriterToReader() *blockingWriterToReader {
	return &blockingWriterToReader{closed: make(chan struct{})}
}

func (r *blockingWriterToReader) Read([]byte) (int, error) {
	<-r.closed
	return 0, os.ErrClosed
}

func (r *blockingWriterToReader) WriteTo(io.Writer) (int64, error) {
	<-r.closed
	return 0, os.ErrClosed
}

func (r *blockingWriterToReader) Close() error {
	r.once.Do(func() {
		close(r.closed)
	})
	return nil
}
