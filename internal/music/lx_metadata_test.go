package music

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestOfficialLXAdapterContract(t *testing.T) {
	b, e := os.ReadFile("testdata/lx_music_metadata_contract.json")
	if e != nil {
		t.Fatal(e)
	}
	var f struct {
		Rows []struct {
			Source     string
			LXMetadata LXMetadata
			Expected   map[string]any
		}
	}
	if e = json.Unmarshal(b, &f); e != nil {
		t.Fatal(e)
	}
	for _, row := range f.Rows {
		got := row.LXMetadata.ScriptFields(row.Source)
		for k, v := range got {
			a, _ := json.Marshal(v)
			b, _ := json.Marshal(row.Expected[k])
			if string(a) != string(b) {
				t.Fatalf("%s.%s: %s != %s", row.Source, k, a, b)
			}
		}
	}
	m := LXMetadata{}.WithQuality("128k", 4194304)
	if !reflect.DeepEqual(m.Qualities["128k"], LXQuality{Size: "4.00 MiB", Bytes: 4194304}) {
		t.Fatal(m)
	}
}
