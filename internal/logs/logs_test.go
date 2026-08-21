package logs

import (
	"strings"
	"testing"
)

func TestWriterTruncatesAtCap(t *testing.T) {
	dir := t.TempDir()
	w, err := Create(dir, "job-1", 512)
	if err != nil {
		t.Fatal(err)
	}
	blob := strings.Repeat("x", 4096)
	// A capped write must report success: a full log stops the log, not the job.
	n, err := w.Write([]byte(blob))
	if err != nil || n != len(blob) {
		t.Fatalf("write: n=%d err=%v", n, err)
	}
	if _, err := w.Write([]byte(blob)); err != nil {
		t.Fatalf("write after cap: %v", err)
	}
	w.Close()
	if !w.Truncated() {
		t.Fatal("cap was not recorded")
	}
	body, err := Read(dir, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > 512+200 {
		t.Fatalf("log grew past the cap: %d bytes", len(body))
	}
	if !strings.Contains(body, "log truncated") {
		t.Fatalf("truncation is not explained in the log:\n%s", body)
	}
}

func TestWriterUnlimited(t *testing.T) {
	dir := t.TempDir()
	w, _ := Create(dir, "job-2", 0)
	w.Printf("hello %s\n", "world")
	w.Close()
	if w.Truncated() {
		t.Fatal("an unlimited writer must not truncate")
	}
	body, _ := Read(dir, "job-2")
	if body != "hello world\n" {
		t.Fatalf("body: %q", body)
	}
	if w.Bytes() != int64(len(body)) {
		t.Fatalf("byte count %d vs %d", w.Bytes(), len(body))
	}
}

func TestTailKeepsTheEnd(t *testing.T) {
	dir := t.TempDir()
	w, _ := Create(dir, "job-3", 0)
	for i := 0; i < 500; i++ {
		w.Printf("line %d\n", i)
	}
	w.Printf("the-last-line\n")
	w.Close()

	tail, err := Tail(dir, "job-3", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) > 300 {
		t.Fatalf("tail too long: %d", len(tail))
	}
	if !strings.Contains(tail, "the-last-line") {
		t.Fatalf("tail lost the end of the log:\n%s", tail)
	}
	if !strings.Contains(tail, "earlier output omitted") {
		t.Fatal("a truncated tail should say so")
	}
}

func TestMissingLogIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	if body, err := Read(dir, "nope"); err != nil || body != "" {
		t.Fatalf("read: %q %v", body, err)
	}
	if tail, err := Tail(dir, "nope", 100); err != nil || tail != "" {
		t.Fatalf("tail: %q %v", tail, err)
	}
	if err := Delete(dir, "nope"); err != nil {
		t.Fatalf("delete: %v", err)
	}
}
