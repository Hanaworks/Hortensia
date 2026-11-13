package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestGroupRejectsWindowsFileAndDirectoryAliases(t *testing.T) {
	for _, kind := range []string{"db-dot", "db-space", "identity-dot", "output-dot", "output-space", "parent-dot"} {
		t.Run(kind, func(t *testing.T) {
			req, inspect, _ := groupFixture(t)
			actual := req.DataOut
			switch kind {
			case "db-dot":
				req.Source.DatabaseFile += "."
			case "db-space":
				req.Source.DatabaseFile += " "
			case "identity-dot":
				req.Source.Materials[0].Path += "."
			case "output-dot":
				req.DataOut += "."
			case "output-space":
				req.DataOut += " "
			case "parent-dot":
				req.DataOut = filepath.Join(filepath.Dir(req.DataOut)+".", filepath.Base(req.DataOut))
			}
			if _, e := CreateGroup(context.Background(), req, inspect); e == nil {
				t.Fatal("Win32 alias accepted")
			}
			if _, e := os.Lstat(actual); !os.IsNotExist(e) {
				t.Fatal("alias created output")
			}
		})
	}
}
func TestGroupRejectsWindowsShortMaterialAlias(t *testing.T) {
	req, inspect, _ := groupFixture(t)
	longName := filepath.Join(filepath.Dir(req.Source.DatabaseFile), "identity-with-long-material-name.json")
	raw, e := os.ReadFile(req.Source.Materials[0].Path)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(longName, raw, 0600); e != nil {
		t.Fatal(e)
	}
	ptr, e := windows.UTF16PtrFromString(longName)
	if e != nil {
		t.Fatal(e)
	}
	buffer := make([]uint16, 32768)
	n, e := windows.GetShortPathName(ptr, &buffer[0], uint32(len(buffer)))
	if e != nil {
		t.Skipf("8.3 capability unavailable: %v", e)
	}
	short := windows.UTF16ToString(buffer[:n])
	if strings.EqualFold(short, longName) {
		t.Skip("filesystem has no distinct 8.3 alias")
	}
	req.Source.Materials[0].Path = short
	if _, e := CreateGroup(context.Background(), req, inspect); e == nil {
		t.Fatal("8.3 material alias accepted")
	}
	if _, e := os.Lstat(req.DataOut); !os.IsNotExist(e) {
		t.Fatal("8.3 alias created output")
	}
}
