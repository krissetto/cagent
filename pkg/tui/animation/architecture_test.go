package animation

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSingleGlobalTimingSourceArchitecture(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate animation package")
	}
	root := filepath.Dir(file)
	forbidden := []string{"SubscribeEvery", "NewSubscriptionEvery", "startEvery", "registerEvery", "unregisterEvery", "ScheduledFor"}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Ext(path) != ".go" {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(data)
		for _, symbol := range forbidden {
			if strings.Contains(text, symbol) && path != file {
				t.Errorf("%s contains forbidden alternate timing API %q", path, symbol)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if TickRate <= 0 {
		t.Fatal("global TickRate must be positive")
	}
}
