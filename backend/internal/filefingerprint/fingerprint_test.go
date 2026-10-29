package filefingerprint

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

type missingStat struct{ os.FileInfo }

func (missingStat) Sys() any { return nil }

func TestFingerprintRejectsFileMutationIncludingPrefixWithRestoredMtime(t *testing.T) {
	for _, mutation := range []string{"unchanged", "prefix preserve mtime", "append", "truncate", "replace inode", "new mtime"} {
		t.Run(mutation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "evidence")
			original := bytes.Repeat([]byte("a"), 512<<10)
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			fingerprint, available := Capture(before)
			if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
				if available {
					t.Fatal("unsupported platform silently accepted a weak witness")
				}
				return
			}
			if !available {
				t.Fatal("native change-time witness unavailable")
			}
			switch mutation {
			case "prefix preserve mtime":
				file, err := os.OpenFile(path, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := file.WriteAt([]byte("b"), 0)
				closeErr := file.Close()
				if writeErr != nil || closeErr != nil {
					t.Fatal(writeErr, closeErr)
				}
				if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
					t.Fatal(err)
				}
			case "append":
				file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := file.Write([]byte("more"))
				closeErr := file.Close()
				if writeErr != nil || closeErr != nil {
					t.Fatal(writeErr, closeErr)
				}
			case "truncate":
				if err := os.Truncate(path, 64); err != nil {
					t.Fatal(err)
				}
			case "replace inode":
				replacement := path + ".new"
				if err := os.WriteFile(replacement, original, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(replacement, before.ModTime(), before.ModTime()); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
			case "new mtime":
				if err := os.Chtimes(path, before.ModTime(), before.ModTime().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if mutation == "prefix preserve mtime" {
				data, err := os.ReadFile(path)
				if err != nil || !os.SameFile(before, after) || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || !bytes.Equal(data[len(data)-(64<<10):], original[len(original)-(64<<10):]) {
					t.Fatal("prefix mutation did not preserve audited weak-witness dimensions", err)
				}
			}
			if fingerprint.Matches(after) != (mutation == "unchanged") {
				t.Fatalf("%s passed an invalid identity/change-time comparison", mutation)
			}
		})
	}
}

func TestFingerprintRequiresRegularFileWithNativeChangeTime(t *testing.T) {
	directory, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(t.TempDir(), "regular")
	if err != nil {
		t.Fatal(err)
	}
	regular, err := file.Stat()
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	for _, info := range []os.FileInfo{nil, directory, missingStat{regular}} {
		if fingerprint, ok := Capture(info); ok || fingerprint.Matches(directory) {
			t.Fatal("unverified fingerprint was usable")
		}
	}
}
