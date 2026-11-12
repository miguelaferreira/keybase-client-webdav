package main

import (
	"testing"
	"time"
)

func TestParseLs(t *testing.T) {
	now := time.Date(2026, 11, 12, 13, 0, 0, 0, time.Local)
	out := []byte("drw-rw-r--      1366 Jul 20 2022 dev\n" +
		"-rw-rw-r--      2006 Nov 03 09:15 name with  spaces.txt\n" +
		"-rw-rw-r--         1 Dec 30 10:00 from-last-year\n" +
		"not a listing line\n")
	got := parseLs(out, now)
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3", len(got))
	}
	if got[0].Name() != "dev" || !got[0].IsDir() || got[0].Size() != 1366 {
		t.Errorf("entry 0 = %+v", got[0])
	}
	if got[1].Name() != "name with  spaces.txt" || got[1].IsDir() || got[1].ModTime().Year() != 2026 {
		t.Errorf("entry 1 = %+v", got[1])
	}
	if got[2].ModTime().Year() != 2025 {
		t.Errorf("a future date without a year must roll back to last year, got %v", got[2].ModTime())
	}
}

func TestParseStat(t *testing.T) {
	out := []byte("/private/testuser/api-env\n2023-03-16 13:51:05 CET\tFILE\t2006\tapi-env\ttestuser\tCOMPLETE\n")
	i, err := parseStat(out, "/private/testuser/api-env")
	if err != nil {
		t.Fatal(err)
	}
	if i.IsDir() || i.Size() != 2006 || i.Name() != "api-env" {
		t.Errorf("info = %+v", i)
	}

	out = []byte("/public/testuser\n0001-01-01 00:00:00 UTC\tDIR\t0\t\t\tNOT_STARTED\n")
	i, err = parseStat(out, "/public/testuser")
	if err != nil {
		t.Fatal(err)
	}
	if !i.IsDir() || i.ModTime().Unix() != 0 {
		t.Errorf("zero time must map to the epoch, got %+v", i)
	}
}

func TestKbPathCannotEscapeKeybase(t *testing.T) {
	for in, want := range map[string]string{
		"/":                    "/keybase",
		"/private/testuser/x":  "/keybase/private/testuser/x",
		"/../../etc/passwd":    "/keybase/etc/passwd",
		"private/../../public": "/keybase/public",
	} {
		if got := kbPath(in); got != want {
			t.Errorf("kbPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsJunk(t *testing.T) {
	for name, want := range map[string]bool{
		"/a/.DS_Store": true, "/a/._x": true, "/a/x": false, "/a/.hidden": false,
	} {
		if isJunk(name) != want {
			t.Errorf("isJunk(%q) != %v", name, want)
		}
	}
}
