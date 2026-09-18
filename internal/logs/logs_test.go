// SPDX-License-Identifier: Apache-2.0

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

func TestReadFromMissingFile(t *testing.T) {
	dir := t.TempDir()
	data, next, err := ReadFrom(dir, "nope", 0)
	if err != nil || len(data) != 0 || next != 0 {
		t.Fatalf("missing: data=%q next=%d err=%v", data, next, err)
	}
}

func TestReadFromOffset(t *testing.T) {
	dir := t.TempDir()
	w, err := Create(dir, "job-4", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("hello\nworld\n")); err != nil {
		t.Fatal(err)
	}
	w.Close()

	all, next, err := ReadFrom(dir, "job-4", 0)
	if err != nil || string(all) != "hello\nworld\n" || next != int64(len(all)) {
		t.Fatalf("offset 0: %q next=%d err=%v", all, next, err)
	}

	rest, next, err := ReadFrom(dir, "job-4", 6)
	if err != nil || string(rest) != "world\n" || next != 12 {
		t.Fatalf("mid-file: %q next=%d err=%v", rest, next, err)
	}

	past, next, err := ReadFrom(dir, "job-4", 99)
	if err != nil || len(past) != 0 || next != 99 {
		t.Fatalf("past EOF: %q next=%d err=%v", past, next, err)
	}

	end, next, err := ReadFrom(dir, "job-4", 12)
	if err != nil || len(end) != 0 || next != 12 {
		t.Fatalf("at EOF: %q next=%d err=%v", end, next, err)
	}
}

func TestWriterStripsANSI(t *testing.T) {
	dir := t.TempDir()
	w, err := Create(dir, "job-ansi", 0)
	if err != nil {
		t.Fatal(err)
	}
	// The reported line, plus an OSC title and a bare two-character escape.
	if _, err := w.Write([]byte("\x1b[42m\x1b[30m generating static routes \x1b[39m\x1b[49m\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("\x1b]0;build\x07done\x1b7\n")); err != nil {
		t.Fatal(err)
	}
	// A sequence split across two writes is the normal case on a pipe: the
	// filter must not leak the tail of it into the file.
	if _, err := w.Write([]byte("half\x1b[3")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("1mred\n")); err != nil {
		t.Fatal(err)
	}
	w.Close()
	body, err := Read(dir, "job-ansi")
	if err != nil {
		t.Fatal(err)
	}
	want := " generating static routes \ndone\nhalfred\n"
	if body != want {
		t.Fatalf("escapes survived:\ngot  %q\nwant %q", body, want)
	}
}

func TestWriteReportsEveryByteAccepted(t *testing.T) {
	dir := t.TempDir()
	w, err := Create(dir, "job-n", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	// io.Writer contract: a short count is an error to os/exec, which would
	// fail the step over colour we chose to drop.
	in := []byte("\x1b[32mok\x1b[0m")
	n, err := w.Write(in)
	if err != nil || n != len(in) {
		t.Fatalf("write: n=%d want %d err=%v", n, len(in), err)
	}
}
