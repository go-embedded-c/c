package headers

import (
	"io/fs"
	"testing"
)

func TestOverlay(t *testing.T) {
	for _, tc := range []struct {
		name, wantFrom string // wantFrom: the embedded file that must be served
	}{
		{AllDir + "/stdio.h", WasiDir + "/stdio.h"},
		{AllDir + "/stdint.h", ClangDir + "/stdint.h"}, // clang's first
		{WasiDir + "/stdint.h", WasiDir + "/stdint.h"}, // what #include_next reaches
		{`include\all\stdio.h`, WasiDir + "/stdio.h"},  // cc/v4's hasFile on Windows
	} {
		got, err := fs.ReadFile(FS(), tc.name)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		want, err := fs.ReadFile(tree, tc.wantFrom)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("%s did not serve %s", tc.name, tc.wantFrom)
		}
	}
	if _, err := FS().Open(AllDir + "/nosuch.h"); err == nil {
		t.Error("a missing header opened")
	}
}
