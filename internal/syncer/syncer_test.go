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
