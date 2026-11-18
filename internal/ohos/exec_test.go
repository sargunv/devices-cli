package ohos

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// printingHDC writes an hdc whose shell prints body, as the device's shell would.
func printingHDC(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hdc")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s' '"+body+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStreamReportsTheMarkedStatusAndKeepsTheOutput(t *testing.T) {
	cases := map[string]struct {
		printed, want string
		status        int
	}{
		"output ends a line":   {"one\r\ntwo\r\n\r\n" + statusMarker + "3\r\n", "one\ntwo\n", 3},
		"output ends mid-line": {"one\ntwo\n" + statusMarker + "0\n", "one\ntwo", 0},
		"no output":            {"\n" + statusMarker + "0\n", "", 0},
		"blank lines kept":     {"one\n\n\n" + statusMarker + "1\n", "one\n\n", 1},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			status, err := hdcShell{path: printingHDC(t, c.printed), key: "k"}.stream(context.Background(), nil, &out, "ignored")
			if err != nil {
				t.Fatal(err)
			}
			if status != c.status || out.String() != c.want {
				t.Errorf("stream = %d, %q; want %d, %q", status, out.String(), c.status, c.want)
			}
		})
	}
}

func TestStreamFailsWithoutAStatus(t *testing.T) {
	_, err := hdcShell{path: printingHDC(t, "hdc lost the device\n"), key: "k"}.stream(context.Background(), nil, &bytes.Buffer{}, "ignored")
	if err == nil {
		t.Fatal("a shell that never reports a status succeeded")
	}
}
